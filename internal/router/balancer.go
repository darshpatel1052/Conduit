package router

import (
	"sync"
	"sync/atomic"

	"github.com/darshpatel1052/conduit/internal/config"
)

// balancer decides the order in which a route's targets are attempted.
//
// It returns an ordering rather than a single pick, because in a gateway
// balancing and failover are the same decision viewed twice: the first element
// is where the request goes, and the rest is the queue it falls through if that
// target is broken or its breaker is open.
type balancer struct {
	strategy string
	n        int

	// rr is the round-robin cursor. atomic rather than mutex-guarded because
	// this is the hottest shared state in the gateway — every request touches
	// it — and a single atomic add is far cheaper than lock/unlock. Exact
	// fairness under races is not worth a contended mutex here; being off by
	// one request in a million does not matter.
	rr atomic.Uint64

	// Smooth weighted round-robin needs to read-modify-write a whole slice
	// atomically, which an atomic cannot express, so this one takes a lock.
	mu      sync.Mutex
	weights []int
	current []int
	total   int
}

func newBalancer(strategy string, targets []config.Target) *balancer {
	b := &balancer{strategy: strategy, n: len(targets)}
	if strategy == config.StrategyWeighted {
		b.weights = make([]int, len(targets))
		b.current = make([]int, len(targets))
		for i, t := range targets {
			w := t.Weight
			if w <= 0 {
				w = 1
			}
			b.weights[i] = w
			b.total += w
		}
	}
	return b
}

// order returns target indices in the order they should be attempted.
func (b *balancer) order() []int {
	switch b.strategy {
	case config.StrategyRoundRobin:
		return b.rotate(int(b.rr.Add(1)-1) % b.n)
	case config.StrategyWeighted:
		return b.rotate(b.smoothWeightedPick())
	default:
		// Priority: configured order is the preference order. Used when the
		// list means "cheap local model, then paid API" rather than "these are
		// interchangeable".
		return b.rotate(0)
	}
}

// rotate produces the full attempt order starting at start.
func (b *balancer) rotate(start int) []int {
	if b.n == 0 {
		return nil
	}
	if start < 0 {
		start = 0
	}
	out := make([]int, 0, b.n)
	for i := 0; i < b.n; i++ {
		out = append(out, (start+i)%b.n)
	}
	return out
}

// smoothWeightedPick implements nginx's smooth weighted round-robin.
//
// The naive alternative — expand weights into a list and cycle it — sends
// weights {5,1} as AAAAAB: five requests hammer one target, then one goes to
// the other. Smooth WRR yields ABAAAA instead: the same 5:1 ratio, but spread
// evenly over time, which is what actually keeps load off any single upstream.
//
// Each pick adds every target's weight to its running counter, takes the
// target with the highest counter, and subtracts the total weight from the
// winner. Counters stay bounded and the long-run distribution matches the
// weights exactly.
func (b *balancer) smoothWeightedPick() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	best := 0
	for i := range b.current {
		b.current[i] += b.weights[i]
		if b.current[i] > b.current[best] {
			best = i
		}
	}
	b.current[best] -= b.total
	return best
}
