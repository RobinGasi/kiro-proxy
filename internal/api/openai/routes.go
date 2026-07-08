package openai

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/RobinGasi/kiro-proxy/internal/account"
	"github.com/RobinGasi/kiro-proxy/internal/apiauth"
	"github.com/RobinGasi/kiro-proxy/internal/config"
	"github.com/RobinGasi/kiro-proxy/internal/converter"
	"github.com/RobinGasi/kiro-proxy/internal/mcp"
	"github.com/RobinGasi/kiro-proxy/internal/model"
	"github.com/RobinGasi/kiro-proxy/internal/payloadguard"
	"github.com/RobinGasi/kiro-proxy/internal/streaming"
	"github.com/RobinGasi/kiro-proxy/internal/upstream"
	"github.com/RobinGasi/kiro-proxy/internal/usage"
)

// Deps carries the collaborators a Handler needs.
type Deps struct {
	Cfg      *config.Config
	Accounts *account.Manager
	Upstream *upstream.Client
	MCP      *mcp.Client
	Logger   *slog.Logger
}

// Handler serves the OpenAI-compatible API surface.
type Handler struct {
	cfg      *config.Config
	accounts *account.Manager
	upstream *upstream.Client
	mcp      *mcp.Client
	log      *slog.Logger
}

// NewHandler builds a Handler from its dependencies.
func NewHandler(d Deps) *Handler {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		cfg:      d.Cfg,
		accounts: d.Accounts,
		upstream: d.Upstream,
		mcp:      d.MCP,
		log:      log,
	}
}

// RegisterRoutes wires the OpenAI endpoints onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", h.handleModels)
	mux.HandleFunc("GET /v1/usage", h.handleUsage)
}

// handleModels returns the available models in OpenAI format.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if !apiauth.CheckKey(r, h.cfg.ProxyAPIKeys) {
		h.writeError(w, http.StatusUnauthorized, "Invalid or missing API key")
		return
	}
	ids := h.accounts.AllAvailableModels()
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, Model{ID: id, Object: "model", OwnedBy: "anthropic"})
	}
	writeJSON(w, http.StatusOK, ModelList{Object: "list", Data: models})
}

// handleUsage returns the account's credit balance. It degrades to a JSON error
// rather than failing hard when the upstream usage call cannot be made.
func (h *Handler) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !apiauth.CheckKey(r, h.cfg.ProxyAPIKeys) {
		h.writeError(w, http.StatusUnauthorized, "Invalid or missing API key")
		return
	}
	ctx := r.Context()
	acct, err := h.accounts.Pick(ctx)
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, "No available accounts")
		return
	}
	limits, err := usage.FetchLimits(ctx, acct.Auth(), nil, h.cfg.KiroClientVersion)
	if err != nil {
		h.log.Warn("openai: usage fetch failed", "err", err)
		h.writeError(w, http.StatusBadGateway, "Failed to fetch usage: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_credits":     limits.TotalCredits,
		"used_credits":      limits.UsedCredits,
		"remaining_credits": limits.RemainingCredits,
	})
}

// handleChatCompletions implements the full request flow for
// POST /v1/chat/completions.
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !apiauth.CheckKey(r, h.cfg.ProxyAPIKeys) {
		h.writeError(w, http.StatusUnauthorized, "Invalid or missing API key")
		return
	}

	ctx := r.Context()
	var req ChatCompletionRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	acct, err := h.accounts.Pick(ctx)
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, "No available accounts")
		return
	}
	am := acct.Auth()
	res := acct.Resolver().Resolve(req.Model)

	// Web-search wiring (mirrors the Anthropic surface): Path B plus Path A, added
	// for parity since the mcp package already supports the OpenAI response shape.
	// The typed tools are rendered as raw maps so the mcp helpers can detect a
	// native server-side tool (Path A) and inject the emulation tool (Path B).
	var rawTools []map[string]any
	if len(req.Tools) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			_ = json.Unmarshal(b, &rawTools)
		}
	}

	// Path A — native, client-supplied server-side web_search tool: fulfill it
	// directly via the Kiro MCP API and return, regardless of cfg.WebSearchEnabled.
	if h.mcp != nil && mcp.HasNativeWebSearch(rawTools) {
		h.log.Info("openai: native web_search (Path A)")
		query := mcp.ExtractQueryFromText(lastUserText(&req))
		if err := h.mcp.HandleNativeWebSearch(ctx, w, am, mcp.NativeWebSearchParams{
			Model:     req.Model,
			Query:     query,
			Messages:  messagesAsMaps(&req),
			Stream:    req.Stream,
			APIFormat: mcp.APIFormatOpenAI,
		}); err != nil {
			h.log.Error("openai: native web_search response write failed", "error", err)
		}
		return
	}

	// Path B — MCP emulation: inject the web_search tool so the model can call it,
	// and rewrite req.Tools so the injected tool reaches Kiro. The model's
	// web_search tool_use is then intercepted in the streaming layer.
	if h.cfg.WebSearchEnabled {
		if injected := mcp.InjectOpenAIWebSearchTool(rawTools, true); len(injected) != len(rawTools) {
			rawTools = injected
			if b, err := json.Marshal(rawTools); err == nil {
				var tools []Tool
				if json.Unmarshal(b, &tools) == nil {
					req.Tools = tools
				}
			}
		}
	}

	convReq := toConverterRequest(&req)

	// toolNameReverse restores any tool names that BuildKiroPayload aliases to
	// satisfy Kiro's 64-character limit (nil when nothing was shortened).
	payload, toolNameReverse, err := converter.BuildKiroPayload(ctx, h.cfg, convReq, am.ProfileARN(), res.InternalID)
	if err != nil {
		var tooLarge *payloadguard.PayloadTooLargeError
		if errors.As(err, &tooLarge) {
			h.writeError(w, http.StatusRequestEntityTooLarge, tooLarge.Error())
			return
		}
		if errors.Is(err, converter.ErrNoMessages) {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.writeError(w, http.StatusBadRequest, "Failed to build request: "+err.Error())
		return
	}

	start := func(ctx context.Context) (*http.Response, error) {
		return h.upstream.SendConversation(ctx, am, payload, req.Stream)
	}

	contextLimit := model.ContextLimitFor(res.Normalized, h.cfg.DefaultMaxInputTokens)
	reasoningMode := h.cfg.FakeReasoningHandling

	if req.Stream {
		h.serveStream(w, r, &req, acct, am, contextLimit, reasoningMode, toolNameReverse, start)
		return
	}
	h.serveNonStream(w, r, &req, acct, contextLimit, reasoningMode, toolNameReverse, start)
}

