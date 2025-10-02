-- System-wide audit log table
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    user_id UUID,
    tenant_id VARCHAR(255),
    event_type VARCHAR(100) NOT NULL,
    action VARCHAR(100) NOT NULL,
    resource TEXT,
    result VARCHAR(20) NOT NULL,
    severity VARCHAR(20) DEFAULT 'info',
    ip INET,
    user_agent TEXT,
    duration_ms BIGINT,
    error_msg TEXT,
    metadata JSONB,
    performed_by UUID,

    -- Indexes for common queries
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for fast querying
CREATE INDEX idx_audit_timestamp ON audit_logs(timestamp DESC);
CREATE INDEX idx_audit_user ON audit_logs(user_id, timestamp DESC);
CREATE INDEX idx_audit_tenant ON audit_logs(tenant_id, timestamp DESC);
CREATE INDEX idx_audit_event_type ON audit_logs(event_type, timestamp DESC);
CREATE INDEX idx_audit_result ON audit_logs(result, timestamp DESC);
CREATE INDEX idx_audit_severity ON audit_logs(severity, timestamp DESC) WHERE severity IN ('error', 'critical');

-- Composite index for common query patterns
CREATE INDEX idx_audit_user_event ON audit_logs(user_id, event_type, timestamp DESC);
CREATE INDEX idx_audit_tenant_event ON audit_logs(tenant_id, event_type, timestamp DESC);

-- JSONB GIN index for metadata searches
CREATE INDEX idx_audit_metadata ON audit_logs USING GIN (metadata);

-- Comment
COMMENT ON TABLE audit_logs IS 'System-wide audit log for all operations';
