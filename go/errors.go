package rapidworkers

import (
	"errors"
	"fmt"
	"time"
)

// ErrNoAPIKey is returned by [NewClient] and [NewClientFromEnv] when there is
// no API key.
var ErrNoAPIKey = errors.New("rapidworkers: no API key: pass one to NewClient or set " + APIKeyEnv +
	"; create a key at " + APIKeysURL)

// APIError is an HTTP or JSON-RPC error answer from RapidWorkers.
//
// [AuthenticationError], [RateLimitError] and [ServiceUnavailableError] wrap
// an APIError, so errors.As(err, &apiErr) with apiErr of type *APIError
// matches all of them.
type APIError struct {
	// StatusCode is the HTTP status of the answer.
	StatusCode int
	// Code is the JSON-RPC error code, or 0 when the answer had none.
	Code int
	// Message describes the error.
	Message string
	// Data is the JSON-RPC error data, if any.
	Data any
}

func (e *APIError) Error() string {
	status := fmt.Sprintf("HTTP %d", e.StatusCode)
	if e.Code != 0 {
		status += fmt.Sprintf(", code %d", e.Code)
	}
	return fmt.Sprintf("rapidworkers: %s (%s)", e.Message, status)
}

// AuthenticationError means the API key is missing, invalid or revoked, or
// the account cannot use it (HTTP 401).
type AuthenticationError struct {
	APIError
}

// Unwrap returns the underlying [APIError].
func (e *AuthenticationError) Unwrap() error { return &e.APIError }

// RateLimitError means too many requests were sent with this API key
// (HTTP 429). It is returned once the retries are used up.
type RateLimitError struct {
	APIError
	// RetryAfter is the wait the server asked for in its Retry-After
	// header, or 0 when it sent none.
	RetryAfter time.Duration
}

// Unwrap returns the underlying [APIError].
func (e *RateLimitError) Unwrap() error { return &e.APIError }

// ServiceUnavailableError means RapidWorkers is temporarily unavailable
// (HTTP 502, 503 or 504). It is returned once the retries are used up, or at
// once after a 502 or 504 for a call that is not safe to resend.
type ServiceUnavailableError struct {
	APIError
}

// Unwrap returns the underlying [APIError].
func (e *ServiceUnavailableError) Unwrap() error { return &e.APIError }

// ConnectionError means the request did not reach RapidWorkers or got no
// answer in time. When the context was canceled or timed out,
// errors.Is(err, context.Canceled) or context.DeadlineExceeded is true.
//
// A call that moves money may still have run: resend it with the same
// idempotency key (set Params.IdempotencyKey yourself to control it).
type ConnectionError struct {
	// Err is the transport error.
	Err error
}

func (e *ConnectionError) Error() string {
	return "rapidworkers: could not reach RapidWorkers: " + e.Err.Error()
}

// Unwrap returns the transport error.
func (e *ConnectionError) Unwrap() error { return e.Err }

// ToolError means the tool ran and refused the call. Message says how to fix
// it.
//
// Typical causes are a validation error, an insufficient balance, a stale
// quote_id or a job in the wrong state.
type ToolError struct {
	// Tool is the name of the tool, such as "create_job".
	Tool string
	// Message says what was wrong and how to fix the call.
	Message string
	// Code is the machine-readable error code, such as
	// "insufficient_balance", or "" when the server sent none.
	Code string
	// Details is the structured error the server sent, without its message
	// (for example code, fields or replayed).
	Details map[string]any
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("rapidworkers: %s: %s", e.Tool, e.Message)
}
