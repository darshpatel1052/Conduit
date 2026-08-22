package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/darshpatel1052/conduit/internal/config"
)

// TracerName identifies Conduit's instrumentation scope.
const TracerName = "github.com/darshpatel1052/conduit"

// Tracer returns the gateway's tracer.
//
// Tracing answers the question metrics cannot: for *this* slow request, where
// did the time actually go — waiting on the embedder, searching the cache,
// queuing behind an open breaker, or generating at the provider? A p99 latency
// graph tells you something is slow; a trace tells you which span owns it.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// InitTracing configures the global tracer provider and returns a shutdown
// function.
func InitTracing(ctx context.Context, cfg config.Tracing, version string, log *slog.Logger) (func(context.Context) error, error) {
	if !cfg.Enabled {
		// A no-op provider is still installed so instrumentation code can call
		// Start unconditionally without nil checks or feature flags at every
		// call site.
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithTimeout(10*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(version),
		semconv.DeploymentEnvironment(cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("build trace resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		// Batched, for the same reason analytics is: a span export per request
		// would put network I/O on the hot path.
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),

		// ParentBased(TraceIDRatio) keeps a distributed trace coherent: if an
		// upstream service already decided to sample a request, honour that
		// decision rather than sampling independently and producing traces with
		// holes in them.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRatio),
		)),
	)

	otel.SetTracerProvider(provider)

	// W3C trace context so a trace started by the caller continues through the
	// gateway instead of restarting at it.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	log.Info("tracing enabled",
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"sample_ratio", cfg.SampleRatio)

	return provider.Shutdown, nil
}

// Span attribute keys. Kept as constants so the same names appear in traces,
// metrics labels and log fields, which is what makes it possible to pivot from
// one to another during an incident.
const (
	AttrRoute         = attribute.Key("conduit.route")
	AttrProvider      = attribute.Key("conduit.provider")
	AttrProviderType  = attribute.Key("conduit.provider_type")
	AttrModel         = attribute.Key("conduit.model")
	AttrUpstreamModel = attribute.Key("conduit.upstream_model")
	AttrStream        = attribute.Key("conduit.stream")
	AttrAttempt       = attribute.Key("conduit.attempt")
	AttrCacheStatus   = attribute.Key("conduit.cache.status")
	AttrCacheScore    = attribute.Key("conduit.cache.similarity")
	AttrPromptTokens  = attribute.Key("conduit.tokens.prompt")
	AttrOutputTokens  = attribute.Key("conduit.tokens.completion")
	AttrCostUSD       = attribute.Key("conduit.cost_usd")
	AttrAPIKeyID      = attribute.Key("conduit.api_key_id")
	AttrBreakerState  = attribute.Key("conduit.breaker_state")
)
