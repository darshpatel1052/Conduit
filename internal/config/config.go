// Package config loads and validates Conduit's configuration.
//
// Configuration is split in two on purpose:
//
//   - conduit.yaml   process-level settings (listen address, timeouts, log
//     format). Changing these needs a restart, so they are read once at boot.
//   - routes.yaml    the routing table (providers and model -> target rules).
//     This is the part operators actually change day to day, so it is watched
//     and hot-reloaded at runtime. See watcher.go.
package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the process-level configuration, loaded once at startup.
type Config struct {
	Server     Server    `yaml:"server" json:"server"`
	Log        Log       `yaml:"log" json:"log"`
	Upstream   Upstream  `yaml:"upstream" json:"upstream"`
	Auth       Auth      `yaml:"auth" json:"auth"`
	RateLimit  RateLimit `yaml:"rate_limit" json:"rate_limit"`
	Cache      Cache     `yaml:"cache" json:"cache"`
	Analytics  Analytics `yaml:"analytics" json:"analytics"`
	Metrics    Metrics   `yaml:"metrics" json:"metrics"`
	Tracing    Tracing   `yaml:"tracing" json:"tracing"`
	RoutesFile string    `yaml:"routes_file" json:"routes_file"`
	Control    Control   `yaml:"control" json:"control"`
}

type Control struct {
	Enabled       bool     `yaml:"enabled" json:"enabled"`
	PostgresDSN   string   `yaml:"postgres_dsn" json:"-"`
	RedisURL      string   `yaml:"redis_url" json:"-"`
	SessionTTL    Duration `yaml:"session_ttl" json:"session_ttl"`
	SecureCookies bool     `yaml:"secure_cookies" json:"secure_cookies"`
}

