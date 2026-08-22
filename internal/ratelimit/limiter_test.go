package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/darshpatel1052/conduit/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func init() {
	// The fail-open tests deliberately kill Redis; go-redis logs each retry to
	// stderr, which buries the actual test output.
	redis.SetLogger(discardLogger{})
}

type discardLogger struct{}

func (discardLogger) Printf(context.Context, string, ...any) {}

// clock advances the limiter's view of time and miniredis's TTL clock together.
// Both must move: the scripts compute windows from the timestamp Go passes in,
// while key expiry is Redis's own business.
type clock struct {
	mu  sync.Mutex
	now time.Time
	mr  *miniredis.Miniredis
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	c.mr.FastForward(d)
}

// newTestLimiter runs against miniredis, which executes the same Lua the real
// Redis does. That matters: the counting logic lives entirely in those scripts,
// so a mock of the Go interface would test nothing that can actually break.
func newTestLimiter(t *testing.T, algorithm string, window time.Duration) (*RedisLimiter, *clock) {
	t.Helper()

	mr := miniredis.RunT(t)
	cfg := config.RateLimit{
		Enabled:   true,
		RedisURL:  "redis://" + mr.Addr(),
		Algorithm: algorithm,
		Window:    config.Duration(window),
		FailOpen:  true,
		Timeout:   config.Duration(2 * time.Second),
	}

	l, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	c := &clock{now: time.Now(), mr: mr}
	l.now = c.Now
	return l, c
}

func TestSlidingWindowEnforcesLimit(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		d, err := l.Allow(ctx, "k1", 5, 1)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d rejected, want allowed (limit 5)", i)
		}
		if want := 5 - i; d.Remaining != want {
			t.Errorf("request %d remaining = %d, want %d", i, d.Remaining, want)
		}
	}

	d, err := l.Allow(ctx, "k1", 5, 1)
	if err != nil {
		t.Fatalf("6th request: %v", err)
	}
	if d.Allowed {
		t.Fatal("6th request allowed, want rejected")
	}
	if d.RetryAfter <= 0 {
		t.Error("rejection must advertise a positive Retry-After, or clients busy-loop")
	}
}

func TestLimitsAreIsolatedPerKey(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if d, _ := l.Allow(ctx, "tenant-a", 3, 1); !d.Allowed {
			t.Fatalf("tenant-a request %d rejected", i+1)
		}
	}
	if d, _ := l.Allow(ctx, "tenant-a", 3, 1); d.Allowed {
		t.Fatal("tenant-a should be exhausted")
	}

	// A different principal must be unaffected: one noisy tenant exhausting its
	// quota cannot be allowed to throttle everybody else.
	if d, _ := l.Allow(ctx, "tenant-b", 3, 1); !d.Allowed {
		t.Fatal("tenant-b was throttled by tenant-a's usage")
	}
}

// TestSlidingWindowAvoidsBoundaryBurst is the reason this is not a fixed
// window. A fixed window with limit N lets a client send N at the very end of
// one window and N at the start of the next — 2N in a moment, all "legal".
func TestSlidingWindowAvoidsBoundaryBurst(t *testing.T) {
	window := 2 * time.Second
	l, clk := newTestLimiter(t, config.AlgorithmSlidingWindow, window)
	ctx := context.Background()

	// Exhaust the current window.
	for i := 0; i < 10; i++ {
		if d, _ := l.Allow(ctx, "burst", 10, 1); !d.Allowed {
			t.Fatalf("setup request %d rejected", i+1)
		}
	}

	// Step just past the boundary. A fixed window would reset to a full
	// allowance here; the sliding window still counts most of the previous one.
	clk.Advance(window)

	allowed := 0
	for i := 0; i < 10; i++ {
		if d, _ := l.Allow(ctx, "burst", 10, 1); d.Allowed {
			allowed++
		}
	}

	if allowed >= 10 {
		t.Errorf("allowed %d immediately after the window boundary; a sliding window "+
			"must still count the previous window, not reset to a full quota", allowed)
	}
}

