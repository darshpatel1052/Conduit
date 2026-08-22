package server

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/darshpatel1052/conduit/internal/analytics"
	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// observation accumulates everything worth reporting about one request, so the
// three sinks (metrics, trace, analytics event) are populated from a single
// source rather than each handler remembering to update all of them.
type observation struct {
	start time.Time
	span  trace.Span

	requestID string
	principal *auth.Principal
	model     string
	stream    bool
	operation string

	status      int
	result      *proxy.Result
	cacheStatus string
	cacheScore  float64
	err         error
	projectCost func(provider, model string, promptTokens, completionTokens int) float64
}

func (s *Server) beginObservation(r *http.Request, model string, stream bool, operation string, principal *auth.Principal, projectCost func(string, string, int, int) float64) *observation {
	span := trace.SpanFromContext(r.Context())

	o := &observation{
		start:       time.Now(),
		span:        span,
		requestID:   requestIDFrom(r),
		principal:   principal,
		model:       model,
		stream:      stream,
		operation:   operation,
		cacheStatus: "disabled",
		status:      http.StatusOK,
		projectCost: projectCost,
	}
	if s.cache != nil && operation == "chat.completions" {
		o.cacheStatus = "miss"
	}

	span.SetAttributes(
		telemetry.AttrModel.String(model),
		telemetry.AttrStream.Bool(stream),
	)
	if principal != nil {
		span.SetAttributes(telemetry.AttrAPIKeyID.String(principal.ID))
	}
	return o
}

// finish publishes the request to metrics, the trace and the analytics queue.
//
// Called from a defer so it runs on every exit path, including error returns.
// A metric that is only updated on the happy path is worse than no metric: it
// makes an outage look like a traffic drop.
func (s *Server) finish(r *http.Request, o *observation) {
	duration := time.Since(o.start)

	route, prov, provType, upstreamModel := o.model, "none", "", ""
	attempts, failover := 0, false
	var usage *provider.Usage

	if o.result != nil {
		if o.result.Route != "" {
			route = o.result.Route
		}
		if o.result.Provider != "" {
			prov = o.result.Provider
		}
		provType = o.result.ProviderType
		upstreamModel = o.result.UpstreamModel
		attempts = len(o.result.Attempts)
		failover = attempts > 1
		usage = o.result.Usage
		if o.result.Duration > 0 {
			duration = o.result.Duration
		}
	}

	cacheHit := o.cacheStatus == "hit_exact" || o.cacheStatus == "hit_semantic"
	if cacheHit {
		prov = "cache"
	}

	var promptTokens, completionTokens int
	if usage != nil {
		promptTokens, completionTokens = usage.PromptTokens, usage.CompletionTokens
	}

	// Cost is attributed to the upstream model, not the route: a virtual model
	// that fans out to a cheap local model and an expensive hosted one has no
	// single price, and averaging them would hide exactly the shift you want to
	// see when failover kicks in.
	cost := s.pricing.Cost(upstreamModel, promptTokens, completionTokens)
	if o.projectCost != nil {
		cost = o.projectCost(prov, upstreamModel, promptTokens, completionTokens)
	}
	if s.accounting != nil && o.principal != nil && o.principal.ProjectID != "" && cost > 0 {
		ctx, cancel := detachedContext(r, 2*time.Second)
		defer cancel()
		if err := s.accounting.Debit(ctx, o.principal.ProjectID, cost); err != nil {
			telemetry.LoggerFrom(r.Context()).Warn("could not debit project budget", "project_id", o.principal.ProjectID, "cost_usd", cost, "error", err)
		}
	}

	stream := strconv.FormatBool(o.stream)

	// --- metrics ---
	if s.metrics != nil {
		s.metrics.RequestsTotal.WithLabelValues(
			route, prov, telemetry.StatusClass(o.status), stream).Inc()
		s.metrics.RequestDuration.WithLabelValues(route, prov, stream).
			Observe(duration.Seconds())

		if o.result != nil && o.result.TTFB > 0 {
			s.metrics.TimeToFirstByte.WithLabelValues(route, prov).
				Observe(o.result.TTFB.Seconds())
		}
		if promptTokens > 0 {
			s.metrics.TokensTotal.WithLabelValues(route, prov, "prompt").Add(float64(promptTokens))
		}
		if completionTokens > 0 {
			s.metrics.TokensTotal.WithLabelValues(route, prov, "completion").Add(float64(completionTokens))
		}
		if cost > 0 {
			s.metrics.CostTotal.WithLabelValues(route, prov).Add(cost)
		}
		if o.cacheStatus != "disabled" {
			s.metrics.CacheLookups.WithLabelValues(route, o.cacheStatus).Inc()
		}
		if cacheHit {
			s.metrics.CacheSimilarity.Observe(o.cacheScore)
		}

		if o.result != nil {
			for _, a := range o.result.Attempts {
				outcome := "success"
				if a.Error != "" {
					outcome = a.Kind
				}
				s.metrics.ProviderAttempts.WithLabelValues(a.Provider, outcome).Inc()
			}
			if failover && o.status < 400 {
				s.metrics.FailoversTotal.WithLabelValues(
					route, o.result.Attempts[0].Provider, prov).Inc()
			}
		}

		// Breaker gauges are refreshed here rather than from a background
		// sampler: the router already holds the state, and a request is the
		// only moment it can have changed.
		for _, p := range s.router.Snapshot().Providers {
			s.metrics.BreakerState.WithLabelValues(p.Name).
				Set(telemetry.BreakerStateValue(p.Breaker.State))
		}
	}

	// --- trace ---
	if o.span.IsRecording() {
		o.span.SetAttributes(
			telemetry.AttrRoute.String(route),
			telemetry.AttrProvider.String(prov),
			telemetry.AttrProviderType.String(provType),
			telemetry.AttrUpstreamModel.String(upstreamModel),
			telemetry.AttrAttempt.Int(attempts),
			telemetry.AttrCacheStatus.String(o.cacheStatus),
			telemetry.AttrPromptTokens.Int(promptTokens),
			telemetry.AttrOutputTokens.Int(completionTokens),
			telemetry.AttrCostUSD.Float64(cost),
			attribute.Int("http.status_code", o.status),
		)
		if cacheHit {
			o.span.SetAttributes(telemetry.AttrCacheScore.Float64(o.cacheScore))
		}
		if o.err != nil {
			o.span.SetStatus(codes.Error, o.err.Error())
			o.span.RecordError(o.err)
		} else {
			o.span.SetStatus(codes.Ok, "")
		}
	}

	// --- analytics ---
	if s.analytics != nil {
		event := analytics.Event{
			Timestamp:        o.start,
			RequestID:        o.requestID,
			Route:            route,
			Operation:        o.operation,
			RequestedModel:   o.model,
			UpstreamModel:    upstreamModel,
			Provider:         prov,
			ProviderType:     provType,
			Status:           o.status,
			Stream:           o.stream,
			Attempts:         attempts,
			Failover:         failover,
			DurationMS:       duration.Milliseconds(),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			CostUSD:          cost,
			CacheStatus:      o.cacheStatus,
			CacheSimilarity:  o.cacheScore,
		}
		if o.principal != nil {
			event.APIKeyID = o.principal.ID
			event.ProjectID = o.principal.ProjectID
			event.Environment = o.principal.Environment
			event.EndUser = o.principal.EndUserPseudonym
			event.PolicyVersion = o.principal.PolicyVersion
		}
		if o.result != nil {
			event.TTFBMS = o.result.TTFB.Milliseconds()
		}
		if o.err != nil {
			event.ErrorMessage = o.err.Error()
			if pe, ok := provider.AsError(o.err); ok {
				event.ErrorKind = string(pe.Kind)
			} else {
				event.ErrorKind = "internal"
			}
		}

		// Record is non-blocking; a full buffer drops the event rather than
		// delaying the response.
		s.analytics.Record(event)
	}
}

