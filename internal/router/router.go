// Package router turns an incoming model name into an ordered list of targets
// to attempt, and owns the live routing table.
package router

import (
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darshpatel1052/conduit/internal/breaker"
	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/provider"
)

// Entry is a live provider: its adapter and its circuit breaker, which share a
// lifetime because breaker state is only meaningful for the endpoint it
// measured.
type Entry struct {
	Name        string
	Type        string
	Provider    provider.Provider
	Breaker     *breaker.Breaker
	fingerprint string
}

// Target is one resolved candidate for a request.
type Target struct {
	Entry *Entry

	// Model is the name to send upstream, which may differ from what the
	// client asked for. This is what lets one virtual model fan out across
	// providers that each call it something else.
	Model  string
	Weight int
}

// Resolution is the routing decision for a single request.
type Resolution struct {
	// RouteModel is the matched route's pattern, which may be a glob or the
	// default route rather than the literal model requested.
	RouteModel  string
	Strategy    string
	MaxAttempts int

	// Targets are in attempt order: index 0 is the chosen target, the rest are
	// the failover queue.
	Targets []Target
}

type routeEntry struct {
	model       string
	strategy    string
	maxAttempts int
	targets     []Target
	bal         *balancer
	key         string // identity used to carry balancer state across reloads
}

// Table is an immutable snapshot of the routing configuration. Requests read a
// table without any locking; a reload swaps in a whole new one.
type Table struct {
	providers map[string]*Entry
	order     []string // provider names, config order, for stable output

	exact map[string]*routeEntry
	globs []*routeEntry
	def   *routeEntry

	routeKeys map[string]*routeEntry

	Checksum string
	LoadedAt time.Time
}

// Router holds the live table and rebuilds it on config reload.
type Router struct {
	// table is swapped atomically. Readers never lock, so a reload cannot add
	// latency to in-flight traffic, and a request that started before a reload
	// keeps using the table it began with rather than seeing a half-applied
	// config.
	table atomic.Pointer[Table]

	// rebuild is serialised: only reloads contend for it, never requests.
	rebuild sync.Mutex

	client *http.Client
	log    *slog.Logger

	// base is the un-tagged logger. Providers and breakers are built from it
	// so they can set their own "component" field; deriving them from r.log
	// would emit two "component" keys in one JSON object, which is invalid
	// enough to confuse log processors and silently lose one of the values.
	base *slog.Logger
}

// New builds a router from an initial routing table.
func New(routes *config.Routes, client *http.Client, log *slog.Logger) (*Router, error) {
	r := &Router{
		client: client,
		log:    log.With("component", "router"),
		base:   log,
	}
	if err := r.Reload(routes); err != nil {
		return nil, err
	}
	return r, nil
}

// Table returns the live routing table.
func (r *Router) Table() *Table { return r.table.Load() }

// Provider looks up a live provider entry by name, or nil if there is none.
func (t *Table) Provider(name string) *Entry {
	if t == nil {
		return nil
	}
	return t.providers[name]
}

