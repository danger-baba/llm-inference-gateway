CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE orgs (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT NOT NULL,
    tpm_limit          BIGINT NOT NULL,
    monthly_budget_usd NUMERIC(12,4),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id             UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    tpm_limit          BIGINT NOT NULL,
    monthly_budget_usd NUMERIC(12,4),
    UNIQUE (org_id, name)
);

-- Virtual keys are what clients present. The raw key is never stored.
CREATE TABLE virtual_keys (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id        UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    key_hash       BYTEA NOT NULL UNIQUE,       -- sha256 of the presented key
    key_prefix     TEXT  NOT NULL,              -- first 8 chars, for display only
    label          TEXT,
    allowed_models TEXT[] NOT NULL DEFAULT '{}',
    tpm_limit      BIGINT,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Belt-and-suspenders with the UNIQUE constraint above: this partial index
-- is what actually enforces "no two *active* keys share a hash" the way
-- the README specifies, and is what a lookup query should hit.
CREATE UNIQUE INDEX virtual_keys_key_hash_active_idx ON virtual_keys (key_hash) WHERE revoked_at IS NULL;

-- Append-only. No UPDATE, no DELETE.
--
-- Deviation from the README's literal DDL: a table partitioned BY RANGE
-- must include the partition key in every unique constraint, so the
-- primary key here is (id, created_at), not id alone. See
-- docs/adr/0007-usage-ledger-partition-primary-key.md.
CREATE TABLE usage_ledger (
    id                BIGSERIAL,
    request_id        UUID NOT NULL,
    org_id            UUID NOT NULL,
    team_id           UUID NOT NULL,
    virtual_key_id    UUID NOT NULL,
    provider          TEXT NOT NULL,
    model             TEXT NOT NULL,
    prompt_tokens     INT  NOT NULL,
    completion_tokens INT  NOT NULL,
    tokens_saved      INT  NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(12,6) NOT NULL,
    cache_tier        TEXT NOT NULL,           -- none | exact | semantic
    attempts          SMALLINT NOT NULL,
    status_code       SMALLINT NOT NULL,
    latency_ms        INT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- A DEFAULT partition keeps every INSERT valid regardless of when this
-- migration actually runs. Phase 9, when the ledger writer exists, should
-- add real monthly partitions (and ideally automate creating the next
-- one) so the DEFAULT partition stays empty in practice — its only job is
-- to make "no partition found for row" impossible.
CREATE TABLE usage_ledger_default PARTITION OF usage_ledger DEFAULT;

CREATE INDEX usage_ledger_org_created_idx ON usage_ledger (org_id, created_at DESC);
CREATE INDEX usage_ledger_vk_created_idx ON usage_ledger (virtual_key_id, created_at DESC);
