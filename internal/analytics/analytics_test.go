package analytics

import (
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/darshpatel1052/conduit/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- pricing ---------------------------------------------------------------

func TestCostSeparatesInputAndOutput(t *testing.T) {
	p := DefaultPricing()

	// gpt-4o-mini: $0.15/M input, $0.60/M output. Output is 4x input, so a
	// single blended rate would misreport both RAG (long prompt, short answer)
	// and generation (short prompt, long answer).
	cost := p.Cost("gpt-4o-mini", 1_000_000, 0)
	if math.Abs(cost-0.15) > 1e-9 {
		t.Errorf("input-only cost = %v, want 0.15", cost)
	}

	cost = p.Cost("gpt-4o-mini", 0, 1_000_000)
	if math.Abs(cost-0.60) > 1e-9 {
		t.Errorf("output-only cost = %v, want 0.60", cost)
	}

	cost = p.Cost("gpt-4o-mini", 500_000, 500_000)
	if want := 0.075 + 0.30; math.Abs(cost-want) > 1e-9 {
		t.Errorf("mixed cost = %v, want %v", cost, want)
	}
}

func TestGPT5MiniPricingIncludesVersionedResponses(t *testing.T) {
	p := DefaultPricing()

	// OpenAI returns a dated snapshot in the completion response. Pricing must
	// still resolve it through the gpt-5-mini family entry.
	cost := p.Cost("gpt-5-mini-2025-08-07", 1_000_000, 1_000_000)
	if want := 0.25 + 2.00; math.Abs(cost-want) > 1e-9 {
		t.Errorf("gpt-5-mini snapshot cost = %v, want %v", cost, want)
	}
}

func TestCostForLocalModelsIsZero(t *testing.T) {
	p := DefaultPricing()

	for _, model := range []string{"llama3.2:3b", "llama3.1:8b", "qwen2.5:7b", "mistral:latest"} {
		if cost := p.Cost(model, 1_000_000, 1_000_000); cost != 0 {
			t.Errorf("%s cost = %v, want 0", model, cost)
		}
		// Zero because it is priced at zero, not because it is unknown. The
		// distinction matters: both are 0.0 but only one is trustworthy.
		if !p.Known(model) {
			t.Errorf("%s should resolve to a known (zero) price, not an unknown one", model)
		}
	}
}

func TestUnknownModelCostsZeroAndIsFlagged(t *testing.T) {
	p := DefaultPricing()

	if cost := p.Cost("some-model-nobody-has-heard-of", 1000, 1000); cost != 0 {
		t.Errorf("unknown model cost = %v, want 0 rather than a guess", cost)
	}
	if p.Known("some-model-nobody-has-heard-of") {
		t.Error("an unpriced model must report as unknown so cost reporting can be honest")
	}
}

// TestVersionedModelFallsBackToPrefix: providers append dates to model names,
// and a versioned name silently costing nothing would understate spend.
func TestVersionedModelFallsBackToPrefix(t *testing.T) {
	p := DefaultPricing()

	base := p.Cost("gpt-4o", 1_000_000, 0)
	versioned := p.Cost("gpt-4o-2024-11-20", 1_000_000, 0)

	if versioned != base {
		t.Errorf("gpt-4o-2024-11-20 cost = %v, want the gpt-4o price %v", versioned, base)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	p := DefaultPricing()

	// "gpt-4o-mini" must not be priced as "gpt-4o", which is ~17x more.
	mini := p.Cost("gpt-4o-mini", 1_000_000, 0)
	full := p.Cost("gpt-4o", 1_000_000, 0)

	if mini == full {
		t.Fatal("gpt-4o-mini was priced as gpt-4o; the longest prefix must win")
	}
	if mini >= full {
		t.Errorf("mini (%v) should be cheaper than full (%v)", mini, full)
	}
}

func TestPricingIsCaseInsensitive(t *testing.T) {
	p := DefaultPricing()
	if p.Cost("GPT-4O-MINI", 1_000_000, 0) != p.Cost("gpt-4o-mini", 1_000_000, 0) {
		t.Error("pricing lookup is case-sensitive; provider casing varies")
	}
}

func TestSetOverridesPrice(t *testing.T) {
	p := DefaultPricing()
	p.Set("gpt-4o-mini", Price{InputPerMillion: 1.0, OutputPerMillion: 2.0})

	if got := p.Cost("gpt-4o-mini", 1_000_000, 0); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("override not applied: got %v, want 1.0", got)
	}
}

// --- pipeline --------------------------------------------------------------

// newTestRecorder builds a recorder with no database, to exercise the queueing
// and drop behaviour in isolation. Record never touches the connection.
func newTestRecorder(bufferSize int) *ClickHouseRecorder {
	return &ClickHouseRecorder{
		events: make(chan Event, bufferSize),
		cfg:    config.Analytics{BufferSize: bufferSize, BatchSize: 10},
		log:    testLogger(),
		done:   make(chan struct{}),
	}
}

// TestRecordNeverBlocks is the safety property of the whole package: the
// request path must not be slowed by analytics, so a full buffer drops rather
// than waits.
func TestRecordNeverBlocks(t *testing.T) {
	r := newTestRecorder(4)

	// Push far past capacity. With a blocking send this would deadlock the test.
	for i := 0; i < 1000; i++ {
		r.Record(Event{Route: "r"})
	}

	stats := r.Stats()
	if stats.QueueDepth != 4 {
		t.Errorf("queue depth = %d, want the buffer size 4", stats.QueueDepth)
	}
	if stats.Dropped != 996 {
		t.Errorf("dropped = %d, want 996", stats.Dropped)
	}
}

func TestDropsAreCounted(t *testing.T) {
	r := newTestRecorder(1)

	dropCallbacks := 0
	r.SetHooks(nil, func() { dropCallbacks++ })

	for i := 0; i < 5; i++ {
		r.Record(Event{})
	}

	if r.Stats().Dropped != 4 {
		t.Errorf("dropped = %d, want 4", r.Stats().Dropped)
	}
	// Drops must be observable, or the gateway silently loses data forever.
	if dropCallbacks != 4 {
		t.Errorf("drop callback fired %d times, want 4", dropCallbacks)
	}
}

func TestQueueDepthHookFires(t *testing.T) {
	r := newTestRecorder(10)

	var lastDepth int
	r.SetHooks(func(d int) { lastDepth = d }, nil)

	r.Record(Event{})
	r.Record(Event{})

	if lastDepth != 2 {
		t.Errorf("reported queue depth = %d, want 2", lastDepth)
	}
}

func TestBoolToUint8(t *testing.T) {
	if boolToUint8(true) != 1 || boolToUint8(false) != 0 {
		t.Error("bool conversion is wrong")
	}
}

func TestTruncateBoundsErrorMessages(t *testing.T) {
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	got := truncate(string(long), 1024)
	if len(got) > 1024+3 {
		t.Errorf("truncate returned %d chars; an unbounded error message would bloat every row", len(got))
	}
}

func TestNoopRecorderIsSafe(t *testing.T) {
	var r Recorder = NoopRecorder{}
	r.Record(Event{Route: "r"})
	if err := r.Close(nil); err != nil {
		t.Errorf("noop Close: %v", err)
	}
}

// --- config validation -----------------------------------------------------

func TestBatchSizeMustFitBuffer(t *testing.T) {
	cfg := config.Default()
	cfg.Analytics.Enabled = true
	cfg.Analytics.BufferSize = 100
	cfg.Analytics.BatchSize = 500

	// A batch larger than the buffer could never fill, so flushes would only
	// ever happen on the timer — silently, and not in the way the operator
	// configured.
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted batch_size > buffer_size")
	}
}
