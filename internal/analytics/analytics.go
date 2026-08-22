// Package analytics records one event per request into ClickHouse, without
// putting a database write on the request path.
//
// The shape of this package is one decision: the request path must never block
// on analytics. A synchronous INSERT would tie every user-visible request to
// the availability and latency of an OLAP database that exists purely for
// reporting — so a ClickHouse hiccup would become a gateway outage, and
// per-row inserts would destroy ClickHouse's performance anyway, since it is a
// columnar store that wants large batches.
//
// Project-managed deployments append to a Redis Stream and acknowledge only
// after ClickHouse accepts a batch. The in-memory recorder in this file remains
// for the standalone YAML mode, where no project or administrator data exists.
package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/darshpatel1052/conduit/internal/config"
)

// Event is one served request.
type Event struct {
	Timestamp time.Time
	RequestID string

	ProjectID     string
	APIKeyID      string
	Environment   string
	EndUser       string
	PolicyVersion int64
	Route         string
	Operation     string

	RequestedModel string
	UpstreamModel  string
	Provider       string
	ProviderType   string

	Status   int
	Stream   bool
	Attempts int
	Failover bool

	DurationMS int64
	TTFBMS     int64

	PromptTokens     int
	CompletionTokens int
	CostUSD          float64

	CacheStatus     string // hit_exact | hit_semantic | miss | bypass | error | disabled
	CacheSimilarity float64

	ErrorKind    string
	ErrorMessage string
}

// Recorder accepts events.
type Recorder interface {
	Record(Event)
	Close(ctx context.Context) error
}

// ClickHouseRecorder is the batching ClickHouse sink.
type ClickHouseRecorder struct {
	conn   driver.Conn
	events chan Event
	cfg    config.Analytics
	log    *slog.Logger

	dropped atomic.Uint64
	written atomic.Uint64

	wg       sync.WaitGroup
	stopOnce sync.Once
	done     chan struct{}

	// onQueueDepth lets the server publish queue depth as a metric without
	// this package importing the metrics registry.
	onQueueDepth func(int)
	onDrop       func()
}

const schemaDDL = `
CREATE TABLE IF NOT EXISTS conduit_requests (
    timestamp         DateTime64(3),
    request_id        String,
    project_id        UUID DEFAULT toUUID('00000000-0000-0000-0000-000000000000'),
    api_key_id        LowCardinality(String),
    environment       LowCardinality(String),
    end_user          String,
    policy_version    UInt64,
    operation         LowCardinality(String),
    route             LowCardinality(String),
    requested_model   LowCardinality(String),
    upstream_model    LowCardinality(String),
    provider          LowCardinality(String),
    provider_type     LowCardinality(String),
    status            UInt16,
    stream            UInt8,
    attempts          UInt8,
    failover          UInt8,
    duration_ms       UInt32,
    ttfb_ms           UInt32,
    prompt_tokens     UInt32,
    completion_tokens UInt32,
    cost_usd          Float64,
    cache_status      LowCardinality(String),
    cache_similarity  Float32,
    error_kind        LowCardinality(String),
    error_message     String
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (timestamp, route, provider)
TTL toDateTime(timestamp) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192`

var schemaMigrations = []string{
	`ALTER TABLE conduit_requests ADD COLUMN IF NOT EXISTS project_id UUID DEFAULT toUUID('00000000-0000-0000-0000-000000000000') AFTER request_id`,
	`ALTER TABLE conduit_requests ADD COLUMN IF NOT EXISTS environment LowCardinality(String) AFTER api_key_id`,
	`ALTER TABLE conduit_requests ADD COLUMN IF NOT EXISTS end_user String AFTER environment`,
	`ALTER TABLE conduit_requests ADD COLUMN IF NOT EXISTS policy_version UInt64 AFTER end_user`,
	`ALTER TABLE conduit_requests ADD COLUMN IF NOT EXISTS operation LowCardinality(String) DEFAULT 'chat.completions' AFTER policy_version`,
}

// New connects to ClickHouse, applies the schema and starts the worker.
func New(ctx context.Context, cfg config.Analytics, log *slog.Logger) (*ClickHouseRecorder, error) {
	conn, err := openClickHouse(ctx, cfg)
	if err != nil {
		return nil, err
	}

	r := &ClickHouseRecorder{
		conn:   conn,
		events: make(chan Event, cfg.BufferSize),
		cfg:    cfg,
		log:    log.With("component", "analytics"),
		done:   make(chan struct{}),
	}

	r.wg.Add(1)
	go r.run()

	r.log.Info("analytics pipeline started",
		"buffer_size", cfg.BufferSize,
		"batch_size", cfg.BatchSize,
		"flush_interval", cfg.FlushInterval.String())
	return r, nil
}

func openClickHouse(ctx context.Context, cfg config.Analytics) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(cfg.ClickHouseDSN)
	if err != nil {
		return nil, fmt.Errorf("parse clickhouse_dsn: %w", err)
	}
	opts.DialTimeout = 5 * time.Second
	opts.MaxOpenConns = 4
	opts.MaxIdleConns = 2
	opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("connect to clickhouse: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	if err := conn.Exec(pingCtx, schemaDDL); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply analytics schema: %w", err)
	}
	for i, statement := range schemaMigrations {
		if err := conn.Exec(pingCtx, statement); err != nil {
			conn.Close()
			return nil, fmt.Errorf("apply analytics migration %d: %w", i+1, err)
		}
	}

	return conn, nil
}

