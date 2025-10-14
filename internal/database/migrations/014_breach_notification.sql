-- Migration 014: Data Breach Notification (GDPR Article 33 & 34)
-- Implements breach tracking, 72-hour deadline monitoring, and notification management

-- Breach records
CREATE TABLE IF NOT EXISTS breach_records (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    breach_type VARCHAR(50) NOT NULL,
    severity VARCHAR(20) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'detected',
    detected_at TIMESTAMPTZ NOT NULL,
    reported_at TIMESTAMPTZ,
    affected_user_count INT NOT NULL DEFAULT 0,
    affected_record_count INT NOT NULL DEFAULT 0,
    data_categories JSONB,
    description TEXT NOT NULL,
    root_cause TEXT,
    consequences TEXT,
    mitigation TEXT,
    notified_authority BOOLEAN DEFAULT false,
    notified_subjects BOOLEAN DEFAULT false,
    authority_notified_at TIMESTAMPTZ,
    subjects_notified_at TIMESTAMPTZ,
    deadline_at TIMESTAMPTZ NOT NULL, -- 72 hours from detection
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Affected users
CREATE TABLE IF NOT EXISTS breach_affected_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    breach_id UUID NOT NULL,
    user_id UUID NOT NULL,
    notified BOOLEAN DEFAULT false,
    notified_at TIMESTAMPTZ,
    method VARCHAR(50),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    FOREIGN KEY (breach_id) REFERENCES breach_records(id) ON DELETE CASCADE,
    UNIQUE(breach_id, user_id)
);

-- Breach notifications
CREATE TABLE IF NOT EXISTS breach_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    breach_id UUID NOT NULL,
    notification_type VARCHAR(50) NOT NULL, -- authority, subject
    recipient VARCHAR(255) NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL,
    method VARCHAR(50) NOT NULL, -- email, sms, dashboard, post
    status VARCHAR(50) NOT NULL,
    content TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    FOREIGN KEY (breach_id) REFERENCES breach_records(id) ON DELETE CASCADE
);

-- Indexes for performance and deadline monitoring
CREATE INDEX IF NOT EXISTS idx_breach_records_detected ON breach_records(detected_at DESC);
CREATE INDEX IF NOT EXISTS idx_breach_records_deadline ON breach_records(deadline_at) WHERE notified_authority = false;
CREATE INDEX IF NOT EXISTS idx_breach_records_severity ON breach_records(severity);
CREATE INDEX IF NOT EXISTS idx_breach_records_status ON breach_records(status);
CREATE INDEX IF NOT EXISTS idx_breach_affected_users_breach ON breach_affected_users(breach_id);
CREATE INDEX IF NOT EXISTS idx_breach_affected_users_user ON breach_affected_users(user_id);
CREATE INDEX IF NOT EXISTS idx_breach_notifications_breach ON breach_notifications(breach_id);
CREATE INDEX IF NOT EXISTS idx_breach_notifications_type ON breach_notifications(notification_type);

-- Comments for documentation
COMMENT ON TABLE breach_records IS 'GDPR Article 33 & 34 - Data breach incidents with 72-hour deadline tracking';
COMMENT ON TABLE breach_affected_users IS 'Users affected by data breaches requiring notification';
COMMENT ON TABLE breach_notifications IS 'Record of all breach notifications sent to authorities and subjects';
COMMENT ON COLUMN breach_records.deadline_at IS '72-hour deadline for authority notification per GDPR Article 33.1';
COMMENT ON COLUMN breach_records.severity IS 'Breach severity: low, medium, high, critical';
COMMENT ON COLUMN breach_records.status IS 'Breach status: detected, assessed, reported, mitigated, closed';
COMMENT ON COLUMN breach_notifications.notification_type IS 'Type: authority (DPA) or subject (affected individual)';
