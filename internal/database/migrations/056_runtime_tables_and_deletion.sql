-- 056_runtime_tables_and_deletion.sql (WP-8, CR-8 + CR-9)
-- Idempotent — safe to re-run on every deploy.
--
-- Until this migration, 14 tables existed only as InitializeSchema() calls in
-- Go that the production binary never invokes — so a database rebuilt from
-- migrations alone (the DR scenario) was missing tenant_quotas and
-- registration's 4-table persist failed on the spot. This migration makes the
-- migration set own every runtime table, and fixes the FK behaviors that made
-- GDPR ExecuteDeletion trip foreign keys.
--
-- DDL sources (kept in exact sync — change there, change here):
--   tenant_quotas / quota_usage_events  internal/usage/quota_manager.go
--   upgrade_triggers / _suggestions     internal/usage/auto_upgrade.go
--   grace_periods                       internal/usage/grace_period.go
--   usage_reports / snapshots / sched   internal/usage/reporting.go
--   billing_policies / credits / invoices  internal/usage/billing_integration.go
--     (billing_charges deliberately NOT here — migration 019 already owns it)
--   user_activities                     internal/auth/activity.go
--   artifacts                           internal/database/postgres.go
--   audit_logs_archive                  internal/audit/compression.go
--
-- Deviations from the Go DDL, on purpose:
--   * tenant_quotas gains spending_cap_cents (migration 043 adds it to
--     existing DBs, but 043 is skipped on a fresh DB where the table doesn't
--     exist yet — so it must be baked in here).
--   * every FK referencing tenant_quotas(tenant_id) is ON DELETE CASCADE so
--     account deletion can remove the quota row without walking nine child
--     tables; artifacts→tenants likewise.

