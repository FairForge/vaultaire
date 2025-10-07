-- GDPR Compliance Tables

-- Track data processing activities (Article 30 records)
CREATE TABLE IF NOT EXISTS gdpr_processing_activities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    activity_name VARCHAR(255) NOT NULL,
    purpose TEXT NOT NULL,
    legal_basis VARCHAR(50) NOT NULL, -- consent, contract, legitimate_interest, legal_obligation
    data_categories TEXT[] NOT NULL,  -- email, name, files, ip_address, etc
    retention_period INTERVAL NOT NULL,
    third_party_processors TEXT[],
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Subject Access Requests (Article 15)
CREATE TABLE IF NOT EXISTS gdpr_subject_access_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_date TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completion_date TIMESTAMPTZ,
    status VARCHAR(50) NOT NULL DEFAULT 'pending', -- pending, processing, completed, failed
    data_export_path TEXT,
    file_count INTEGER DEFAULT 0,
    total_size_bytes BIGINT DEFAULT 0,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Right to Erasure / Deletion Requests (Article 17)
CREATE TABLE IF NOT EXISTS gdpr_deletion_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    user_email VARCHAR(255) NOT NULL, -- Store before deletion
    request_date TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completion_date TIMESTAMPTZ,
    status VARCHAR(50) NOT NULL DEFAULT 'pending', -- pending, processing, completed, failed
    deletion_method VARCHAR(50) NOT NULL, -- soft_delete, hard_delete, anonymize
    files_deleted INTEGER DEFAULT 0,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Data breach notification log (Article 33/34)
CREATE TABLE IF NOT EXISTS gdpr_breach_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    breach_date TIMESTAMPTZ NOT NULL,
    detection_date TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    breach_type VARCHAR(100) NOT NULL, -- unauthorized_access, data_loss, ransomware, etc
    affected_users INTEGER NOT NULL DEFAULT 0,
    severity VARCHAR(20) NOT NULL, -- low, medium, high, critical
    notification_required BOOLEAN NOT NULL DEFAULT true,
    authority_notified BOOLEAN NOT NULL DEFAULT false,
    authority_notification_date TIMESTAMPTZ,
    users_notified BOOLEAN NOT NULL DEFAULT false,
    description TEXT NOT NULL,
    mitigation_actions TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for performance
CREATE INDEX idx_sar_user_id ON gdpr_subject_access_requests(user_id);
CREATE INDEX idx_sar_status ON gdpr_subject_access_requests(status) WHERE status IN ('pending', 'processing');
CREATE INDEX idx_sar_date ON gdpr_subject_access_requests(request_date DESC);

CREATE INDEX idx_deletion_user_id ON gdpr_deletion_requests(user_id);
CREATE INDEX idx_deletion_status ON gdpr_deletion_requests(status) WHERE status IN ('pending', 'processing');
CREATE INDEX idx_deletion_date ON gdpr_deletion_requests(request_date DESC);

CREATE INDEX idx_breach_date ON gdpr_breach_log(breach_date DESC);
CREATE INDEX idx_breach_severity ON gdpr_breach_log(severity) WHERE severity IN ('high', 'critical');

-- Comments for documentation
COMMENT ON TABLE gdpr_processing_activities IS 'Article 30 GDPR: Records of processing activities';
COMMENT ON TABLE gdpr_subject_access_requests IS 'Article 15 GDPR: Right of access by the data subject';
COMMENT ON TABLE gdpr_deletion_requests IS 'Article 17 GDPR: Right to erasure (right to be forgotten)';
COMMENT ON TABLE gdpr_breach_log IS 'Article 33/34 GDPR: Data breach notifications';
