package router

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustRoutes(t *testing.T, yaml string) *config.Routes {
	t.Helper()
	r, err := config.ParseRoutes([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	return r
}

func mustRouter(t *testing.T, yaml string) *Router {
	t.Helper()
	r, err := New(mustRoutes(t, yaml), &http.Client{}, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

const twoProviderConfig = `
providers:
  - name: a
    type: ollama
    base_url: http://localhost:1111
  - name: b
    type: ollama
    base_url: http://localhost:2222
routes:
  - model: priority-route
    strategy: priority
    targets:
      - provider: a
      - provider: b
  - model: rr-route
    strategy: round_robin
    targets:
      - provider: a
      - provider: b
  - model: weighted-route
    strategy: weighted
    targets:
      - provider: a
        weight: 3
      - provider: b
        weight: 1
`

// TestPriorityAlwaysStartsFirst confirms priority is failover ordering, not
// balancing: the same target leads every time.
func TestPriorityAlwaysStartsFirst(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	for i := 0; i < 5; i++ {
		res, err := r.Resolve("priority-route")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if res.Targets[0].Entry.Name != "a" {
			t.Fatalf("request %d started at %q, want a", i, res.Targets[0].Entry.Name)
		}
		// The remaining targets are the failover queue.
		if len(res.Targets) != 2 || res.Targets[1].Entry.Name != "b" {
			t.Fatalf("failover queue = %v, want [b]", res.Targets[1:])
		}
	}
}

func TestRoundRobinRotates(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	var seen []string
	for i := 0; i < 4; i++ {
		res, _ := r.Resolve("rr-route")
		seen = append(seen, res.Targets[0].Entry.Name)
	}

	want := []string{"a", "b", "a", "b"}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("round-robin order = %v, want %v", seen, want)
		}
	}
}

// TestRoundRobinKeepsFullFailoverQueue: balancing picks where a request starts,
// but every other target must still be reachable if that one fails.
func TestRoundRobinKeepsFullFailoverQueue(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	res, _ := r.Resolve("rr-route")
	if len(res.Targets) != 2 {
		t.Fatalf("targets = %d, want 2: balancing must not discard failover options", len(res.Targets))
	}
	if res.Targets[0].Entry.Name == res.Targets[1].Entry.Name {
		t.Fatal("failover queue repeats the primary target")
	}
}

// TestWeightedIsSmooth checks the distribution AND the interleaving. A naive
// weight-expansion balancer produces AAAB, which bursts load onto one target;
// smooth WRR produces ABAA.
func TestWeightedIsSmooth(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	var seq []string
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		res, _ := r.Resolve("weighted-route")
		name := res.Targets[0].Entry.Name
		seq = append(seq, name)
		counts[name]++
	}

	if counts["a"] != 6 || counts["b"] != 2 {
		t.Errorf("distribution over 8 picks = %v, want a:6 b:2 for weights 3:1", counts)
	}
	// b must not be starved to the end of the cycle.
	firstB := -1
	for i, n := range seq {
		if n == "b" {
			firstB = i
			break
		}
	}
	if firstB > 2 {
		t.Errorf("sequence %v bursts the heavy target; smooth WRR should interleave b early", seq)
	}
}

func TestExactMatchBeatsGlob(t *testing.T) {
	r := mustRouter(t, `
providers:
  - name: specific
    type: ollama
    base_url: http://localhost:1111
  - name: catchall
    type: ollama
    base_url: http://localhost:2222
routes:
  - model: "gpt-*"
    targets:
      - provider: catchall
  - model: gpt-4o-mini
    targets:
      - provider: specific
`)

	res, err := r.Resolve("gpt-4o-mini")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Targets[0].Entry.Name != "specific" {
		t.Errorf("exact route lost to the glob: got %q", res.Targets[0].Entry.Name)
	}

	res, err = r.Resolve("gpt-3.5-turbo")
	if err != nil {
		t.Fatalf("Resolve glob: %v", err)
	}
	if res.Targets[0].Entry.Name != "catchall" {
		t.Errorf("glob route not matched: got %q", res.Targets[0].Entry.Name)
	}
	if res.Targets[0].Model != "gpt-3.5-turbo" {
		t.Errorf("glob target model = %q, want matched model %q", res.Targets[0].Model, "gpt-3.5-turbo")
	}
}

func TestLongestGlobWins(t *testing.T) {
	r := mustRouter(t, `
providers:
  - name: broad
    type: ollama
    base_url: http://localhost:1111
  - name: narrow
    type: ollama
    base_url: http://localhost:2222
routes:
  - model: "gpt-*"
    targets:
      - provider: broad
  - model: "gpt-4o-*"
    targets:
      - provider: narrow
`)

	res, _ := r.Resolve("gpt-4o-mini")
	if res.Targets[0].Entry.Name != "narrow" {
		t.Errorf("got %q, want the more specific glob to win regardless of file order",
			res.Targets[0].Entry.Name)
	}
}

func TestUnknownModelIsRejected(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	_, err := r.Resolve("no-such-model")
	if err == nil {
		t.Fatal("Resolve of an unknown model succeeded; it must not silently pick a route")
	}
	pe, ok := provider.AsError(err)
	if !ok || pe.Kind != provider.KindNoRoute {
		t.Errorf("error = %v, want a no_route provider error", err)
	}
}

func TestDefaultModelCatchesUnmatched(t *testing.T) {
	r := mustRouter(t, `
providers:
  - name: a
    type: ollama
    base_url: http://localhost:1111
routes:
  - model: fallback
    targets:
      - provider: a
default_model: fallback
`)

	res, err := r.Resolve("something-unheard-of")
	if err != nil {
		t.Fatalf("Resolve with a default route: %v", err)
	}
	if res.RouteModel != "fallback" {
		t.Errorf("route = %q, want fallback", res.RouteModel)
	}
}

func TestMaxAttemptsCapsFailover(t *testing.T) {
	r := mustRouter(t, `
providers:
  - name: a
    type: ollama
    base_url: http://localhost:1111
  - name: b
    type: ollama
    base_url: http://localhost:2222
  - name: c
    type: ollama
    base_url: http://localhost:3333
routes:
  - model: capped
    strategy: priority
    max_attempts: 2
    targets:
      - provider: a
      - provider: b
      - provider: c
`)

	res, _ := r.Resolve("capped")
	if res.MaxAttempts != 2 {
		t.Errorf("MaxAttempts = %d, want 2: an uncapped fan-out multiplies load during an outage",
			res.MaxAttempts)
	}
}

func TestTargetModelRewrite(t *testing.T) {
	r := mustRouter(t, `
providers:
  - name: a
    type: ollama
    base_url: http://localhost:1111
routes:
  - model: virtual-name
    targets:
      - provider: a
        model: actual-upstream-name
`)

	res, _ := r.Resolve("virtual-name")
	if res.Targets[0].Model != "actual-upstream-name" {
		t.Errorf("upstream model = %q, want the rewritten name", res.Targets[0].Model)
	}
}

func TestTargetModelDefaultsToRouteModel(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	res, _ := r.Resolve("priority-route")
	if res.Targets[0].Model != "priority-route" {
		t.Errorf("upstream model = %q, want it to default to the route model", res.Targets[0].Model)
	}
}

// TestReloadPreservesBreakerState is the one that matters for operating this
// thing: editing an unrelated route must not reset every circuit breaker to
// closed and unleash a retry storm at providers already known to be down.
func TestReloadPreservesBreakerState(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	entryBefore := r.Table().providers["a"]

	// Trip provider a's breaker.
	for i := 0; i < 10; i++ {
		done, err := entryBefore.Breaker.Allow()
		if err != nil {
			break
		}
		done(&provider.Error{Kind: provider.KindUpstream, Retryable: true, CountsAsFailure: true})
	}
	if entryBefore.Breaker.State() != "open" {
		t.Fatalf("setup failed: breaker state = %q, want open", entryBefore.Breaker.State())
	}

	// Reload with an added, unrelated route. Provider configs are untouched.
	if err := r.Reload(mustRoutes(t, twoProviderConfig+`
  - model: newly-added
    targets:
      - provider: b
`)); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	entryAfter := r.Table().providers["a"]
	if entryAfter != entryBefore {
		t.Error("provider entry was rebuilt despite an unchanged config")
	}
	if got := entryAfter.Breaker.State(); got != "open" {
		t.Fatalf("breaker state after reload = %q, want open: reloading config must not "+
			"resurrect a provider that is still down", got)
	}
	if _, ok := r.Table().exact["newly-added"]; !ok {
		t.Error("the new route was not applied")
	}
}

// TestReloadRebuildsChangedProvider is the other half: when a provider's
// configuration actually changes, its adapter and breaker must be rebuilt so
// the new settings take effect.
func TestReloadRebuildsChangedProvider(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)
	before := r.Table().providers["a"]

	changed := `
providers:
  - name: a
    type: ollama
    base_url: http://localhost:9999
  - name: b
    type: ollama
    base_url: http://localhost:2222
routes:
  - model: priority-route
    strategy: priority
    targets:
      - provider: a
      - provider: b
`
	if err := r.Reload(mustRoutes(t, changed)); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if r.Table().providers["a"] == before {
		t.Error("provider entry was reused after its base_url changed")
	}
	if r.Table().providers["b"] != r.Table().providers["b"] {
		t.Error("unstable table")
	}
}

