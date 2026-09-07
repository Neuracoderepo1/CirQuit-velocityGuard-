-- 0001_control_plane_init.sql
-- Control-plane schema: tenants, API keys, budget accounts.
--
-- NOTE on architecture (see internal/risk, internal/reservation):
-- these tables back the CONTROL PLANE only (tenant/key management,
-- durable budget config). The synchronous risk-decision hot path does
-- NOT query Postgres per request — it reads from in-process state
-- that is seeded/refreshed from these tables at startup and on
-- config change. See docs/architecture.md (TODO) for the full
-- failure-mode contract.

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- for gen_random_uuid()

CREATE TABLE IF NOT EXISTS tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- API keys are never stored in plaintext. `key_hash` is SHA-256 of the
-- full secret. `key_prefix` (e.g. "vg_live_ab12") is stored separately
-- so the UI/API can display a non-secret identifier without ever
-- retrieving the secret again after creation.
CREATE TABLE IF NOT EXISTS api_keys (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_prefix     TEXT NOT NULL,
    key_hash       TEXT NOT NULL UNIQUE,
    scopes         TEXT[] NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_id ON api_keys(tenant_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS budget_accounts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    period           TEXT NOT NULL CHECK (period IN ('hourly', 'daily', 'monthly')),
    limit_minor_units BIGINT NOT NULL CHECK (limit_minor_units >= 0),
    currency         TEXT NOT NULL DEFAULT 'USD',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, period)
);

CREATE INDEX IF NOT EXISTS idx_budget_accounts_tenant_id ON budget_accounts(tenant_id);
