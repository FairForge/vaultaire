-- Data Retention Policies System (Backend-Aware)

-- Retention policies can be global, tenant-specific, or backend-specific
CREATE TABLE IF NOT EXISTS retention_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    description TEXT,
    data_category VARCHAR(100) NOT NULL,
    retention_period INTERVAL NOT NULL,
    grace_period INTERVAL NOT NULL DEFAULT '7 days',
    action VARCHAR(50) NOT NULL DEFAULT 'delete',
    enabled BOOLEAN NOT NULL DEFAULT true,

    -- Scope control
    tenant_id VARCHAR(255), -- NULL = global
    backend_id VARCHAR(255), -- NULL = all backends, specific = only this backend
    container_name VARCHAR(255), -- NULL = all containers, specific = only this container

    -- Backend feature toggles
    use_backend_object_lock BOOLEAN DEFAULT false, -- Use Lyve Object Lock if available
    use_backend_versioning BOOLEAN DEFAULT false,  -- Use S3 versioning if available
    use_backend_lifecycle BOOLEAN DEFAULT false,   -- Use S3 lifecycle policies if available

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_action CHECK (action IN ('delete', 'archive', 'anonymize'))
);

-- Legal holds can also be backend-specific
CREATE TABLE IF NOT EXISTS legal_holds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    case_number VARCHAR(100),
    created_by UUID NOT NULL,
    expires_at TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    status VARCHAR(50) NOT NULL DEFAULT 'active',

    -- Backend-specific holds
    backend_id VARCHAR(255), -- NULL = all backends
    apply_object_lock BOOLEAN DEFAULT false, -- Use S3 Object Lock if available

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_status CHECK (status IN ('active', 'expired', 'released'))
);

-- Backend capability tracking
CREATE TABLE IF NOT EXISTS backend_capabilities (
    backend_id VARCHAR(255) PRIMARY KEY,
    supports_object_lock BOOLEAN DEFAULT false,
    supports_versioning BOOLEAN DEFAULT false,
    supports_lifecycle BOOLEAN DEFAULT false,
    supports_legal_hold BOOLEAN DEFAULT false,
    supports_retention_period BOOLEAN DEFAULT false,
    last_checked TIMESTAMPTZ DEFAULT NOW()
);

-- Retention jobs
CREATE TABLE IF NOT EXISTS retention_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id UUID REFERENCES retention_policies(id) ON DELETE SET NULL,
    backend_id VARCHAR(255), -- Which backend was processed
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    status VARCHAR(50) NOT NULL DEFAULT 'running',
    items_scanned INTEGER NOT NULL DEFAULT 0,
    items_deleted INTEGER NOT NULL DEFAULT 0,
    items_skipped INTEGER NOT NULL DEFAULT 0,
    dry_run BOOLEAN NOT NULL DEFAULT false,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_job_status CHECK (status IN ('running', 'completed', 'failed'))
);

-- Indexes
CREATE INDEX idx_retention_policy_backend ON retention_policies(backend_id, enabled)
    WHERE backend_id IS NOT NULL AND enabled = true;
CREATE INDEX idx_retention_policy_scope ON retention_policies(tenant_id, backend_id, container_name);
CREATE INDEX idx_legal_holds_backend ON legal_holds(backend_id, status)
    WHERE status = 'active';
CREATE INDEX idx_retention_jobs_backend ON retention_jobs(backend_id, created_at DESC);

-- Comments
COMMENT ON TABLE retention_policies IS 'Backend-aware retention policies with feature toggles';
COMMENT ON TABLE backend_capabilities IS 'Track which features each backend supports';
COMMENT ON COLUMN retention_policies.use_backend_object_lock IS 'Use Seagate Lyve Object Lock or S3 Object Lock if available';
