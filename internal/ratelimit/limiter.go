// Package ratelimit enforces per-key request and token limits across every
// gateway replica.
//
// The state lives in Redis rather than in process memory, and that is the whole
// point. An in-memory counter enforces the limit per replica: run five gateways
// behind a load balancer with a 60/min limit each and a client gets 300/min.
// Worse, the effective limit changes every time the deployment scales, so the
// number in the contract stops meaning anything. Shared state is what makes the
// limit a property of the system instead of a property of one process.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/darshpatel1052/conduit/internal/config"
)

// Decision is the outcome of a limit check.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration

	// ResetAt is when the caller can expect capacity back.
	ResetAt time.Time

	// Degraded is true when Redis could not be consulted and the configured
	// fail-open policy admitted the request. Surfaced so the caller can log and
	// alert: silently not enforcing limits is worse than not having them.
	Degraded bool
}

// Limiter checks and consumes quota.
type Limiter interface {
	// Allow consumes cost units against key, returning whether to proceed.
	Allow(ctx context.Context, key string, limit, cost int) (Decision, error)

	// Debit consumes units without rejecting.
	//
	// This exists for token limits, which cannot work like request limits: the
	// token cost of a completion is unknown until it has been generated. So the
	// gateway checks the budget before the call and debits the true amount
	// after, letting a request that straddles the limit through and charging it
	// to the next window. Refusing to serve until the cost is known would mean
	// refusing to serve at all.
	Debit(ctx context.Context, key string, limit, cost int) error

	// Peek reports the current state without consuming.
	Peek(ctx context.Context, key string, limit int) (Decision, error)

	Close() error
}

// RedisLimiter is the distributed implementation.
type RedisLimiter struct {
	client   *redis.Client
	cfg      config.RateLimit
	log      *slog.Logger
	window   time.Duration
	timeout  time.Duration
	failOpen bool

	// now is injectable so tests can advance time deterministically instead of
	// sleeping. Time is supplied by the caller rather than read inside Lua on
	// purpose: Redis's TIME command makes a script non-deterministic, which
	// historically barred it from replication and still complicates it. Passing
	// the timestamp in keeps the scripts pure functions of their arguments.
	now func() time.Time
}

// New connects to Redis and returns a limiter.
//
// The connection is verified eagerly: a typo in the URL should fail at startup,
// where someone is watching, rather than silently degrading every request to
// fail-open once traffic arrives.
func New(cfg config.RateLimit, log *slog.Logger) (*RedisLimiter, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis_url: %w", err)
	}

	// Bound every Redis operation. The limiter is on the hot path, so a Redis
	// that hangs must degrade quickly rather than stall the gateway.
	opts.DialTimeout = cfg.Timeout.D()
	opts.ReadTimeout = cfg.Timeout.D()
	opts.WriteTimeout = cfg.Timeout.D()
	opts.PoolSize = 32
	opts.MinIdleConns = 4

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("connect to redis at %s: %w", opts.Addr, err)
	}

	l := &RedisLimiter{
		client:   client,
		cfg:      cfg,
		log:      log.With("component", "ratelimit"),
		window:   cfg.Window.D(),
		timeout:  cfg.Timeout.D(),
		failOpen: cfg.FailOpen,
		now:      time.Now,
	}
	l.log.Info("rate limiter connected",
		"addr", opts.Addr, "algorithm", cfg.Algorithm,
		"window", cfg.Window.String(), "fail_open", cfg.FailOpen)
	return l, nil
}

// Allow is the gate: it asks whether cost fits within the budget and consumes
// it only if so. A refusal records nothing, because the refused request never
// happens.
func (l *RedisLimiter) Allow(ctx context.Context, key string, limit, cost int) (Decision, error) {
	return l.eval(ctx, key, limit, cost, evalConsume)
}

// Peek reports the current standing without consuming any of it.
func (l *RedisLimiter) Peek(ctx context.Context, key string, limit int) (Decision, error) {
	return l.eval(ctx, key, limit, 0, evalDryRun)
}

