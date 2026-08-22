// Package proxy executes a routed request against providers, applying circuit
// breaking and failover, and relays the result to the client.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/router"
)

// Proxy forwards requests to providers.
type Proxy struct {
	router   *router.Router
	upstream config.Upstream
	log      *slog.Logger
}

// New builds a proxy over a router.
func New(r *router.Router, upstream config.Upstream, log *slog.Logger) *Proxy {
	return &Proxy{router: r, upstream: upstream, log: log.With("component", "proxy")}
}

// Attempt records one try against one target, successful or not.
type Attempt struct {
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Latency  time.Duration `json:"latency"`
	Error    string        `json:"error,omitempty"`
	Kind     string        `json:"kind,omitempty"`
}

// Result is the metadata a request produced, for response headers and logs.
type Result struct {
	Route         string
	Provider      string
	ProviderType  string
	UpstreamModel string
	Attempts      []Attempt
	Usage         *provider.Usage
	TTFB          time.Duration
	Duration      time.Duration
}

// Complete performs a non-streaming completion with failover.
func (p *Proxy) Complete(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, *Result, error) {
	start := time.Now()
	res := &Result{}

	resolution, err := p.router.Resolve(req.Model)
	if err != nil {
		return nil, res, err
	}
	res.Route = resolution.RouteModel

	// A total deadline is correct for non-streaming: the client is blocked
	// waiting for one complete answer, so an answer that arrives after the
	// deadline is worthless anyway.
	ctx, cancel := context.WithTimeout(ctx, p.upstream.RequestTimeout.D())
	defer cancel()

	var lastErr error
	for i, target := range resolution.Targets {
		if i >= resolution.MaxAttempts {
			break
		}
		// If the client has already gone, stop. Continuing would burn provider
		// tokens producing a response with no reader.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
			return nil, res, canceledError(ctxErr)
		}

		attemptStart := time.Now()

		done, err := target.Entry.Breaker.Allow()
		if err != nil {
			// Breaker is open: rejected without touching the network, which is
			// the whole benefit — a dead provider costs microseconds, not a
			// full timeout, before the next target is tried.
			res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))
			lastErr = err
			continue
		}

		resp, err := target.Entry.Provider.Complete(ctx, req, target.Model)
		done(err)

		res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))

		if err == nil {
			res.Provider = target.Entry.Name
			res.ProviderType = target.Entry.Type
			res.UpstreamModel = target.Model
			res.Usage = resp.Usage
			res.Duration = time.Since(start)
			res.TTFB = res.Duration
			return resp, res, nil
		}

		lastErr = err
		if !provider.IsRetryable(err) {
			// Failing over a bad request would just multiply load during an
			// incident and produce the same rejection N times.
			res.Duration = time.Since(start)
			return nil, res, err
		}

		p.log.Warn("provider attempt failed, trying next target",
			"provider", target.Entry.Name,
			"upstream_model", target.Model,
			"attempt", i+1,
			"error", err)
	}

	res.Duration = time.Since(start)
	return nil, res, exhausted(resolution, res.Attempts, lastErr)
}

// Embed performs a non-streaming embeddings request with the same routing,
// circuit-breaking, and pre-response failover rules as generation requests.
func (p *Proxy) Embed(ctx context.Context, req *provider.EmbeddingRequest) (*provider.EmbeddingResponse, *Result, error) {
	start := time.Now()
	res := &Result{}
	resolution, err := p.router.Resolve(req.Model)
	if err != nil {
		return nil, res, err
	}
	res.Route = resolution.RouteModel
	ctx, cancel := context.WithTimeout(ctx, p.upstream.RequestTimeout.D())
	defer cancel()

	var lastErr error
	for i, target := range resolution.Targets {
		if i >= resolution.MaxAttempts {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
			return nil, res, canceledError(ctxErr)
		}
		attemptStart := time.Now()
		done, err := target.Entry.Breaker.Allow()
		if err != nil {
			res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))
			lastErr = err
			continue
		}
		resp, err := target.Entry.Provider.Embed(ctx, req, target.Model)
		done(err)
		res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))
		if err == nil {
			res.Provider = target.Entry.Name
			res.ProviderType = target.Entry.Type
			res.UpstreamModel = target.Model
			res.Usage = resp.Usage
			res.Duration = time.Since(start)
			res.TTFB = res.Duration
			return resp, res, nil
		}
		lastErr = err
		if !provider.IsRetryable(err) {
			res.Duration = time.Since(start)
			return nil, res, err
		}
		p.log.Warn("embedding provider attempt failed, trying next target",
			"provider", target.Entry.Name, "upstream_model", target.Model, "attempt", i+1, "error", err)
	}
	res.Duration = time.Since(start)
	return nil, res, exhausted(resolution, res.Attempts, lastErr)
}

