package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/router"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testUpstream() config.Upstream {
	return config.Upstream{
		ConnectTimeout:          config.Duration(2 * time.Second),
		ResponseHeaderTimeout:   config.Duration(5 * time.Second),
		RequestTimeout:          config.Duration(5 * time.Second),
		StreamFirstChunkTimeout: config.Duration(3 * time.Second),
		StreamIdleTimeout:       config.Duration(2 * time.Second),
		MaxIdleConns:            10,
		MaxIdleConnsPerHost:     10,
		IdleConnTimeout:         config.Duration(30 * time.Second),
	}
}

// completionServer answers with a fixed completion and counts calls.
func completionServer(t *testing.T, name string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c","object":"chat.completion","model":%q,
			"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, name, "from-"+name)
	}))
}

// statusServer always fails with the given status.
func statusServer(t *testing.T, status int, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		io.WriteString(w, `{"error":{"message":"nope"}}`)
	}))
}

// streamServer emits n text deltas then [DONE].
func streamServer(t *testing.T, name string, n int, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n",
				fmt.Sprintf("%s-%d", name, i))
			fl.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

func buildProxy(t *testing.T, yaml string) *Proxy {
	t.Helper()
	routes, err := config.ParseRoutes([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	up := testUpstream()
	rtr, err := router.New(routes, provider.NewHTTPClient(up), testLogger())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return New(rtr, up, testLogger())
}

func chatRequest(t *testing.T, model string, stream bool) *provider.ChatRequest {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%v}`, model, stream)
	req, err := provider.ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseChatRequest: %v", err)
	}
	return req
}

// --- failover --------------------------------------------------------------

func TestFailoverToHealthyProvider(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := statusServer(t, http.StatusInternalServerError, &badHits)
	defer bad.Close()
	good := completionServer(t, "good", &goodHits)
	defer good.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: bad
    type: ollama
    base_url: %s
  - name: good
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    targets:
      - provider: bad
      - provider: good
`, bad.URL, good.URL))

	resp, res, err := p.Complete(context.Background(), chatRequest(t, "m", false))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if res.Provider != "good" {
		t.Errorf("served by %q, want good", res.Provider)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(res.Attempts))
	}
	if badHits.Load() != 1 || goodHits.Load() != 1 {
		t.Errorf("hits: bad=%d good=%d, want 1 each", badHits.Load(), goodHits.Load())
	}
	if resp.Choices[0].Message.Text() != "from-good" {
		t.Errorf("content = %q", resp.Choices[0].Message.Text())
	}
}

// TestNoFailoverOnClientError is the load-amplification guard: a request that is
// wrong will be wrong at every provider, so trying them all just multiplies
// traffic during an incident.
func TestNoFailoverOnClientError(t *testing.T) {
	var firstHits, secondHits atomic.Int64
	first := statusServer(t, http.StatusBadRequest, &firstHits)
	defer first.Close()
	second := completionServer(t, "second", &secondHits)
	defer second.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: first
    type: ollama
    base_url: %s
  - name: second
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    targets:
      - provider: first
      - provider: second
`, first.URL, second.URL))

	_, res, err := p.Complete(context.Background(), chatRequest(t, "m", false))
	if err == nil {
		t.Fatal("want an error for a 400 from the first provider")
	}
	if secondHits.Load() != 0 {
		t.Errorf("second provider was called %d times; a 400 must not trigger failover", secondHits.Load())
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(res.Attempts))
	}
	if provider.StatusCodeFor(err) != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 passed through to the client", provider.StatusCodeFor(err))
	}
}

func TestAllTargetsFailedReportsEveryAttempt(t *testing.T) {
	var h1, h2 atomic.Int64
	s1 := statusServer(t, http.StatusInternalServerError, &h1)
	defer s1.Close()
	s2 := statusServer(t, http.StatusTooManyRequests, &h2)
	defer s2.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: one
    type: ollama
    base_url: %s
  - name: two
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    targets:
      - provider: one
      - provider: two
`, s1.URL, s2.URL))

	_, res, err := p.Complete(context.Background(), chatRequest(t, "m", false))
	if err == nil {
		t.Fatal("want an error when every target fails")
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(res.Attempts))
	}
	// The message must name each provider and why it was passed over;
	// a bare 503 tells an operator nothing during an incident.
	msg := err.Error()
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not mention provider %q", msg, want)
		}
	}
}

