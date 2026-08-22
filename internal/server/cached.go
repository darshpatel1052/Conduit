package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/darshpatel1052/conduit/internal/auth"
	"github.com/darshpatel1052/conduit/internal/cache"
	"github.com/darshpatel1052/conduit/internal/provider"
	"github.com/darshpatel1052/conduit/internal/proxy"
	"github.com/darshpatel1052/conduit/internal/telemetry"
)

// lookupCache attempts to serve a request from the semantic cache.
//
// Returns true when the response has been written and the handler should stop.
// Every failure path here returns false: a broken cache must cost latency, not
// availability, so any error falls through to the provider.
func (s *Server) lookupCache(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest,
	routeModel string, principal *auth.Principal, obs *observation) bool {

	if s.cache == nil {
		return false
	}
	if principal != nil && principal.ProjectID != "" && (principal.CacheMode == "disabled" || principal.CacheMode == "") {
		w.Header().Set("X-Conduit-Cache", "disabled")
		obs.cacheStatus = "disabled"
		return false
	}

	log := telemetry.LoggerFrom(r.Context())

	maxTemperature := s.cfg.Cache.MaxTemperature
	if principal != nil && principal.ProjectID != "" {
		maxTemperature = principal.CacheMaxTemperature
	}
	if ok, reason := cache.Cacheable(req, maxTemperature); !ok {
		log.Debug("cache bypassed", "reason", reason)
		w.Header().Set("X-Conduit-Cache", "bypass")
		obs.cacheStatus = "bypass"
		return false
	}

	start := time.Now()
	semantic := principal == nil || principal.ProjectID == "" || principal.CacheMode == "semantic"
	threshold := s.cfg.Cache.SimilarityThreshold
	if principal != nil && principal.ProjectID != "" {
		threshold = principal.CacheSemanticThreshold
	}
	entry, err := s.cache.LookupWithPolicy(r.Context(), cacheNamespace(principal), req, routeModel, semantic, threshold)
	if err != nil {
		log.Warn("cache lookup failed, falling through to provider", "error", err)
		w.Header().Set("X-Conduit-Cache", "error")
		obs.cacheStatus = "error"
		return false
	}

	lookupTime := time.Since(start)
	if s.metrics != nil {
		s.metrics.CacheLatency.Observe(lookupTime.Seconds())
	}

	if entry == nil {
		w.Header().Set("X-Conduit-Cache", "miss")
		obs.cacheStatus = "miss"
		return false
	}

	// Expose why this was a hit. A semantic cache that cannot show its
	// similarity score is impossible to tune, and tuning the threshold is the
	// main operational task it creates.
	h := w.Header()
	h.Set("X-Conduit-Cache", "hit")
	h.Set("X-Conduit-Cache-Similarity", strconv.FormatFloat(entry.Similarity, 'f', 4, 64))
	h.Set("X-Conduit-Cache-Age-Seconds", strconv.FormatInt(int64(entry.Age.Seconds()), 10))
	h.Set("X-Conduit-Route", routeModel)
	if entry.Exact {
		h.Set("X-Conduit-Cache-Match", "exact")
		obs.cacheStatus = "hit_exact"
	} else {
		h.Set("X-Conduit-Cache-Match", "semantic")
		obs.cacheStatus = "hit_semantic"
	}
	obs.cacheScore = entry.Similarity
	obs.result = &proxy.Result{
		Route: routeModel,
		Usage: &provider.Usage{
			PromptTokens:     entry.PromptTokens,
			CompletionTokens: entry.CompletionTokens,
			TotalTokens:      entry.PromptTokens + entry.CompletionTokens,
		},
	}
	h.Set("X-Conduit-Cache-Lookup-Ms", strconv.FormatInt(lookupTime.Milliseconds(), 10))

	log.Info("served from semantic cache",
		"route", routeModel,
		"match", h.Get("X-Conduit-Cache-Match"),
		"similarity", entry.Similarity,
		"age_seconds", int64(entry.Age.Seconds()),
		"lookup_ms", lookupTime.Milliseconds(),
		"saved_completion_tokens", entry.CompletionTokens)

	// Cached tokens were never generated, so they are not debited against the
	// caller's token budget. That is the point: the saving is real.

	if req.Stream {
		s.replayAsStream(w, r, entry, req)
		return true
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.Response)
	return true
}