func TestSlidingWindowRecoversAfterFullWindow(t *testing.T) {
	window := time.Second
	l, clk := newTestLimiter(t, config.AlgorithmSlidingWindow, window)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		l.Allow(ctx, "recover", 3, 1)
	}
	if d, _ := l.Allow(ctx, "recover", 3, 1); d.Allowed {
		t.Fatal("setup: expected exhaustion")
	}

	// Two full windows later the old counts have aged out entirely.
	clk.Advance(2 * window)

	if d, _ := l.Allow(ctx, "recover", 3, 1); !d.Allowed {
		t.Error("quota did not recover after two full windows elapsed")
	}
}

func TestTokenBucketAllowsBurstThenThrottles(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmTokenBucket, time.Minute)
	ctx := context.Background()

	// A bucket starts full, so a previously idle client can burst.
	allowed := 0
	for i := 0; i < 15; i++ {
		if d, _ := l.Allow(ctx, "bursty", 10, 1); d.Allowed {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("burst allowed %d, want exactly the bucket capacity of 10", allowed)
	}
}

func TestTokenBucketRefillsOverTime(t *testing.T) {
	window := time.Second
	l, clk := newTestLimiter(t, config.AlgorithmTokenBucket, window)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		l.Allow(ctx, "refill", 10, 1)
	}
	if d, _ := l.Allow(ctx, "refill", 10, 1); d.Allowed {
		t.Fatal("setup: bucket should be empty")
	}

	// Half a window should restore roughly half the capacity.
	clk.Advance(window / 2)

	allowed := 0
	for i := 0; i < 10; i++ {
		if d, _ := l.Allow(ctx, "refill", 10, 1); d.Allowed {
			allowed++
		}
	}
	if allowed < 3 || allowed > 7 {
		t.Errorf("allowed %d after half a window, want roughly half of 10", allowed)
	}
}

func TestCostGreaterThanOne(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	// Token limits charge many units at once.
	if d, _ := l.Allow(ctx, "tokens", 1000, 800); !d.Allowed {
		t.Fatal("800 of 1000 should be allowed")
	}
	if d, _ := l.Allow(ctx, "tokens", 1000, 400); d.Allowed {
		t.Fatal("a further 400 would exceed 1000; want rejection")
	}
}

// TestDebitRecordsCostsThatExceedTheLimit is the regression for a bug live
// verification caught: Debit shared Allow's semantics, so a cost larger than
// the whole budget was rejected and therefore never written down. The effect
// was the exact inverse of the intent — a caller whose every request blew the
// entire per-minute allowance was recorded as having used nothing, and so was
// never limited, while cheap callers were. Tokens are the case that matters:
// the cost of a completion is only knowable after it has been generated, so
// refusing to record it does not un-spend it.
func TestDebitRecordsCostsThatExceedTheLimit(t *testing.T) {
	for _, algorithm := range []string{config.AlgorithmSlidingWindow, config.AlgorithmTokenBucket} {
		t.Run(algorithm, func(t *testing.T) {
			l, _ := newTestLimiter(t, algorithm, time.Minute)
			ctx := context.Background()

			// One completion that ran away with more than the minute's budget.
			if err := l.Debit(ctx, "runaway", 40, 970); err != nil {
				t.Fatalf("Debit: %v", err)
			}

			d, err := l.Peek(ctx, "runaway", 40)
			if err != nil {
				t.Fatalf("Peek: %v", err)
			}
			if d.Allowed {
				t.Fatal("budget still reads as available after a debit of 970 against a limit of 40")
			}
			if d.RetryAfter <= 0 {
				t.Error("a rejected Peek must advertise a backoff, or clients busy-loop")
			}
		})
	}
}

// TestDebitCarriesOverageIntoTheNextWindow: the overage is not forgiven at the
// window edge. Having overshot by 24x, the caller stays blocked well past the
// point where a fresh window would have reset them.
func TestDebitCarriesOverageIntoTheNextWindow(t *testing.T) {
	l, c := newTestLimiter(t, config.AlgorithmTokenBucket, time.Minute)
	ctx := context.Background()

	if err := l.Debit(ctx, "carried", 40, 970); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	// A full window later the bucket has refilled by 40, nowhere near the 930
	// it is in the hole for.
	c.Advance(time.Minute)
	if d, _ := l.Peek(ctx, "carried", 40); d.Allowed {
		t.Error("the deficit was forgiven after one window; the overage must carry")
	}

	// Far enough out, it recovers rather than being blocked forever.
	c.Advance(30 * time.Minute)
	if d, _ := l.Peek(ctx, "carried", 40); !d.Allowed {
		t.Error("still blocked after 30 windows; the deficit must drain, not persist")
	}
}

