package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// sseEvent is one decoded Server-Sent Event.
type sseEvent struct {
	Event string
	Data  []byte
}

// sseDecoder parses an SSE byte stream into events.
//
// bufio.Reader rather than bufio.Scanner: Scanner caps a token at 64 KiB by
// default and returns bufio.ErrTooLong past it, which would blow up on a chunk
// carrying a long tool-call argument or an inlined image. Reader grows to fit.
type sseDecoder struct {
	r     *bufio.Reader
	data  bytes.Buffer
	event string
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{r: bufio.NewReaderSize(r, 16<<10)}
}

// Next returns the next complete event, or io.EOF at end of stream.
func (d *sseDecoder) Next() (sseEvent, error) {
	d.data.Reset()
	d.event = ""

	for {
		line, err := d.r.ReadBytes('\n')
		if err != nil {
			// A final event without its terminating blank line is still a
			// complete event as far as every LLM provider is concerned.
			if err == io.EOF && (len(line) > 0 || d.data.Len() > 0) {
				d.consumeLine(line)
				if d.data.Len() > 0 {
					return d.flush(), nil
				}
			}
			return sseEvent{}, err
		}

		trimmed := bytes.TrimRight(line, "\r\n")

		// A blank line terminates the event.
		if len(trimmed) == 0 {
			if d.data.Len() > 0 || d.event != "" {
				return d.flush(), nil
			}
			continue
		}
		d.consumeLine(trimmed)
	}
}

func (d *sseDecoder) consumeLine(line []byte) {
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return
	}

	// Lines beginning with a colon are comments. Providers send them as
	// keep-alives through idle periods; they carry no payload.
	if line[0] == ':' {
		return
	}

	field, value, found := bytes.Cut(line, []byte(":"))
	if !found {
		field, value = line, nil
	}
	// Exactly one leading space after the colon is part of the framing.
	value = bytes.TrimPrefix(value, []byte(" "))

	switch string(field) {
	case "data":
		if d.data.Len() > 0 {
			// Multi-line data fields join with a newline, per the SSE spec.
			d.data.WriteByte('\n')
		}
		d.data.Write(value)
	case "event":
		d.event = string(value)
	case "id", "retry":
		// Not meaningful for a completion stream.
	}
}

func (d *sseDecoder) flush() sseEvent {
	ev := sseEvent{Event: d.event}
	if d.data.Len() > 0 {
		ev.Data = append([]byte(nil), d.data.Bytes()...)
	}
	d.data.Reset()
	d.event = ""
	return ev
}

// sseTerminator is the sentinel OpenAI-compatible APIs send to close a stream.
var sseTerminator = []byte("[DONE]")

// errorPayload sniffs a mid-stream error frame. A provider that fails after
// headers are sent cannot use an HTTP status to say so, so it puts an error
// object in the stream instead. Missing this makes a failed generation look
// like a successful but truncated one.
func errorPayload(providerName string, data []byte) *Error {
	if !bytes.Contains(data, []byte(`"error"`)) {
		return nil
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || len(probe.Error) == 0 {
		return nil
	}
	// An explicit JSON null under "error" is not an error.
	if bytes.Equal(bytes.TrimSpace(probe.Error), []byte("null")) {
		return nil
	}
	e := &Error{
		Provider:        providerName,
		Kind:            KindUpstream,
		Message:         extractUpstreamMessage(data),
		Retryable:       true,
		CountsAsFailure: true,
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("upstream reported an error mid-stream: %s", truncate(data, 256))
	}
	return e
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
