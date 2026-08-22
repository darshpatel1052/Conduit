package analytics

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// finite replaces NaN and ±Inf with zero.
//
// ClickHouse returns NaN from a quantile over zero matching rows, which is a
// routine occurrence here: ask for time-to-first-byte in a window with no
// streaming requests and every percentile comes back NaN. JSON has no way to
// represent it, so encoding/json fails on the whole document rather than the
// one field — one empty quantile would blank the entire dashboard. Zero is the
// honest answer for "no requests matched", and the request count sitting beside
// it says so explicitly.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// Querier runs the read-only aggregates the dashboard displays.
//
// These live here rather than in the dashboard for two reasons. The schema has
// exactly one owner, so a column rename breaks a compile rather than a chart at
// runtime; and the dashboard never holds a database credential — it talks to
// the gateway's admin API, which is the same surface an operator can curl. A
// browser-facing service with direct ClickHouse access is a browser-facing
// service that can issue any query it likes.
type Querier struct {
	conn driver.Conn
}

// NewQuerier reuses the recorder's connection: the pool, the credentials and
// the health of the link are already established, and a second pool would only
// add a second thing that can be misconfigured.
func NewQuerier(r *ClickHouseRecorder) *Querier {
	if r == nil {
		return nil
	}
	return &Querier{conn: r.conn}
}

// Window is a validated lookback period.
//
// The values are an allow-list rather than a parsed duration on purpose. This
// endpoint is reachable by a browser, and the window ends up in a SQL
// expression; accepting arbitrary input here is how a dashboard filter becomes
// a query-of-death that table-scans ninety days of history. A fixed set also
// keeps the bucket count bounded, so no chart can ask for a million points.
type Window struct {
	Name     string
	Duration time.Duration
	Bucket   time.Duration
}

var windows = map[string]Window{
	"15m": {Name: "15m", Duration: 15 * time.Minute, Bucket: 30 * time.Second},
	"1h":  {Name: "1h", Duration: time.Hour, Bucket: 2 * time.Minute},
	"6h":  {Name: "6h", Duration: 6 * time.Hour, Bucket: 10 * time.Minute},
	"24h": {Name: "24h", Duration: 24 * time.Hour, Bucket: 30 * time.Minute},
	"7d":  {Name: "7d", Duration: 7 * 24 * time.Hour, Bucket: 3 * time.Hour},
}

// ParseWindow resolves a caller-supplied window name, defaulting to one hour.
func ParseWindow(s string) (Window, error) {
	if s == "" {
		return windows["1h"], nil
	}
	w, ok := windows[s]
	if !ok {
		return Window{}, fmt.Errorf("unknown window %q: use one of 15m, 1h, 6h, 24h, 7d", s)
	}
	return w, nil
}