// TestReloadPreservesBalancerCursor: a reload must not reset every round-robin
// cursor to zero and dogpile the first target in each pool.
func TestReloadPreservesBalancerCursor(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	res, _ := r.Resolve("rr-route")
	if res.Targets[0].Entry.Name != "a" {
		t.Fatalf("setup: first pick = %q, want a", res.Targets[0].Entry.Name)
	}

	if err := r.Reload(mustRoutes(t, twoProviderConfig)); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	res, _ = r.Resolve("rr-route")
	if res.Targets[0].Entry.Name != "b" {
		t.Errorf("pick after reload = %q, want b: the cursor was reset", res.Targets[0].Entry.Name)
	}
}

func TestReadyRequiresAtLeastOneHealthyProvider(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	if !r.Ready() {
		t.Fatal("a fresh router should be ready")
	}

	// Trip only one provider: the gateway is still useful.
	entry := r.Table().providers["a"]
	for i := 0; i < 10; i++ {
		done, err := entry.Breaker.Allow()
		if err != nil {
			break
		}
		done(&provider.Error{Kind: provider.KindUpstream, Retryable: true, CountsAsFailure: true})
	}
	if !r.Ready() {
		t.Error("readiness dropped with one provider down; that would pull a working replica out of service")
	}

	// Trip the other too: now there is nothing left to serve with.
	entry = r.Table().providers["b"]
	for i := 0; i < 10; i++ {
		done, err := entry.Breaker.Allow()
		if err != nil {
			break
		}
		done(&provider.Error{Kind: provider.KindUpstream, Retryable: true, CountsAsFailure: true})
	}
	if r.Ready() {
		t.Error("readiness should be false when every provider's breaker is open")
	}
}

func TestModelsListsRoutes(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	models := r.Models()
	want := map[string]bool{"priority-route": true, "rr-route": true, "weighted-route": true}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %d entries", models, len(want))
	}
	for _, m := range models {
		if !want[m] {
			t.Errorf("unexpected model %q", m)
		}
	}
}

func TestSnapshotReportsBreakerState(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	snap := r.Snapshot()
	if len(snap.Providers) != 2 {
		t.Fatalf("snapshot providers = %d, want 2", len(snap.Providers))
	}
	if snap.Checksum == "" {
		t.Error("snapshot is missing the config checksum")
	}
	for _, p := range snap.Providers {
		if p.Breaker.State != "closed" {
			t.Errorf("provider %q breaker = %q, want closed", p.Name, p.Breaker.State)
		}
	}
}

// Concurrency guard: resolution reads a table with no locking while a reload
// swaps it. Run with -race.
func TestConcurrentResolveAndReload(t *testing.T) {
	r := mustRouter(t, twoProviderConfig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if _, err := r.Resolve("rr-route"); err != nil {
				t.Errorf("Resolve during reload: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if err := r.Reload(mustRoutes(t, twoProviderConfig)); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	}
	<-done
}
