package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const validRoutes = `
providers:
  - name: local
    type: ollama
    base_url: http://localhost:11434
routes:
  - model: m
    targets:
      - provider: local
`

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    time.Duration
		wantErr bool
	}{
		{"seconds string", `read_timeout: 30s`, 30 * time.Second, false},
		{"minutes string", `read_timeout: 2m`, 2 * time.Minute, false},
		{"bare number is seconds", `read_timeout: 45`, 45 * time.Second, false},
		{"zero", `read_timeout: 0s`, 0, false},
		{"garbage", `read_timeout: "not a duration"`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s struct {
				ReadTimeout Duration `yaml:"read_timeout"`
			}
			err := yamlUnmarshal([]byte(tc.yaml), &s)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if s.ReadTimeout.D() != tc.want {
				t.Errorf("got %v, want %v", s.ReadTimeout.D(), tc.want)
			}
		})
	}
}

// TestWriteTimeoutMustBeZero pins the single most damaging misconfiguration
// available here: a non-zero write deadline silently truncates every stream
// that outlives it, and it looks like a model problem rather than a config one.
func TestWriteTimeoutMustBeZero(t *testing.T) {
	cfg := Default()
	cfg.Server.WriteTimeout = Duration(30 * time.Second)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a non-zero write_timeout")
	}
	if !strings.Contains(err.Error(), "write_timeout") {
		t.Errorf("error %q should name the offending field", err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty addr", func(c *Config) { c.Server.Addr = "" }},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }},
		{"no routes file", func(c *Config) { c.RoutesFile = "" }},
		{"zero body limit", func(c *Config) { c.Server.MaxRequestBytes = 0 }},
		{"zero connect timeout", func(c *Config) { c.Upstream.ConnectTimeout = 0 }},
		{"zero idle timeout", func(c *Config) { c.Upstream.StreamIdleTimeout = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}

func TestExpandEnvLeavesUnsetVarsAlone(t *testing.T) {
	t.Setenv("CONDUIT_TEST_SET", "resolved")

	in := []byte("a: ${CONDUIT_TEST_SET}\nb: ${CONDUIT_TEST_NEVER_SET}\n")
	got := string(ExpandEnv(in))

	if !strings.Contains(got, "a: resolved") {
		t.Errorf("set variable was not expanded: %q", got)
	}
	// Blanking an unset secret would turn a missing credential into a silent
	// empty string; leaving it verbatim makes the mistake obvious.
	if !strings.Contains(got, "b: ${CONDUIT_TEST_NEVER_SET}") {
		t.Errorf("unset variable was blanked instead of left verbatim: %q", got)
	}
}

func TestDataServiceEnvironmentOverrides(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://redis.internal:6379/2")
	t.Setenv("POSTGRES_DSN", "postgres://cache.internal/conduit")
	t.Setenv("CLICKHOUSE_DSN", "clickhouse://analytics.internal/conduit")

	cfg := Default()
	cfg.applyEnvOverrides()

	if cfg.RateLimit.RedisURL != "redis://redis.internal:6379/2" {
		t.Errorf("RedisURL = %q", cfg.RateLimit.RedisURL)
	}
	if cfg.Cache.PostgresDSN != "postgres://cache.internal/conduit" {
		t.Errorf("PostgresDSN = %q", cfg.Cache.PostgresDSN)
	}
	if cfg.Analytics.ClickHouseDSN != "clickhouse://analytics.internal/conduit" {
		t.Errorf("ClickHouseDSN = %q", cfg.Analytics.ClickHouseDSN)
	}
}

// TestUnknownFieldIsRejected: a typo like "provder:" would otherwise be
// silently ignored, leaving a route mysteriously absent at runtime.
func TestUnknownFieldIsRejected(t *testing.T) {
	_, err := ParseRoutes([]byte(`
providers:
  - name: local
    type: ollama
    base_url: http://localhost:11434
    base_urll: http://typo
routes:
  - model: m
    targets:
      - provider: local
`))
	if err == nil {
		t.Fatal("ParseRoutes accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "base_urll") {
		t.Errorf("error %q should name the unknown field", err)
	}
}

func TestRoutesValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			"unknown provider type",
			"providers:\n  - name: p\n    type: bedrock\n    base_url: http://x\nroutes:\n  - model: m\n    targets:\n      - provider: p\n",
			"unknown type",
		},
		{
			"route references missing provider",
			"providers:\n  - name: p\n    type: ollama\n    base_url: http://x\nroutes:\n  - model: m\n    targets:\n      - provider: nope\n",
			"unknown provider",
		},
		{
			"duplicate provider",
			"providers:\n  - name: p\n    type: ollama\n    base_url: http://x\n  - name: p\n    type: ollama\n    base_url: http://y\nroutes:\n  - model: m\n    targets:\n      - provider: p\n",
			"duplicate provider",
		},
		{
			"duplicate route",
			"providers:\n  - name: p\n    type: ollama\n    base_url: http://x\nroutes:\n  - model: m\n    targets:\n      - provider: p\n  - model: m\n    targets:\n      - provider: p\n",
			"duplicate route",
		},
		{
			"route with no targets",
			"providers:\n  - name: p\n    type: ollama\n    base_url: http://x\nroutes:\n  - model: m\n    targets: []\n",
			"no targets",
		},
		{
			"unknown strategy",
			"providers:\n  - name: p\n    type: ollama\n    base_url: http://x\nroutes:\n  - model: m\n    strategy: random\n    targets:\n      - provider: p\n",
			"unknown strategy",
		},
		{
			"bad base url",
			"providers:\n  - name: p\n    type: ollama\n    base_url: \"ftp://x\"\nroutes:\n  - model: m\n    targets:\n      - provider: p\n",
			"scheme must be http",
		},
		{
			"default model with no route",
			"providers:\n  - name: p\n    type: ollama\n    base_url: http://x\nroutes:\n  - model: m\n    targets:\n      - provider: p\ndefault_model: ghost\n",
			"default_model",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRoutes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestRoutesDefaults(t *testing.T) {
	r, err := ParseRoutes([]byte(`
providers:
  - name: p
    type: ollama
routes:
  - model: m
    targets:
      - provider: p
`))
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}

	if r.Providers[0].BaseURL != "http://localhost:11434" {
		t.Errorf("ollama base_url = %q, want the default", r.Providers[0].BaseURL)
	}
	if r.Routes[0].Strategy != StrategyPriority {
		t.Errorf("strategy = %q, want %q by default", r.Routes[0].Strategy, StrategyPriority)
	}
	if r.Routes[0].Targets[0].Model != "m" {
		t.Errorf("target model = %q, want it to default to the route model", r.Routes[0].Targets[0].Model)
	}
	if r.Routes[0].Targets[0].Weight != 1 {
		t.Errorf("weight = %d, want 1", r.Routes[0].Targets[0].Weight)
	}
	def := DefaultBreaker()
	if r.Providers[0].Breaker.ConsecutiveFailures != def.ConsecutiveFailures {
		t.Errorf("breaker defaults not applied: %+v", r.Providers[0].Breaker)
	}
}

func TestFingerprintDetectsMeaningfulChanges(t *testing.T) {
	base := ProviderConfig{
		Name: "p", Type: "ollama", BaseURL: "http://x",
		Breaker: DefaultBreaker(),
	}

	same := base
	if base.Fingerprint() != same.Fingerprint() {
		t.Error("identical configs produced different fingerprints")
	}

	changed := base
	changed.BaseURL = "http://y"
	if base.Fingerprint() == changed.Fingerprint() {
		t.Error("a changed base_url did not change the fingerprint")
	}

	changedBreaker := base
	changedBreaker.Breaker.Cooldown = Duration(time.Hour)
	if base.Fingerprint() == changedBreaker.Fingerprint() {
		t.Error("changed breaker settings did not change the fingerprint")
	}
}

func TestFingerprintIsStableAcrossHeaderOrdering(t *testing.T) {
	a := ProviderConfig{
		Name: "p", Type: "ollama", BaseURL: "http://x",
		Headers: map[string]string{"x-a": "1", "x-b": "2", "x-c": "3"},
	}
	first := a.Fingerprint()
	// Map iteration order is randomised per run; the fingerprint must not be.
	for i := 0; i < 50; i++ {
		if a.Fingerprint() != first {
			t.Fatal("fingerprint is unstable across map iteration order, which would " +
				"rebuild providers (and reset their breakers) on every reload")
		}
	}
}

func TestCredentialPrefersEnvironment(t *testing.T) {
	t.Setenv("CONDUIT_TEST_KEY", "from-env")

	p := ProviderConfig{APIKeyEnv: "CONDUIT_TEST_KEY", APIKey: "from-file"}
	if got := p.Credential(); got != "from-env" {
		t.Errorf("Credential() = %q, want the environment to win", got)
	}

	p2 := ProviderConfig{APIKeyEnv: "CONDUIT_TEST_UNSET", APIKey: "from-file"}
	if got := p2.Credential(); got != "from-file" {
		t.Errorf("Credential() = %q, want the file value as fallback", got)
	}
}

// --- watcher ---------------------------------------------------------------

// TestWatcherReloadsOnAtomicRename covers the failure mode that makes naive
// file watches useless: editors and kubectl do not write in place, they write a
// temp file and rename over the target. A watch on the original inode survives
// the rename and then never fires again.
func TestWatcherReloadsOnAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yaml")
	if err := os.WriteFile(path, []byte(validRoutes), 0o644); err != nil {
		t.Fatal(err)
	}

	initial, err := LoadRoutes(path)
	if err != nil {
		t.Fatal(err)
	}

	applied := make(chan *Routes, 4)
	w := NewWatcher(path, initial, testLogger(), func(r *Routes) { applied <- r })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(200 * time.Millisecond) // let the watch establish

	updated := validRoutes + `
  - model: added
    targets:
      - provider: local
`
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // atomic replace
		t.Fatal(err)
	}

	select {
	case r := <-applied:
		if len(r.Routes) != 2 {
			t.Errorf("reloaded table has %d routes, want 2", len(r.Routes))
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no reload fired after an atomic rename; the watcher is watching the file, not the directory")
	}
}

// TestWatcherRejectsInvalidConfig: a broken edit must never be able to
// black-hole live traffic.
func TestWatcherRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yaml")
	if err := os.WriteFile(path, []byte(validRoutes), 0o644); err != nil {
		t.Fatal(err)
	}

	initial, err := LoadRoutes(path)
	if err != nil {
		t.Fatal(err)
	}

	applied := make(chan *Routes, 4)
	w := NewWatcher(path, initial, testLogger(), func(r *Routes) { applied <- r })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(path, []byte("providers: [oops\n  broken yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-applied:
		t.Fatalf("an invalid config was applied: %+v", r)
	case <-time.After(1500 * time.Millisecond):
		// Correct: rejected and discarded.
	}
}

func TestWatcherIgnoresUnchangedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yaml")
	if err := os.WriteFile(path, []byte(validRoutes), 0o644); err != nil {
		t.Fatal(err)
	}

	initial, err := LoadRoutes(path)
	if err != nil {
		t.Fatal(err)
	}

	applied := make(chan *Routes, 4)
	w := NewWatcher(path, initial, testLogger(), func(r *Routes) { applied <- r })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// Rewrite identical bytes: a touch should not churn the routing table.
	if err := os.WriteFile(path, []byte(validRoutes), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-applied:
		t.Fatal("a no-op write triggered a reload; the checksum guard is not working")
	case <-time.After(1200 * time.Millisecond):
	}
}

func TestChecksumChangesWithContent(t *testing.T) {
	a, err := ParseRoutes([]byte(validRoutes))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseRoutes([]byte(validRoutes + "\n# a comment\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Checksum() == b.Checksum() {
		t.Error("checksum did not change with content")
	}
}

// yamlUnmarshal is a thin helper so the duration test exercises the same
// decoder path the real loader uses.
func yamlUnmarshal(b []byte, v any) error { return yaml.Unmarshal(b, v) }