// Overview is the headline row of the dashboard.
type Overview struct {
	Window       string  `json:"window"`
	Requests     uint64  `json:"requests"`
	Errors       uint64  `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	RateLimited  uint64  `json:"rate_limited"`
	RequestsPerS float64 `json:"requests_per_second"`

	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	TTFBMS float64 `json:"ttfb_p95_ms"`

	PromptTokens     uint64  `json:"prompt_tokens"`
	CompletionTokens uint64  `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`

	CacheHits    uint64  `json:"cache_hits"`
	CacheLookups uint64  `json:"cache_lookups"`
	CacheHitRate float64 `json:"cache_hit_rate"`

	// CostSavedUSD is what the cache hits would have cost had they been
	// generated, priced at the same rate as the misses for that model. It is an
	// estimate by construction — the tokens were never produced — so it is
	// reported separately from CostUSD rather than folded into it.
	CostSavedUSD float64 `json:"cost_saved_usd"`

	Failovers uint64 `json:"failovers"`
}

// ProjectOverview is the project-scoped summary used by the admin dashboard.
// It intentionally contains only metadata aggregates; prompts and responses
// never enter the analytical store.
func (q *Querier) ProjectOverview(ctx context.Context, projectID string, w Window) (*Overview, error) {
	const query = `SELECT count(),countIf(status>=400),countIf(status=429),quantileIf(0.50)(duration_ms,status<400),
		quantileIf(0.95)(duration_ms,status<400),quantileIf(0.99)(duration_ms,status<400),
		quantileIf(0.95)(ttfb_ms,stream=1 AND ttfb_ms>0),sum(prompt_tokens),sum(completion_tokens),sum(cost_usd),
		countIf(cache_status IN ('hit_exact','hit_semantic')),countIf(cache_status IN ('hit_exact','hit_semantic','miss')),countIf(failover=1)
		FROM conduit_requests WHERE project_id=toUUID(?) AND timestamp>=?`
	var o Overview
	if err := q.conn.QueryRow(ctx, query, projectID, time.Now().Add(-w.Duration)).Scan(&o.Requests, &o.Errors, &o.RateLimited, &o.P50MS, &o.P95MS, &o.P99MS, &o.TTFBMS, &o.PromptTokens, &o.CompletionTokens, &o.CostUSD, &o.CacheHits, &o.CacheLookups, &o.Failovers); err != nil {
		return nil, fmt.Errorf("project overview query: %w", err)
	}
	o.Window = w.Name
	o.P50MS, o.P95MS, o.P99MS = finite(o.P50MS), finite(o.P95MS), finite(o.P99MS)
	o.TTFBMS, o.CostUSD = finite(o.TTFBMS), finite(o.CostUSD)
	if o.Requests > 0 {
		o.ErrorRate = float64(o.Errors) / float64(o.Requests)
		o.RequestsPerS = float64(o.Requests) / w.Duration.Seconds()
	}
	if o.CacheLookups > 0 {
		o.CacheHitRate = float64(o.CacheHits) / float64(o.CacheLookups)
	}
	return &o, nil
}

type ProjectRequest struct {
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"request_id"`
	APIKeyID         string    `json:"api_key_id"`
	Environment      string    `json:"environment"`
	EndUserPseudonym string    `json:"end_user_pseudonym"`
	Operation        string    `json:"operation"`
	Profile          string    `json:"profile"`
	UpstreamModel    string    `json:"upstream_model"`
	Provider         string    `json:"provider"`
	Status           uint16    `json:"status"`
	Stream           bool      `json:"stream"`
	Attempts         uint8     `json:"attempts"`
	DurationMS       uint32    `json:"duration_ms"`
	TTFBMS           uint32    `json:"ttfb_ms"`
	PromptTokens     uint32    `json:"prompt_tokens"`
	CompletionTokens uint32    `json:"completion_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	CacheStatus      string    `json:"cache_status"`
	Failover         bool      `json:"failover"`
	ErrorKind        string    `json:"error_kind"`
}

// ProjectRecent returns metadata-only request rows within one project. A raw
// end-user identifier never reaches this query; the control API converts it to
// the same project-scoped HMAC used on the inference path.
func (q *Querier) ProjectRecent(ctx context.Context, projectID, endUserPseudonym string, limit int) ([]ProjectRequest, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const query = `SELECT timestamp,request_id,api_key_id,environment,end_user,operation,route,upstream_model,provider,
		status,stream,attempts,duration_ms,ttfb_ms,prompt_tokens,completion_tokens,cost_usd,cache_status,failover,error_kind
		FROM conduit_requests WHERE project_id=toUUID(?) AND (?='' OR end_user=?) ORDER BY timestamp DESC LIMIT ?`
	rows, err := q.conn.Query(ctx, query, projectID, endUserPseudonym, endUserPseudonym, uint64(limit))
	if err != nil {
		return nil, fmt.Errorf("project recent requests query: %w", err)
	}
	defer rows.Close()
	out := make([]ProjectRequest, 0, limit)
	for rows.Next() {
		var row ProjectRequest
		var stream, failover uint8
		if err := rows.Scan(&row.Timestamp, &row.RequestID, &row.APIKeyID, &row.Environment, &row.EndUserPseudonym, &row.Operation,
			&row.Profile, &row.UpstreamModel, &row.Provider, &row.Status, &stream, &row.Attempts, &row.DurationMS,
			&row.TTFBMS, &row.PromptTokens, &row.CompletionTokens, &row.CostUSD, &row.CacheStatus, &failover, &row.ErrorKind); err != nil {
			return nil, fmt.Errorf("scan project recent request: %w", err)
		}
		row.Stream = stream == 1
		row.Failover = failover == 1
		row.CostUSD = finite(row.CostUSD)
		out = append(out, row)
	}
	return out, rows.Err()
}

// Overview aggregates the whole window into a single row.
func (q *Querier) Overview(ctx context.Context, w Window) (*Overview, error) {
	since := time.Now().Add(-w.Duration)

	const query = `
SELECT
    count()                                                    AS requests,
    countIf(status >= 400)                                     AS errors,
    countIf(status = 429)                                      AS rate_limited,
    -- Latency is measured over served requests only. A flood of instant 429s
    -- would otherwise drag the percentiles down and make an overloaded
    -- gateway look faster than a healthy one.
    quantileIf(0.50)(duration_ms, status < 400)                 AS p50,
    quantileIf(0.95)(duration_ms, status < 400)                 AS p95,
    quantileIf(0.99)(duration_ms, status < 400)                 AS p99,
    quantileIf(0.95)(ttfb_ms, stream = 1 AND ttfb_ms > 0)       AS ttfb_p95,
    sum(prompt_tokens)                                          AS prompt_tokens,
    sum(completion_tokens)                                      AS completion_tokens,
    sum(cost_usd)                                               AS cost_usd,
    countIf(cache_status IN ('hit_exact', 'hit_semantic'))       AS cache_hits,
    countIf(cache_status IN ('hit_exact', 'hit_semantic', 'miss')) AS cache_lookups,
    countIf(failover = 1)                                       AS failovers
FROM conduit_requests
WHERE timestamp >= ?`

	row := q.conn.QueryRow(ctx, query, since)

	var o Overview
	if err := row.Scan(
		&o.Requests, &o.Errors, &o.RateLimited,
		&o.P50MS, &o.P95MS, &o.P99MS, &o.TTFBMS,
		&o.PromptTokens, &o.CompletionTokens, &o.CostUSD,
		&o.CacheHits, &o.CacheLookups, &o.Failovers,
	); err != nil {
		return nil, fmt.Errorf("overview query: %w", err)
	}

	o.Window = w.Name
	o.P50MS, o.P95MS, o.P99MS = finite(o.P50MS), finite(o.P95MS), finite(o.P99MS)
	o.TTFBMS = finite(o.TTFBMS)
	o.CostUSD = finite(o.CostUSD)

	if o.Requests > 0 {
		o.ErrorRate = float64(o.Errors) / float64(o.Requests)
		o.RequestsPerS = float64(o.Requests) / w.Duration.Seconds()
	}
	if o.CacheLookups > 0 {
		o.CacheHitRate = float64(o.CacheHits) / float64(o.CacheLookups)
	}

	saved, err := q.cacheSavings(ctx, since)
	if err != nil {
		return nil, err
	}
	o.CostSavedUSD = saved

	return &o, nil
}

// cacheSavings prices the tokens the cache served but did not generate.
//
// Cache hits are recorded with the token counts of the stored response and a
// cost of zero (nothing was spent). To value them, each hit is charged at the
// average per-token cost that the same model's *misses* incurred in the same
// window — so a model nobody paid for shows no savings, which is correct.
func (q *Querier) cacheSavings(ctx context.Context, since time.Time) (float64, error) {
	const query = `
SELECT sum(hit_tokens * rate) AS saved
FROM (
    SELECT
        upstream_model,
        sumIf(prompt_tokens + completion_tokens,
              cache_status IN ('hit_exact', 'hit_semantic'))  AS hit_tokens,
        -- Guard the divisor: a window with hits but no priced misses has no
        -- basis for a rate, and 0/0 would poison the sum with NaN.
        if(sumIf(prompt_tokens + completion_tokens, cache_status NOT IN ('hit_exact','hit_semantic')) > 0,
           sumIf(cost_usd, cache_status NOT IN ('hit_exact','hit_semantic'))
             / sumIf(prompt_tokens + completion_tokens, cache_status NOT IN ('hit_exact','hit_semantic')),
           0)                                                  AS rate
    FROM conduit_requests
    WHERE timestamp >= ?
    GROUP BY upstream_model
)`

	var saved float64
	if err := q.conn.QueryRow(ctx, query, since).Scan(&saved); err != nil {
		return 0, fmt.Errorf("cache savings query: %w", err)
	}
	return finite(saved), nil
}

// Point is one bucket of the time series.
type Point struct {
	Time      time.Time `json:"time"`
	Requests  uint64    `json:"requests"`
	Errors    uint64    `json:"errors"`
	CacheHits uint64    `json:"cache_hits"`
	P95MS     float64   `json:"p95_ms"`
	Tokens    uint64    `json:"tokens"`
	CostUSD   float64   `json:"cost_usd"`
}

// TimeSeries returns evenly spaced buckets across the window.
func (q *Querier) TimeSeries(ctx context.Context, w Window) ([]Point, error) {
	since := time.Now().Add(-w.Duration)

	// WITH FILL emits empty buckets rather than skipping them. Without it a gap
	// in traffic renders as a straight line between the surrounding points,
	// which reads as steady traffic — the exact opposite of an outage.
	//
	// FROM and TO are given explicitly because a bare WITH FILL only fills
	// *between* the rows it found. An hour in which all the traffic landed in
	// one bucket would come back as a single point, and the chart would rescale
	// its axis to that one bucket — so every window looks equally busy and the
	// range picker appears to do nothing. Bounding the fill to the requested
	// window makes the axis mean what the picker says.
	const query = `
SELECT
    toStartOfInterval(timestamp, toIntervalSecond(?)) AS bucket,
    count()                                            AS requests,
    countIf(status >= 400)                             AS errors,
    countIf(cache_status IN ('hit_exact', 'hit_semantic')) AS cache_hits,
    quantileIf(0.95)(duration_ms, status < 400)         AS p95,
    sum(prompt_tokens + completion_tokens)              AS tokens,
    sum(cost_usd)                                       AS cost_usd
FROM conduit_requests
WHERE timestamp >= ?
GROUP BY bucket
ORDER BY bucket
    WITH FILL FROM toDateTime(?) TO now() STEP toIntervalSecond(?)`

	bucketSecs := int64(w.Bucket.Seconds())

	// Aligned in Go rather than in SQL so the fill's first bucket lands on the
	// same boundary toStartOfInterval produces; an unaligned FROM would offset
	// every filled bucket from every real one and double the series.
	//
	// Passed as Unix seconds through toDateTime, not as a time.Time: the driver
	// sends a time.Time as DateTime64, while toStartOfInterval with a second
	// interval yields plain DateTime, and ClickHouse rejects a WITH FILL whose
	// bounds do not match the column's type exactly.
	alignedSince := since.Truncate(w.Bucket).Unix()

	rows, err := q.conn.Query(ctx, query, bucketSecs, since, alignedSince, bucketSecs)
	if err != nil {
		return nil, fmt.Errorf("timeseries query: %w", err)
	}
	defer rows.Close()

	points := make([]Point, 0, 64)
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Time, &p.Requests, &p.Errors, &p.CacheHits,
			&p.P95MS, &p.Tokens, &p.CostUSD); err != nil {
			return nil, fmt.Errorf("scan timeseries: %w", err)
		}
		// Buckets emitted by WITH FILL have no rows at all, so their quantile
		// is NaN by construction. Empty buckets are the point of WITH FILL.
		p.P95MS, p.CostUSD = finite(p.P95MS), finite(p.CostUSD)
		points = append(points, p)
	}
	return points, rows.Err()
}

// ModelStat is per-model usage.
type ModelStat struct {
	Model            string  `json:"model"`
	Requests         uint64  `json:"requests"`
	Errors           uint64  `json:"errors"`
	P95MS            float64 `json:"p95_ms"`
	PromptTokens     uint64  `json:"prompt_tokens"`
	CompletionTokens uint64  `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
}

