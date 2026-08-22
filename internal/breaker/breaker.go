// Package breaker wraps a circuit breaker around each provider.
//
// The problem it solves: when a provider degrades, the damage is not that its
// requests fail — it is that they fail *slowly*. Every request sits until the
// timeout fires, holding a goroutine and a connection, and the gateway's own
// latency collapses even for the models that are perfectly healthy. Worse, all
// that doomed traffic keeps arriving at an upstream that is trying to recover.
//
// A breaker converts a slow failure into an instant one. After enough failures
// it opens, and subsequent calls are rejected in nanoseconds without touching
// the network, so the request can fail over to a healthy provider immediately.
// After a cooldown it half-opens and lets a small number of probes through: if
// they succeed it closes, if they fail it re-opens. That is the whole state
// machine — closed, open, half-open — and the half-open state is the part that
// matters, because it is how the system recovers on its own instead of waiting
// for a human to notice.
package breaker

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
)

// Breaker guards one provider.
type Breaker struct {
	name     string
	disabled bool
	cb       *gobreaker.TwoStepCircuitBreaker[any]
	log      *slog.Logger

	shortCircuited atomic.Uint64
	stateChanges   atomic.Uint64
	lastChangeUnix atomic.Int64
}

// New builds a breaker for a provider.
//
// The two-step API (Allow, then report) is used rather than Execute because a
// streaming request does not finish when the call returns — it finishes when
// the last token arrives, possibly minutes later. Two-step lets the outcome be
// reported at the real end of the request, so a stream that dies halfway
// through still counts against the provider's health. Execute would record a
// success the moment the response headers came back.
func New(name string, cfg config.BreakerConfig, log *slog.Logger) *Breaker {
	b := &Breaker{
		name:     name,
		disabled: cfg.Disabled,
		log:      log.With("provider", name, "component", "breaker"),
	}
	b.lastChangeUnix.Store(time.Now().Unix())

	if cfg.Disabled {
		return b
	}

	settings := gobreaker.Settings{
		Name: name,

		// How many probes are admitted while half-open. Deliberately small:
		// the point is to test whether the provider recovered, not to hand it
		// the full firehose the instant it shows a pulse.
		MaxRequests: cfg.HalfOpenMaxRequests,

		// Rolling window over which failures are counted while closed. Without
		// it, counts accumulate forever and a provider that failed five times
		// last Tuesday would trip on one failure today.
		Interval: cfg.Interval.D(),

		// How long to stay open before probing (open -> half-open).
		Timeout: cfg.Cooldown.D(),

		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Two independent trip conditions, because each catches a failure
			// mode the other misses.
			//
			// Consecutive failures catch a hard outage fast: five in a row and
			// the provider is plainly down, whatever the overall rate says.
			if cfg.ConsecutiveFailures > 0 && counts.ConsecutiveFailures >= cfg.ConsecutiveFailures {
				return true
			}
			// Failure ratio catches partial degradation: a provider failing
			// 60% of requests never strings together five failures under mixed
			// traffic, but it is unquestionably sick. MinRequests keeps a
			// single unlucky failure on a quiet route from tripping on a 100%
			// failure rate over a sample size of one.
			if cfg.MinRequests > 0 && counts.Requests >= cfg.MinRequests {
				ratio := float64(counts.TotalFailures) / float64(counts.Requests)
				if ratio >= cfg.FailureRatio {
					return true
				}
			}
			return false
		},

		// IsExcluded keeps client-caused errors out of the health calculation
		// entirely. A caller sending malformed JSON in a loop must not be able
		// to trip a breaker on a provider that is answering everything
		// correctly — otherwise one bad client takes an upstream away from
		// every other tenant.
		IsExcluded: func(err error) bool {
			return err != nil && !provider.CountsAsFailure(err)
		},

		OnStateChange: func(name string, from, to gobreaker.State) {
			b.stateChanges.Add(1)
			b.lastChangeUnix.Store(time.Now().Unix())

			level := slog.LevelWarn
			if to == gobreaker.StateClosed {
				level = slog.LevelInfo
			}
			b.log.Log(context.Background(), level, "circuit breaker state change",
				"from", from.String(), "to", to.String())
		},
	}

	b.cb = gobreaker.NewTwoStepCircuitBreaker[any](settings)
	return b
}