// recordRejection publishes a request that was refused before it ever reached a
// provider: rate limited, over its token budget, or unauthenticated.
//
// These never pass through finish, because they are rejected in middleware or
// before the observation exists — so without this they are invisible in both
// metrics and analytics. That absence is worse than it sounds. "Requests by
// status" that silently omits every 429 makes a throttled tenant look like a
// tenant who stopped sending traffic, which are opposite problems with opposite
// responses. Rejections are also the first signal of credential stuffing and
// the number a customer disputes on a bill, so they are the rows most worth
// keeping.
//
// model is empty when the rejection happened before the body was parsed.
func (s *Server) recordRejection(r *http.Request, p *auth.Principal, model string, status int, errorKind, message string) {
	route := model
	if route == "" {
		// Never the raw path: it is caller-controlled, and a label that an
		// attacker can vary is an unbounded time series.
		route = "unknown"
	}

	if s.metrics != nil {
		s.metrics.RequestsTotal.WithLabelValues(
			route, "none", telemetry.StatusClass(status), "false").Inc()
	}

	if s.analytics != nil {
		event := analytics.Event{
			Timestamp:      time.Now(),
			RequestID:      requestIDFrom(r),
			Operation:      operationForPath(r.URL.Path),
			Route:          route,
			RequestedModel: model,
			Provider:       "none",
			Status:         status,
			ErrorKind:      errorKind,
			ErrorMessage:   message,
		}
		if p != nil {
			event.APIKeyID = p.ID
			event.ProjectID = p.ProjectID
			event.Environment = p.Environment
			event.EndUser = p.EndUserPseudonym
			event.PolicyVersion = p.PolicyVersion
		}
		s.analytics.Record(event)
	}
}

func operationForPath(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "chat.completions"
	case "/v1/responses":
		return "responses"
	case "/v1/embeddings":
		return "embeddings"
	default:
		return "other"
	}
}

func requestIDFrom(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// handleMetrics serves the Prometheus endpoint.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		writeError(w, r, http.StatusNotFound, "not_found", "metrics are disabled")
		return
	}
	s.metrics.Handler().ServeHTTP(w, r)
}

// handleAdminAnalytics reports pipeline health.
func (s *Server) handleAdminAnalytics(w http.ResponseWriter, r *http.Request) {
	if s.analyticsStats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "stats": s.analyticsStats()})
}