// Analytics configures the async ClickHouse event pipeline.
type Analytics struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	ClickHouseDSN string `yaml:"clickhouse_dsn" json:"-"`
	RedisURL      string `yaml:"redis_url" json:"-"`

	// BufferSize is the in-memory queue depth. When it fills, events are
	// dropped rather than blocking the request path — losing analytics is
	// annoying, stalling inference is not acceptable.
	BufferSize int `yaml:"buffer_size" json:"buffer_size"`

	// BatchSize and FlushInterval are both needed. Size alone leaves a
	// low-traffic gateway's events sitting in memory until restart; interval
	// alone turns a traffic burst into many small inserts, which is the access
	// pattern a columnar store handles worst.
	BatchSize     int      `yaml:"batch_size" json:"batch_size"`
	FlushInterval Duration `yaml:"flush_interval" json:"flush_interval"`

	WriteTimeout Duration `yaml:"write_timeout" json:"write_timeout"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Path    string `yaml:"path" json:"path"`
}

// Tracing configures OpenTelemetry.
type Tracing struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	Endpoint    string `yaml:"endpoint" json:"endpoint"`
	ServiceName string `yaml:"service_name" json:"service_name"`
	Environment string `yaml:"environment" json:"environment"`

	// SampleRatio bounds trace volume. Tracing every request at scale costs
	// more in storage than the gateway costs in compute.
	SampleRatio float64 `yaml:"sample_ratio" json:"sample_ratio"`
}

// Cache configures the semantic response cache.
type Cache struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	PostgresDSN string `yaml:"postgres_dsn" json:"-"`
	MaxConns    int    `yaml:"max_conns" json:"max_conns"`

	// SemanticEnabled turns on vector search. With it off the cache still
	// serves exact repeats, which needs no embedding call at all — a useful
	// mode when the embedder is slow or unavailable.
	SemanticEnabled bool `yaml:"semantic_enabled" json:"semantic_enabled"`

	// SimilarityThreshold is the correctness dial, in cosine similarity where
	// 1.0 is identical. Too high and paraphrases miss and the cache earns
	// nothing; too low and a merely-similar question gets someone else's
	// answer, delivered confidently in the model's voice. A miss costs
	// latency; a false hit costs trust, so the default errs high.
	SimilarityThreshold float64 `yaml:"similarity_threshold" json:"similarity_threshold"`

	// MaxTemperature gates caching by determinism of intent. Above this the
	// caller is paying for variety, and replaying a stored sample silently
	// removes the randomness they asked for.
	MaxTemperature float64 `yaml:"max_temperature" json:"max_temperature"`

	TTL           Duration `yaml:"ttl" json:"ttl"`
	PurgeInterval Duration `yaml:"purge_interval" json:"purge_interval"`

	// Timeout bounds cache operations. The cache exists to reduce latency, so
	// a slow cache must be abandoned rather than waited on.
	Timeout Duration `yaml:"timeout" json:"timeout"`

	Embedding Embedding `yaml:"embedding" json:"embedding"`
}

// Embedding configures the vectoriser behind the semantic cache.
type Embedding struct {
	BaseURL   string `yaml:"base_url" json:"base_url"`
	Model     string `yaml:"model" json:"model"`
	APIKeyEnv string `yaml:"api_key_env" json:"api_key_env"`

	// Dimensions must match the model's output width and the pgvector column.
	// nomic-embed-text is 768; OpenAI text-embedding-3-small is 1536. Changing
	// model means changing this, which means a schema migration.
	Dimensions int      `yaml:"dimensions" json:"dimensions"`
	Timeout    Duration `yaml:"timeout" json:"timeout"`
}

// Auth configures API-key authentication.
type Auth struct {
	// Enabled turns key checking on. Off by default so a fresh clone runs
	// against local Ollama without ceremony; production sets it true.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Header and Scheme locate the credential. Defaults match OpenAI
	// ("Authorization: Bearer sk-..."), so existing clients need no change.
	Header string `yaml:"header" json:"header"`
	Scheme string `yaml:"scheme" json:"scheme"`

	Keys []APIKey `yaml:"keys" json:"keys"`
}

// APIKey is one issued credential.
//
// Only the hash is ever stored. A gateway's config file ends up in git, in
// Kubernetes, and in backups; storing the plaintext means a leak of any of
// those is a leak of every customer's key. Hashing makes the config file
// useless to an attacker, and `conduit keygen` is the only place plaintext
// exists — once, on stdout.
type APIKey struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`

	// Admin grants access to /admin/* observability and routing endpoints.
	// Inference keys are deliberately non-admin by default: the dashboard exposes
	// global tenant metadata and provider health that ordinary callers must not
	// be able to inspect.
	Admin bool `yaml:"admin" json:"admin"`

	// Hash is the hex-encoded SHA-256 of the plaintext key.
	Hash string `yaml:"hash" json:"-"`

	// Disabled revokes a key without deleting it, preserving the audit trail.
	Disabled bool `yaml:"disabled" json:"disabled"`

	// Models optionally restricts which routes this key may call. Empty means
	// all. Entries may be globs.
	Models []string `yaml:"models" json:"models,omitempty"`

	// Limits override the global defaults for this key.
	RequestsPerMinute int `yaml:"requests_per_minute" json:"requests_per_minute"`
	TokensPerMinute   int `yaml:"tokens_per_minute" json:"tokens_per_minute"`
}

// RateLimit configures the Redis-backed distributed limiter.
type RateLimit struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	RedisURL string `yaml:"redis_url" json:"redis_url"`

	// Algorithm selects the counting strategy.
	//
	//   sliding_window  smooth; no burst at a window edge, which is the flaw
	//                   that lets a fixed window pass 2x the limit across a
	//                   boundary. Best for protecting an upstream.
	//   token_bucket    allows a controlled burst up to Burst, then settles to
	//                   the steady rate. Better for bursty interactive traffic.
	Algorithm string `yaml:"algorithm" json:"algorithm"`

	Window Duration `yaml:"window" json:"window"`

	// Burst applies to token_bucket only; defaults to the per-window limit.
	Burst int `yaml:"burst" json:"burst"`

	// FailOpen decides what happens when Redis itself is unavailable.
	//
	// True (the default) serves the request. The reasoning: the limiter exists
	// to protect upstreams from abuse, and a Redis outage is not evidence of
	// abuse — failing closed would convert a cache-tier blip into a total
	// outage for every tenant. Set false where limits are a billing boundary
	// rather than a safety valve.
	FailOpen bool `yaml:"fail_open" json:"fail_open"`

	// Timeout bounds each Redis call. It must be short: the limiter sits in
	// front of every request, so a slow Redis would add its latency to all of
	// them.
	Timeout Duration `yaml:"timeout" json:"timeout"`

	// Defaults apply to any key that does not override them.
	DefaultRequestsPerMinute int `yaml:"default_requests_per_minute" json:"default_requests_per_minute"`
	DefaultTokensPerMinute   int `yaml:"default_tokens_per_minute" json:"default_tokens_per_minute"`

	// AnonymousRequestsPerMinute limits unauthenticated traffic when auth is
	// disabled, keyed by client IP.
	AnonymousRequestsPerMinute int `yaml:"anonymous_requests_per_minute" json:"anonymous_requests_per_minute"`
}

