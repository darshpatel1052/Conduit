// Package cache implements Conduit's semantic response cache.
//
// A conventional cache is keyed by exact bytes, which is nearly useless for
// prompts: "What is the capital of France?" and "what's the capital city of
// France?" are the same question and would miss every time. This cache keys by
// *meaning* — it embeds the prompt and looks for a stored entry whose vector is
// close enough — so paraphrases hit.
//
// That power is also the danger, and most of the design here is about bounding
// it. A similarity threshold set too low serves the answer to a merely-similar
// question, which is a correctness bug that looks like a hallucination. See
// Cacheable and the threshold discussion below.
package cache

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/darshpatel1052/conduit/internal/config"
	"github.com/darshpatel1052/conduit/internal/embed"
	"github.com/darshpatel1052/conduit/internal/provider"
)

//go:embed schema.sql
var schemaSQL string

// Entry is a cached completion.
type Entry struct {
	ID               int64
	Prompt           string
	Response         json.RawMessage
	PromptTokens     int
	CompletionTokens int
	Similarity       float64
	Exact            bool
	Age              time.Duration
	HitCount         int
}

// Cache is the semantic cache.
type Cache struct {
	pool     *pgxpool.Pool
	embedder embed.Embedder
	cfg      config.Cache
	log      *slog.Logger
}

// New connects to Postgres and applies the schema.
func New(ctx context.Context, cfg config.Cache, embedder embed.Embedder, log *slog.Logger) (*Cache, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres_dsn: %w", err)
	}
	poolCfg.MaxConns = int32(cfg.MaxConns)
	poolCfg.MinConns = 2
	poolCfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	c := &Cache{
		pool:     pool,
		embedder: embedder,
		cfg:      cfg,
		log:      log.With("component", "cache"),
	}

	if err := c.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	c.log.Info("semantic cache ready",
		"embed_model", embedder.Model(),
		"dimensions", embedder.Dimensions(),
		"threshold", cfg.SimilarityThreshold,
		"ttl", cfg.TTL.String(),
		"max_temperature", cfg.MaxTemperature)
	return c, nil
}

