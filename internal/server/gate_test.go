package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/darshpatel1052/conduit/internal/analytics"
	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/ratelimit"
	"github.com/darshpatel1052/conduit/internal/router"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// captureRecorder collects analytics events in memory so tests can assert on
// what the gateway reported rather than on what it logged.
type captureRecorder struct {
	mu     sync.Mutex
	events []analytics.Event
}

func (c *captureRecorder) Record(e analytics.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureRecorder) Close(context.Context) error { return nil }

func (c *captureRecorder) withStatus(status int) []analytics.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []analytics.Event
	for _, e := range c.events {
		if e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

// newGatedGateway builds a gateway with auth and/or rate limiting enabled.
func newGatedGateway(t *testing.T, upstreamURL string, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	srv, _, _ := newGatedGatewayWithDeps(t, upstreamURL, mutate)
	return srv
}

// newGatedGatewayWithDeps additionally hands back the analytics capture and the
// metrics registry, for tests that assert a request was *reported*, not just
// answered.
func newGatedGatewayWithDeps(t *testing.T, upstreamURL string, mutate func(*config.Config)) (
	*httptest.Server, *captureRecorder, *telemetry.Metrics) {
	t.Helper()

	routes, err := config.ParseRoutes([]byte(fmt.Sprintf(`
providers:
  - name: p
    type: ollama
    base_url: %s
routes:
  - model: m
    targets:
      - provider: p
  - model: restricted-model
    targets:
      - provider: p
`, upstreamURL)))
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}

	cfg := config.Default()
	cfg.Upstream.RequestTimeout = config.Duration(5 * time.Second)
	mutate(cfg)

	rtr, err := router.New(routes, provider.NewHTTPClient(cfg.Upstream), testLogger())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	authenticator, err := auth.New(cfg.Auth, cfg.RateLimit)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	var limiter ratelimit.Limiter = ratelimit.NoopLimiter{}
	if cfg.RateLimit.Enabled {
		mr := miniredis.RunT(t)
		cfg.RateLimit.RedisURL = "redis://" + mr.Addr()
		rl, err := ratelimit.New(cfg.RateLimit, testLogger())
		if err != nil {
			t.Fatalf("ratelimit.New: %v", err)
		}
		t.Cleanup(func() { rl.Close() })
		limiter = rl
	}

	capture := &captureRecorder{}
	metrics := telemetry.NewMetrics()

	s := New(cfg, rtr, proxy.New(rtr, cfg.Upstream, testLogger()),
		Deps{Auth: authenticator, Limiter: limiter, Analytics: capture, Metrics: metrics},
		testLogger())
	return httptest.NewServer(s.http.Handler), capture, metrics
}

func postWithKey(t *testing.T, url, key, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func stringReader(s string) *stringReadCloser { return &stringReadCloser{s: s} }

type stringReadCloser struct {
	s string
	i int
}

func (r *stringReadCloser) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func TestAuthRejectsMissingAndInvalidKeys(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	plaintext, hash, _ := auth.GenerateKey()
	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{{ID: "k1", Hash: hash}}
	})
	defer gw.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	resp, _ := postWithKey(t, gw.URL, "", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 should carry WWW-Authenticate so clients know how to authenticate")
	}

	resp, _ = postWithKey(t, gw.URL, "ck_bogus", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad key: status = %d, want 401", resp.StatusCode)
	}

	resp, _ = postWithKey(t, gw.URL, plaintext, body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("valid key: status = %d, want 200", resp.StatusCode)
	}
}

// TestProbesBypassAuth: an orchestrator has no credential, and requiring one
// would make every pod look dead.
func TestProbesBypassAuth(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	_, hash, _ := auth.GenerateKey()
	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{{ID: "k1", Hash: hash}}
	})
	defer gw.Close()

	for _, path := range []string{"/healthz", "/readyz", "/livez"} {
		resp, err := http.Get(gw.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d with auth enabled, want 200", path, resp.StatusCode)
		}
	}
}