// ByModel breaks the window down by requested model.
func (q *Querier) ByModel(ctx context.Context, w Window) ([]ModelStat, error) {
	const query = `
SELECT
    requested_model,
    count()                                     AS requests,
    countIf(status >= 400)                      AS errors,
    quantileIf(0.95)(duration_ms, status < 400)  AS p95,
    sum(prompt_tokens)                           AS prompt_tokens,
    sum(completion_tokens)                       AS completion_tokens,
    sum(cost_usd)                                AS cost_usd,
    if(countIf(cache_status IN ('hit_exact','hit_semantic','miss')) > 0,
       countIf(cache_status IN ('hit_exact','hit_semantic'))
         / countIf(cache_status IN ('hit_exact','hit_semantic','miss')),
       0)                                        AS cache_hit_rate
FROM conduit_requests
WHERE timestamp >= ? AND requested_model != ''
GROUP BY requested_model
ORDER BY requests DESC
LIMIT 50`

	rows, err := q.conn.Query(ctx, query, time.Now().Add(-w.Duration))
	if err != nil {
		return nil, fmt.Errorf("by-model query: %w", err)
	}
	defer rows.Close()

	out := make([]ModelStat, 0, 16)
	for rows.Next() {
		var m ModelStat
		if err := rows.Scan(&m.Model, &m.Requests, &m.Errors, &m.P95MS,
			&m.PromptTokens, &m.CompletionTokens, &m.CostUSD, &m.CacheHitRate); err != nil {
			return nil, fmt.Errorf("scan by-model: %w", err)
		}
		m.P95MS, m.CostUSD, m.CacheHitRate = finite(m.P95MS), finite(m.CostUSD), finite(m.CacheHitRate)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProviderStat is per-provider traffic and reliability.
type ProviderStat struct {
	Provider  string  `json:"provider"`
	Requests  uint64  `json:"requests"`
	Errors    uint64  `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	P95MS     float64 `json:"p95_ms"`
	CostUSD   float64 `json:"cost_usd"`
	Failovers uint64  `json:"failovers"`
}

// ByProvider breaks the window down by the provider that served each request.
func (q *Querier) ByProvider(ctx context.Context, w Window) ([]ProviderStat, error) {
	const query = `
SELECT
    provider,
    count()                                     AS requests,
    countIf(status >= 400)                      AS errors,
    quantileIf(0.95)(duration_ms, status < 400)  AS p95,
    sum(cost_usd)                                AS cost_usd,
    countIf(failover = 1)                        AS failovers
FROM conduit_requests
WHERE timestamp >= ?
GROUP BY provider
ORDER BY requests DESC
LIMIT 50`

	rows, err := q.conn.Query(ctx, query, time.Now().Add(-w.Duration))
	if err != nil {
		return nil, fmt.Errorf("by-provider query: %w", err)
	}
	defer rows.Close()

	out := make([]ProviderStat, 0, 8)
	for rows.Next() {
		var p ProviderStat
		if err := rows.Scan(&p.Provider, &p.Requests, &p.Errors, &p.P95MS,
			&p.CostUSD, &p.Failovers); err != nil {
			return nil, fmt.Errorf("scan by-provider: %w", err)
		}
		p.P95MS, p.CostUSD = finite(p.P95MS), finite(p.CostUSD)
		if p.Requests > 0 {
			p.ErrorRate = float64(p.Errors) / float64(p.Requests)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecentRequest is one row of the live request feed.
type RecentRequest struct {
	Timestamp      time.Time `json:"timestamp"`
	RequestID      string    `json:"request_id"`
	APIKeyID       string    `json:"api_key_id"`
	Route          string    `json:"route"`
	RequestedModel string    `json:"requested_model"`
	Provider       string    `json:"provider"`
	Status         uint16    `json:"status"`
	Stream         bool      `json:"stream"`
	DurationMS     uint32    `json:"duration_ms"`
	TTFBMS         uint32    `json:"ttfb_ms"`

	// uint64, not uint32: ClickHouse widens the sum of two UInt32 columns to
	// UInt64, and scanning that into a uint32 is an error rather than a
	// truncation — the whole feed fails, not one column.
	Tokens       uint64  `json:"tokens"`
	CostUSD      float64 `json:"cost_usd"`
	CacheStatus  string  `json:"cache_status"`
	Failover     bool    `json:"failover"`
	ErrorKind    string  `json:"error_kind"`
	ErrorMessage string  `json:"error_message"`
}

// Recent returns the most recent requests, newest first.
func (q *Querier) Recent(ctx context.Context, limit int) ([]RecentRequest, error) {
	// Bounded regardless of what the caller asks for: this is the one endpoint
	// that returns rows rather than aggregates, so it is the one that can be
	// turned into a full table scan by a large enough number.
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	const query = `
SELECT timestamp, request_id, api_key_id, route, requested_model, provider,
       status, stream, duration_ms, ttfb_ms,
       prompt_tokens + completion_tokens AS tokens,
       cost_usd, cache_status, failover, error_kind, error_message
FROM conduit_requests
ORDER BY timestamp DESC
LIMIT ?`

	rows, err := q.conn.Query(ctx, query, uint64(limit))
	if err != nil {
		return nil, fmt.Errorf("recent query: %w", err)
	}
	defer rows.Close()

	out := make([]RecentRequest, 0, limit)
	for rows.Next() {
		var (
			r RecentRequest
			// ClickHouse has no Bool here — the columns are UInt8, so they are
			// scanned as such and converted once.
			streamFlag, failoverFlag uint8
		)
		if err := rows.Scan(&r.Timestamp, &r.RequestID, &r.APIKeyID, &r.Route,
			&r.RequestedModel, &r.Provider, &r.Status, &streamFlag, &r.DurationMS,
			&r.TTFBMS, &r.Tokens, &r.CostUSD, &r.CacheStatus, &failoverFlag,
			&r.ErrorKind, &r.ErrorMessage); err != nil {
			return nil, fmt.Errorf("scan recent: %w", err)
		}
		r.Stream = streamFlag == 1
		r.Failover = failoverFlag == 1
		r.CostUSD = finite(r.CostUSD)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Windows lists the selectable windows, for the dashboard's picker.
func Windows() []string { return []string{"15m", "1h", "6h", "24h", "7d"} }