// Server configures the inbound HTTP listener.
type Server struct {
	Addr string `yaml:"addr" json:"addr"`

	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers. This is the main defence against slowloris-style connection
	// exhaustion and should always be set.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout" json:"read_header_timeout"`

	// ReadTimeout bounds the whole request read (headers + body).
	ReadTimeout Duration `yaml:"read_timeout" json:"read_timeout"`

	// WriteTimeout MUST be zero for a streaming gateway. http.Server applies
	// it as an absolute deadline on the response write, so any non-zero value
	// silently truncates SSE streams that run longer than it. Per-request
	// bounds are enforced with context deadlines instead, which we control
	// per route and can keep open for as long as tokens keep arriving.
	WriteTimeout Duration `yaml:"write_timeout" json:"write_timeout"`

	IdleTimeout     Duration `yaml:"idle_timeout" json:"idle_timeout"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`

	// MaxRequestBytes caps the inbound request body. Prompts are user input;
	// without a cap a client can make the gateway buffer arbitrary memory.
	MaxRequestBytes int64 `yaml:"max_request_bytes" json:"max_request_bytes"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level" json:"level"`   // debug | info | warn | error
	Format string `yaml:"format" json:"format"` // json | text
	Source bool   `yaml:"source" json:"source"`
}

// Upstream configures the shared HTTP client used to talk to providers.
//
// Go's http.DefaultClient has no timeout at all: a provider that accepts the
// connection and then never responds parks a goroutine and a connection
// forever, and under load that is how a proxy dies. Every field here exists to
// bound some part of that failure mode.
type Upstream struct {
	// ConnectTimeout bounds TCP dial + TLS handshake.
	ConnectTimeout Duration `yaml:"connect_timeout" json:"connect_timeout"`

	// ResponseHeaderTimeout bounds the wait for response headers. It lives on
	// the shared transport, so it applies to every provider at once and can
	// only ever be a generous backstop — a value tight enough for a hosted API
	// would kill a local model that is still loading weights into VRAM. The
	// real per-provider bound is ProviderConfig.Timeout, which is enforced
	// with a context deadline and can differ per upstream.
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout" json:"response_header_timeout"`

	// RequestTimeout bounds a complete non-streaming request.
	RequestTimeout Duration `yaml:"request_timeout" json:"request_timeout"`

	// StreamFirstChunkTimeout bounds time-to-first-token, which is a different
	// quantity from the gap between tokens and deserves its own budget. First
	// token can legitimately take a long time — queueing at the provider, or a
	// cold model being loaded into memory — while a long gap *mid*-generation
	// means something has wedged. Collapsing the two forces a choice between
	// killing cold starts and tolerating dead streams.
	StreamFirstChunkTimeout Duration `yaml:"stream_first_chunk_timeout" json:"stream_first_chunk_timeout"`

	// StreamIdleTimeout bounds the gap *between* streamed chunks. A stream may
	// legitimately run for minutes, so a total deadline is wrong; silence is
	// the real signal that an upstream has wedged.
	StreamIdleTimeout Duration `yaml:"stream_idle_timeout" json:"stream_idle_timeout"`

	// MaxIdleConnsPerHost defaults to 2 in net/http, which is far too low for
	// a proxy: past two in-flight requests to one provider every extra request
	// pays a fresh TCP+TLS handshake, and the connections are then thrown away
	// instead of pooled.
	MaxIdleConns        int      `yaml:"max_idle_conns" json:"max_idle_conns"`
	MaxIdleConnsPerHost int      `yaml:"max_idle_conns_per_host" json:"max_idle_conns_per_host"`
	MaxConnsPerHost     int      `yaml:"max_conns_per_host" json:"max_conns_per_host"`
	IdleConnTimeout     Duration `yaml:"idle_conn_timeout" json:"idle_conn_timeout"`

	// DisableCompression is true by default: token streams are small,
	// incremental writes and gzip framing works against prompt latency.
	DisableCompression bool `yaml:"disable_compression" json:"disable_compression"`
}

