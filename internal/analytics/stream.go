package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/redis/go-redis/v9"

	"github.com/darshpatel1052/conduit/internal/config"
)

const analyticsStream = "conduit:analytics:requests"
const analyticsGroup = "clickhouse-writers"

type StreamRecorder struct {
	redis    *redis.Client
	conn     driver.Conn
	cfg      config.Analytics
	log      *slog.Logger
	consumer string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	written  atomic.Uint64
	dropped  atomic.Uint64
	onDepth  func(int)
	onDrop   func()
}

func NewStream(ctx context.Context, cfg config.Analytics, log *slog.Logger) (*StreamRecorder, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse analytics Redis URL: %w", err)
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping analytics Redis: %w", err)
	}
	conn, err := openClickHouse(ctx, cfg)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	_ = client.XGroupCreateMkStream(ctx, analyticsStream, analyticsGroup, "0").Err()
	workerCtx, workerCancel := context.WithCancel(context.Background())
	r := &StreamRecorder{redis: client, conn: conn, cfg: cfg, log: log.With("component", "analytics_stream"), consumer: hostname() + "-" + strconv.Itoa(os.Getpid()), cancel: workerCancel}
	r.wg.Add(1)
	go r.run(workerCtx)
	return r, nil
}

func (r *StreamRecorder) SetHooks(depth func(int), drop func()) { r.onDepth = depth; r.onDrop = drop }

func (r *StreamRecorder) Record(event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		r.markDrop()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := r.redis.XAdd(ctx, &redis.XAddArgs{Stream: analyticsStream, Values: map[string]any{"event": payload}}).Err(); err != nil {
		r.markDrop()
		r.log.Error("could not append analytics event", "error", err)
		return
	}
	if r.onDepth != nil {
		if depth, err := r.redis.XLen(ctx, analyticsStream).Result(); err == nil {
			r.onDepth(int(depth))
		}
	}
}

func (r *StreamRecorder) run(ctx context.Context) {
	defer r.wg.Done()
	r.reclaim(ctx)
	for {
		result, err := r.redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: analyticsGroup, Consumer: r.consumer, Streams: []string{analyticsStream, ">"}, Count: int64(r.cfg.BatchSize), Block: r.cfg.FlushInterval.D()}).Result()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil && !errors.Is(err, redis.Nil) {
			r.log.Error("analytics stream read failed", "error", err)
			continue
		}
		if len(result) == 0 {
			continue
		}
		r.consume(ctx, result[0].Messages)
	}
}

func (r *StreamRecorder) reclaim(ctx context.Context) {
	start := "0-0"
	for {
		messages, next, err := r.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: analyticsStream, Group: analyticsGroup, Consumer: r.consumer, MinIdle: 30 * time.Second, Start: start, Count: int64(r.cfg.BatchSize)}).Result()
		if err != nil || len(messages) == 0 {
			return
		}
		r.consume(ctx, messages)
		if next == "0-0" {
			return
		}
		start = next
	}
}

func (r *StreamRecorder) consume(ctx context.Context, messages []redis.XMessage) {
	events := make([]Event, 0, len(messages))
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		raw, ok := message.Values["event"].(string)
		if !ok {
			if bytes, yes := message.Values["event"].([]byte); yes {
				raw = string(bytes)
			}
		}
		var event Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			_ = r.redis.XAdd(ctx, &redis.XAddArgs{Stream: analyticsStream + ":dead", Values: map[string]any{"source_id": message.ID, "reason": err.Error()}}).Err()
			_, _ = r.redis.XAck(ctx, analyticsStream, analyticsGroup, message.ID).Result()
			continue
		}
		events = append(events, event)
		ids = append(ids, message.ID)
	}
	if len(events) == 0 {
		return
	}
	if err := insertEvents(r.conn, r.cfg, events); err != nil {
		r.log.Error("ClickHouse insert failed; events remain pending", "count", len(events), "error", err)
		return
	}
	_, _ = r.redis.XAck(ctx, analyticsStream, analyticsGroup, ids...).Result()
	_, _ = r.redis.XDel(ctx, analyticsStream, ids...).Result()
	r.written.Add(uint64(len(events)))
}

func (r *StreamRecorder) Close(ctx context.Context) error {
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.conn.Close()
	return r.redis.Close()
}
func (r *StreamRecorder) Stats() Stats {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	depth, _ := r.redis.XLen(ctx, analyticsStream).Result()
	return Stats{QueueDepth: int(depth), BufferSize: r.cfg.BufferSize, Written: r.written.Load(), Dropped: r.dropped.Load()}
}
func (r *StreamRecorder) Querier() *Querier { return &Querier{conn: r.conn} }
func (r *StreamRecorder) markDrop() {
	r.dropped.Add(1)
	if r.onDrop != nil {
		r.onDrop()
	}
}
func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "gateway"
	}
	return name
}
