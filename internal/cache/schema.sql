-- Conduit semantic cache schema.
--
-- Applied idempotently at startup. The embedding dimension is substituted from
-- config, because a pgvector column has a fixed width and changing embedding
-- models is therefore a migration rather than a config edit.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS semantic_cache (
    id           BIGSERIAL PRIMARY KEY,

    -- Cache entries are isolated by caller when authentication is enabled.
    -- "shared" is used only for an unauthenticated, single-trust-zone setup.
    cache_namespace TEXT NOT NULL DEFAULT 'shared',

    -- Exact-match fast path. The hash covers prompt and generation settings,
    -- letting an identical request skip the embedding call and vector search.
    prompt_hash  TEXT NOT NULL,

    -- Semantic matches may vary the prompt but not generation behavior. This
    -- fingerprints every request field except messages and transport fields.
    request_options_hash TEXT NOT NULL DEFAULT '',

    -- The route the entry was produced for. Entries are never shared across
    -- routes: the same question asked of two different models has two different
    -- right answers, and serving one for the other is the bug that makes people
    -- distrust caches.
    route_model  TEXT NOT NULL,

    -- The embedding model that produced the vector. Vectors from different
    -- models are not comparable, so entries are scoped to it.
    embed_model  TEXT NOT NULL,

    prompt       TEXT NOT NULL,
    response     JSONB NOT NULL,

    -- Denormalised for analytics and for reporting savings without parsing the
    -- stored response.
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,

    embedding    VECTOR(%d) NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    hit_count    INT NOT NULL DEFAULT 0,
    last_hit_at  TIMESTAMPTZ
);

-- The exact-match path. Scoped by route and embedding model for the reasons
-- above.
CREATE INDEX IF NOT EXISTS semantic_cache_exact_idx
    ON semantic_cache (prompt_hash, route_model, embed_model);

-- Added separately so an existing installation can migrate without dropping
-- its cache table. The old index remains harmless and can be removed during a
-- maintenance window.
ALTER TABLE semantic_cache
    ADD COLUMN IF NOT EXISTS cache_namespace TEXT NOT NULL DEFAULT 'shared';

ALTER TABLE semantic_cache
    ADD COLUMN IF NOT EXISTS request_options_hash TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS semantic_cache_exact_scoped_idx
    ON semantic_cache (cache_namespace, prompt_hash, route_model, embed_model);

CREATE INDEX IF NOT EXISTS semantic_cache_semantic_scope_idx
    ON semantic_cache (cache_namespace, request_options_hash, route_model, embed_model);

-- Expiry sweeps.
CREATE INDEX IF NOT EXISTS semantic_cache_expires_idx
    ON semantic_cache (expires_at);

-- The vector index.
--
-- HNSW over IVFFlat: IVFFlat has to be built against existing data to pick
-- sensible cluster centroids, so it is useless on an empty table and degrades
-- as the distribution drifts — exactly wrong for a cache that starts empty and
-- fills continuously. HNSW builds incrementally, gives better recall at the
-- same latency, and needs no retraining. It costs more memory and slower
-- inserts, both of which are the right trade for a read-heavy cache.
--
-- vector_cosine_ops because semantic similarity is about direction, not
-- magnitude: two paraphrases of the same question point the same way, and
-- their vector lengths are irrelevant.
CREATE INDEX IF NOT EXISTS semantic_cache_embedding_idx
    ON semantic_cache USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);