// Reload swaps in a new routing table built from cfg.
//
// The subtle part is what is *carried over*. Providers whose configuration is
// byte-identical keep their existing adapter and, crucially, their existing
// circuit breaker. Rebuilding breakers on every reload would mean that editing
// one unrelated route resets every breaker to closed — and the gateway would
// immediately fire a wave of traffic at providers it had already established
// were down. Hot reload has to be safe to do during an incident, because that
// is exactly when someone will do it.
func (r *Router) Reload(cfg *config.Routes) error {
	r.rebuild.Lock()
	defer r.rebuild.Unlock()

	prev := r.table.Load()

	next := &Table{
		providers: make(map[string]*Entry, len(cfg.Providers)),
		order:     make([]string, 0, len(cfg.Providers)),
		exact:     make(map[string]*routeEntry, len(cfg.Routes)),
		routeKeys: make(map[string]*routeEntry, len(cfg.Routes)),
		Checksum:  cfg.Checksum(),
		LoadedAt:  time.Now().UTC(),
	}

	var reused, rebuilt int
	for _, pc := range cfg.Providers {
		fp := pc.Fingerprint()

		if prev != nil {
			if old, ok := prev.providers[pc.Name]; ok && old.fingerprint == fp {
				next.providers[pc.Name] = old
				next.order = append(next.order, pc.Name)
				reused++
				continue
			}
		}

		impl, err := provider.New(pc, r.client, r.base)
		if err != nil {
			return fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		next.providers[pc.Name] = &Entry{
			Name:        pc.Name,
			Type:        pc.Type,
			Provider:    impl,
			Breaker:     breaker.New(pc.Name, pc.Breaker, r.base),
			fingerprint: fp,
		}
		next.order = append(next.order, pc.Name)
		rebuilt++
	}

	for i := range cfg.Routes {
		rc := cfg.Routes[i]

		targets := make([]Target, 0, len(rc.Targets))
		for _, tc := range rc.Targets {
			entry, ok := next.providers[tc.Provider]
			if !ok {
				// config validation already rejects this; belt and braces.
				return fmt.Errorf("route %q references unknown provider %q", rc.Model, tc.Provider)
			}
			targets = append(targets, Target{Entry: entry, Model: tc.Model, Weight: tc.Weight})
		}

		re := &routeEntry{
			model:       rc.Model,
			strategy:    rc.Strategy,
			maxAttempts: rc.MaxAttempts,
			targets:     targets,
			key:         routeKey(rc),
		}

		// Carry the balancer across reloads when the route is unchanged, so a
		// reload does not reset every round-robin cursor back to target zero
		// and dogpile the first provider in each pool.
		if prev != nil {
			if old, ok := prev.routeKeys[re.key]; ok {
				re.bal = old.bal
			}
		}
		if re.bal == nil {
			re.bal = newBalancer(rc.Strategy, rc.Targets)
		}

		next.exact[rc.Model] = re
		next.routeKeys[re.key] = re
		if strings.ContainsAny(rc.Model, "*?[") {
			next.globs = append(next.globs, re)
		}
		if cfg.DefaultModel != "" && rc.Model == cfg.DefaultModel {
			next.def = re
		}
	}

	// Longest pattern first, so "gpt-4o-*" wins over "gpt-*" regardless of the
	// order the operator happened to write them in the file.
	sort.SliceStable(next.globs, func(i, j int) bool {
		return len(next.globs[i].model) > len(next.globs[j].model)
	})

	r.table.Store(next)
	r.log.Info("routing table applied",
		"providers", len(next.providers),
		"routes", len(next.exact),
		"providers_reused", reused,
		"providers_rebuilt", rebuilt,
		"checksum", shortSum(next.Checksum),
	)
	return nil
}

// routeKey identifies a route's behaviour for balancer-state carry-over.
func routeKey(rc config.Route) string {
	var b strings.Builder
	b.WriteString(rc.Model)
	b.WriteString("|")
	b.WriteString(rc.Strategy)
	for _, t := range rc.Targets {
		fmt.Fprintf(&b, "|%s:%s:%d", t.Provider, t.Model, t.Weight)
	}
	return b.String()
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Resolve maps a requested model onto an ordered list of targets.
//
// Matching is exact, then glob (longest first), then the default route. Exact
// always wins so a specific rule cannot be shadowed by a catch-all, and an
// unmatched model is an error rather than a silent redirect: quietly serving a
// different model than the caller asked for is a worse failure than a 404.
func (r *Router) Resolve(model string) (*Resolution, error) {
	table := r.table.Load()
	if table == nil {
		return nil, &provider.Error{
			Kind: provider.KindNoRoute, Message: "routing table is not loaded",
		}
	}

	re, ok := table.exact[model]
	if !ok {
		for _, g := range table.globs {
			if matched, err := path.Match(g.model, model); err == nil && matched {
				re = g
				ok = true
				break
			}
		}
	}
	if !ok {
		re = table.def
	}
	if re == nil {
		return nil, &provider.Error{
			Kind:      provider.KindNoRoute,
			Message:   fmt.Sprintf("no route configured for model %q", model),
			Retryable: false,
		}
	}

	ordered := re.bal.order()
	targets := make([]Target, 0, len(ordered))
	for _, idx := range ordered {
		target := re.targets[idx]
		// An omitted target model on a glob route means "pass through the
		// concrete model that matched the glob". Without this, a gpt-* route
		// sends the literal string "gpt-*" upstream, which no provider serves.
		if target.Model == "" {
			target.Model = model
		}
		targets = append(targets, target)
	}

	maxAttempts := re.maxAttempts
	if maxAttempts <= 0 || maxAttempts > len(targets) {
		maxAttempts = len(targets)
	}

	return &Resolution{
		RouteModel:  re.model,
		Strategy:    re.strategy,
		MaxAttempts: maxAttempts,
		Targets:     targets,
	}, nil
}

// Models lists the model names the gateway serves, for /v1/models.
func (r *Router) Models() []string {
	table := r.table.Load()
	if table == nil {
		return nil
	}
	out := make([]string, 0, len(table.exact))
	for m := range table.exact {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Ready reports whether the gateway can serve traffic at all: a table is
// loaded and at least one provider's breaker is not open.
//
// It is deliberately not "every provider is healthy". A gateway with three
// providers and one dead one is doing precisely its job; reporting unready
// there would make Kubernetes pull a working pod out of service.
func (r *Router) Ready() bool {
	table := r.table.Load()
	if table == nil || len(table.providers) == 0 {
		return false
	}
	for _, e := range table.providers {
		if e.Breaker.Healthy() {
			return true
		}
	}
	return false
}

// Snapshot is the admin view of the live routing state.
type Snapshot struct {
	Checksum  string             `json:"checksum"`
	LoadedAt  time.Time          `json:"loaded_at"`
	Providers []ProviderSnapshot `json:"providers"`
	Routes    []RouteSnapshot    `json:"routes"`
}

// ProviderSnapshot reports one provider and its breaker.
type ProviderSnapshot struct {
	Name    string           `json:"name"`
	Type    string           `json:"type"`
	Breaker breaker.Snapshot `json:"breaker"`
}

// RouteSnapshot reports one route's configuration.
type RouteSnapshot struct {
	Model       string           `json:"model"`
	Strategy    string           `json:"strategy"`
	MaxAttempts int              `json:"max_attempts"`
	Targets     []TargetSnapshot `json:"targets"`
}

// TargetSnapshot reports one target within a route.
type TargetSnapshot struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Weight   int    `json:"weight"`
	State    string `json:"breaker_state"`
}

// Snapshot renders the live table for the admin endpoint. Being able to see
// which breakers are open, right now, without a debugger or a log grep, is the
// difference between explaining an incident and guessing at it.
func (r *Router) Snapshot() Snapshot {
	table := r.table.Load()
	if table == nil {
		return Snapshot{}
	}

	snap := Snapshot{Checksum: table.Checksum, LoadedAt: table.LoadedAt}
	for _, name := range table.order {
		e := table.providers[name]
		snap.Providers = append(snap.Providers, ProviderSnapshot{
			Name: e.Name, Type: e.Type, Breaker: e.Breaker.Snapshot(),
		})
	}

	models := make([]string, 0, len(table.exact))
	for m := range table.exact {
		models = append(models, m)
	}
	sort.Strings(models)

	for _, m := range models {
		re := table.exact[m]
		rs := RouteSnapshot{Model: re.model, Strategy: re.strategy, MaxAttempts: re.maxAttempts}
		for _, t := range re.targets {
			rs.Targets = append(rs.Targets, TargetSnapshot{
				Provider: t.Entry.Name,
				Model:    t.Model,
				Weight:   t.Weight,
				State:    t.Entry.Breaker.State(),
			})
		}
		snap.Routes = append(snap.Routes, rs)
	}
	return snap
}
