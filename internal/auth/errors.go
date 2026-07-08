// Package auth implements the Kiro/AWS SSO token lifecycle: credential loading
// (env, JSON files, kiro-cli SQLite), automatic refresh for both Kiro Desktop
// Auth and AWS SSO OIDC, and error classification for account failover.
package auth

import "errors"

// ErrReLoginRequired indicates that the underlying credentials can no longer be
// refreshed and a fresh interactive login is required (e.g. the AWS SSO client
// registration has expired, or the refresh token was invalidated and no valid
// access token remains). Callers can test for it with errors.Is.
var ErrReLoginRequired = errors.New("re-login required")

// ErrorType classifies a Kiro API error for the multi-account failover logic.
type ErrorType string

const (
	// ErrorTypeFatal indicates the request itself is at fault (bad payload,
	// context overflow, validation error, server error). Retrying on another
	// account will not help, so the error should be returned to the client.
	ErrorTypeFatal ErrorType = "fatal"

	// ErrorTypeRecoverable indicates the account is at fault (expired token,
	// rate limit, quota/billing). The request should be retried on another
	// available account.
	ErrorTypeRecoverable ErrorType = "recoverable"
)

// ClassifyError decides whether a Kiro API error is account-specific
// (RECOVERABLE → try the next account) or request-specific (FATAL → return to
// the client immediately), based on the HTTP status code and the optional Kiro
// error reason (empty string means "no reason").
//
// RECOVERABLE:
//   - 400 + "INVALID_MODEL_ID" (model may be unavailable on this subscription)
//   - 402 (payment required / monthly quota exceeded / billing)
//   - 403 (token expired or invalid)
//   - 429 (rate limit exceeded)
//
// FATAL:
//   - 400 + "CONTENT_LENGTH_EXCEEDS_THRESHOLD" (context overflow)
//   - 400 + any other/empty reason (malformed request)
//   - 422 (validation error)
//   - 5xx (Kiro server error)
//   - anything else (treated as fatal to avoid wasting retries)
func ClassifyError(statusCode int, reason string) ErrorType {
	switch {
	case statusCode == 402:
		// Payment required (quota/billing). Kiro returns 402 for
		// MONTHLY_REQUEST_COUNT.
		return ErrorTypeRecoverable
	case statusCode == 403:
		// Token expired/invalid.
		return ErrorTypeRecoverable
	case statusCode == 429:
		// Rate limit exceeded.
		return ErrorTypeRecoverable
	case statusCode == 400:
		switch reason {
		case "INVALID_MODEL_ID":
			// Model not available on this account (subscription level);
			// a different account may have access.
			return ErrorTypeRecoverable
		case "CONTENT_LENGTH_EXCEEDS_THRESHOLD":
			// Context overflow — will fail on every account.
			return ErrorTypeFatal
		default:
			// Generic bad request (malformed payload / validation),
			// including "Improperly formed request" with no reason.
			return ErrorTypeFatal
		}
	case statusCode == 422:
		// Validation error (malformed request).
		return ErrorTypeFatal
	case statusCode >= 500 && statusCode < 600:
		// Server errors. Retrying on another account won't help if Kiro is down.
		return ErrorTypeFatal
	default:
		// Unknown errors are treated as fatal to avoid wasting retries.
		return ErrorTypeFatal
	}
}
