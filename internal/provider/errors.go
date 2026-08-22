package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Error is a provider failure carrying the two decisions the proxy needs to
// make, pre-computed at the point where the most context is available.
//
// Those decisions are deliberately separate, because they answer different
// questions and get different answers:
//
//   - Retryable: "would a different provider plausibly succeed?" This drives
//     failover. A malformed request fails identically everywhere, so retrying
//     it just multiplies load during an incident.
//
//   - CountsAsFailure: "is this provider unhealthy?" This drives the circuit
//     breaker. A client hammering the gateway with invalid JSON must never be
//     able to trip a breaker on a provider that is answering perfectly — that
//     would let bad client traffic take out a healthy upstream for everyone.
//
// A 400 is neither. A 429 is both. A 401 is retryable (the request is fine,
// this provider's credential is not) and a failure (this provider is unusable
// until someone fixes it).
type Error struct {
	Provider   string
	Kind       Kind
	StatusCode int
	Code       string
	Message    string

	Retryable       bool
	CountsAsFailure bool

	Err error
}

// Kind is a coarse classification used in logs, metrics and client responses.
type Kind string

const (
	KindInvalidRequest Kind = "invalid_request"
	KindAuth           Kind = "authentication"
	KindNotFound       Kind = "not_found"
	KindRateLimit      Kind = "rate_limit"
	KindUpstream       Kind = "upstream_error"
	KindTimeout        Kind = "timeout"
	KindConnection     Kind = "connection_error"
	KindCanceled       Kind = "canceled"
	KindCircuitOpen    Kind = "circuit_open"
	KindNoRoute        Kind = "no_route"
	KindInternal       Kind = "internal"
)

func (e *Error) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		fmt.Fprintf(&b, "provider %s: ", e.Provider)
	}
	b.WriteString(string(e.Kind))
	if e.StatusCode > 0 {
		fmt.Fprintf(&b, " (status %d)", e.StatusCode)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil && e.Message == "" {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// AsError extracts a *Error from an error chain.
func AsError(err error) (*Error, bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// IsRetryable reports whether failing over to another target is worthwhile.
// Unclassified errors are treated as retryable: an unknown failure is more
// likely to be a flaky upstream than a bad request, and the attempt cap plus
// the circuit breaker bound the cost of guessing wrong.
func IsRetryable(err error) bool {
	if pe, ok := AsError(err); ok {
		return pe.Retryable
	}
	return !errors.Is(err, context.Canceled)
}

// CountsAsFailure reports whether the circuit breaker should count this error.
func CountsAsFailure(err error) bool {
	if pe, ok := AsError(err); ok {
		return pe.CountsAsFailure
	}
	return !errors.Is(err, context.Canceled)
}

// StatusCodeFor maps an error onto the HTTP status the client should see.
func StatusCodeFor(err error) int {
	pe, ok := AsError(err)
	if !ok {
		return http.StatusBadGateway
	}
	switch pe.Kind {
	case KindInvalidRequest:
		return http.StatusBadRequest
	case KindAuth:
		// The gateway's credential failed upstream, not the client's. Echoing
		// 401 would tell the caller to fix a key they do not own.
		return http.StatusBadGateway
	case KindNotFound:
		return http.StatusNotFound
	case KindRateLimit:
		return http.StatusTooManyRequests
	case KindTimeout:
		return http.StatusGatewayTimeout
	case KindCircuitOpen, KindConnection, KindUpstream:
		return http.StatusServiceUnavailable
	case KindNoRoute:
		return http.StatusNotFound
	case KindCanceled:
		// Client hung up; nobody is listening. 499 mirrors nginx.
		return 499
	default:
		return http.StatusBadGateway
	}
}

// classifyStatus turns an upstream HTTP status into a classified error.
func classifyStatus(providerName string, status int, body []byte) *Error {
	e := &Error{
		Provider:   providerName,
		StatusCode: status,
		Message:    extractUpstreamMessage(body),
	}

	switch {
	case status == http.StatusBadRequest,
		status == http.StatusRequestEntityTooLarge,
		status == http.StatusUnprocessableEntity:
		// The request itself is wrong. Every provider will say the same, and
		// the provider is healthy, so: no failover, no breaker impact.
		e.Kind = KindInvalidRequest
		e.Retryable = false
		e.CountsAsFailure = false

	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		e.Kind = KindAuth
		e.Retryable = true
		e.CountsAsFailure = true

	case status == http.StatusNotFound:
		// Usually a model this provider does not serve. Another target in the
		// pool may well serve it, so fail over — but a target that cannot
		// serve the route is unhealthy for that route's purposes.
		e.Kind = KindNotFound
		e.Retryable = true
		e.CountsAsFailure = true

	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimit
		e.Retryable = true
		e.CountsAsFailure = true

	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		e.Kind = KindTimeout
		e.Retryable = true
		e.CountsAsFailure = true

	case status >= 500:
		e.Kind = KindUpstream
		e.Retryable = true
		e.CountsAsFailure = true

	default:
		e.Kind = KindUpstream
		e.Retryable = status != http.StatusNotImplemented
		e.CountsAsFailure = true
	}

	if e.Message == "" {
		e.Message = fmt.Sprintf("upstream returned %d %s", status, http.StatusText(status))
	}
	return e
}

// classifyTransport turns a transport-level failure into a classified error.
func classifyTransport(providerName string, ctx context.Context, err error) *Error {
	e := &Error{Provider: providerName, Err: err, Message: err.Error()}

	switch {
	case errors.Is(err, context.Canceled):
		// Distinguish "the client hung up" from "our own deadline fired".
		// Only the former means there is nobody left to serve: retrying it
		// would burn provider tokens generating a response into the void.
		if ctx != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			e.Kind = KindCanceled
			e.Retryable = false
			e.CountsAsFailure = false
			e.Message = "request canceled by client"
			return e
		}
		e.Kind = KindConnection
		e.Retryable = true
		e.CountsAsFailure = true

	case errors.Is(err, context.DeadlineExceeded):
		e.Kind = KindTimeout
		e.Retryable = true
		e.CountsAsFailure = true
		e.Message = "upstream timed out"

	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			e.Kind = KindTimeout
		} else {
			e.Kind = KindConnection
		}
		e.Retryable = true
		e.CountsAsFailure = true
	}
	return e
}

// extractUpstreamMessage digs the human-readable message out of a provider's
// error body. OpenAI nests it under "error", Anthropic under "error.message",
// Ollama returns a flat {"error": "..."} string.
func extractUpstreamMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Error) > 0 {
		var s string
		if err := json.Unmarshal(envelope.Error, &s); err == nil {
			return s
		}
		var obj struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(envelope.Error, &obj); err == nil && obj.Message != "" {
			return obj.Message
		}
	}

	s := strings.TrimSpace(string(body))
	const max = 512
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
