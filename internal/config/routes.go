package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider type identifiers.
const (
	TypeOllama    = "ollama"
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
)

// Load-balancing strategies for a route's target list.
const (
	// StrategyPriority always starts at the first target and walks down the
	// list on failure. This is failover, not balancing: use it when target
	// order encodes preference (cheap local model first, paid API as backup).
	StrategyPriority = "priority"

	// StrategyRoundRobin spreads requests evenly across targets.
	StrategyRoundRobin = "round_robin"

	// StrategyWeighted spreads requests in proportion to target weights, so a
	// beefier instance or a higher rate-limit tier takes more traffic.
	StrategyWeighted = "weighted"
)

// Routes is the hot-reloadable routing table.
type Routes struct {
	Providers []ProviderConfig `yaml:"providers" json:"providers"`
	Routes    []Route          `yaml:"routes" json:"routes"`

	// DefaultModel names the route used when an incoming model matches
	// nothing. Empty means unknown models are rejected with 404, which is the
	// safer default: silently serving a different model than the caller asked
	// for is worse than an error.
	DefaultModel string `yaml:"default_model" json:"default_model"`

	// checksum of the source bytes, used to detect no-op reloads.
	checksum string `yaml:"-" json:"-"`
}

// ProviderConfig describes one upstream the gateway can talk to. A "provider"
// here is an endpoint, not a vendor: two API keys for the same vendor, or two
// Ollama hosts, are two providers and can be balanced across.
type ProviderConfig struct {
	Name    string `yaml:"name" json:"name"`
	Type    string `yaml:"type" json:"type"`
	BaseURL string `yaml:"base_url" json:"base_url"`

	// APIKeyEnv names an environment variable holding the credential. Prefer
	// this over APIKey so secrets never live in a file that gets committed.
	APIKeyEnv string `yaml:"api_key_env" json:"api_key_env"`
	APIKey    string `yaml:"api_key" json:"-"`

	// Headers are sent verbatim on every upstream request (org IDs, gateway
	// routing headers, Azure deployment hints).
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`

	// Timeout overrides upstream.request_timeout for this provider. A local
	// Ollama on CPU is far slower than a hosted API and deserves more room.
	Timeout Duration `yaml:"timeout" json:"timeout,omitempty"`

	Breaker BreakerConfig `yaml:"breaker" json:"breaker"`
}

// BreakerConfig tunes the circuit breaker guarding a provider.
type BreakerConfig struct {
	// Disabled turns the breaker off entirely (useful for a single-provider
	// dev setup where failing fast just hides the real error).
	Disabled bool `yaml:"disabled" json:"disabled"`

	// ConsecutiveFailures trips the breaker after N failures in a row.
	ConsecutiveFailures uint32 `yaml:"consecutive_failures" json:"consecutive_failures"`

	// FailureRatio trips the breaker when the failure rate within Interval
	// exceeds this fraction, provided at least MinRequests were seen. Ratio
	// tripping catches a provider that is failing 60% of the time, which
	// consecutive-failure counting can miss entirely under mixed traffic.
	FailureRatio float64 `yaml:"failure_ratio" json:"failure_ratio"`
	MinRequests  uint32  `yaml:"min_requests" json:"min_requests"`

	// Interval is the rolling window over which counts are accumulated while
	// the breaker is closed.
	Interval Duration `yaml:"interval" json:"interval"`

	// Cooldown is how long the breaker stays open before admitting probe
	// traffic (open -> half-open).
	Cooldown Duration `yaml:"cooldown" json:"cooldown"`

	// HalfOpenMaxRequests is how many probes are admitted while half-open. Keep
	// it small: the point is to test the water, not to re-flood a sick provider.
	HalfOpenMaxRequests uint32 `yaml:"half_open_max_requests" json:"half_open_max_requests"`
}

// Route maps an incoming model name onto an ordered list of targets.
type Route struct {
	// Model is matched exactly first, then as a glob ("gpt-4*"). Exact wins,
	// so a specific rule always beats a catch-all regardless of file order.
	Model    string   `yaml:"model" json:"model"`
	Strategy string   `yaml:"strategy" json:"strategy"`
	Targets  []Target `yaml:"targets" json:"targets"`

	// MaxAttempts caps how many targets a single request may try before giving
	// up. Without a cap, a fleet-wide outage turns one client request into N
	// upstream requests and multiplies the load on an already-struggling
	// system. 0 means "try every target".
	MaxAttempts int `yaml:"max_attempts" json:"max_attempts"`
}

// Target is one candidate provider for a route.
type Target struct {
	Provider string `yaml:"provider" json:"provider"`

	// Model rewrites the model name sent upstream. This is what lets a single
	// virtual model name ("conduit-chat") fan out to llama3.2 on Ollama and
	// gpt-4o-mini on OpenAI.
	Model  string `yaml:"model" json:"model,omitempty"`
	Weight int    `yaml:"weight" json:"weight,omitempty"`
}

// DefaultBreaker returns the breaker settings applied when a provider omits them.
func DefaultBreaker() BreakerConfig {
	return BreakerConfig{
		ConsecutiveFailures: 5,
		FailureRatio:        0.6,
		MinRequests:         10,
		Interval:            Duration(60 * time.Second),
		Cooldown:            Duration(30 * time.Second),
		HalfOpenMaxRequests: 1,
	}
}

// LoadRoutes reads, expands, validates and normalises the routing table.
func LoadRoutes(path string) (*Routes, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read routes %s: %w", path, err)
	}
	return ParseRoutes(raw)
}

// ParseRoutes is LoadRoutes without the file read, so the hot-reload path and
// the tests exercise exactly the same validation.
func ParseRoutes(raw []byte) (*Routes, error) {
	sum := sha256.Sum256(raw)

	var r Routes
	// KnownFields makes a typo like "provder:" a startup error instead of a
	// silently ignored key that leaves a route mysteriously missing.
	dec := yaml.NewDecoder(strings.NewReader(string(ExpandEnv(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse routes: %w", err)
	}

	r.checksum = hex.EncodeToString(sum[:])
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Checksum identifies the exact bytes this table was parsed from.
func (r *Routes) Checksum() string { return r.checksum }

func (r *Routes) applyDefaults() {
	def := DefaultBreaker()
	for i := range r.Providers {
		p := &r.Providers[i]
		p.Type = strings.ToLower(strings.TrimSpace(p.Type))
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")

		if p.BaseURL == "" {
			switch p.Type {
			case TypeOllama:
				p.BaseURL = "http://localhost:11434"
			case TypeOpenAI:
				p.BaseURL = "https://api.openai.com"
			case TypeAnthropic:
				p.BaseURL = "https://api.anthropic.com"
			}
		}

		b := &p.Breaker
		if b.ConsecutiveFailures == 0 {
			b.ConsecutiveFailures = def.ConsecutiveFailures
		}
		if b.FailureRatio == 0 {
			b.FailureRatio = def.FailureRatio
		}
		if b.MinRequests == 0 {
			b.MinRequests = def.MinRequests
		}
		if b.Interval == 0 {
			b.Interval = def.Interval
		}
		if b.Cooldown == 0 {
			b.Cooldown = def.Cooldown
		}
		if b.HalfOpenMaxRequests == 0 {
			b.HalfOpenMaxRequests = def.HalfOpenMaxRequests
		}
	}

	for i := range r.Routes {
		rt := &r.Routes[i]
		rt.Strategy = strings.ToLower(strings.TrimSpace(rt.Strategy))
		if rt.Strategy == "" {
			rt.Strategy = StrategyPriority
		}
		for j := range rt.Targets {
			t := &rt.Targets[j]
			// A glob route such as "gpt-*" stands for many real upstream
			// models. Leave an omitted target model empty so the router can
			// substitute the caller's matched model at request time. Exact and
			// virtual routes keep their convenient route-name default.
			if t.Model == "" && !strings.ContainsAny(rt.Model, "*?[") {
				t.Model = rt.Model
			}
			if t.Weight <= 0 {
				t.Weight = 1
			}
		}
	}
}

// ApplyDefaultsForRuntime normalises routes assembled from control-plane rows.
func (r *Routes) ApplyDefaultsForRuntime() { r.applyDefaults() }

// Validate enforces every invariant the router relies on, so the router itself
// can be written without defensive checks on its own configuration.
func (r *Routes) Validate() error {
	if len(r.Providers) == 0 {
		return fmt.Errorf("routes: at least one provider must be defined")
	}
	if len(r.Routes) == 0 {
		return fmt.Errorf("routes: at least one route must be defined")
	}

	seenProvider := make(map[string]bool, len(r.Providers))
	for _, p := range r.Providers {
		if p.Name == "" {
			return fmt.Errorf("routes: provider name must not be empty")
		}
		if seenProvider[p.Name] {
			return fmt.Errorf("routes: duplicate provider %q", p.Name)
		}
		seenProvider[p.Name] = true

		switch p.Type {
		case TypeOllama, TypeOpenAI, TypeAnthropic:
		default:
			return fmt.Errorf("routes: provider %q has unknown type %q (want %s, %s or %s)",
				p.Name, p.Type, TypeOllama, TypeOpenAI, TypeAnthropic)
		}
		if err := validateURL(p.BaseURL); err != nil {
			return fmt.Errorf("routes: provider %q base_url %q: %w", p.Name, p.BaseURL, err)
		}
		if p.Breaker.FailureRatio < 0 || p.Breaker.FailureRatio > 1 {
			return fmt.Errorf("routes: provider %q breaker.failure_ratio must be between 0 and 1", p.Name)
		}
		if p.Timeout < 0 {
			return fmt.Errorf("routes: provider %q timeout must not be negative", p.Name)
		}
	}

	seenRoute := make(map[string]bool, len(r.Routes))
	for _, rt := range r.Routes {
		if rt.Model == "" {
			return fmt.Errorf("routes: route model must not be empty")
		}
		if seenRoute[rt.Model] {
			return fmt.Errorf("routes: duplicate route for model %q", rt.Model)
		}
		seenRoute[rt.Model] = true

		switch rt.Strategy {
		case StrategyPriority, StrategyRoundRobin, StrategyWeighted:
		default:
			return fmt.Errorf("routes: route %q has unknown strategy %q (want %s, %s or %s)",
				rt.Model, rt.Strategy, StrategyPriority, StrategyRoundRobin, StrategyWeighted)
		}
		if len(rt.Targets) == 0 {
			return fmt.Errorf("routes: route %q has no targets", rt.Model)
		}
		if rt.MaxAttempts < 0 {
			return fmt.Errorf("routes: route %q max_attempts must not be negative", rt.Model)
		}
		if strings.ContainsAny(rt.Model, "*?[") {
			if _, err := path.Match(rt.Model, "probe"); err != nil {
				return fmt.Errorf("routes: route %q is not a valid glob: %w", rt.Model, err)
			}
		}
		for _, t := range rt.Targets {
			if !seenProvider[t.Provider] {
				return fmt.Errorf("routes: route %q references unknown provider %q", rt.Model, t.Provider)
			}
		}
	}

	if r.DefaultModel != "" && !seenRoute[r.DefaultModel] {
		return fmt.Errorf("routes: default_model %q has no matching route", r.DefaultModel)
	}
	return nil
}

// Credential resolves a provider's API key, preferring the environment.
func (p ProviderConfig) Credential() string {
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v
		}
	}
	return p.APIKey
}

// Fingerprint identifies a provider's behavioural configuration. On hot reload
// the router reuses the live provider (and, critically, its circuit breaker
// state) when this is unchanged, so editing an unrelated route does not
// silently reset every breaker back to closed and unleash a retry storm at a
// provider that is still down.
func (p ProviderConfig) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%v|%v|",
		p.Name, p.Type, p.BaseURL, p.APIKeyEnv, p.Timeout, p.Breaker)
	keys := make([]string, 0, len(p.Headers))
	for k := range p.Headers {
		keys = append(keys, k)
	}
	// Map iteration order is random; sort so the fingerprint is stable.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s|", k, p.Headers[k])
	}
	// The credential is included so a rotated key rebuilds the client, but it
	// is hashed, never stored or logged in plaintext.
	fmt.Fprintf(h, "cred:%x", sha256.Sum256([]byte(p.Credential())))
	return hex.EncodeToString(h.Sum(nil))
}