// TestOpenBreakerFailsFastAndSkips: the whole point of the breaker is that a
// dead provider costs microseconds rather than a full timeout.
func TestOpenBreakerFailsFastAndSkips(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := statusServer(t, http.StatusInternalServerError, &badHits)
	defer bad.Close()
	good := completionServer(t, "good", &goodHits)
	defer good.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: bad
    type: ollama
    base_url: %s
    breaker:
      consecutive_failures: 2
      min_requests: 100
      cooldown: 10s
  - name: good
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    targets:
      - provider: bad
      - provider: good
`, bad.URL, good.URL))

	// Two failures trip bad's breaker.
	for i := 0; i < 2; i++ {
		if _, _, err := p.Complete(context.Background(), chatRequest(t, "m", false)); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	hitsAfterTrip := badHits.Load()

	// Subsequent requests must not touch the bad provider at all.
	for i := 0; i < 3; i++ {
		_, res, err := p.Complete(context.Background(), chatRequest(t, "m", false))
		if err != nil {
			t.Fatalf("post-trip request %d: %v", i+1, err)
		}
		if res.Provider != "good" {
			t.Fatalf("served by %q, want good", res.Provider)
		}
		if res.Attempts[0].Kind != string(provider.KindCircuitOpen) {
			t.Errorf("first attempt kind = %q, want %q", res.Attempts[0].Kind, provider.KindCircuitOpen)
		}
	}

	if got := badHits.Load(); got != hitsAfterTrip {
		t.Errorf("bad provider received %d more requests after its breaker opened; "+
			"an open breaker must not reach the network", got-hitsAfterTrip)
	}
}

func TestNoRouteReturnsError(t *testing.T) {
	var hits atomic.Int64
	srv := completionServer(t, "a", &hits)
	defer srv.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: a
    type: ollama
    base_url: %s
routes:
  - model: known
    targets:
      - provider: a
`, srv.URL))

	_, _, err := p.Complete(context.Background(), chatRequest(t, "unknown", false))
	if err == nil {
		t.Fatal("want an error for an unrouted model")
	}
	if provider.StatusCodeFor(err) != http.StatusNotFound {
		t.Errorf("status = %d, want 404", provider.StatusCodeFor(err))
	}
}

func TestMaxAttemptsIsEnforced(t *testing.T) {
	var h1, h2, h3 atomic.Int64
	s1 := statusServer(t, 500, &h1)
	defer s1.Close()
	s2 := statusServer(t, 500, &h2)
	defer s2.Close()
	s3 := completionServer(t, "three", &h3)
	defer s3.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: one
    type: ollama
    base_url: %s
  - name: two
    type: ollama
    base_url: %s
  - name: three
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    max_attempts: 2
    targets:
      - provider: one
      - provider: two
      - provider: three
`, s1.URL, s2.URL, s3.URL))

	_, res, err := p.Complete(context.Background(), chatRequest(t, "m", false))
	if err == nil {
		t.Fatal("want an error: the third target is beyond max_attempts")
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(res.Attempts))
	}
	if h3.Load() != 0 {
		t.Error("the third target was contacted despite max_attempts: 2")
	}
}

// --- streaming -------------------------------------------------------------

// TestStreamFailsOverBeforeFirstToken defines the failover window: a stream can
// only be rerouted while nothing has been written to the client.
func TestStreamFailsOverBeforeFirstToken(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := statusServer(t, http.StatusServiceUnavailable, &badHits)
	defer bad.Close()
	good := streamServer(t, "good", 3, &goodHits)
	defer good.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: bad
    type: ollama
    base_url: %s
  - name: good
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    targets:
      - provider: bad
      - provider: good
`, bad.URL, good.URL))

	handle, res, err := p.OpenStream(context.Background(), chatRequest(t, "m", true))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer handle.Stream.Close()

	if res.Provider != "good" {
		t.Errorf("stream served by %q, want good", res.Provider)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(res.Attempts))
	}

	// The buffered first chunk must be replayed, not swallowed.
	var got []string
	for {
		chunk, err := handle.Stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		for _, c := range chunk.Choices {
			if c.Delta != nil {
				got = append(got, c.Delta.Text())
			}
		}
	}
	if len(got) != 3 {
		t.Fatalf("received %d chunks (%v), want 3: the first chunk was lost during failover", len(got), got)
	}
	if got[0] != "good-0" {
		t.Errorf("first chunk = %q, want good-0", got[0])
	}
}

// TestStreamFailsOverWhenProviderDiesAfterHeaders covers the nastier case: the
// provider returns 200, so a naive gateway would commit to it, and only then
// errors. Because the first chunk is pulled before returning, this is still
// recoverable.
func TestStreamFailsOverWhenProviderDiesAfterHeaders(t *testing.T) {
	var badHits, goodHits atomic.Int64

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, `data: {"error":{"message":"died after headers"}}`+"\n\n")
		w.(http.Flusher).Flush()
	}))
	defer bad.Close()

	good := streamServer(t, "good", 2, &goodHits)
	defer good.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: bad
    type: ollama
    base_url: %s
  - name: good
    type: ollama
    base_url: %s
routes:
  - model: m
    strategy: priority
    targets:
      - provider: bad
      - provider: good
`, bad.URL, good.URL))

	handle, res, err := p.OpenStream(context.Background(), chatRequest(t, "m", true))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer handle.Stream.Close()

	if res.Provider != "good" {
		t.Errorf("served by %q, want good: a 200-then-error must still fail over", res.Provider)
	}
	if badHits.Load() != 1 {
		t.Errorf("bad provider hits = %d, want 1", badHits.Load())
	}
}

func TestStreamReportsToBreakerAtStreamEnd(t *testing.T) {
	var hits atomic.Int64
	srv := streamServer(t, "s", 2, &hits)
	defer srv.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: s
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: s
`, srv.URL))

	handle, _, err := p.OpenStream(context.Background(), chatRequest(t, "m", true))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	entry := p.router.Table().Provider("s")
	if entry == nil {
		t.Fatal("provider entry not found")
	}

	// Drain the stream to completion.
	for {
		if _, err := handle.Stream.Recv(); err != nil {
			break
		}
	}
	handle.Stream.Close()

	if got := entry.Breaker.Snapshot().TotalSuccesses; got != 1 {
		t.Errorf("TotalSuccesses = %d, want 1: a completed stream must be reported to the breaker", got)
	}
}