func (c *Cache) migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	stmt := fmt.Sprintf(schemaSQL, c.embedder.Dimensions())
	if _, err := c.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("apply cache schema: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (c *Cache) Close() { c.pool.Close() }

// Ping reports database reachability, for readiness.
func (c *Cache) Ping(ctx context.Context) error { return c.pool.Ping(ctx) }

// Cacheable decides whether a request may be served from, or written to, cache.
//
// This gate is where most of the judgement lives, because an LLM is not a pure
// function. At temperature > 0 the model is *supposed* to produce a different
// answer each time — variety is the feature being paid for — so caching it
// returns a stale sample and quietly removes the randomness the caller asked
// for. Caching is therefore restricted to low-temperature, deterministic-intent
// traffic: factual lookups, classification, extraction. That is also where the
// hit rate is highest, so the restriction costs far less than it looks.
//
// The other exclusions are correctness, not taste:
//
//   - tools: the right response depends on tool state the cache cannot see.
//   - n > 1: the caller explicitly asked for multiple distinct samples.
//   - seed: the caller is pinning determinism themselves and expects the
//     provider's exact behaviour, not ours.
func Cacheable(req *provider.ChatRequest, maxTemperature float64) (bool, string) {
	if req.Temperature != nil && *req.Temperature > maxTemperature {
		return false, fmt.Sprintf("temperature %.2f exceeds the cacheable maximum of %.2f",
			*req.Temperature, maxTemperature)
	}
	if req.N != nil && *req.N > 1 {
		return false, "n > 1 requests multiple distinct samples"
	}
	if len(req.Tools) > 0 {
		return false, "tool calls depend on state the cache cannot observe"
	}
	if req.Seed != nil {
		return false, "seed pins determinism at the provider"
	}
	if len(req.Messages) == 0 {
		return false, "no messages to key on"
	}
	return true, ""
}

// PromptKey builds the exact-match fingerprint for a request. Model selection
// is scoped separately by routeModel, while stream controls transport rather
// than the generated answer. Every other field is retained, including fields
// Conduit does not understand, so different generation settings cannot collide.
func PromptKey(req *provider.ChatRequest) string {
	body := requestFingerprintBody(req, true)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// OptionsKey isolates the non-prompt part of the request. Semantic lookup may
// compare different messages, but only when their generation settings match.
func OptionsKey(req *provider.ChatRequest) string {
	body := requestFingerprintBody(req, false)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func requestFingerprintBody(req *provider.ChatRequest, includeMessages bool) []byte {
	fields := make(map[string]json.RawMessage)
	if len(req.Raw) > 0 {
		_ = json.Unmarshal(req.Raw, &fields)
	}
	if len(fields) == 0 {
		body, _ := json.Marshal(req)
		_ = json.Unmarshal(body, &fields)
	}
	delete(fields, "model")
	delete(fields, "stream")
	delete(fields, "stream_options")
	if !includeMessages {
		delete(fields, "messages")
	}
	body, _ := json.Marshal(fields)
	return body
}

// Lookup searches for a usable cached response.
//
// Returns (nil, nil) on a miss. A cache error is never fatal to the request:
// the caller logs it and goes to the provider, because a broken cache should
// cost latency, not availability.
func (c *Cache) Lookup(ctx context.Context, namespace string, req *provider.ChatRequest, routeModel string) (*Entry, error) {
	return c.LookupWithPolicy(ctx, namespace, req, routeModel, c.cfg.SemanticEnabled, c.cfg.SimilarityThreshold)
}

func (c *Cache) LookupWithPolicy(ctx context.Context, namespace string, req *provider.ChatRequest, routeModel string, semantic bool, threshold float64) (*Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout.D())
	defer cancel()

	hash := PromptKey(req)

	// Exact-match first. It skips the embedding round trip entirely, which is
	// the single biggest latency win available here — an identical repeat costs
	// one indexed lookup instead of an embedding call plus a vector search.
	if entry, err := c.lookupExact(ctx, namespace, hash, routeModel); err != nil {
		return nil, err
	} else if entry != nil {
		return entry, nil
	}

	if !c.cfg.SemanticEnabled || !semantic {
		return nil, nil
	}

	vec, err := c.embedder.Embed(ctx, req.PromptText())
	if err != nil {
		return nil, fmt.Errorf("embed prompt: %w", err)
	}

	return c.lookupSemantic(ctx, namespace, OptionsKey(req), vec, routeModel, threshold)
}

func (c *Cache) lookupExact(ctx context.Context, namespace, hash, routeModel string) (*Entry, error) {
	const q = `
		SELECT id, prompt, response, prompt_tokens, completion_tokens,
		       EXTRACT(EPOCH FROM (NOW() - created_at)), hit_count
		FROM semantic_cache
		WHERE cache_namespace = $1
		  AND prompt_hash = $2
		  AND route_model = $3
		  AND embed_model = $4
		  AND expires_at > NOW()
		ORDER BY created_at DESC
		LIMIT 1`

	var (
		e       Entry
		ageSecs float64
	)
	err := c.pool.QueryRow(ctx, q, namespace, hash, routeModel, c.embedder.Model()).
		Scan(&e.ID, &e.Prompt, &e.Response, &e.PromptTokens, &e.CompletionTokens, &ageSecs, &e.HitCount)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("exact cache lookup: %w", err)
	}

	e.Exact = true
	e.Similarity = 1.0
	e.Age = time.Duration(ageSecs * float64(time.Second))
	c.recordHit(e.ID)
	return &e, nil
}

func (c *Cache) lookupSemantic(ctx context.Context, namespace, optionsHash string, vec []float32, routeModel string, threshold float64) (*Entry, error) {
	// `embedding <=> $1` is pgvector's cosine *distance* operator: 0 means
	// identical, 2 means opposite. Similarity is 1 - distance. Ordering by the
	// operator directly is what lets the HNSW index serve the query; wrapping it
	// in an expression would force a sequential scan and defeat the index.
	const q = `
		SELECT id, prompt, response, prompt_tokens, completion_tokens,
		       1 - (embedding <=> $1) AS similarity,
		       EXTRACT(EPOCH FROM (NOW() - created_at)), hit_count
	FROM semantic_cache
	WHERE cache_namespace = $2
	  AND request_options_hash = $3
	  AND route_model = $4
	  AND embed_model = $5
	  AND expires_at > NOW()
	  -- Exact-only mode stores a zero placeholder vector. Its cosine distance
	  -- is NaN, so exclude it rather than letting stale exact-cache rows poison
	  -- a later semantic lookup after semantic mode is enabled.
	  AND (embedding <=> embedding) <> 'NaN'::float8
	ORDER BY embedding <=> $1
	LIMIT 1`

	var (
		e       Entry
		ageSecs float64
	)
	err := c.pool.QueryRow(ctx, q, pgvector.NewVector(vec), namespace, optionsHash, routeModel, c.embedder.Model()).
		Scan(&e.ID, &e.Prompt, &e.Response, &e.PromptTokens, &e.CompletionTokens,
			&e.Similarity, &ageSecs, &e.HitCount)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("semantic cache lookup: %w", err)
	}

	// The threshold is the correctness dial. Too high and paraphrases miss and
	// the cache earns nothing; too low and "How do I reset my password?" is
	// answered with the entry for "How do I reset my router?" — confidently,
	// and in the model's voice, which is what makes it dangerous. 0.95 is
	// deliberately conservative: a miss costs latency, a false hit costs trust.
	if e.Similarity < threshold {
		return nil, nil
	}

	e.Age = time.Duration(ageSecs * float64(time.Second))
	c.recordHit(e.ID)
	return &e, nil
}

