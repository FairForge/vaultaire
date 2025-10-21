-- Privacy Controls for GDPR Article 25 (Data Protection by Design)
CREATE TABLE IF NOT EXISTS privacy_controls (
    id VARCHAR(255) PRIMARY KEY,
    type VARCHAR(50) NOT NULL,
    purpose TEXT,
    data_types TEXT[], -- Array of data types this control applies to
    enabled BOOLEAN DEFAULT true,
    config JSONB, -- Flexible configuration
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_privacy_controls_type ON privacy_controls(type);
CREATE INDEX idx_privacy_controls_enabled ON privacy_controls(enabled);

-- Data Minimization Policies
CREATE TABLE IF NOT EXISTS data_minimization_policies (
    id SERIAL PRIMARY KEY,
    purpose VARCHAR(255) UNIQUE NOT NULL,
    required_data TEXT[], -- Minimum required fields
    optional_data TEXT[], -- Optional fields user can consent to
    retention_days INTEGER DEFAULT 90,
    active BOOLEAN DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_minimization_purpose ON data_minimization_policies(purpose);

-- Purpose Bindings (enforce purpose limitation)
CREATE TABLE IF NOT EXISTS purpose_bindings (
    id SERIAL PRIMARY KEY,
    data_id VARCHAR(255) NOT NULL,
    purpose VARCHAR(255) NOT NULL,
    lawful_basis VARCHAR(50) NOT NULL,
    bound_at TIMESTAMPTZ DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    UNIQUE(data_id, purpose)
);

CREATE INDEX idx_purpose_bindings_data ON purpose_bindings(data_id);
CREATE INDEX idx_purpose_bindings_expires ON purpose_bindings(expires_at) WHERE expires_at IS NOT NULL;

-- Pseudonymization Mappings (reversible anonymization)
CREATE TABLE IF NOT EXISTS pseudonym_mappings (
    pseudonym VARCHAR(255) PRIMARY KEY,
    original_hash VARCHAR(64) NOT NULL, -- SHA256 of original value
    field_name VARCHAR(100) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_pseudonym_field ON pseudonym_mappings(field_name);