// StreamHandle is an open upstream stream that has already produced its first
// chunk, along with the metadata of the target that served it.
type StreamHandle struct {
	Stream provider.Stream
	Target router.Target
	Result *Result

	// cancel tears down the upstream request. Held here so the relay loop's
	// idle watchdog can abort a stalled provider while blocked inside Recv.
	cancel context.CancelFunc
}

// Cancel aborts the upstream stream immediately.
func (h *StreamHandle) Cancel() {
	if h.cancel != nil {
		h.cancel()
	}
}

// OpenStream starts a streaming completion, failing over between targets.
//
// The hard constraint that shapes this: once a single byte of the response has
// reached the client, the status code is committed and failover is impossible —
// the client is already mid-stream and cannot be silently moved to a different
// provider. So the failover window is exactly "up to and including the first
// chunk". This function opens the upstream stream AND pulls the first chunk
// before returning, so any failure up to that point is still recoverable. After
// it returns, a failure can only be reported into the stream, not routed around.
func (p *Proxy) OpenStream(ctx context.Context, req *provider.ChatRequest) (*StreamHandle, *Result, error) {
	start := time.Now()
	res := &Result{}

	resolution, err := p.router.Resolve(req.Model)
	if err != nil {
		return nil, res, err
	}
	res.Route = resolution.RouteModel

	var lastErr error
	for i, target := range resolution.Targets {
		if i >= resolution.MaxAttempts {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
			return nil, res, canceledError(ctxErr)
		}

		attemptStart := time.Now()

		done, err := target.Entry.Breaker.Allow()
		if err != nil {
			res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))
			lastErr = err
			continue
		}

		// Each attempt gets its own cancellable context. When an attempt
		// fails, cancelling it tears down that upstream connection
		// immediately; without this, a half-open stream to a failing provider
		// would linger until its own timeout, holding a connection and (on a
		// paid API) potentially still being billed for generation.
		attemptCtx, cancelAttempt := context.WithCancel(ctx)

		stream, err := target.Entry.Provider.Stream(attemptCtx, req, target.Model)
		if err != nil {
			cancelAttempt()
			done(err)
			res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))
			lastErr = err
			if !provider.IsRetryable(err) {
				return nil, res, err
			}
			continue
		}

		// Pull the first chunk while failover is still possible. A provider
		// that accepts the connection and then immediately errors is a common
		// failure mode, and catching it here is the difference between a clean
		// failover and a client receiving an empty broken stream.
		first, err := p.firstChunk(attemptCtx, stream)
		if err != nil && !errors.Is(err, io.EOF) {
			stream.Close()
			cancelAttempt()
			done(err)
			res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), err))
			lastErr = err
			if !provider.IsRetryable(err) {
				return nil, res, err
			}
			p.log.Warn("stream attempt failed before first token, trying next target",
				"provider", target.Entry.Name, "attempt", i+1, "error", err)
			continue
		}

		res.Attempts = append(res.Attempts, attemptFrom(target, time.Since(attemptStart), nil))
		res.Provider = target.Entry.Name
		res.ProviderType = target.Entry.Type
		res.UpstreamModel = target.Model
		res.TTFB = time.Since(start)

		wrapped := &guardedStream{
			inner:  stream,
			first:  first,
			done:   done,
			cancel: cancelAttempt,
		}
		if errors.Is(err, io.EOF) {
			wrapped.firstIsEOF = true
		}

		return &StreamHandle{
			Stream: wrapped,
			Target: target,
			Result: res,
			cancel: cancelAttempt,
		}, res, nil
	}

	res.Duration = time.Since(start)
	return nil, res, exhausted(resolution, res.Attempts, lastErr)
}

