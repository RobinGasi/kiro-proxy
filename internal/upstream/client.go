// Package upstream is the Kiro transport+retry layer. It POSTs conversation
// payloads to the GenerateAssistantResponse endpoint and lists the models Kiro
// currently exposes, applying a consistent retry/backoff and 403-refresh policy.
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/util"
)

// Endpoint paths on the Kiro runtime hosts.
const (
	generateAssistantResponsePath = "/generateAssistantResponse"
	listAvailableModelsPath       = "/ListAvailableModels"
)

// Body-size caps for the small, fully-buffered responses (error payloads and the
// model list). The streaming GenerateAssistantResponse body is never buffered.
const (
	maxErrorBodyBytes = 1 << 20 // 1 MiB
	maxModelListBytes = 4 << 20 // 4 MiB
)

// AuthProvider supplies the per-request credentials and host routing the
// transport needs. *auth.KiroAuthManager already satisfies this interface.
type AuthProvider interface {
	// GetAccessToken returns a currently-valid bearer token, refreshing it
	// transparently if it is close to expiry.
	GetAccessToken(ctx context.Context) (string, error)
	// ForceRefresh unconditionally refreshes the token and returns the new one.
	ForceRefresh(ctx context.Context) (string, error)
	// ProfileARN returns the AWS CodeWhisperer profile ARN, or "" if unset.
	ProfileARN() string
	// APIHost is the runtime host for GenerateAssistantResponse.
	APIHost() string
	// QHost is the runtime host for ListAvailableModels.
	QHost() string
	// Fingerprint is the stable per-installation machine fingerprint.
	Fingerprint() string
}

// Client is the Kiro transport. It is safe for concurrent use; all per-request
// state is local and the underlying *http.Client handles connection pooling.
type Client struct {
	hc  *http.Client
	cfg *config.Config
	log *slog.Logger
}

// New constructs a Client. hc is the shared upstream *http.Client (typically
// built by package transport), cfg supplies the retry/backoff knobs and the
// spoofed Kiro client version, and log receives retry diagnostics. A nil log
// falls back to slog.Default.
func New(hc *http.Client, cfg *config.Config, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{hc: hc, cfg: cfg, log: log}
}

// SendConversation POSTs the already-serialized conversationState JSON payload to
// the GenerateAssistantResponse endpoint at am.APIHost() and returns the raw
// *http.Response with its Body still OPEN, so the caller can stream the AWS
// event-stream and close it. The x-amz-target header (set by util.KiroHeaders)
// selects the operation.
//
// Retry policy:
//   - 200: returned immediately, body open.
//   - 403: am.ForceRefresh is called and the request retried with the new token.
//   - 429 / 5xx: exponential backoff (cfg.BaseRetryDelay * 2^attempt) up to the
//     retry budget; after exhaustion the last such response is returned so the
//     caller sees the real status and error body.
//   - network/timeout errors: retried with the same backoff (TLS/certificate
//     failures are treated as fatal and not retried).
//
// The retry budget is cfg.FirstTokenMaxRetries when stream is true (the
// streaming path may legitimately stall before the first token) and
// cfg.MaxRetries otherwise.
//
// Any other status (notably 400) is returned as-is with the body open. This is
// deliberate (Bug #4): the transport does not interpret or mask the upstream
// error, so the caller can read the {"message","reason"} body and use
// auth.ClassifyError to tell a genuine bad/unknown model id (INVALID_MODEL_ID)
// apart from a recoverable subscription gap.
func (c *Client) SendConversation(ctx context.Context, am AuthProvider, payload []byte, stream bool) (*http.Response, error) {
	url := am.APIHost() + generateAssistantResponsePath
	return c.doWithRetry(ctx, am, stream, func(token string) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	})
}

