package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/darshpatel1052/conduit/internal/provider"
)

// Relay pumps an open upstream stream out to the client as SSE.
//
// Three things have to be right in this loop, and each one is a way real
// gateways leak resources:
//
//  1. Every frame is flushed. Otherwise the response buffers and the client
//     sees one lump at the end.
//
//  2. Client disconnects abort the upstream. The stream's context descends
//     from the inbound request's, so when the client hangs up, net/http
//     cancels it, the upstream connection tears down, and Recv returns. That
//     propagation is what stops the gateway from cheerfully paying a provider
//     to generate 2000 tokens for a client that closed the tab.
//
//  3. A stalled provider is cut loose. The idle watchdog bounds the gap
//     between chunks, not the total duration — a long generation is normal, a
//     silent one is not.
func (p *Proxy) Relay(ctx context.Context, w http.ResponseWriter, h *StreamHandle, req *provider.ChatRequest) error {
	sse := newSSEWriter(w)
	defer h.Stream.Close()

	if err := sse.WriteHeaders(streamHeaders(h.Result)); err != nil {
		return err
	}

	// Usage is always requested upstream so the gateway can account for tokens,
	// but a client that did not ask for it should not receive an extra
	// choices-empty frame it may not know how to parse.
	clientWantsUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	idle := p.upstream.StreamIdleTimeout.D()
	watchdog := time.AfterFunc(idle, h.Cancel)
	defer watchdog.Stop()

	// TTFB was measured from the request's start; the relay adds generation
	// time on top of it.
	relayStart := time.Now()
	chunks := 0
	for {
		chunk, err := h.Stream.Recv()
		watchdog.Reset(idle)

		if err != nil {
			if errors.Is(err, io.EOF) {
				h.Result.Duration = h.Result.TTFB + time.Since(relayStart)
				return sse.WriteDone()
			}

			// The client leaving is not an error worth reporting to anyone;
			// there is nobody on the other end to read it.
			if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
				h.Result.Duration = h.Result.TTFB + time.Since(relayStart)
				p.log.Info("client disconnected mid-stream, upstream aborted",
					"provider", h.Result.Provider,
					"chunks_relayed", chunks,
					"duration_ms", h.Result.Duration.Milliseconds())
				return nil
			}

			// Past the first byte the status code is committed, so a failure
			// can only be surfaced inside the stream.
			kind := string(provider.KindUpstream)
			msg := err.Error()
			if pe, ok := provider.AsError(err); ok {
				kind = string(pe.Kind)
				msg = pe.Message
			}
			p.log.Error("stream failed after first token",
				"provider", h.Result.Provider, "chunks_relayed", chunks, "error", err)
			return sse.WriteError(kind, msg)
		}

		if chunk == nil {
			continue
		}
		if chunk.Usage != nil {
			h.Result.Usage = chunk.Usage
		}

		// A usage-only frame carries no choices.
		if len(chunk.Choices) == 0 && chunk.Usage != nil && !clientWantsUsage {
			continue
		}

		payload := chunk.Raw
		if len(payload) == 0 {
			b, err := json.Marshal(chunk)
			if err != nil {
				continue
			}
			payload = b
		}

		if err := sse.WriteData(payload); err != nil {
			// Writing failed: the client is gone. Returning unwinds to the
			// deferred Close, which cancels the upstream.
			h.Result.Duration = h.Result.TTFB + time.Since(relayStart)
			p.log.Info("client write failed mid-stream, upstream aborted",
				"provider", h.Result.Provider, "chunks_relayed", chunks, "error", err)
			return nil
		}
		chunks++
	}
}

// streamHeaders exposes the routing decision to the client. These are set
// before the first byte, because after that headers are immutable.
func streamHeaders(res *Result) map[string]string {
	h := map[string]string{
		"X-Conduit-Provider": res.Provider,
		"X-Conduit-Route":    res.Route,
		"X-Conduit-Model":    res.UpstreamModel,
		"X-Conduit-Attempts": strconv.Itoa(len(res.Attempts)),
	}
	if res.TTFB > 0 {
		h["X-Conduit-TTFB-Ms"] = strconv.FormatInt(res.TTFB.Milliseconds(), 10)
	}
	return h
}
