package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter emits Server-Sent Events to a client.
//
// Why SSE and not WebSockets: a token stream is strictly one-directional,
// server to client. WebSockets buy bidirectionality nobody needs here and cost
// a protocol upgrade, a framing layer, and the loss of plain HTTP semantics —
// no proxy caching rules, no standard status codes, no curl. SSE is just an
// HTTP response that stays open, which is exactly what this is.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	// http.ResponseController rather than a w.(http.Flusher) type assertion.
	// The assertion breaks the moment any middleware wraps the ResponseWriter
	// to capture the status code — the wrapper is not a Flusher, the assertion
	// fails, and streaming silently degrades to a buffered response that
	// arrives all at once at the end. ResponseController walks Unwrap() chains
	// and finds the real flusher underneath.
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// WriteHeaders sends the SSE response headers. Nothing may be written to the
// client before this, because once the first byte is out the status code and
// headers are fixed — which is also why failover has to happen before it.
func (s *sseWriter) WriteHeaders(extra map[string]string) error {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	// Tell nginx and friends not to buffer. Without this an intermediary
	// happily accumulates the whole stream and delivers it in one lump, which
	// looks exactly like the gateway failing to stream.
	h.Set("X-Accel-Buffering", "no")

	for k, v := range extra {
		h.Set(k, v)
	}

	s.w.WriteHeader(http.StatusOK)
	return s.rc.Flush()
}

// WriteData sends one data frame and flushes it.
//
// The flush is the entire point. Without it the bytes sit in Go's response
// buffer until it fills or the handler returns, so tokens generated over
// twenty seconds arrive as a single burst at the end — technically a stream,
// visibly not one.
func (s *sseWriter) WriteData(payload []byte) error {
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(payload); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}

// WriteDone sends the OpenAI stream terminator.
func (s *sseWriter) WriteDone() error {
	return s.WriteData([]byte("[DONE]"))
}

// WriteError reports a failure that happened after the stream was already
// open. The HTTP status is long since committed to 200, so the only honest
// channel left is an error frame in the stream itself; a client that stops
// reading here sees a truncated answer rather than a silent one.
func (s *sseWriter) WriteError(kind, message string) error {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    kind,
			"message": message,
			"code":    kind,
		},
	})
	if err != nil {
		payload = []byte(fmt.Sprintf(`{"error":{"type":%q,"message":"stream failed"}}`, kind))
	}
	if err := s.WriteData(payload); err != nil {
		return err
	}
	return s.WriteDone()
}