// TestRelayStreamsAndTerminates exercises the client-facing SSE path end to end
// through a real ResponseWriter.
func TestRelayStreamsAndTerminates(t *testing.T) {
	var hits atomic.Int64
	upstream := streamServer(t, "up", 3, &hits)
	defer upstream.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: up
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: up
`, upstream.URL))

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := chatRequest(t, "m", true)
		handle, _, err := p.OpenStream(r.Context(), req)
		if err != nil {
			t.Errorf("OpenStream: %v", err)
			return
		}
		if err := p.Relay(r.Context(), w, handle, req); err != nil {
			t.Errorf("Relay: %v", err)
		}
	}))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("X-Conduit-Provider") != "up" {
		t.Errorf("X-Conduit-Provider = %q", resp.Header.Get("X-Conduit-Provider"))
	}
	// Buffering proxies must be told to stay out of the way.
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering: no is missing; an intermediary may buffer the stream")
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.HasSuffix(strings.TrimSpace(text), "data: [DONE]") {
		t.Errorf("stream did not end with the [DONE] terminator:\n%s", text)
	}
	if n := strings.Count(text, "data: "); n != 4 { // 3 chunks + [DONE]
		t.Errorf("frame count = %d, want 4", n)
	}
}

// TestRelayStripsUsageChunkUnlessRequested: Conduit always asks upstream for
// token counts so it can do cost accounting, but must not surface an extra
// choices-empty frame to a client that did not ask for one.
func TestRelayStripsUsageChunkUnlessRequested(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		fl.Flush()
		io.WriteString(w, `data: {"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		fl.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: up
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: up
`, upstream.URL))

	run := func(t *testing.T, includeUsage bool) (string, *Result) {
		t.Helper()
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`
		if includeUsage {
			body = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
		}
		req, err := provider.ParseChatRequest([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		var captured *Result
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handle, res, err := p.OpenStream(r.Context(), req)
			if err != nil {
				t.Errorf("OpenStream: %v", err)
				return
			}
			captured = res
			p.Relay(r.Context(), w, handle, req)
		}))
		defer gw.Close()

		resp, err := http.Get(gw.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return string(out), captured
	}

	t.Run("client did not ask", func(t *testing.T) {
		out, res := run(t, false)
		if strings.Contains(out, `"usage"`) {
			t.Errorf("usage frame leaked to a client that did not request it:\n%s", out)
		}
		// The gateway still recorded it for accounting.
		if res.Usage == nil || res.Usage.TotalTokens != 2 {
			t.Errorf("gateway did not record usage internally: %+v", res.Usage)
		}
	})

	t.Run("client asked", func(t *testing.T) {
		out, _ := run(t, true)
		if !strings.Contains(out, `"usage"`) {
			t.Errorf("usage frame missing for a client that requested it:\n%s", out)
		}
	})
}

// TestClientDisconnectAbortsUpstream is the resource-leak guard: without it the
// gateway keeps paying a provider to generate tokens nobody will read.
func TestClientDisconnectAbortsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // the gateway must cancel this
		close(upstreamDone)
	}))
	defer upstream.Close()

	p := buildProxy(t, fmt.Sprintf(`
providers:
  - name: up
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: up
`, upstream.URL))

	req := chatRequest(t, "m", true)
	ctx, cancel := context.WithCancel(context.Background())

	handle, _, err := p.OpenStream(ctx, req)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	cancel()              // the client hangs up
	handle.Stream.Close() // the handler unwinds

	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was still running after the client disconnected")
	}
}

// TestStreamIdleTimeoutAbortsStalledProvider: a provider that opens a stream,
// sends one token and then goes silent must not hold the request open forever.
func TestStreamIdleTimeoutAbortsStalledProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // then stall indefinitely
	}))
	defer upstream.Close()

	up := testUpstream()
	up.StreamIdleTimeout = config.Duration(300 * time.Millisecond)

	routes, err := config.ParseRoutes([]byte(fmt.Sprintf(`
providers:
  - name: up
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: up
`, upstream.URL)))
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	rtr, err := router.New(routes, provider.NewHTTPClient(up), testLogger())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	p := New(rtr, up, testLogger())

	req := chatRequest(t, "m", true)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle, _, err := p.OpenStream(r.Context(), req)
		if err != nil {
			t.Errorf("OpenStream: %v", err)
			return
		}
		p.Relay(r.Context(), w, handle, req)
	}))
	defer gw.Close()

	start := time.Now()
	resp, err := http.Get(gw.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("request took %v; the idle watchdog did not fire", elapsed)
	}
}