// Default returns the baseline configuration. A file, when present, is
// unmarshalled over the top of this, so any omitted key keeps its default.
func Default() *Config {
	return &Config{
		Server: Server{
			Addr:              ":8080",
			ReadHeaderTimeout: Duration(10 * time.Second),
			ReadTimeout:       Duration(60 * time.Second),
			WriteTimeout:      0,
			IdleTimeout:       Duration(120 * time.Second),
			ShutdownTimeout:   Duration(20 * time.Second),
			MaxRequestBytes:   8 << 20, // 8 MiB
		},
		Log: Log{
			Level:  "info",
			Format: "json",
		},
		Upstream: Upstream{
			ConnectTimeout:          Duration(5 * time.Second),
			ResponseHeaderTimeout:   Duration(180 * time.Second),
			RequestTimeout:          Duration(120 * time.Second),
			StreamFirstChunkTimeout: Duration(120 * time.Second),
			StreamIdleTimeout:       Duration(60 * time.Second),
			MaxIdleConns:            256,
			MaxIdleConnsPerHost:     64,
			MaxConnsPerHost:         0, // unlimited; breakers bound the damage
			IdleConnTimeout:         Duration(90 * time.Second),
			DisableCompression:      true,
		},
		Auth: Auth{
			Enabled: false,
			Header:  "Authorization",
			Scheme:  "Bearer",
		},
		RateLimit: RateLimit{
			Enabled:                    false,
			RedisURL:                   "redis://localhost:6379/0",
			Algorithm:                  AlgorithmSlidingWindow,
			Window:                     Duration(time.Minute),
			FailOpen:                   true,
			Timeout:                    Duration(250 * time.Millisecond),
			DefaultRequestsPerMinute:   60,
			DefaultTokensPerMinute:     0, // 0 = unlimited
			AnonymousRequestsPerMinute: 120,
		},
		Cache: Cache{
			Enabled:             false,
			PostgresDSN:         "postgres://conduit:conduit@localhost:5432/conduit?sslmode=disable",
			MaxConns:            10,
			SemanticEnabled:     true,
			SimilarityThreshold: 0.95,
			MaxTemperature:      0.3,
			TTL:                 Duration(24 * time.Hour),
			PurgeInterval:       Duration(10 * time.Minute),
			Timeout:             Duration(3 * time.Second),
			Embedding: Embedding{
				BaseURL:    "http://localhost:11434",
				Model:      "nomic-embed-text",
				Dimensions: 768,
				Timeout:    Duration(5 * time.Second),
			},
		},
		Analytics: Analytics{
			Enabled:       false,
			ClickHouseDSN: "clickhouse://conduit:conduit@localhost:9000/conduit",
			RedisURL:      "redis://localhost:6379/0",
			BufferSize:    10000,
			BatchSize:     500,
			FlushInterval: Duration(5 * time.Second),
			WriteTimeout:  Duration(15 * time.Second),
		},
		Metrics: Metrics{
			Enabled: true,
			Path:    "/metrics",
		},
		Tracing: Tracing{
			Enabled:     false,
			Endpoint:    "localhost:4318",
			ServiceName: "conduit",
			Environment: "development",
			SampleRatio: 0.1,
		},
		Control: Control{
			Enabled:       false,
			PostgresDSN:   "postgres://conduit:conduit@localhost:5432/conduit?sslmode=disable",
			RedisURL:      "redis://localhost:6379/0",
			SessionTTL:    Duration(8 * time.Hour),
			SecureCookies: false,
		},
		RoutesFile: "configs/routes.yaml",
	}
}