// TestAllowStillRefusesWithoutRecording guards the other half of the
// distinction. Allow must not start behaving like Debit: a refused request was
// never served, so charging for it would bill callers for 429s.
func TestAllowStillRefusesWithoutRecording(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if d, _ := l.Allow(ctx, "gated", 5, 10); d.Allowed {
			t.Fatal("a cost of 10 against a limit of 5 must be refused")
		}
	}
	// Twenty refusals consumed nothing, so the budget is still intact.
	if d, _ := l.Allow(ctx, "gated", 5, 5); !d.Allowed {
		t.Error("refused requests were charged against the budget")
	}
}

func TestPeekDoesNotConsume(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if _, err := l.Peek(ctx, "peeked", 3); err != nil {
			t.Fatalf("Peek: %v", err)
		}
	}
	// Peeking ten times must not have spent any of the allowance.
	for i := 0; i < 3; i++ {
		if d, _ := l.Allow(ctx, "peeked", 3, 1); !d.Allowed {
			t.Fatalf("request %d rejected; Peek consumed quota", i+1)
		}
	}
}

func TestZeroLimitMeansUnlimited(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		if d, _ := l.Allow(ctx, "unlimited", 0, 1); !d.Allowed {
			t.Fatalf("request %d rejected under a zero (unlimited) limit", i+1)
		}
	}
}

// TestFailOpenWhenRedisDown covers the availability decision: the limiter
// protects upstreams from abuse, and a Redis outage is not abuse. Failing
// closed would turn a cache-tier blip into a total outage for every tenant.
func TestFailOpenWhenRedisDown(t *testing.T) {
	l, clk := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	clk.mr.Close() // Redis disappears

	d, err := l.Allow(ctx, "k", 5, 1)
	if err != nil {
		t.Fatalf("fail-open should not surface an error: %v", err)
	}
	if !d.Allowed {
		t.Error("request rejected while fail_open is true")
	}
	if !d.Degraded {
		t.Error("Degraded must be set so the caller can alert; silently not enforcing is worse than not enforcing")
	}
}

func TestFailClosedWhenConfigured(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := config.RateLimit{
		Enabled:   true,
		RedisURL:  "redis://" + mr.Addr(),
		Algorithm: config.AlgorithmSlidingWindow,
		Window:    config.Duration(time.Minute),
		FailOpen:  false,
		Timeout:   config.Duration(500 * time.Millisecond),
	}
	l, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer l.Close()

	mr.Close()

	d, _ := l.Allow(context.Background(), "k", 5, 1)
	if d.Allowed {
		t.Error("request allowed while fail_open is false")
	}
	if !d.Degraded {
		t.Error("Degraded should be set")
	}
}

// TestConcurrentAllowIsAtomic is the reason the logic lives in Lua. With a
// read-then-write from Go, concurrent callers would both observe room and both
// increment, overshooting the limit.
func TestConcurrentAllowIsAtomic(t *testing.T) {
	l, _ := newTestLimiter(t, config.AlgorithmSlidingWindow, time.Minute)
	ctx := context.Background()

	const limit = 50
	const attempts = 200

	var mu sync.Mutex
	allowed := 0

	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(ctx, "concurrent", limit, 1)
			if err == nil && d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Errorf("allowed %d of %d concurrent requests, want exactly %d: "+
			"the check-and-increment is not atomic", allowed, attempts, limit)
	}
}

func TestKeyNamespacing(t *testing.T) {
	if got := Key("req", "key:abc"); got != "conduit:rl:req:key:abc" {
		t.Errorf("Key = %q", got)
	}
	// Request and token budgets must not share a counter.
	if Key("req", "x") == Key("tok", "x") {
		t.Error("request and token dimensions collide")
	}
}

func TestNoopLimiterAlwaysAllows(t *testing.T) {
	var l Limiter = NoopLimiter{}
	d, err := l.Allow(context.Background(), "k", 1, 1000)
	if err != nil || !d.Allowed {
		t.Errorf("noop limiter rejected: %+v %v", d, err)
	}
}
