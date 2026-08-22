package server

import (
	"encoding/json"
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
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/router"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestGateway builds a full gateway (middleware, handlers, router, proxy)
// in front of the given upstream, and returns an httptest server for it. This
// exercises the real wiring rather than a handler in isolation.
func newTestGateway(t *testing.T, routesYAML string) *httptest.Server {
	t.Helper()

	routes, err := config.ParseRoutes([]byte(routesYAML))
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}

	cfg := config.Default()
	cfg.Upstream.RequestTimeout = config.Duration(5 * time.Second)
	cfg.Upstream.StreamFirstChunkTimeout = config.Duration(3 * time.Second)
	cfg.Upstream.StreamIdleTimeout = config.Duration(2 * time.Second)

	rtr, err := router.New(routes, provider.NewHTTPClient(cfg.Upstream), testLogger())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	s := New(cfg, rtr, proxy.New(rtr, cfg.Upstream, testLogger()), Deps{}, testLogger())
	return httptest.NewServer(s.http.Handler)
}

func upstreamCompletion(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},
			"provider_extension":"preserved"}`)
	}))
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestHealthzIsDependencyFree(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	// Liveness must not depend on upstreams: a provider outage that flipped
	// liveness would make the orchestrator restart a perfectly healthy gateway.
	up.Close()

	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d with the upstream down, want 200", resp.StatusCode)
	}
}

func TestChatCompletionsHappyPath(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	resp, body := post(t, gw.URL, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Conduit-Provider") != "p" {
		t.Errorf("X-Conduit-Provider = %q", resp.Header.Get("X-Conduit-Provider"))
	}
	// The upstream's own bytes are relayed, so fields Conduit does not model
	// survive the hop.
	if !strings.Contains(body, "provider_extension") {
		t.Error("an unmodelled upstream field was dropped from the response")
	}
}

func TestEmbeddingsAndResponsesEndpoints(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/embeddings":
			io.WriteString(w, `{"object":"list","model":"vector-model","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
		case "/v1/chat/completions":
			io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"text-model","choices":[{"index":0,"message":{"role":"assistant","content":"portable answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: search
    targets:
      - provider: p
        model: vector-model
  - model: quality
    targets:
      - provider: p
        model: text-model
`, up.URL))
	defer gw.Close()

	for _, tc := range []struct{ path, body, contains string }{
		{"/v1/embeddings", `{"model":"search","input":"hello world"}`, `"embedding":[0.1,0.2]`},
		{"/v1/responses", `{"model":"quality","input":"hello"}`, `"text":"portable answer"`},
	} {
		resp, err := http.Post(gw.URL+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(payload), tc.contains) {
			t.Fatalf("%s returned %d: %s", tc.path, resp.StatusCode, payload)
		}
	}
}

func TestResponsesStreamingContract(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, frame := range []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"portable "}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"stream"}}]}`,
			`{"id":"c1","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: quality
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"quality","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	body := string(payload)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected stream response: status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	for _, event := range []string{"response.created", "response.output_text.delta", "response.completed", "portable stream"} {
		if !strings.Contains(body, event) {
			t.Errorf("stream is missing %q: %s", event, body)
		}
	}
}

func TestErrorsUseOpenAIEnvelope(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"malformed json", `{not json`, http.StatusBadRequest},
		{"no messages", `{"model":"m","messages":[]}`, http.StatusBadRequest},
		{"unknown model", `{"model":"ghost","messages":[{"role":"user","content":"x"}]}`, http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := post(t, gw.URL, tc.body)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", resp.StatusCode, tc.wantStatus, body)
			}
			// Clients parse OpenAI's envelope; failing in a different shape
			// breaks every SDK's error handling.
			var env struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("error body is not JSON: %s", body)
			}
			if env.Error.Message == "" || env.Error.Type == "" {
				t.Errorf("error envelope is incomplete: %s", body)
			}
		})
	}
}

func TestBodyLimitRejectsOversizedRequests(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	routes, err := config.ParseRoutes([]byte(fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL)))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Server.MaxRequestBytes = 512 // tiny, to make the test cheap

	rtr, err := router.New(routes, provider.NewHTTPClient(cfg.Upstream), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, rtr, proxy.New(rtr, cfg.Upstream, testLogger()), Deps{}, testLogger())
	gw := httptest.NewServer(s.http.Handler)
	defer gw.Close()

	huge := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%q}]}`, strings.Repeat("x", 2000))
	resp, body := post(t, gw.URL, huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body: %s)", resp.StatusCode, body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: alpha
    targets:
      - provider: p
  - model: beta
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestAdminRoutesExposesBreakerState(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/admin/routes")
	if err != nil {
		t.Fatalf("GET /admin/routes: %v", err)
	}
	defer resp.Body.Close()

	var snap router.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Providers) != 1 || snap.Providers[0].Breaker.State != "closed" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if len(snap.Routes) != 1 || len(snap.Routes[0].Targets) != 1 {
		t.Fatalf("routes missing from snapshot: %+v", snap.Routes)
	}
}

// TestStreamingThroughFullStack is the end-to-end streaming check: it goes
// through the real middleware chain, which is where a ResponseWriter wrapper
// would break flushing if it did not implement Unwrap.
func TestStreamingThroughFullStack(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
			fl.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()

	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	resp, body := post(t, gw.URL, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(body, fmt.Sprintf("tok%d", i)) {
			t.Errorf("missing token %d in stream:\n%s", i, body)
		}
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("stream is missing the [DONE] terminator")
	}
}

// TestStreamFailoverReturnsJSONErrorBeforeFirstByte: because failover happens
// before anything is written, a total failure is still a normal JSON error with
// a real status code rather than a broken half-open stream.
func TestStreamFailoverReturnsJSONErrorBeforeFirstByte(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"unavailable"}}`)
	}))
	defer down.Close()

	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, down.URL))
	defer gw.Close()

	resp, body := post(t, gw.URL, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want a real error status; body: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON for a pre-stream failure", ct)
	}
	if !strings.Contains(body, `"error"`) {
		t.Errorf("body is not an error envelope: %s", body)
	}
}

func TestMethodNotAllowedAndNotFound(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()
	gw := newTestGateway(t, fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
`, up.URL))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on the completions endpoint = %d, want 405", resp.StatusCode)
	}

	resp, err = http.Get(gw.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", resp.StatusCode)
	}
}