// Rate-limiting algorithms.
const (
	AlgorithmSlidingWindow = "sliding_window"
	AlgorithmTokenBucket   = "token_bucket"
)

// Load reads the process configuration from path, layering it over Default.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(ExpandEnv(raw), cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	cfg.applyEnvOverrides()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvOverrides lets the container runtime win over the file, which is how
// the same image ships to every environment.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("CONDUIT_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("CONDUIT_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("CONDUIT_LOG_FORMAT"); v != "" {
		c.Log.Format = v
	}
	if v := os.Getenv("CONDUIT_ROUTES_FILE"); v != "" {
		c.RoutesFile = v
	}
	if v := os.Getenv("REDIS_URL"); v != "" {
		c.RateLimit.RedisURL = v
		c.Control.RedisURL = v
		c.Analytics.RedisURL = v
	}
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		c.Cache.PostgresDSN = v
		c.Control.PostgresDSN = v
	}
	if v := os.Getenv("CLICKHOUSE_DSN"); v != "" {
		c.Analytics.ClickHouseDSN = v
	}
	if v := os.Getenv("CONDUIT_MAX_REQUEST_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Server.MaxRequestBytes = n
		}
	}
}

// Validate rejects configurations that would fail confusingly at runtime.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr must not be empty")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		return fmt.Errorf("log.format %q must be json or text", c.Log.Format)
	}
	if c.RoutesFile == "" {
		return fmt.Errorf("routes_file must not be empty")
	}
	if c.Server.MaxRequestBytes <= 0 {
		return fmt.Errorf("server.max_request_bytes must be > 0")
	}
	if c.Upstream.ConnectTimeout <= 0 {
		return fmt.Errorf("upstream.connect_timeout must be > 0")
	}
	if c.Upstream.RequestTimeout <= 0 {
		return fmt.Errorf("upstream.request_timeout must be > 0")
	}
	if c.Upstream.StreamIdleTimeout <= 0 {
		return fmt.Errorf("upstream.stream_idle_timeout must be > 0")
	}
	if c.Upstream.StreamFirstChunkTimeout <= 0 {
		return fmt.Errorf("upstream.stream_first_chunk_timeout must be > 0")
	}
	if c.Server.WriteTimeout > 0 {
		return fmt.Errorf("server.write_timeout must be 0: a non-zero write deadline truncates SSE streams")
	}

	if c.Auth.Enabled && !c.Control.Enabled {
		if len(c.Auth.Keys) == 0 {
			return fmt.Errorf("auth.enabled is true but no auth.keys are configured: " +
				"every request would be rejected")
		}
		seen := make(map[string]bool, len(c.Auth.Keys))
		for i, k := range c.Auth.Keys {
			if k.ID == "" {
				return fmt.Errorf("auth.keys[%d].id must not be empty", i)
			}
			if seen[k.ID] {
				return fmt.Errorf("auth.keys: duplicate id %q", k.ID)
			}
			seen[k.ID] = true
			if len(k.Hash) != 64 {
				return fmt.Errorf("auth.keys[%s].hash must be a 64-character hex SHA-256 "+
					"(generate one with `conduit keygen`)", k.ID)
			}
			if _, err := hex.DecodeString(k.Hash); err != nil {
				return fmt.Errorf("auth.keys[%s].hash is not valid hex: %w", k.ID, err)
			}
		}
	}
	if c.Control.Enabled {
		if c.Control.PostgresDSN == "" || c.Control.RedisURL == "" {
			return fmt.Errorf("control.postgres_dsn and control.redis_url are required when project administration is enabled")
		}
		if c.Control.SessionTTL <= 0 {
			return fmt.Errorf("control.session_ttl must be > 0")
		}
	}

	if c.RateLimit.Enabled {
		switch c.RateLimit.Algorithm {
		case AlgorithmSlidingWindow, AlgorithmTokenBucket:
		default:
			return fmt.Errorf("rate_limit.algorithm %q must be %s or %s",
				c.RateLimit.Algorithm, AlgorithmSlidingWindow, AlgorithmTokenBucket)
		}
		if c.RateLimit.RedisURL == "" {
			return fmt.Errorf("rate_limit.redis_url must be set when rate limiting is enabled")
		}
		if c.RateLimit.Window <= 0 {
			return fmt.Errorf("rate_limit.window must be > 0")
		}
		if c.RateLimit.Timeout <= 0 {
			return fmt.Errorf("rate_limit.timeout must be > 0")
		}
	}

	if c.Cache.Enabled {
		if c.Cache.PostgresDSN == "" {
			return fmt.Errorf("cache.postgres_dsn must be set when the cache is enabled")
		}
		if c.Cache.SimilarityThreshold <= 0 || c.Cache.SimilarityThreshold > 1 {
			return fmt.Errorf("cache.similarity_threshold must be between 0 and 1, got %v",
				c.Cache.SimilarityThreshold)
		}
		if c.Cache.SimilarityThreshold < 0.8 {
			// Not a hard error — it is tunable on purpose — but low thresholds
			// are how a semantic cache starts answering the wrong question.
			return fmt.Errorf("cache.similarity_threshold of %v is dangerously low: "+
				"below ~0.8 merely-related prompts match and the cache serves wrong answers. "+
				"Set it explicitly above 0.8, or disable the cache",
				c.Cache.SimilarityThreshold)
		}
		if c.Cache.MaxTemperature < 0 || c.Cache.MaxTemperature > 2 {
			return fmt.Errorf("cache.max_temperature must be between 0 and 2")
		}
		if c.Cache.TTL <= 0 {
			return fmt.Errorf("cache.ttl must be > 0")
		}
		if c.Cache.Timeout <= 0 {
			return fmt.Errorf("cache.timeout must be > 0")
		}
		if c.Cache.MaxConns <= 0 {
			return fmt.Errorf("cache.max_conns must be > 0")
		}
		if c.Cache.Embedding.Dimensions <= 0 {
			return fmt.Errorf("cache.embedding.dimensions must be > 0 and match the model's output width")
		}
		if c.Cache.Embedding.Model == "" {
			return fmt.Errorf("cache.embedding.model must be set")
		}
		if err := validateURL(c.Cache.Embedding.BaseURL); err != nil {
			return fmt.Errorf("cache.embedding.base_url: %w", err)
		}
	}

	if c.Analytics.Enabled {
		if c.Analytics.ClickHouseDSN == "" {
			return fmt.Errorf("analytics.clickhouse_dsn must be set when analytics is enabled")
		}
		if c.Analytics.BufferSize <= 0 {
			return fmt.Errorf("analytics.buffer_size must be > 0")
		}
		if c.Analytics.BatchSize <= 0 {
			return fmt.Errorf("analytics.batch_size must be > 0")
		}
		if c.Analytics.BatchSize > c.Analytics.BufferSize {
			return fmt.Errorf("analytics.batch_size (%d) must not exceed buffer_size (%d): "+
				"a batch could never fill", c.Analytics.BatchSize, c.Analytics.BufferSize)
		}
		if c.Analytics.FlushInterval <= 0 {
			return fmt.Errorf("analytics.flush_interval must be > 0")
		}
		if c.Analytics.WriteTimeout <= 0 {
			return fmt.Errorf("analytics.write_timeout must be > 0")
		}
	}

	if c.Metrics.Enabled && c.Metrics.Path == "" {
		return fmt.Errorf("metrics.path must be set when metrics are enabled")
	}

	if c.Tracing.Enabled {
		if c.Tracing.Endpoint == "" {
			return fmt.Errorf("tracing.endpoint must be set when tracing is enabled")
		}
		if c.Tracing.SampleRatio < 0 || c.Tracing.SampleRatio > 1 {
			return fmt.Errorf("tracing.sample_ratio must be between 0 and 1")
		}
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv substitutes ${VAR} references in a config file with the process
// environment. Unset variables are left verbatim rather than blanked, so a
// missing secret surfaces as an obviously wrong value instead of silently
// becoming an empty string.
func ExpandEnv(b []byte) []byte {
	return envRef.ReplaceAllFunc(b, func(match []byte) []byte {
		name := string(envRef.FindSubmatch(match)[1])
		if val, ok := os.LookupEnv(name); ok {
			return []byte(val)
		}
		return match
	})
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}