-- ---------------------------------------------------------------------------
-- 1. Quota core (the table registration writes to — THE DR-blocker)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS tenant_quotas (
    tenant_id TEXT PRIMARY KEY,
    storage_limit_bytes BIGINT NOT NULL DEFAULT 5368709120,
    storage_used_bytes BIGINT NOT NULL DEFAULT 0,
    bandwidth_limit_bytes BIGINT DEFAULT NULL,
    bandwidth_used_bytes BIGINT NOT NULL DEFAULT 0,
    tier VARCHAR(50) NOT NULL DEFAULT 'free',
    spending_cap_cents BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS quota_usage_events (
    id SERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    operation VARCHAR(20) NOT NULL,
    bytes_delta BIGINT NOT NULL,
    object_key TEXT NOT NULL,
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_time
    ON quota_usage_events(tenant_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_usage_events_operation
    ON quota_usage_events(operation);

-- ---------------------------------------------------------------------------
-- 2. Quota satellites (auto-upgrade, grace periods, reporting, billing)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS upgrade_triggers (
    tenant_id TEXT PRIMARY KEY REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    limit_hits INT DEFAULT 0,
    last_hit_at TIMESTAMP,
    last_suggestion_at TIMESTAMP,
    suggestion_count INT DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS upgrade_suggestions (
    id SERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    current_tier VARCHAR(50),
    recommended_tier VARCHAR(50),
    reason TEXT,
    status VARCHAR(20) DEFAULT 'pending',
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_upgrade_triggers_hits
    ON upgrade_triggers(limit_hits)
    WHERE limit_hits > 0;

CREATE TABLE IF NOT EXISTS grace_periods (
    tenant_id TEXT PRIMARY KEY REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    started_at TIMESTAMP NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMP NOT NULL,
    extension_count INT DEFAULT 0,
    last_notification TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_grace_periods_expires
    ON grace_periods(expires_at)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS usage_reports (
    id SERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    period VARCHAR(20) NOT NULL,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    storage_used BIGINT,
    storage_limit BIGINT,
    bandwidth_used BIGINT,
    object_count BIGINT,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS usage_daily_snapshots (
    id SERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    snapshot_date DATE NOT NULL,
    storage_used BIGINT,
    bandwidth_used BIGINT,
    object_count BIGINT,
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(tenant_id, snapshot_date)
);

CREATE TABLE IF NOT EXISTS report_schedules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    period VARCHAR(20) NOT NULL,
    recipients TEXT[],
    format VARCHAR(10) NOT NULL,
    next_run TIMESTAMP NOT NULL,
    enabled BOOLEAN DEFAULT true,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_daily_snapshots_tenant_date
    ON usage_daily_snapshots(tenant_id, snapshot_date DESC);

CREATE TABLE IF NOT EXISTS billing_policies (
    tenant_id TEXT PRIMARY KEY REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    policy VARCHAR(20) NOT NULL DEFAULT 'standard',
    overage_enabled BOOLEAN DEFAULT false,
    prepaid_balance DECIMAL(10,2) DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS billing_credits (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    amount DECIMAL(10,2) NOT NULL,
    balance DECIMAL(10,2) NOT NULL,
    description TEXT,
    expires_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS invoices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    period DATE NOT NULL,
    total_amount DECIMAL(10,2) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    due_date DATE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credits_tenant_balance
    ON billing_credits(tenant_id, balance) WHERE balance > 0;

-- ---------------------------------------------------------------------------
-- 3. User activity + artifact metadata + audit archive
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS user_activities (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(255) NOT NULL,
    action VARCHAR(50) NOT NULL,
    resource VARCHAR(255),
    ip VARCHAR(45),
    user_agent TEXT,
    metadata TEXT,
    timestamp TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_activities_user_id ON user_activities(user_id);
CREATE INDEX IF NOT EXISTS idx_user_activities_timestamp ON user_activities(timestamp);

CREATE TABLE IF NOT EXISTS artifacts (
    id SERIAL PRIMARY KEY,
    tenant_id VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    container VARCHAR(255) NOT NULL,
    name VARCHAR(255) NOT NULL,
    size BIGINT NOT NULL,
    content_type VARCHAR(255),
    etag VARCHAR(255),
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, container, name)
);

CREATE TABLE IF NOT EXISTS audit_logs_archive (
    LIKE audit_logs INCLUDING ALL
) WITH (
    fillfactor = 100,
    autovacuum_enabled = false
);

CREATE INDEX IF NOT EXISTS idx_audit_archive_timestamp
    ON audit_logs_archive(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_archive_user
    ON audit_logs_archive(user_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_archive_tenant
    ON audit_logs_archive(tenant_id, timestamp DESC);

-- ---------------------------------------------------------------------------
-- 4. FK retrofits for databases that predate this migration (prod, CI).
--    Fresh DBs get these behaviors from the inline DDL above; existing DBs
--    have the tables already (CREATE IF NOT EXISTS no-ops) so the constraints
--    must be fixed in place. Both blocks are no-ops once applied.
-- ---------------------------------------------------------------------------

-- webhook_deliveries.event_id: migration 033 created it without ON DELETE, so
-- deleting a tenant's events tripped the FK. CASCADE it, and index the column
-- (FK columns need an index for the cascade not to seq-scan deliveries).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'webhook_deliveries_event_id_fkey'
          AND conrelid = 'webhook_deliveries'::regclass
          AND confdeltype <> 'c'
    ) THEN
        ALTER TABLE webhook_deliveries DROP CONSTRAINT webhook_deliveries_event_id_fkey;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'webhook_deliveries_event_id_fkey'
          AND conrelid = 'webhook_deliveries'::regclass
    ) THEN
        ALTER TABLE webhook_deliveries ADD CONSTRAINT webhook_deliveries_event_id_fkey
            FOREIGN KEY (event_id) REFERENCES events(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event
    ON webhook_deliveries(event_id);

-- Any FK referencing tenant_quotas that predates this migration (runtime
-- InitializeSchema created them without ON DELETE): recreate with CASCADE.
-- All such FKs are single-column (tenant_id).
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT con.conname,
               con.conrelid::regclass::text AS tbl,
               (SELECT a.attname FROM pg_attribute a
                 WHERE a.attrelid = con.conrelid AND a.attnum = con.conkey[1]) AS col
        FROM pg_constraint con
        WHERE con.contype = 'f'
          AND con.confrelid = 'tenant_quotas'::regclass
          AND con.confdeltype <> 'c'
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.conname);
        EXECUTE format(
            'ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE',
            r.tbl, r.conname, r.col);
    END LOOP;
END $$;