// recordHit updates statistics without blocking the response.
//
// Fire-and-forget with its own context: the caller is about to serve a cache
// hit and must not wait on a bookkeeping UPDATE, and a failed statistics write
// is not a reason to fail the request.
func (c *Cache) recordHit(id int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := c.pool.Exec(ctx,
			`UPDATE semantic_cache SET hit_count = hit_count + 1, last_hit_at = NOW() WHERE id = $1`, id)
		if err != nil {
			c.log.Debug("could not record cache hit", "id", id, "error", err)
		}
	}()
}

// Store writes a completion to the cache.
func (c *Cache) Store(ctx context.Context, namespace string, req *provider.ChatRequest, routeModel string,
	response json.RawMessage, usage *provider.Usage) error {
	return c.StoreWithPolicy(ctx, namespace, req, routeModel, response, usage, c.cfg.SemanticEnabled, c.cfg.TTL.D())
}

func (c *Cache) StoreWithPolicy(ctx context.Context, namespace string, req *provider.ChatRequest, routeModel string,
	response json.RawMessage, usage *provider.Usage, semantic bool, ttl time.Duration) error {

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout.D())
	defer cancel()

	prompt := req.PromptText()

	var vec []float32
	if c.cfg.SemanticEnabled && semantic {
		v, err := c.embedder.Embed(ctx, prompt)
		if err != nil {
			return fmt.Errorf("embed for store: %w", err)
		}
		vec = v
	} else {
		// Exact-match only mode still needs a value for the NOT NULL column.
		vec = make([]float32, c.embedder.Dimensions())
	}

	var promptTokens, completionTokens int
	if usage != nil {
		promptTokens, completionTokens = usage.PromptTokens, usage.CompletionTokens
	}

	const q = `
		INSERT INTO semantic_cache
			(cache_namespace, prompt_hash, request_options_hash, route_model, embed_model, prompt, response,
			 prompt_tokens, completion_tokens, embedding, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW() + $11::interval)`

	_, err := c.pool.Exec(ctx, q,
		namespace, PromptKey(req), OptionsKey(req), routeModel, c.embedder.Model(), prompt, response,
		promptTokens, completionTokens, pgvector.NewVector(vec),
		fmt.Sprintf("%d seconds", int(ttl.Seconds())))
	if err != nil {
		return fmt.Errorf("store cache entry: %w", err)
	}
	return nil
}

// Purge deletes expired entries and returns how many were removed.
//
// TTL is enforced on read as well (expires_at > NOW()), so a missed sweep is a
// storage problem rather than a correctness one — stale rows are never served,
// they just take up space.
func (c *Cache) Purge(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tag, err := c.pool.Exec(ctx, `DELETE FROM semantic_cache WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, fmt.Errorf("purge expired cache entries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RunPurgeLoop periodically removes expired entries until ctx is cancelled.
func (c *Cache) RunPurgeLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := c.Purge(ctx)
			if err != nil {
				c.log.Warn("cache purge failed", "error", err)
				continue
			}
			if n > 0 {
				c.log.Info("purged expired cache entries", "count", n)
			}
		}
	}
}

// Stats summarises cache contents, for the admin endpoint.
type Stats struct {
	Entries     int64     `json:"entries"`
	TotalHits   int64     `json:"total_hits"`
	OldestEntry time.Time `json:"oldest_entry,omitempty"`
	EmbedModel  string    `json:"embed_model"`
	Dimensions  int       `json:"dimensions"`
	Threshold   float64   `json:"similarity_threshold"`
	TTL         string    `json:"ttl"`
	SavedTokens int64     `json:"saved_completion_tokens"`
}

// Stats reports cache contents and cumulative savings.
func (c *Cache) Stats(ctx context.Context) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	s := Stats{
		EmbedModel: c.embedder.Model(),
		Dimensions: c.embedder.Dimensions(),
		Threshold:  c.cfg.SimilarityThreshold,
		TTL:        c.cfg.TTL.String(),
	}

	const q = `
		SELECT COUNT(*),
		       COALESCE(SUM(hit_count), 0),
		       COALESCE(MIN(created_at), NOW()),
		       COALESCE(SUM(hit_count * completion_tokens), 0)
		FROM semantic_cache
		WHERE expires_at > NOW()`

	if err := c.pool.QueryRow(ctx, q).
		Scan(&s.Entries, &s.TotalHits, &s.OldestEntry, &s.SavedTokens); err != nil {
		return s, fmt.Errorf("cache stats: %w", err)
	}
	return s, nil
}

func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows in result set")
}