// firstChunk reads the first chunk under the time-to-first-token budget, which
// is deliberately looser than the inter-token budget: a cold model load or a
// queue at the provider is slow but healthy, whereas a mid-stream gap of the
// same length is not.
func (p *Proxy) firstChunk(ctx context.Context, s provider.Stream) (*provider.ChatChunk, error) {
	type result struct {
		chunk *provider.ChatChunk
		err   error
	}
	ch := make(chan result, 1)

	go func() {
		chunk, err := s.Recv()
		ch <- result{chunk, err}
	}()

	timer := time.NewTimer(p.upstream.StreamFirstChunkTimeout.D())
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.chunk, r.err
	case <-timer.C:
		return nil, &provider.Error{
			Kind:            provider.KindTimeout,
			Message:         "provider opened a stream but sent no tokens before the first-token timeout",
			Retryable:       true,
			CountsAsFailure: true,
		}
	case <-ctx.Done():
		// The goroutine above is not leaked: cancelling ctx closes the
		// upstream connection, Recv returns, and the buffered channel accepts
		// the send with nobody reading it.
		return nil, canceledError(ctx.Err())
	}
}

// guardedStream owns the breaker report and the attempt's cancel function for
// the lifetime of a stream.
//
// Reporting to the breaker at stream *end* rather than at stream start is the
// reason the two-step breaker API is used: a stream that delivers headers fine
// and then dies after two tokens is a provider failure, and a breaker told
// "success" the moment the connection opened would never see it.
type guardedStream struct {
	inner provider.Stream

	first      *provider.ChatChunk
	firstUsed  bool
	firstIsEOF bool

	done   func(error)
	cancel context.CancelFunc

	reported atomic.Bool
	closed   atomic.Bool
}

func (s *guardedStream) Recv() (*provider.ChatChunk, error) {
	if !s.firstUsed {
		s.firstUsed = true
		if s.firstIsEOF {
			s.report(nil)
			return nil, io.EOF
		}
		return s.first, nil
	}

	chunk, err := s.inner.Recv()
	switch {
	case err == nil:
		return chunk, nil
	case errors.Is(err, io.EOF):
		s.report(nil) // stream completed normally
		return nil, io.EOF
	default:
		s.report(err)
		return nil, err
	}
}

func (s *guardedStream) Close() error {
	// A stream closed without reaching EOF means the client left. That is not
	// evidence about the provider's health, so it is reported as a
	// non-counting error rather than as a success or a failure.
	s.report(&provider.Error{
		Kind:            provider.KindCanceled,
		Message:         "stream closed before completion",
		Retryable:       false,
		CountsAsFailure: false,
	})

	var err error
	if s.closed.CompareAndSwap(false, true) {
		err = s.inner.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	return err
}

func (s *guardedStream) report(err error) {
	if s.reported.CompareAndSwap(false, true) && s.done != nil {
		s.done(err)
	}
}

func attemptFrom(t router.Target, latency time.Duration, err error) Attempt {
	a := Attempt{Provider: t.Entry.Name, Model: t.Model, Latency: latency}
	if err != nil {
		a.Error = err.Error()
		if pe, ok := provider.AsError(err); ok {
			a.Kind = string(pe.Kind)
		} else {
			a.Kind = string(provider.KindInternal)
		}
	}
	return a
}

func canceledError(err error) error {
	return &provider.Error{
		Kind:            provider.KindCanceled,
		Message:         "request canceled by client",
		Retryable:       false,
		CountsAsFailure: false,
		Err:             err,
	}
}

// exhausted builds the error returned when every target has been tried.
//
// It enumerates what each target did, because "503 Service Unavailable" alone
// tells an operator nothing: the useful question during an incident is whether
// three providers timed out or two had open breakers and the third rejected
// the credential, and that answer should be in the response, not only in a log
// someone has to go find.
func exhausted(resolution *router.Resolution, attempts []Attempt, lastErr error) error {
	if lastErr == nil {
		return &provider.Error{
			Kind:      provider.KindNoRoute,
			Message:   fmt.Sprintf("route %q has no targets available", resolution.RouteModel),
			Retryable: false,
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "all %d target(s) for route %q failed", len(attempts), resolution.RouteModel)
	for _, a := range attempts {
		fmt.Fprintf(&b, "; %s(%s): %s", a.Provider, a.Model, a.Error)
	}

	kind := provider.KindUpstream
	if pe, ok := provider.AsError(lastErr); ok {
		kind = pe.Kind
		// If every attempt was short-circuited, the honest summary is that the
		// pool is open, not that some individual call failed.
		if kind == provider.KindCircuitOpen {
			allOpen := true
			for _, a := range attempts {
				if a.Kind != string(provider.KindCircuitOpen) {
					allOpen = false
					break
				}
			}
			if !allOpen {
				kind = provider.KindUpstream
			}
		}
	}

	return &provider.Error{
		Kind:            kind,
		Message:         b.String(),
		Retryable:       false,
		CountsAsFailure: false,
		Err:             lastErr,
	}
}