// serveStream drives a streaming response.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, req *ChatCompletionRequest, acct *account.Account, am mcp.AuthManager, contextLimit int, reasoningMode string, toolNameReverse map[string]string, start func(context.Context) (*http.Response, error)) {
	evCh, errCh, err := streaming.StreamWithFirstTokenRetry(r.Context(), h.cfg, start)
	if err != nil {
		h.accounts.RecordFailure(acct, http.StatusBadGateway, err.Error())
		h.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Peek the first event so an upstream error can be returned with a real
	// status code before the SSE headers are committed.
	first, ok := <-evCh
	if !ok {
		streamErr := <-errCh
		status, msg := classifyStreamError(streamErr)
		h.accounts.RecordFailure(acct, status, msg)
		h.writeError(w, status, msg)
		return
	}

	h.accounts.RecordSuccess(acct)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if streamErr := streamToWriter(r.Context(), h.mcp, am, w, req.Model, contextLimit, reasoningMode, toolNameReverse, first, evCh, errCh); streamErr != nil {
		h.log.Error("openai: streaming error after headers", "err", streamErr)
	}
}

// serveNonStream drives a non-streaming response.
func (h *Handler) serveNonStream(w http.ResponseWriter, r *http.Request, req *ChatCompletionRequest, acct *account.Account, contextLimit int, reasoningMode string, toolNameReverse map[string]string, start func(context.Context) (*http.Response, error)) {
	evCh, errCh, err := streaming.StreamWithFirstTokenRetry(r.Context(), h.cfg, start)
	if err != nil {
		h.accounts.RecordFailure(acct, http.StatusBadGateway, err.Error())
		h.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	result, err := streaming.CollectStreamResult(evCh, errCh)
	if err != nil {
		status, msg := classifyStreamError(err)
		h.accounts.RecordFailure(acct, status, msg)
		h.writeError(w, status, msg)
		return
	}

	h.accounts.RecordSuccess(acct)

	resp := buildNonStreamingResponse(req.Model, contextLimit, reasoningMode, toolNameReverse, result)
	if rem, ok := creditsRemaining(result.Usage); ok {
		w.Header().Set("x-kiro-credits-remaining", rem)
	}
	writeJSON(w, http.StatusOK, resp)
}

// lastUserText returns the text of the last user message, used to derive the
// web_search query for Path A. It falls back to the last message of any role
// when no user message is present.
func lastUserText(req *ChatCompletionRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(req.Messages[i].Role), "user") {
			return req.Messages[i].Content.Text()
		}
	}
	if n := len(req.Messages); n > 0 {
		return req.Messages[n-1].Content.Text()
	}
	return ""
}

// messagesAsMaps renders the request messages as plain maps for the MCP
// token-counting path (HandleNativeWebSearch), via a marshal/unmarshal round-trip.
func messagesAsMaps(req *ChatCompletionRequest) []map[string]any {
	var msgs []map[string]any
	if b, err := json.Marshal(req.Messages); err == nil {
		_ = json.Unmarshal(b, &msgs)
	}
	return msgs
}

// classifyStreamError maps a terminal stream error to an HTTP status and a
// client-facing message, parsing upstream error bodies when available.
func classifyStreamError(err error) (int, string) {
	if err == nil {
		return http.StatusInternalServerError, "Unknown streaming error"
	}
	var statusErr *streaming.UpstreamStatusError
	if errors.As(err, &statusErr) {
		msg := statusErr.Body
		if m := parseUpstreamMessage(statusErr.Body); m != "" {
			msg = m
		}
		return statusErr.StatusCode, msg
	}
	return http.StatusBadGateway, err.Error()
}

// parseUpstreamMessage extracts the human-readable message from a Kiro error
// body of the form {"message": "...", "reason": "..."}.
func parseUpstreamMessage(body string) string {
	if body == "" {
		return ""
	}
	var parsed struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal([]byte(body), &parsed) != nil {
		return ""
	}
	if parsed.Message != "" {
		return parsed.Message
	}
	return parsed.Reason
}

// writeError emits an OpenAI-shaped error envelope.
func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "kiro_api_error",
			"code":    status,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