func TestModelRestrictionIsEnforced(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	plaintext, hash, _ := auth.GenerateKey()
	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{{ID: "limited", Hash: hash, Models: []string{"m"}}}
	})
	defer gw.Close()

	resp, _ := postWithKey(t, gw.URL, plaintext,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("permitted model: status = %d, want 200", resp.StatusCode)
	}

	resp, body := postWithKey(t, gw.URL, plaintext,
		`{"model":"restricted-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("restricted model: status = %d, want 403 (body: %s)", resp.StatusCode, body)
	}
}

func TestRateLimitReturns429WithHeaders(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	plaintext, hash, _ := auth.GenerateKey()
	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{{ID: "k1", Hash: hash, RequestsPerMinute: 3}}
		c.RateLimit.Enabled = true
		c.RateLimit.Algorithm = config.AlgorithmSlidingWindow
	})
	defer gw.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	for i := 1; i <= 3; i++ {
		resp, b := postWithKey(t, gw.URL, plaintext, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d (body: %s)", i, resp.StatusCode, b)
		}
		if resp.Header.Get("X-RateLimit-Limit") != "3" {
			t.Errorf("request %d: X-RateLimit-Limit = %q, want 3", i, resp.Header.Get("X-RateLimit-Limit"))
		}
		wantRemaining := strconv.Itoa(3 - i)
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("request %d: X-RateLimit-Remaining = %q, want %q", i, got, wantRemaining)
		}
	}

	resp, _ := postWithKey(t, gw.URL, plaintext, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th request: status = %d, want 429", resp.StatusCode)
	}

	// Retry-After is what makes a 429 actionable; without it clients busy-loop.
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("429 is missing Retry-After")
	}
	if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer of seconds", ra)
	}
}

// TestRejectionsAreReported is the regression for a gap live verification
// found: rejections happen in middleware, before the observation that reports a
// request exists, so 429s and 401s reached neither ClickHouse nor Prometheus.
// A throttled tenant then looks identical to a tenant who stopped sending
// traffic — opposite problems needing opposite responses — and the counter an
// operator would alert on stayed at zero no matter how much was rejected.
func TestRejectionsAreReported(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	plaintext, hash, _ := auth.GenerateKey()
	gw, capture, metrics := newGatedGatewayWithDeps(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{{ID: "k1", Hash: hash, RequestsPerMinute: 2}}
		c.RateLimit.Enabled = true
		c.RateLimit.Algorithm = config.AlgorithmSlidingWindow
	})
	defer gw.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		postWithKey(t, gw.URL, plaintext, body)
	}
	postWithKey(t, gw.URL, "ck_not-a-real-key", body)

	rejections := capture.withStatus(http.StatusTooManyRequests)
	if len(rejections) != 1 {
		t.Fatalf("recorded %d rate-limit rejections, want 1", len(rejections))
	}
	if rejections[0].ErrorKind != "rate_limit" {
		t.Errorf("error_kind = %q, want rate_limit", rejections[0].ErrorKind)
	}
	// Without the key, the analytics cannot answer "who is being throttled?",
	// which is the only question worth asking of this row.
	if rejections[0].APIKeyID != "k1" {
		t.Errorf("api_key_id = %q, want k1", rejections[0].APIKeyID)
	}

	unauthorized := capture.withStatus(http.StatusUnauthorized)
	if len(unauthorized) != 1 {
		t.Fatalf("recorded %d auth rejections, want 1", len(unauthorized))
	}
	// A failed credential has no identity to attribute, and recording the
	// rejected key would put a possibly-valid secret into the analytics store.
	if unauthorized[0].APIKeyID != "" {
		t.Errorf("api_key_id = %q on a failed auth; want it empty", unauthorized[0].APIKeyID)
	}

	if got := counterValue(t, metrics.RateLimitRejections.WithLabelValues("requests")); got != 1 {
		t.Errorf("conduit_rate_limit_rejections_total{dimension=requests} = %v, want 1", got)
	}
}

// TestTokenRejectionsAreReportedSeparately: "too many calls" and "too many
// tokens" have different causes and different fixes, so an alert has to be able
// to tell them apart.
func TestTokenRejectionsAreReportedSeparately(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	plaintext, hash, _ := auth.GenerateKey()
	gw, capture, metrics := newGatedGatewayWithDeps(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		// A budget of 1 against an upstream that reports 2 tokens per call, so
		// the first response alone overshoots. That also covers the debit bug:
		// a cost exceeding the whole budget still has to be recorded.
		c.Auth.Keys = []config.APIKey{{ID: "k1", Hash: hash, RequestsPerMinute: 100, TokensPerMinute: 1}}
		c.RateLimit.Enabled = true
		c.RateLimit.Algorithm = config.AlgorithmSlidingWindow
	})
	defer gw.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	// The first is served and debited; the fake upstream reports more tokens
	// than the budget allows, so the second is refused on tokens, not requests.
	postWithKey(t, gw.URL, plaintext, body)
	resp, _ := postWithKey(t, gw.URL, plaintext, body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 on the token budget", resp.StatusCode)
	}

	if got := counterValue(t, metrics.RateLimitRejections.WithLabelValues("tokens")); got != 1 {
		t.Errorf("rejections{dimension=tokens} = %v, want 1", got)
	}
	if got := counterValue(t, metrics.RateLimitRejections.WithLabelValues("requests")); got != 0 {
		t.Errorf("rejections{dimension=requests} = %v, want 0; a token rejection was miscounted as a request one", got)
	}

	rejections := capture.withStatus(http.StatusTooManyRequests)
	if len(rejections) != 1 {
		t.Fatalf("recorded %d rejections, want 1", len(rejections))
	}
	// The model is known by this point, so the row can answer which model is
	// burning the budget.
	if rejections[0].RequestedModel != "m" {
		t.Errorf("requested_model = %q, want m", rejections[0].RequestedModel)
	}
}

// counterValue reads a counter's current value out of the Prometheus client.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestRateLimitIsPerKey: one tenant exhausting its quota must not throttle
// another. This is the property an in-memory counter cannot give you across
// replicas, and the reason state lives in Redis.
func TestRateLimitIsPerKey(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	keyA, hashA, _ := auth.GenerateKey()
	keyB, hashB, _ := auth.GenerateKey()

	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{
			{ID: "a", Hash: hashA, RequestsPerMinute: 2},
			{ID: "b", Hash: hashB, RequestsPerMinute: 2},
		}
		c.RateLimit.Enabled = true
	})
	defer gw.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 2; i++ {
		postWithKey(t, gw.URL, keyA, body)
	}
	if resp, _ := postWithKey(t, gw.URL, keyA, body); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("key A should be exhausted, got %d", resp.StatusCode)
	}

	if resp, _ := postWithKey(t, gw.URL, keyB, body); resp.StatusCode != http.StatusOK {
		t.Errorf("key B was throttled by key A's usage: got %d", resp.StatusCode)
	}
}

// TestSharedRedisEnforcesLimitAcrossInstances is the whole point of the design:
// two gateway processes sharing one Redis enforce one combined limit, exactly
// as replicas behind a load balancer would.
func TestSharedRedisEnforcesLimitAcrossInstances(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	plaintext, hash, _ := auth.GenerateKey()
	mr := miniredis.RunT(t)

	build := func() *httptest.Server {
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
		cfg.Auth.Enabled = true
		cfg.Auth.Keys = []config.APIKey{{ID: "shared", Hash: hash, RequestsPerMinute: 4}}
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.RedisURL = "redis://" + mr.Addr()

		rtr, err := router.New(routes, provider.NewHTTPClient(cfg.Upstream), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		a, err := auth.New(cfg.Auth, cfg.RateLimit)
		if err != nil {
			t.Fatal(err)
		}
		rl, err := ratelimit.New(cfg.RateLimit, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rl.Close() })

		s := New(cfg, rtr, proxy.New(rtr, cfg.Upstream, testLogger()),
			Deps{Auth: a, Limiter: rl}, testLogger())
		return httptest.NewServer(s.http.Handler)
	}

	gw1, gw2 := build(), build()
	defer gw1.Close()
	defer gw2.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	allowed := 0
	for i := 0; i < 8; i++ {
		target := gw1
		if i%2 == 1 {
			target = gw2
		}
		if resp, _ := postWithKey(t, target.URL, plaintext, body); resp.StatusCode == http.StatusOK {
			allowed++
		}
	}

	if allowed != 4 {
		t.Errorf("allowed %d requests across two instances, want 4: per-instance "+
			"counters would have allowed 8", allowed)
	}
}

func TestAnonymousTrafficIsStillLimited(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = false
		c.RateLimit.Enabled = true
		c.RateLimit.AnonymousRequestsPerMinute = 2
	})
	defer gw.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		if resp, _ := postWithKey(t, gw.URL, "", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("anonymous request %d rejected early", i+1)
		}
	}
	// An unauthenticated gateway with no limit at all is a free proxy for
	// whoever finds it.
	if resp, _ := postWithKey(t, gw.URL, "", body); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("anonymous traffic is unlimited: got %d, want 429", resp.StatusCode)
	}
}

func TestAuthDisabledAllowsEverything(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = false
	})
	defer gw.Close()

	resp, body := postWithKey(t, gw.URL, "", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d with auth disabled, want 200 (body: %s)", resp.StatusCode, body)
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},                     // never advertise an immediate retry
		{10 * time.Millisecond, 1}, // sub-second rounds up
		{1500 * time.Millisecond, 2},
		{3 * time.Second, 3},
	}
	for _, tc := range tests {
		if got := retryAfterSeconds(tc.in); got != tc.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestMetricsEndpointBypassesAuth is a regression test: /metrics returned 401
// with auth on, which silently breaks Prometheus scraping — the gateway looks
// healthy while all its dashboards go blank. Scrapers carry no credential, so
// the endpoint must answer without one. It exposes aggregate counters, not
// prompt content, and belongs on the pod network rather than the internet.
func TestMetricsEndpointBypassesAuth(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	_, hash, _ := auth.GenerateKey()
	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{{ID: "k1", Hash: hash}}
		c.Metrics.Enabled = true
	})
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics returned %d with auth enabled, want 200", resp.StatusCode)
	}

	// The exemption must not leak to anything else.
	for _, path := range []string{"/admin/routes", "/v1/models"} {
		resp, err := http.Get(gw.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s returned %d, want 401: the metrics exemption must be narrow", path, resp.StatusCode)
		}
	}
}

func TestAdminEndpointsRequireAdminCapability(t *testing.T) {
	up := upstreamCompletion(t)
	defer up.Close()

	userKey, userHash, _ := auth.GenerateKey()
	adminKey, adminHash, _ := auth.GenerateKey()
	gw := newGatedGateway(t, up.URL, func(c *config.Config) {
		c.Auth.Enabled = true
		c.Auth.Keys = []config.APIKey{
			{ID: "user", Hash: userHash},
			{ID: "ops", Hash: adminHash, Admin: true},
		}
	})
	defer gw.Close()

	request := func(key string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, gw.URL+"/admin/routes", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	userResp := request(userKey)
	userResp.Body.Close()
	if userResp.StatusCode != http.StatusForbidden {
		t.Fatalf("ordinary inference key: status = %d, want 403", userResp.StatusCode)
	}

	adminResp := request(adminKey)
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin key: status = %d, want 200", adminResp.StatusCode)
	}
}
