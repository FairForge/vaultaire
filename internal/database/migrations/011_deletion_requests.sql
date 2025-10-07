-- GDPR Article 17: Right to Erasure

-- Deletion requests track user data deletion
CREATE TABLE IF NOT EXISTS deletion_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    requested_by UUID NOT NULL, -- Who initiated (user or admin)
    reason TEXT NOT NULL,
    scope VARCHAR(50) NOT NULL DEFAULT 'all', -- all, containers, specific
    container_filter TEXT[], -- NULL = all containers, or specific list
    include_backups BOOLEAN NOT NULL DEFAULT true,
    preserve_audit BOOLEAN NOT NULL DEFAULT true, -- Keep audit logs

    scheduled_for TIMESTAMPTZ, -- NULL = immediate
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    status VARCHAR(50) NOT NULL DEFAULT 'pending', -- pending, in_progress, completed, failed, cancelled

    items_deleted INTEGER DEFAULT 0,
    bytes_deleted BIGINT DEFAULT 0,
    proof_hash VARCHAR(64), -- SHA-256 of deletion certificate
    error_message TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT valid_status CHECK (status IN ('pending', 'in_progress', 'completed', 'failed', 'cancelled')),
    CONSTRAINT valid_scope CHECK (scope IN ('all', 'containers', 'specific'))
);

-- Deletion proofs provide cryptographic evidence of deletion
CREATE TABLE IF NOT EXISTS deletion_proofs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id UUID NOT NULL REFERENCES deletion_requests(id) ON DELETE CASCADE,
    backend_id VARCHAR(255) NOT NULL,
    container VARCHAR(255) NOT NULL,
    artifact VARCHAR(512) NOT NULL,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    proof_type VARCHAR(50) NOT NULL, -- file_deleted, backup_deleted, metadata_cleared, version_deleted
    proof_data JSONB, -- Additional verification data
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT valid_proof_type CHECK (proof_type IN ('file_deleted', 'backup_deleted', 'metadata_cleared', 'version_deleted'))
);

-- Indexes for performance
CREATE INDEX idx_deletion_requests_user ON deletion_requests(user_id, status);
CREATE INDEX idx_deletion_requests_scheduled ON deletion_requests(scheduled_for) WHERE status = 'pending' AND scheduled_for IS NOT NULL;
CREATE INDEX idx_deletion_requests_status ON deletion_requests(status, created_at DESC);
CREATE INDEX idx_deletion_proofs_request ON deletion_proofs(request_id);

-- Comments
COMMENT ON TABLE deletion_requests IS 'GDPR Article 17: Right to erasure requests';
COMMENT ON TABLE deletion_proofs IS 'Cryptographic proof of data deletion';
COMMENT ON COLUMN deletion_requests.proof_hash IS 'SHA-256 hash of all deletion proofs for verification';
