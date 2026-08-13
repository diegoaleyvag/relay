package core

import "fmt"

// ErrorClass drives the failure contract: how the engine reacts to a failure.
type ErrorClass int

const (
	// ClassRetryable: transient; the engine backs off and retries with the same
	// idempotency key, bounded by the retry budget.
	ClassRetryable ErrorClass = iota
	// ClassTerminal: do not retry; the run fails, preserving partial results.
	ClassTerminal
	// ClassSkippable: skip the current step, record it as skipped, preserve
	// partials and continue (degraded success).
	ClassSkippable
)

func (c ErrorClass) String() string {
	switch c {
	case ClassRetryable:
		return "retryable"
	case ClassTerminal:
		return "terminal"
	case ClassSkippable:
		return "skippable"
	default:
		return "unknown"
	}
}

// ErrorCode is a stable, machine-readable failure code. Codes are part of the
// public failure contract and appear (redacted) in the event history.
type ErrorCode string

const (
	CodeTimeout          ErrorCode = "TIMEOUT"
	CodeTransport        ErrorCode = "TRANSPORT"
	CodeToolUnavailable  ErrorCode = "TOOL_UNAVAILABLE"
	CodeRateLimited      ErrorCode = "RATE_LIMITED"
	CodeStoreUnavailable ErrorCode = "STORE_UNAVAILABLE"
	CodeMalformed        ErrorCode = "MALFORMED_RESPONSE"
	CodeResponseTooLarge ErrorCode = "RESPONSE_TOO_LARGE"
	CodePermissionDenied ErrorCode = "PERMISSION_DENIED"
	CodeToolNotFound     ErrorCode = "TOOL_NOT_FOUND"
	CodeInvalidInput     ErrorCode = "INVALID_INPUT"
	CodeSourceNotFound   ErrorCode = "SOURCE_NOT_FOUND"
	CodeSourceTooLarge   ErrorCode = "SOURCE_TOO_LARGE"
	CodeDeadlineExceeded ErrorCode = "DEADLINE_EXCEEDED"
	CodeCancelled        ErrorCode = "CANCELLED"
	CodeRetriesExhausted ErrorCode = "RETRIES_EXHAUSTED"
	CodeReviewRejected   ErrorCode = "REVIEW_REJECTED"
	CodeInternal         ErrorCode = "INTERNAL"
)

// defaultClass maps each code to its default class. Policy may override the
// classification of MALFORMED_RESPONSE and PERMISSION_DENIED (see Policy).
func defaultClass(code ErrorCode) ErrorClass {
	switch code {
	case CodeTimeout, CodeTransport, CodeToolUnavailable, CodeRateLimited, CodeStoreUnavailable:
		return ClassRetryable
	case CodeMalformed:
		return ClassSkippable
	default:
		// PERMISSION_DENIED, RESPONSE_TOO_LARGE, TOOL_NOT_FOUND, INVALID_INPUT,
		// SOURCE_NOT_FOUND, SOURCE_TOO_LARGE, DEADLINE_EXCEEDED, CANCELLED,
		// RETRIES_EXHAUSTED, REVIEW_REJECTED, INTERNAL and any unknown code.
		return ClassTerminal
	}
}

// Classify returns the default class for a code.
func Classify(code ErrorCode) ErrorClass { return defaultClass(code) }

// RelayError is the structured, machine-readable failure type. Its Message and
// Details are already redacted (safe to store and display); the wrapped cause
// is never serialized into the event history.
type RelayError struct {
	Code    ErrorCode
	Class   ErrorClass
	Message string            // human-readable, redacted
	Details map[string]string // machine-readable, redacted
	cause   error             // never serialized
}

// NewError builds a RelayError with the default class for the code.
func NewError(code ErrorCode, msg string) *RelayError {
	return &RelayError{Code: code, Class: defaultClass(code), Message: msg}
}

// WithClass overrides the error class (used by Policy).
func (e *RelayError) WithClass(c ErrorClass) *RelayError {
	e.Class = c
	return e
}

// WithDetail attaches a redacted machine-readable detail.
func (e *RelayError) WithDetail(k, v string) *RelayError {
	if e.Details == nil {
		e.Details = map[string]string{}
	}
	e.Details[k] = v
	return e
}

// WithCause wraps an underlying error. The cause is available via Unwrap but is
// never written to durable state or telemetry.
func (e *RelayError) WithCause(err error) *RelayError {
	e.cause = err
	return e
}

// MarkInjected flags a failure as deliberately injected by the fault harness so
// it is visible in the event history (never silent).
func (e *RelayError) MarkInjected(kind string) *RelayError {
	return e.WithDetail("injected", kind)
}

// Injected reports whether this error was produced by the fault harness.
func (e *RelayError) Injected() bool {
	_, ok := e.Details["injected"]
	return ok
}

func (e *RelayError) Error() string { return string(e.Code) + ": " + e.Message }

func (e *RelayError) Unwrap() error { return e.cause }

// Is matches by code so errors.Is(err, &RelayError{Code: ...}) works.
func (e *RelayError) Is(target error) bool {
	o, ok := target.(*RelayError)
	return ok && o.Code == e.Code
}

// Retryable reports whether the failure should be retried after backoff.
func (e *RelayError) Retryable() bool { return e.Class == ClassRetryable }

// Skippable reports whether the current step may be skipped (degraded success).
func (e *RelayError) Skippable() bool { return e.Class == ClassSkippable }

// Terminal reports whether the failure fails the run.
func (e *RelayError) Terminal() bool { return e.Class == ClassTerminal }

// AsRelayError coerces any error into a *RelayError, wrapping unknown errors as
// INTERNAL (terminal) so the engine always has a classified failure.
func AsRelayError(err error) *RelayError {
	if err == nil {
		return nil
	}
	if re, ok := err.(*RelayError); ok {
		return re
	}
	return NewError(CodeInternal, fmt.Sprintf("unclassified error: %v", err)).WithCause(err)
}