// ListAvailableModels calls Kiro's ListAvailableModels operation at am.QHost()
// (GET with origin=AI_EDITOR, plus profileArn when the account has one) and
// returns the model IDs. It uses the same retry/backoff/403-refresh policy as
// SendConversation.
//
// On any failure it returns a non-nil error (so callers can fall back to a
// static list); when the upstream responds with a non-200 the error includes the
// upstream detail (message and reason) rather than masking it.
func (c *Client) ListAvailableModels(ctx context.Context, am AuthProvider) ([]string, error) {
	url := am.QHost() + listAvailableModelsPath
	resp, err := c.doWithRetry(ctx, am, false, func(token string) (*http.Request, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if rerr != nil {
			return nil, rerr
		}
		q := req.URL.Query()
		q.Set("origin", "AI_EDITOR")
		if arn := am.ProfileARN(); arn != "" {
			q.Set("profileArn", arn)
		}
		req.URL.RawQuery = q.Encode()
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list available models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list available models: upstream returned %d: %s",
			resp.StatusCode, readErrorDetail(resp))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes))
	if err != nil {
		return nil, fmt.Errorf("list available models: read response: %w", err)
	}
	var parsed struct {
		Models []struct {
			ModelID string `json:"modelId"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("list available models: decode response: %w", err)
	}
	ids := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.ModelID != "" {
			ids = append(ids, m.ModelID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("list available models: response contained no models")
	}
	return ids, nil
}

// doWithRetry runs build (which produces a fresh request for the current token)
// under the shared retry/backoff/403-refresh policy and returns the resulting
// response. The returned response (success or exhausted 429/5xx) has its Body
// readable; the caller owns closing it.
func (c *Client) doWithRetry(ctx context.Context, am AuthProvider, stream bool, build func(token string) (*http.Request, error)) (*http.Response, error) {
	maxRetries := c.cfg.MaxRetries
	if stream {
		maxRetries = c.cfg.FirstTokenMaxRetries
	}
	if maxRetries < 1 {
		maxRetries = 1
	}

	var (
		lastResp *http.Response
		lastErr  error
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		token, err := am.GetAccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("get access token: %w", err)
		}

		req, err := build(token)
		if err != nil {
			return nil, err
		}
		for k, v := range util.KiroHeaders(token, am.Fingerprint(), c.cfg.KiroClientVersion) {
			req.Header.Set(k, v)
		}
		if stream {
			// Issue #38: avoid CLOSE_WAIT connection leaks on streamed responses
			// by not returning the connection to the pool.
			req.Close = true
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if !isRetryableNetErr(err) {
				break
			}
			if attempt == maxRetries-1 {
				break
			}
			delay := c.backoff(attempt)
			c.log.Warn("upstream network error, backing off",
				"attempt", attempt+1, "max", maxRetries, "delay", delay, "err", err)
			if serr := sleepCtx(ctx, delay); serr != nil {
				return nil, serr
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil

		case resp.StatusCode == http.StatusForbidden:
			// Token expired/invalid: refresh and retry with the new token.
			drain(resp)
			c.log.Warn("upstream returned 403, refreshing token",
				"attempt", attempt+1, "max", maxRetries)
			if _, rerr := am.ForceRefresh(ctx); rerr != nil {
				return nil, fmt.Errorf("token refresh after 403: %w", rerr)
			}
			continue

		case resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode < 600):
			// Rate limit or server error: buffer the body (so it survives the
			// connection being reused on the next attempt) and back off.
			buffered, berr := bufferResponse(resp)
			if berr != nil {
				lastErr = berr
			} else {
				lastResp = buffered
			}
			if attempt == maxRetries-1 {
				break
			}
			delay := c.backoff(attempt)
			c.log.Warn("upstream returned retryable status, backing off",
				"status", resp.StatusCode, "attempt", attempt+1, "max", maxRetries, "delay", delay)
			if serr := sleepCtx(ctx, delay); serr != nil {
				return nil, serr
			}
			continue

		default:
			// Any other status (e.g. 400) is returned untouched so the caller can
			// read and classify the upstream error body (Bug #4).
			return resp, nil
		}
	}

	// Retries exhausted: prefer returning the last retryable HTTP response (so
	// the caller sees the real status/body), otherwise surface the last error.
	if lastResp != nil {
		c.log.Warn("upstream retries exhausted, returning last response",
			"status", lastResp.StatusCode)
		return lastResp, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("request failed after %d attempt(s): %w", maxRetries, lastErr)
	}
	return nil, fmt.Errorf("request failed after %d attempt(s)", maxRetries)
}

// backoff returns the exponential backoff delay for the given zero-based attempt:
// cfg.BaseRetryDelay * 2^attempt seconds.
func (c *Client) backoff(attempt int) time.Duration {
	secs := c.cfg.BaseRetryDelay * math.Pow(2, float64(attempt))
	return time.Duration(secs * float64(time.Second))
}

// sleepCtx sleeps for d, returning early with ctx.Err() if the context is
// cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isRetryableNetErr reports whether a transport error is worth retrying. TLS and
// certificate-verification failures are persistent and not retried; all other
// transport errors (timeouts, resets, refused connections, DNS) are.
func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return false
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return false
	}
	return true
}

// drain reads and discards the remainder of resp.Body and closes it, freeing the
// connection for reuse.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
}

// bufferResponse reads resp.Body fully into memory, closes the original body, and
// replaces it with an in-memory reader so resp stays usable after the underlying
// connection is reused on the next retry.
func bufferResponse(resp *http.Response) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// readErrorDetail extracts a human-readable detail from a Kiro error response
// body, which is typically {"message": "...", "reason": "..."}.
func readErrorDetail(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if len(body) == 0 {
		return "(empty response body)"
	}
	var kerr struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(body, &kerr) == nil && (kerr.Message != "" || kerr.Reason != "") {
		msg := strings.TrimSpace(kerr.Message)
		if kerr.Reason != "" {
			if msg == "" {
				return "reason: " + kerr.Reason
			}
			return fmt.Sprintf("%s (reason: %s)", msg, kerr.Reason)
		}
		return msg
	}
	return strings.TrimSpace(string(body))
}
