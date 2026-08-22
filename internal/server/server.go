// Package server wires the HTTP surface: middleware chain, routes, and the
// listener's lifecycle.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/darshpatel1052/conduit/internal/analytics"
	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/cache"
	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/control"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/ratelimit"
	"github.com/darshpatel1052/conduit/internal/router"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// detachedContext derives a context that survives the request's cancellation,
// for work that must finish even after the client hangs up (billing, analytics).
// The request's values are preserved so logging keeps its request ID.
func detachedContext(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
}

// Deps are the optional subsystems the server hangs off. They are grouped so
// adding one (a cache, an analytics sink) does not churn every New call site.
type Deps struct {
	Auth      *auth.Authenticator
	Limiter   ratelimit.Limiter
	Cache     *cache.Cache
	Metrics   *telemetry.Metrics
	Analytics analytics.Recorder
	Pricing   *analytics.Pricing

	// Querier is the read side of analytics, powering the dashboard. Nil when
	// analytics is disabled, in which case the stats endpoints report that
	// rather than failing.
	Querier *analytics.Querier

	// AnalyticsStats is a closure rather than the concrete recorder so the
	// admin endpoint can report health without the server depending on which
	// implementation is installed.
	AnalyticsStats func() analytics.Stats
	ControlRuntime *control.RuntimeManager
	ControlHandler http.Handler
	ControlReady   func(context.Context) error
	Accounting     *control.Accounting
}

// Server is the gateway's HTTP frontend.
type Server struct {
	cfg     *config.Config
	router  *router.Router
	proxy   *proxy.Proxy
	auth    *auth.Authenticator
	limiter ratelimit.Limiter
	cache   *cache.Cache
	log     *slog.Logger

	metrics        *telemetry.Metrics
	analytics      analytics.Recorder
	analyticsStats func() analytics.Stats
	querier        *analytics.Querier
	pricing        *analytics.Pricing
	controlRuntime *control.RuntimeManager
	controlReady   func(context.Context) error
	accounting     *control.Accounting
	http           *http.Server
	started        time.Time
}

// New builds the server and its route table.
func New(cfg *config.Config, rtr *router.Router, pxy *proxy.Proxy, deps Deps, log *slog.Logger) *Server {
	s := &Server{
		cfg:            cfg,
		router:         rtr,
		proxy:          pxy,
		auth:           deps.Auth,
		limiter:        deps.Limiter,
		cache:          deps.Cache,
		metrics:        deps.Metrics,
		analytics:      deps.Analytics,
		analyticsStats: deps.AnalyticsStats,
		querier:        deps.Querier,
		pricing:        deps.Pricing,
		controlRuntime: deps.ControlRuntime,
		controlReady:   deps.ControlReady,
		accounting:     deps.Accounting,
		log:            log,
		started:        time.Now(),
	}

	// Defaults keep the hot path free of nil checks.
	if s.auth == nil {
		a, err := auth.New(cfg.Auth, cfg.RateLimit)
		if err != nil {
			// Config validation already rejects malformed keys; this cannot
			// fail in practice, and an unauthenticated fallback would be worse.
			panic("server: building authenticator: " + err.Error())
		}
		s.auth = a
	}
	if s.limiter == nil {
		s.limiter = ratelimit.NoopLimiter{}
	}
	if s.pricing == nil {
		s.pricing = analytics.DefaultPricing()
	}
	if s.metrics == nil {
		// Each server gets its own registry, so building one here cannot
		// collide with another instance's collectors.
		s.metrics = telemetry.NewMetrics()
	}
	if s.analytics == nil {
		s.analytics = analytics.NoopRecorder{}
	}

	mux := chi.NewRouter()

	// Order matters. RequestID first so everything downstream can log it;
	// recoverer before the handlers so a panic is contained; the body limit
	// before anything reads a body.
	mux.Use(middleware.RequestID)
	mux.Use(middleware.RealIP)
	// Tracing first, so every later span (auth, cache, provider) hangs off one
	// root and a trace covers the whole request rather than starting halfway in.
	mux.Use(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "conduit",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}))
	})
	mux.Use(requestLogger(log))
	mux.Use(recoverer(log))
	mux.Use(limitBody(cfg.Server.MaxRequestBytes))

	// Auth before rate limiting, because the limit is per-principal and there
	// is no principal to key on until the caller is identified. Both run before
	// any handler so an unauthenticated or over-quota request is rejected
	// without parsing a body or touching a provider.
	mux.Use(s.authenticate)
	mux.Use(s.rateLimit)

	mux.Get("/healthz", s.handleHealthz)
	mux.Get("/livez", s.handleHealthz)
	mux.Get("/readyz", s.handleReadyz)

	mux.Post("/v1/chat/completions", s.handleChatCompletions)
	mux.Post("/v1/embeddings", s.handleEmbeddings)
	mux.Post("/v1/responses", s.handleResponses)
	mux.Get("/v1/models", s.handleModels)
	if deps.ControlHandler != nil {
		mux.Mount("/control/v1", deps.ControlHandler)
	}

	mux.Get("/admin/routes", s.handleAdminRoutes)
	mux.Get("/admin/cache", s.handleAdminCache)
	mux.Get("/admin/analytics", s.handleAdminAnalytics)
	mux.Get("/admin/stats", s.handleAdminStats)
	mux.Get("/admin/stats/recent", s.handleAdminStatsRecent)

	if cfg.Metrics.Enabled {
		// Unauthenticated on purpose: a scrape target that needs a credential
		// is a scrape target nobody configures. Expose it on the pod network,
		// not the internet.
		mux.Get(cfg.Metrics.Path, s.handleMetrics)
	}

	mux.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no such endpoint: "+r.URL.Path)
	})
	mux.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
			r.Method+" is not allowed on "+r.URL.Path)
	})

	s.http = &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
		ReadTimeout:       cfg.Server.ReadTimeout.D(),

		// Intentionally left at zero: see config.Server.WriteTimeout. Any
		// value here is an absolute deadline on the whole response and would
		// guillotine long token streams mid-sentence.
		WriteTimeout: cfg.Server.WriteTimeout.D(),

		IdleTimeout: cfg.Server.IdleTimeout.D(),
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// ListenAndServe runs until ctx is cancelled, then drains.
//
// Graceful shutdown matters more for a gateway than for a typical API: an
// in-flight streaming request may be thirty seconds from finishing, and killing
// the process drops a half-written answer the user already paid for. Shutdown
// stops accepting new connections and lets the existing ones finish, up to the
// configured budget.
func (s *Server) ListenAndServe(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.log.Info("gateway listening",
			"addr", s.cfg.Server.Addr,
			"routes_file", s.cfg.RoutesFile,
			"models", s.router.Models())
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err

	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining connections",
			"timeout", s.cfg.Server.ShutdownTimeout.String())

		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout.D())
		defer cancel()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			// Deadline hit: some requests were still running. Force the
			// listener closed rather than hanging forever.
			s.log.Warn("graceful shutdown timed out, closing remaining connections", "error", err)
			_ = s.http.Close()
			return err
		}
		s.log.Info("shutdown complete")
		return nil
	}
}