// replayAsStream serves a cached response to a streaming client as SSE.
//
// Without this, enabling the cache would change the shape of the response for
// streaming callers — a client that asked for a stream would suddenly get a
// single JSON body, and every SDK parsing deltas would break. Replaying keeps
// the interface uniform: the client cannot tell a cache hit from a very fast
// model, which is exactly the abstraction a gateway should provide.
func (s *Server) replayAsStream(w http.ResponseWriter, r *http.Request,
	entry *cache.Entry, req *provider.ChatRequest) {

	log := telemetry.LoggerFrom(r.Context())

	var stored provider.ChatResponse
	if err := json.Unmarshal(entry.Response, &stored); err != nil {
		log.Warn("cached response is not a chat completion, cannot replay", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry.Response)
		return
	}

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	writeFrame := func(v any) bool {
		payload, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(payload); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	created := time.Now().Unix()
	id := stored.ID
	if id == "" {
		id = "chatcmpl-cached"
	}

	// Opening role-only delta, as a real stream would send.
	if !writeFrame(provider.ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: stored.Model,
		Choices: []provider.Choice{{Index: 0, Delta: &provider.Message{Role: "assistant"}}},
	}) {
		return
	}

	var text string
	var finish *string
	if len(stored.Choices) > 0 {
		if stored.Choices[0].Message != nil {
			text = stored.Choices[0].Message.Text()
		}
		finish = stored.Choices[0].FinishReason
	}

	// Chunked by words rather than emitted as one frame, so the client's
	// incremental rendering path behaves the same as it would for a live
	// stream. No artificial delay is inserted: the response is already known,
	// and pacing it to imitate generation would throw away the entire latency
	// win the cache exists to deliver.
	for _, piece := range chunkText(text, 8) {
		content, err := json.Marshal(piece)
		if err != nil {
			continue
		}
		if !writeFrame(provider.ChatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: stored.Model,
			Choices: []provider.Choice{{Index: 0, Delta: &provider.Message{Content: content}}},
		}) {
			return
		}
	}

	if !writeFrame(provider.ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: stored.Model,
		Choices: []provider.Choice{{Index: 0, Delta: &provider.Message{}, FinishReason: finish}},
	}) {
		return
	}

	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		// Report the usage the original generation cost. Reporting zero would
		// be more literally true but would corrupt any client-side accounting
		// that sums usage across a conversation.
		if !writeFrame(provider.ChatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: stored.Model,
			Choices: []provider.Choice{},
			Usage: &provider.Usage{
				PromptTokens:     entry.PromptTokens,
				CompletionTokens: entry.CompletionTokens,
				TotalTokens:      entry.PromptTokens + entry.CompletionTokens,
			},
		}) {
			return
		}
	}

	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	_ = rc.Flush()
}

// chunkText splits text into word-groups of roughly n words.
func chunkText(text string, n int) []string {
	if text == "" {
		return nil
	}
	fields := splitKeepingSpaces(text)
	if len(fields) == 0 {
		return []string{text}
	}

	var out []string
	for i := 0; i < len(fields); i += n {
		end := i + n
		if end > len(fields) {
			end = len(fields)
		}
		var b []byte
		for _, f := range fields[i:end] {
			b = append(b, f...)
		}
		out = append(out, string(b))
	}
	return out
}

// splitKeepingSpaces splits on word boundaries without discarding the
// whitespace, so reassembling the chunks reproduces the original exactly.
func splitKeepingSpaces(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// storeInCache writes a fresh completion to the cache.
//
// Runs on a detached context: for a streaming request the client's context is
// already cancelled by the time the full response is assembled, and the
// generation has been paid for either way — it should be kept.
func (s *Server) storeInCache(r *http.Request, req *provider.ChatRequest, routeModel string,
	response json.RawMessage, usage *provider.Usage) {

	if s.cache == nil || len(response) == 0 {
		return
	}
	maxTemperature := s.cfg.Cache.MaxTemperature
	if principal, ok := auth.PrincipalFrom(r.Context()); ok && principal.ProjectID != "" {
		maxTemperature = principal.CacheMaxTemperature
	}
	if ok, _ := cache.Cacheable(req, maxTemperature); !ok {
		return
	}

	ctx, cancel := detachedContext(r, s.cfg.Cache.Timeout.D()+2*time.Second)
	defer cancel()

	principal, _ := auth.PrincipalFrom(r.Context())
	if principal != nil && principal.ProjectID != "" && (principal.CacheMode == "disabled" || principal.CacheMode == "") {
		return
	}
	semantic := principal == nil || principal.ProjectID == "" || principal.CacheMode == "semantic"
	ttl := s.cfg.Cache.TTL.D()
	if principal != nil && principal.ProjectID != "" {
		ttl = time.Duration(principal.CacheTTLSeconds) * time.Second
	}
	if err := s.cache.StoreWithPolicy(ctx, cacheNamespace(principal), req, routeModel, response, usage, semantic, ttl); err != nil {
		telemetry.LoggerFrom(r.Context()).Warn("could not store cache entry",
			"route", routeModel, "error", err)
	}
}

func cacheNamespace(principal *auth.Principal) string {
	if principal == nil || principal.Anonymous {
		return "shared"
	}
	if principal.ProjectID != "" {
		return "project:" + principal.ProjectID + ":policy:" + strconv.FormatInt(principal.PolicyVersion, 10)
	}
	return "key:" + principal.ID
}

// handleAdminCache reports cache contents and cumulative savings.
func (s *Server) handleAdminCache(w http.ResponseWriter, r *http.Request) {
	if s.cache == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	stats, err := s.cache.Stats(r.Context())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "cache_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "stats": stats})
}