// Debit is accounting, not gating: the cost has already been incurred, so it is
// recorded unconditionally. Sharing Allow's semantics here would mean a cost
// larger than the whole budget gets rejected and therefore never written down —
// so the request that most deserves to be counted is the one that is not, and a
// caller who only ever sends expensive requests is never limited at all.
func (l *RedisLimiter) Debit(ctx context.Context, key string, limit, cost int) error {
	if cost <= 0 || limit <= 0 {
		return nil
	}
	_, err := l.eval(ctx, key, limit, cost, evalForce)
	return err
}

// evalMode distinguishes the three operations that share the Lua scripts.
type evalMode int

const (
	evalConsume evalMode = iota // gate: consume only if it fits
	evalDryRun                  // report only, change nothing
	evalForce                   // record regardless of whether it fits
)

func (l *RedisLimiter) eval(ctx context.Context, key string, limit, cost int, mode evalMode) (Decision, error) {
	if limit <= 0 {
		// Unlimited.
		return Decision{Allowed: true, Limit: 0, Remaining: -1}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	dry, force := 0, 0
	switch mode {
	case evalDryRun:
		dry = 1
	case evalForce:
		force = 1
	}
	now := l.now().UnixMilli()
	windowMS := l.window.Milliseconds()

	var (
		res []any
		err error
	)

	switch l.cfg.Algorithm {
	case config.AlgorithmTokenBucket:
		burst := l.cfg.Burst
		if burst <= 0 {
			burst = limit
		}
		res, err = tokenBucketScript.Run(ctx, l.client, []string{key},
			now, windowMS, limit, burst, cost, dry, force).Slice()
	default:
		res, err = slidingWindowScript.Run(ctx, l.client, []string{key},
			now, windowMS, limit, cost, dry, force).Slice()
	}

	if err != nil {
		return l.degrade(key, limit, err)
	}
	if len(res) < 4 {
		return l.degrade(key, limit, fmt.Errorf("unexpected script reply of length %d", len(res)))
	}

	allowed := toInt(res[0]) == 1
	remaining := toInt(res[1])
	retryMS := toInt(res[2])

	d := Decision{
		Allowed:    allowed,
		Limit:      limit,
		Remaining:  remaining,
		RetryAfter: time.Duration(retryMS) * time.Millisecond,
		ResetAt:    l.now().Add(time.Duration(retryMS) * time.Millisecond),
	}
	if remaining < 0 {
		d.Remaining = 0
	}
	if !allowed && d.RetryAfter <= 0 {
		// Never advertise a zero backoff: a client that retries immediately
		// turns a rate-limit response into a busy loop.
		d.RetryAfter = time.Second
		d.ResetAt = l.now().Add(time.Second)
	}
	return d, nil
}

// degrade applies the fail-open / fail-closed policy when Redis is unreachable.
func (l *RedisLimiter) degrade(key string, limit int, err error) (Decision, error) {
	if errors.Is(err, context.Canceled) {
		// The client left; this is not a Redis problem.
		return Decision{Allowed: false, Limit: limit}, err
	}

	l.log.Error("rate limiter unavailable, applying fail-open policy",
		"fail_open", l.failOpen, "error", err)

	if l.failOpen {
		return Decision{
			Allowed:   true,
			Limit:     limit,
			Remaining: -1,
			Degraded:  true,
		}, nil
	}
	return Decision{
		Allowed:    false,
		Limit:      limit,
		RetryAfter: time.Second,
		ResetAt:    time.Now().Add(time.Second),
		Degraded:   true,
	}, nil
}

func (l *RedisLimiter) Close() error { return l.client.Close() }

// Ping reports whether Redis is reachable, for readiness reporting.
func (l *RedisLimiter) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	return l.client.Ping(ctx).Err()
}

func toInt(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}

// Key builds a namespaced Redis key. The dimension keeps request limits and
// token limits in separate counters for the same principal.
func Key(dimension, principal string) string {
	return "conduit:rl:" + dimension + ":" + principal
}

// NoopLimiter is used when rate limiting is disabled, so callers never need a
// nil check on the hot path.
type NoopLimiter struct{}

func (NoopLimiter) Allow(context.Context, string, int, int) (Decision, error) {
	return Decision{Allowed: true, Remaining: -1}, nil
}
func (NoopLimiter) Debit(context.Context, string, int, int) error { return nil }
func (NoopLimiter) Peek(context.Context, string, int) (Decision, error) {
	return Decision{Allowed: true, Remaining: -1}, nil
}
func (NoopLimiter) Close() error { return nil }