// setResultHeaders exposes the routing decision on the response, which turns
// "which provider served this?" from a log-diving exercise into a header.
func setResultHeaders(w http.ResponseWriter, res *proxy.Result) {
	if res == nil {
		return
	}
	h := w.Header()
	if res.Provider != "" {
		h.Set("X-Conduit-Provider", res.Provider)
	}
	if res.Route != "" {
		h.Set("X-Conduit-Route", res.Route)
	}
	if res.UpstreamModel != "" {
		h.Set("X-Conduit-Model", res.UpstreamModel)
	}
	h.Set("X-Conduit-Attempts", strconv.Itoa(len(res.Attempts)))
	if res.Duration > 0 {
		h.Set("X-Conduit-Duration-Ms", strconv.FormatInt(res.Duration.Milliseconds(), 10))
	}
}

// logAttempts records the routing outcome, including every failed attempt.
// During an incident the failed attempts are the interesting part: they say
// which providers were tried and exactly why each one was passed over.
func logAttempts(log *slog.Logger, req *provider.ChatRequest, res *proxy.Result, err error) {
	if res == nil {
		return
	}

	attrs := []any{
		"requested_model", req.Model,
		"route", res.Route,
		"stream", req.Stream,
		"attempts", len(res.Attempts),
	}
	if res.Provider != "" {
		attrs = append(attrs, "provider", res.Provider, "upstream_model", res.UpstreamModel)
	}
	if res.Usage != nil {
		attrs = append(attrs,
			"prompt_tokens", res.Usage.PromptTokens,
			"completion_tokens", res.Usage.CompletionTokens)
	}
	if res.Duration > 0 {
		attrs = append(attrs, "duration_ms", res.Duration.Milliseconds())
	}

	// Only log the per-attempt detail when something actually went wrong;
	// on the happy path it is noise.
	if len(res.Attempts) > 1 || err != nil {
		failed := make([]string, 0, len(res.Attempts))
		for _, a := range res.Attempts {
			if a.Error != "" {
				failed = append(failed, a.Provider+"="+a.Kind)
			}
		}
		if len(failed) > 0 {
			attrs = append(attrs, "failed_attempts", failed)
		}
	}

	if err != nil {
		attrs = append(attrs, "error", err.Error())
		log.Error("request could not be served", attrs...)
		return
	}
	if len(res.Attempts) > 1 {
		log.Warn("request served after failover", attrs...)
		return
	}
	log.Debug("request routed", attrs...)
}

// usageField reads a token count safely when usage is absent.
func usageField(res *proxy.Result, get func(*provider.Usage) int) int {
	if res == nil || res.Usage == nil {
		return 0
	}
	return get(res.Usage)
}
