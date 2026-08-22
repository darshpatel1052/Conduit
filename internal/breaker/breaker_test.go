package breaker

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() config.BreakerConfig {
	return config.BreakerConfig{
		ConsecutiveFailures: 3,
		FailureRatio:        0.5,
		MinRequests:         100, // effectively off, so tests isolate one trip rule
		Interval:            config.Duration(time.Minute),
		Cooldown:            config.Duration(50 * time.Millisecond),
		HalfOpenMaxRequests: 1,
	}
}

func upstreamFailure() error {
	return &provider.Error{
		Provider: "test", Kind: provider.KindUpstream,
		Retryable: true, CountsAsFailure: true,
	}
}

// TestAllowReportsOutcome is a regression test for a bug where the returned
// callback captured a named return value and so ended up calling itself,
// silently reporting nothing. The breaker's counts stayed at zero and it could
// never trip — the failure mode was total, and completely invisible.
func TestAllowReportsOutcome(t *testing.T) {
	b := New("test", testConfig(), testLogger())

	done, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() on a fresh breaker: %v", err)
	}
	done(upstreamFailure())

	if got := b.Snapshot().TotalFailures; got != 1 {
		t.Fatalf("TotalFailures = %d, want 1 (the outcome callback did not reach the breaker)", got)
	}
	if got := b.Snapshot().ConsecutiveFailures; got != 1 {
		t.Fatalf("ConsecutiveFailures = %d, want 1", got)
	}
}

func TestTripsAfterConsecutiveFailures(t *testing.T) {
	b := New("test", testConfig(), testLogger())

	for i := 0; i < 3; i++ {
		done, err := b.Allow()
		if err != nil {
			t.Fatalf("attempt %d rejected too early: %v", i+1, err)
		}
		done(upstreamFailure())
	}

	if got := b.State(); got != "open" {
		t.Fatalf("state = %q after 3 consecutive failures, want open", got)
	}

	// An open breaker must reject without touching the network.
	if _, err := b.Allow(); err == nil {
		t.Fatal("Allow() succeeded while open, want rejection")
	} else {
		pe, ok := provider.AsError(err)
		if !ok {
			t.Fatalf("rejection error is %T, want *provider.Error", err)
		}
		if pe.Kind != provider.KindCircuitOpen {
			t.Errorf("rejection kind = %q, want %q", pe.Kind, provider.KindCircuitOpen)
		}
		// Retryable so the request fails over; not a failure because no
		// request was actually made.
		if !pe.Retryable {
			t.Error("rejection should be retryable so the proxy fails over to the next target")
		}
		if pe.CountsAsFailure {
			t.Error("a short-circuited request must not count as a provider failure")
		}
	}
	if got := b.Snapshot().ShortCircuited; got != 1 {
		t.Errorf("ShortCircuited = %d, want 1", got)
	}
}

// TestClientErrorsDoNotTrip is the important one: a caller sending malformed
// requests in a loop must not be able to take a healthy provider out of service
// for everybody else.
func TestClientErrorsDoNotTrip(t *testing.T) {
	b := New("test", testConfig(), testLogger())

	clientErr := &provider.Error{
		Provider: "test", Kind: provider.KindInvalidRequest,
		Retryable: false, CountsAsFailure: false,
	}

	for i := 0; i < 20; i++ {
		done, err := b.Allow()
		if err != nil {
			t.Fatalf("breaker tripped on client errors at attempt %d", i+1)
		}
		done(clientErr)
	}

	if got := b.State(); got != "closed" {
		t.Fatalf("state = %q after 20 client errors, want closed", got)
	}
	if got := b.Snapshot().TotalFailures; got != 0 {
		t.Errorf("TotalFailures = %d, want 0: client errors must be excluded", got)
	}
}

func TestHalfOpenRecovery(t *testing.T) {
	b := New("test", testConfig(), testLogger())

	for i := 0; i < 3; i++ {
		done, _ := b.Allow()
		done(upstreamFailure())
	}
	if b.State() != "open" {
		t.Fatalf("state = %q, want open", b.State())
	}

	// Wait out the cooldown; the breaker should admit a probe.
	time.Sleep(70 * time.Millisecond)
	if got := b.State(); got != "half-open" {
		t.Fatalf("state = %q after cooldown, want half-open", got)
	}

	done, err := b.Allow()
	if err != nil {
		t.Fatalf("half-open breaker rejected the probe: %v", err)
	}
	done(nil) // probe succeeds

	if got := b.State(); got != "closed" {
		t.Fatalf("state = %q after a successful probe, want closed", got)
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b := New("test", testConfig(), testLogger())

	for i := 0; i < 3; i++ {
		done, _ := b.Allow()
		done(upstreamFailure())
	}
	time.Sleep(70 * time.Millisecond)

	done, err := b.Allow()
	if err != nil {
		t.Fatalf("half-open breaker rejected the probe: %v", err)
	}
	done(upstreamFailure()) // probe fails

	if got := b.State(); got != "open" {
		t.Fatalf("state = %q after a failed probe, want open", got)
	}
}

func TestDoubleReportIsIgnored(t *testing.T) {
	b := New("test", testConfig(), testLogger())

	done, _ := b.Allow()
	done(upstreamFailure())
	done(upstreamFailure()) // a buggy caller reporting twice

	if got := b.Snapshot().TotalFailures; got != 1 {
		t.Fatalf("TotalFailures = %d, want 1: a double report must not corrupt counts", got)
	}
}

func TestDisabledBreakerAlwaysAllows(t *testing.T) {
	cfg := testConfig()
	cfg.Disabled = true
	b := New("test", cfg, testLogger())

	for i := 0; i < 50; i++ {
		done, err := b.Allow()
		if err != nil {
			t.Fatalf("disabled breaker rejected at attempt %d", i+1)
		}
		done(upstreamFailure())
	}
	if got := b.State(); got != "disabled" {
		t.Errorf("state = %q, want disabled", got)
	}
	if !b.Healthy() {
		t.Error("a disabled breaker should report healthy")
	}
}

func TestFailureRatioTrips(t *testing.T) {
	cfg := testConfig()
	cfg.ConsecutiveFailures = 1000 // disable the consecutive rule
	cfg.MinRequests = 10
	cfg.FailureRatio = 0.5
	b := New("test", cfg, testLogger())

	// Alternate success/failure: never 3 in a row, but a 50% failure rate.
	// The loop runs past MinRequests because the trip condition is only
	// evaluated on a failure — a success can never push a breaker open, so the
	// threshold is reached on the first failing request after MinRequests.
	for i := 0; i < 12; i++ {
		done, err := b.Allow()
		if err != nil {
			break
		}
		if i%2 == 0 {
			done(upstreamFailure())
		} else {
			done(nil)
		}
	}

	if got := b.State(); got != "open" {
		t.Fatalf("state = %q, want open: a 50%% failure rate should trip the ratio rule "+
			"even though failures never occur consecutively", got)
	}
}

func TestRejectionUnwrapsToSentinel(t *testing.T) {
	b := New("test", testConfig(), testLogger())
	for i := 0; i < 3; i++ {
		done, _ := b.Allow()
		done(upstreamFailure())
	}

	_, err := b.Allow()
	if err == nil {
		t.Fatal("expected rejection")
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("rejection should not unwrap to an unrelated sentinel")
	}
	if !provider.IsRetryable(err) {
		t.Error("breaker rejection must be retryable")
	}
	if provider.CountsAsFailure(err) {
		t.Error("breaker rejection must not count as a failure")
	}
}
