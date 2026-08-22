package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Metrics holds every Prometheus collector the gateway exports.
//
// Metrics, logs and traces answer different questions and none substitutes for
// the others: metrics say *how many and how fast* across all traffic (cheap,
// aggregatable, alertable), traces say *where the time went* in one request,
// and logs say *what happened* in detail. Cardinality is the constraint that
// shapes this file — every distinct label combination is a separate time series
// in memory and in Prometheus, so labels here are bounded sets (provider,
// model, status class) and never unbounded ones like request ID or API key.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	TimeToFirstByte *prometheus.HistogramVec

	TokensTotal *prometheus.CounterVec
	CostTotal   *prometheus.CounterVec

	ProviderAttempts *prometheus.CounterVec
	FailoversTotal   *prometheus.CounterVec
	BreakerState     *prometheus.GaugeVec

	CacheLookups    *prometheus.CounterVec
	CacheLatency    prometheus.Histogram
	CacheSimilarity prometheus.Histogram

	RateLimitRejections *prometheus.CounterVec

	StreamsActive    prometheus.Gauge
	AnalyticsQueue   prometheus.Gauge
	AnalyticsDropped prometheus.Counter
}

// NewMetrics builds and registers the collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_requests_total",
			Help: "Inference requests, by profile, provider, outcome and stream mode.",
		}, []string{"route", "provider", "status", "stream"}),

		// Latency buckets are chosen for LLM traffic, not web traffic. The
		// default Prometheus buckets top out at 10s, which would lump every
		// real generation into +Inf and make p99 meaningless — a 30-second
		// completion is normal here, not an outlier.
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "conduit_request_duration_seconds",
			Help:    "End-to-end request duration.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120},
		}, []string{"route", "provider", "stream"}),

		// Time to first byte is the number users actually feel in a streaming
		// UI; total duration mostly measures how long the answer was.
		TimeToFirstByte: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "conduit_time_to_first_token_seconds",
			Help:    "Latency until the first token reaches the client.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}, []string{"route", "provider"}),

		TokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_tokens_total",
			Help: "Tokens processed, by direction.",
		}, []string{"route", "provider", "direction"}),

		CostTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_cost_usd_total",
			Help: "Estimated spend in USD.",
		}, []string{"route", "provider"}),

		ProviderAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_provider_attempts_total",
			Help: "Individual upstream attempts, including those that failed.",
		}, []string{"provider", "outcome"}),

		FailoversTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_failovers_total",
			Help: "Requests served only after falling through to another target.",
		}, []string{"route", "from_provider", "to_provider"}),

		// A gauge rather than a counter: breaker state is a level, not an
		// accumulation. 0 closed, 1 half-open, 2 open — alertable directly.
		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "conduit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=half-open, 2=open).",
		}, []string{"provider"}),

		CacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_cache_lookups_total",
			Help: "Semantic cache lookups by result (hit_exact, hit_semantic, miss, bypass, error).",
		}, []string{"route", "result"}),

		CacheLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "conduit_cache_lookup_duration_seconds",
			Help:    "Time spent deciding a cache hit or miss.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}),

		// The distribution of scores is what tells you whether the threshold is
		// set sensibly: a pile of near-misses just under it means it is too
		// strict, hits clustered barely above it mean it is too loose.
		CacheSimilarity: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "conduit_cache_similarity",
			Help:    "Cosine similarity of served cache hits.",
			Buckets: []float64{0.80, 0.85, 0.90, 0.92, 0.94, 0.95, 0.96, 0.97, 0.98, 0.99, 1.0},
		}),

		RateLimitRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conduit_rate_limit_rejections_total",
			Help: "Requests rejected by the rate limiter, by dimension.",
		}, []string{"dimension"}),

		StreamsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "conduit_active_streams",
			Help: "Streaming responses currently open.",
		}),

		AnalyticsQueue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "conduit_analytics_queue_depth",
			Help: "Events buffered for the analytics sink.",
		}),

		AnalyticsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "conduit_analytics_dropped_total",
			Help: "Analytics events dropped because the buffer was full.",
		}),
	}

	reg.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.TimeToFirstByte,
		m.TokensTotal, m.CostTotal,
		m.ProviderAttempts, m.FailoversTotal, m.BreakerState,
		m.CacheLookups, m.CacheLatency, m.CacheSimilarity,
		m.RateLimitRejections,
		m.StreamsActive, m.AnalyticsQueue, m.AnalyticsDropped,
	)

	// Go runtime and process metrics: goroutine count and heap size are what
	// tell you a proxy is leaking, and a proxy that streams is exactly the kind
	// that leaks goroutines.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler serves the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A scrape failure should be visible, not silent.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// Registry exposes the registry for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// BreakerStateValue maps a breaker's string state to its gauge value.
func BreakerStateValue(state string) float64 {
	switch state {
	case "closed":
		return 0
	case "half-open":
		return 1
	case "open":
		return 2
	default:
		return -1 // disabled
	}
}

// StatusClass buckets an HTTP status into a low-cardinality label. Using the
// raw status would multiply every series by the number of distinct codes for
// no analytical gain.
func StatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "success"
	case status == 429:
		return "rate_limited"
	case status >= 400 && status < 500:
		return "client_error"
	case status >= 500:
		return "server_error"
	default:
		return "unknown"
	}
}
