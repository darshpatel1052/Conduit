package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/darshpatel1052/conduit/internal/analytics"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// handleAdminStats returns everything the dashboard's main view needs in one
// response.
//
// One endpoint rather than five because the panels are read together and a
// dashboard that fires five requests per refresh spends most of its latency
// budget on round trips — and, worse, can render panels from different moments
// as though they described the same one. The pieces are still individually
// available (/admin/routes, /admin/cache, /admin/analytics) for anyone with
// curl and a specific question.
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	window, err := analytics.ParseWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// The routing table and breaker states come from memory and are always
	// available, so they are reported even when analytics is switched off. A
	// dashboard that goes completely blank without ClickHouse would be useless
	// during exactly the incident you would open it for.
	payload := map[string]any{
		"window":         window.Name,
		"windows":        analytics.Windows(),
		"routes":         s.router.Snapshot(),
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
	}

	if s.querier == nil {
		payload["analytics_enabled"] = false
		writeJSON(w, http.StatusOK, payload)
		return
	}
	payload["analytics_enabled"] = true

	ctx := r.Context()
	log := telemetry.LoggerFrom(ctx)

	// Each section degrades on its own. A single failing query should cost one
	// panel, not the page.
	if overview, err := s.querier.Overview(ctx, window); err != nil {
		log.Warn("stats: overview query failed", "error", err)
		payload["overview_error"] = err.Error()
	} else {
		payload["overview"] = overview
	}

	if series, err := s.querier.TimeSeries(ctx, window); err != nil {
		log.Warn("stats: timeseries query failed", "error", err)
		payload["timeseries_error"] = err.Error()
	} else {
		payload["timeseries"] = series
	}

	if models, err := s.querier.ByModel(ctx, window); err != nil {
		log.Warn("stats: by-model query failed", "error", err)
		payload["models_error"] = err.Error()
	} else {
		payload["models"] = models
	}

	if providers, err := s.querier.ByProvider(ctx, window); err != nil {
		log.Warn("stats: by-provider query failed", "error", err)
		payload["providers_error"] = err.Error()
	} else {
		payload["providers"] = providers
	}

	writeJSON(w, http.StatusOK, payload)
}

// handleAdminStatsRecent returns the live request feed.
//
// Separate from the bundle above because it refreshes on its own cadence: the
// feed is the panel people watch during an incident, and re-running four
// aggregate queries to move a table along would be wasteful.
func (s *Server) handleAdminStatsRecent(w http.ResponseWriter, r *http.Request) {
	if s.querier == nil {
		writeJSON(w, http.StatusOK, map[string]any{"analytics_enabled": false, "requests": []any{}})
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.querier.Recent(r.Context(), limit)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "analytics_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"analytics_enabled": true, "requests": rows})
}