// Allow reports whether a request may proceed.
//
// On success it returns a done callback that MUST be invoked exactly once with
// the request's outcome (nil for success). Failing to call it leaks a half-open
// slot and can wedge the breaker in half-open forever.
// The return values are deliberately unnamed. With a named `done` return, the
// closure below would capture the return *variable* rather than gobreaker's
// callback — and `return func(...)...` would then reassign that same variable,
// so the returned closure would end up calling itself and silently reporting
// nothing to the breaker. Counts would sit at zero forever and the breaker
// would never trip.
func (b *Breaker) Allow() (func(err error), error) {
	if b.disabled || b.cb == nil {
		return func(error) {}, nil
	}

	report, err := b.cb.Allow()
	if err != nil {
		b.shortCircuited.Add(1)
		return nil, b.rejection(err)
	}

	// Guard against a double report, which would corrupt the counts.
	var once atomic.Bool
	return func(reqErr error) {
		if once.CompareAndSwap(false, true) {
			report(reqErr)
		}
	}, nil
}

// rejection converts gobreaker's sentinel errors into Conduit's vocabulary.
//
// A rejection is retryable — the whole point is to fail over to another target
// immediately — but it does not count as a failure anywhere, because no request
// was actually made.
func (b *Breaker) rejection(err error) error {
	msg := "circuit breaker is open for provider " + b.name
	if errors.Is(err, gobreaker.ErrTooManyRequests) {
		msg = "circuit breaker is half-open and probe slots are full for provider " + b.name
	}
	return &provider.Error{
		Provider:        b.name,
		Kind:            provider.KindCircuitOpen,
		Message:         msg,
		Retryable:       true,
		CountsAsFailure: false,
		Err:             err,
	}
}

// State returns the current breaker state as a string.
func (b *Breaker) State() string {
	if b.disabled || b.cb == nil {
		return "disabled"
	}
	return b.cb.State().String()
}

// Healthy reports whether the breaker is currently admitting traffic. Used for
// readiness, not for routing: routing must go through Allow so half-open probe
// accounting stays correct.
func (b *Breaker) Healthy() bool {
	if b.disabled || b.cb == nil {
		return true
	}
	return b.cb.State() != gobreaker.StateOpen
}

// Snapshot is a point-in-time view of a breaker, for the admin endpoint and
// later for metrics.
type Snapshot struct {
	Provider             string    `json:"provider"`
	State                string    `json:"state"`
	Requests             uint32    `json:"requests"`
	TotalSuccesses       uint32    `json:"total_successes"`
	TotalFailures        uint32    `json:"total_failures"`
	ConsecutiveSuccesses uint32    `json:"consecutive_successes"`
	ConsecutiveFailures  uint32    `json:"consecutive_failures"`
	ShortCircuited       uint64    `json:"short_circuited"`
	StateChanges         uint64    `json:"state_changes"`
	LastStateChange      time.Time `json:"last_state_change"`
}

// Snapshot captures the breaker's current counters.
func (b *Breaker) Snapshot() Snapshot {
	s := Snapshot{
		Provider:        b.name,
		State:           b.State(),
		ShortCircuited:  b.shortCircuited.Load(),
		StateChanges:    b.stateChanges.Load(),
		LastStateChange: time.Unix(b.lastChangeUnix.Load(), 0).UTC(),
	}
	if b.cb != nil {
		c := b.cb.Counts()
		s.Requests = c.Requests
		s.TotalSuccesses = c.TotalSuccesses
		s.TotalFailures = c.TotalFailures
		s.ConsecutiveSuccesses = c.ConsecutiveSuccesses
		s.ConsecutiveFailures = c.ConsecutiveFailures
	}
	return s
}
