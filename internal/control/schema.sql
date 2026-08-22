CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS conduit_schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS admin_users (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email                    TEXT NOT NULL UNIQUE,
    display_name             TEXT NOT NULL,
    password_hash            TEXT NOT NULL,
    platform_owner           BOOLEAN NOT NULL DEFAULT FALSE,
    status                   TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('invited', 'active', 'disabled')),
    password_change_required BOOLEAN NOT NULL DEFAULT FALSE,
    last_login_at            TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS admin_invitations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT NOT NULL,
    token_hash  BYTEA NOT NULL UNIQUE,
    invited_by  UUID NOT NULL REFERENCES admin_users(id),
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS projects (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                  TEXT NOT NULL UNIQUE,
    name                  TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    configuration_version BIGINT NOT NULL DEFAULT 1,
    created_by            UUID NOT NULL REFERENCES admin_users(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS project_memberships (
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    admin_id   UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('project_owner', 'project_admin', 'project_viewer')),
    created_by UUID NOT NULL REFERENCES admin_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, admin_id)
);

CREATE TABLE IF NOT EXISTS project_policies (
    project_id                       UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    version                          BIGINT NOT NULL DEFAULT 1,
    requests_per_minute              INTEGER NOT NULL DEFAULT 120 CHECK (requests_per_minute > 0),
    tokens_per_minute                INTEGER NOT NULL DEFAULT 100000 CHECK (tokens_per_minute > 0),
    end_user_requests_per_minute     INTEGER CHECK (end_user_requests_per_minute > 0),
    monthly_budget_usd               NUMERIC(14,4) CHECK (monthly_budget_usd >= 0),
    budget_warning_percent           INTEGER NOT NULL DEFAULT 80 CHECK (budget_warning_percent BETWEEN 1 AND 100),
    require_end_user                 BOOLEAN NOT NULL DEFAULT FALSE,
    cache_mode                       TEXT NOT NULL DEFAULT 'disabled' CHECK (cache_mode IN ('disabled', 'exact', 'semantic')),
    cache_ttl_seconds                INTEGER NOT NULL DEFAULT 3600 CHECK (cache_ttl_seconds > 0),
    semantic_threshold               DOUBLE PRECISION NOT NULL DEFAULT 0.93 CHECK (semantic_threshold BETWEEN 0 AND 1),
    maximum_cacheable_temperature    DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (maximum_cacheable_temperature BETWEEN 0 AND 2),
    analytics_retention_days         INTEGER NOT NULL DEFAULT 30 CHECK (analytics_retention_days IN (7, 30, 90)),
    updated_by                       UUID NOT NULL REFERENCES admin_users(id),
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS provider_connections (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id            UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name                  TEXT NOT NULL,
    provider_type         TEXT NOT NULL CHECK (provider_type IN ('openai', 'anthropic', 'ollama', 'openai_compatible')),
    base_url              TEXT NOT NULL,
    timeout_seconds       INTEGER NOT NULL DEFAULT 120 CHECK (timeout_seconds BETWEEN 1 AND 1800),
    breaker_policy        JSONB NOT NULL DEFAULT '{}'::jsonb,
    encrypted_credential  BYTEA,
    credential_fingerprint TEXT NOT NULL DEFAULT '',
    credential_rotated_at TIMESTAMPTZ,
    created_by            UUID NOT NULL REFERENCES admin_users(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name),
    UNIQUE (project_id, id)
);

CREATE TABLE IF NOT EXISTS model_profiles (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'generation' CHECK (kind IN ('generation', 'embedding')),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    strategy      TEXT NOT NULL DEFAULT 'priority' CHECK (strategy IN ('priority', 'round_robin', 'weighted')),
    max_attempts  INTEGER NOT NULL DEFAULT 1 CHECK (max_attempts > 0),
    created_by    UUID NOT NULL REFERENCES admin_users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name),
    UNIQUE (project_id, id)
);

ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'generation';

CREATE TABLE IF NOT EXISTS model_profile_targets (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id                 UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    profile_id                 UUID NOT NULL,
    provider_connection_id     UUID NOT NULL,
    upstream_model_id          TEXT NOT NULL,
    priority                   INTEGER NOT NULL DEFAULT 1 CHECK (priority > 0),
    weight                     INTEGER NOT NULL DEFAULT 1 CHECK (weight > 0),
    input_price_per_million    NUMERIC(14,6) NOT NULL DEFAULT 0 CHECK (input_price_per_million >= 0),
    output_price_per_million   NUMERIC(14,6) NOT NULL DEFAULT 0 CHECK (output_price_per_million >= 0),
    pricing_updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (project_id, profile_id) REFERENCES model_profiles(project_id, id) ON DELETE CASCADE,
    FOREIGN KEY (project_id, provider_connection_id) REFERENCES provider_connections(project_id, id) ON DELETE RESTRICT,
    UNIQUE (profile_id, priority)
);

CREATE TABLE IF NOT EXISTS application_keys (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id                TEXT NOT NULL UNIQUE,
    project_id               UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    label                    TEXT NOT NULL,
    environment              TEXT NOT NULL DEFAULT 'development' CHECK (environment IN ('development', 'staging', 'production', 'test')),
    key_hash                 BYTEA NOT NULL,
    prefix                   TEXT NOT NULL,
    allowed_profiles         TEXT[] NOT NULL DEFAULT '{}',
    requests_per_minute      INTEGER CHECK (requests_per_minute > 0),
    tokens_per_minute        INTEGER CHECK (tokens_per_minute > 0),
    expires_at               TIMESTAMPTZ,
    revoked_at               TIMESTAMPTZ,
    created_by               UUID NOT NULL REFERENCES admin_users(id),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at             TIMESTAMPTZ,
    last_used_ip             INET
);

CREATE INDEX IF NOT EXISTS application_keys_project_idx ON application_keys(project_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_admin_id UUID REFERENCES admin_users(id),
    project_id    UUID REFERENCES projects(id),
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    before_json   JSONB,
    after_json    JSONB,
    request_id    TEXT NOT NULL DEFAULT '',
    source_ip     INET,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS audit_events_project_time_idx ON audit_events(project_id, created_at DESC);

INSERT INTO conduit_schema_migrations(version) VALUES (1) ON CONFLICT DO NOTHING;
INSERT INTO conduit_schema_migrations(version) VALUES (2) ON CONFLICT DO NOTHING;