// SetHooks registers callbacks for queue-depth and drop metrics.
func (r *ClickHouseRecorder) SetHooks(onQueueDepth func(int), onDrop func()) {
	r.onQueueDepth = onQueueDepth
	r.onDrop = onDrop
}

// Record queues an event.
//
// Non-blocking by design. The default branch is the entire safety property of
// this package: if the buffer is full — ClickHouse is slow, or down, or a
// traffic spike outran the writer — the event is discarded and counted. The
// alternative, blocking, would apply backpressure from a reporting system onto
// live inference, which is exactly backwards.
func (r *ClickHouseRecorder) Record(e Event) {
	select {
	case r.events <- e:
		if r.onQueueDepth != nil {
			r.onQueueDepth(len(r.events))
		}
	default:
		n := r.dropped.Add(1)
		if r.onDrop != nil {
			r.onDrop()
		}
		// Log sparsely: if the buffer is full, it is full for many events, and
		// logging each one would turn a metrics problem into a log flood.
		if n%1000 == 1 {
			r.log.Warn("analytics buffer full, dropping events",
				"dropped_total", n, "buffer_size", r.cfg.BufferSize)
		}
	}
}

// run drains the queue, batching by count and by elapsed time.
func (r *ClickHouseRecorder) run() {
	defer r.wg.Done()

	batch := make([]Event, 0, r.cfg.BatchSize)
	ticker := time.NewTicker(r.cfg.FlushInterval.D())
	defer ticker.Stop()

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		if err := r.insert(batch); err != nil {
			r.log.Error("analytics batch insert failed, events lost",
				"count", len(batch), "reason", reason, "error", err)
		} else {
			r.written.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-r.events:
			if !ok {
				// Channel closed on shutdown: write what is left and stop.
				flush("shutdown")
				close(r.done)
				return
			}
			batch = append(batch, e)

			// Two flush triggers, because either alone is wrong. Size alone
			// means a low-traffic gateway's events sit in memory indefinitely
			// and are lost on restart. Time alone means a burst produces many
			// small inserts, which is the access pattern ClickHouse handles
			// worst.
			if len(batch) >= r.cfg.BatchSize {
				flush("batch_full")
			}

		case <-ticker.C:
			flush("interval")
		}
	}
}

func (r *ClickHouseRecorder) insert(batch []Event) error {
	return insertEvents(r.conn, r.cfg, batch)
}

func insertEvents(conn driver.Conn, cfg config.Analytics, batch []Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.WriteTimeout.D())
	defer cancel()

	b, err := conn.PrepareBatch(ctx, `INSERT INTO conduit_requests (
		timestamp,request_id,project_id,api_key_id,environment,end_user,policy_version,operation,route,requested_model,
		upstream_model,provider,provider_type,status,stream,attempts,failover,duration_ms,ttfb_ms,prompt_tokens,
		completion_tokens,cost_usd,cache_status,cache_similarity,error_kind,error_message)`)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}

	for _, e := range batch {
		err := b.Append(
			e.Timestamp,
			e.RequestID,
			uuidOrZero(e.ProjectID),
			e.APIKeyID,
			e.Environment,
			e.EndUser,
			uint64(e.PolicyVersion),
			e.Operation,
			e.Route,
			e.RequestedModel,
			e.UpstreamModel,
			e.Provider,
			e.ProviderType,
			uint16(e.Status),
			boolToUint8(e.Stream),
			uint8(e.Attempts),
			boolToUint8(e.Failover),
			uint32(e.DurationMS),
			uint32(e.TTFBMS),
			uint32(e.PromptTokens),
			uint32(e.CompletionTokens),
			e.CostUSD,
			e.CacheStatus,
			float32(e.CacheSimilarity),
			e.ErrorKind,
			truncate(e.ErrorMessage, 1024),
		)
		if err != nil {
			return fmt.Errorf("append row: %w", err)
		}
	}
	return b.Send()
}

func uuidOrZero(value string) string {
	if value == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return value
}

// Close drains the queue and stops the worker.
//
// Called during graceful shutdown, after the HTTP listener has stopped
// accepting. Without an explicit drain, everything still buffered at SIGTERM is
// lost — and a rolling deploy would silently punch a hole in the analytics for
// every pod it replaced.
func (r *ClickHouseRecorder) Close(ctx context.Context) error {
	var err error
	r.stopOnce.Do(func() {
		close(r.events)

		select {
		case <-r.done:
		case <-ctx.Done():
			err = fmt.Errorf("analytics drain timed out with %d events still queued", len(r.events))
		}

		r.log.Info("analytics pipeline stopped",
			"written", r.written.Load(), "dropped", r.dropped.Load())
		r.conn.Close()
	})
	return err
}

// Stats reports pipeline health.
type Stats struct {
	QueueDepth int    `json:"queue_depth"`
	BufferSize int    `json:"buffer_size"`
	Written    uint64 `json:"written"`
	Dropped    uint64 `json:"dropped"`
}

// Stats returns current counters.
func (r *ClickHouseRecorder) Stats() Stats {
	return Stats{
		QueueDepth: len(r.events),
		BufferSize: r.cfg.BufferSize,
		Written:    r.written.Load(),
		Dropped:    r.dropped.Load(),
	}
}

// Ping reports database reachability.
func (r *ClickHouseRecorder) Ping(ctx context.Context) error { return r.conn.Ping(ctx) }

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// NoopRecorder is used when analytics is disabled.
type NoopRecorder struct{}

func (NoopRecorder) Record(Event)                {}
func (NoopRecorder) Close(context.Context) error { return nil }
