-- Migration 013: Consent Management (GDPR Article 7 & 8)
-- Implements consent tracking with granular controls, audit trail, and age verification

-- Consent purposes (categories)
CREATE TABLE IF NOT EXISTS consent_purposes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    required BOOLEAN DEFAULT false,
    legal_basis VARCHAR(50), -- consent, legitimate_interest, contract, legal_obligation
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- User consents
CREATE TABLE IF NOT EXISTS user_consents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    purpose_id UUID NOT NULL,
    granted BOOLEAN NOT NULL,
    granted_at TIMESTAMPTZ,
    withdrawn_at TIMESTAMPTZ,
    method VARCHAR(50), -- ui, api, import, admin
    ip_address VARCHAR(45),
    user_agent TEXT,
    terms_version VARCHAR(20),
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    FOREIGN KEY (purpose_id) REFERENCES consent_purposes(id) ON DELETE CASCADE,
    UNIQUE(user_id, purpose_id)
);

-- Consent audit log
CREATE TABLE IF NOT EXISTS consent_audit (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    purpose_id UUID NOT NULL,
    action VARCHAR(50) NOT NULL, -- grant, withdraw, update
    granted BOOLEAN,
    method VARCHAR(50),
    ip_address VARCHAR(45),
    user_agent TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_user_consents_user ON user_consents(user_id);
CREATE INDEX IF NOT EXISTS idx_user_consents_purpose ON user_consents(purpose_id);
CREATE INDEX IF NOT EXISTS idx_user_consents_granted ON user_consents(granted);
CREATE INDEX IF NOT EXISTS idx_consent_audit_user ON consent_audit(user_id);
CREATE INDEX IF NOT EXISTS idx_consent_audit_purpose ON consent_audit(purpose_id);
CREATE INDEX IF NOT EXISTS idx_consent_audit_created ON consent_audit(created_at DESC);

-- Insert default consent purposes
INSERT INTO consent_purposes (name, description, required, legal_basis) VALUES
    ('marketing', 'Marketing communications and promotional offers', false, 'consent'),
    ('analytics', 'Analytics and performance monitoring', false, 'legitimate_interest'),
    ('data_sharing', 'Sharing data with third parties', false, 'consent'),
    ('service', 'Data processing for service delivery', true, 'contract'),
    ('cookies', 'Non-essential cookies', false, 'consent')
ON CONFLICT (name) DO NOTHING;

-- Comments for documentation
COMMENT ON TABLE consent_purposes IS 'GDPR Article 7 - Defines available consent purposes';
COMMENT ON TABLE user_consents IS 'GDPR Article 7 - Tracks user consent grants and withdrawals';
COMMENT ON TABLE consent_audit IS 'GDPR Article 7 - Complete audit trail of all consent actions';
COMMENT ON COLUMN user_consents.granted_at IS 'Timestamp when consent was granted';
COMMENT ON COLUMN user_consents.withdrawn_at IS 'Timestamp when consent was withdrawn';
COMMENT ON COLUMN consent_audit.action IS 'Action taken: grant, withdraw, or update';
