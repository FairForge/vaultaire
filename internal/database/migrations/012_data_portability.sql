-- Migration: 012_data_portability.sql
-- Description: Add tables for GDPR Article 20 - Right to Data Portability

-- Track data portability requests
CREATE TABLE IF NOT EXISTS portability_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    request_type VARCHAR(50) NOT NULL CHECK (request_type IN ('export', 'transfer')),
    status VARCHAR(50) NOT NULL CHECK (status IN ('pending', 'processing', 'ready', 'completed', 'failed', 'expired')),
    format VARCHAR(50) CHECK (format IN ('json', 'archive', 's3')),
    export_url TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    transfer_destination TEXT,
    file_count INTEGER DEFAULT 0,
    total_size BIGINT DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    metadata JSONB,
    CONSTRAINT fk_portability_user FOREIGN KEY (user_id)
        REFERENCES users(id) ON DELETE CASCADE
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_portability_user_id ON portability_requests(user_id);
CREATE INDEX IF NOT EXISTS idx_portability_status ON portability_requests(status);
CREATE INDEX IF NOT EXISTS idx_portability_created_at ON portability_requests(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_portability_expires_at ON portability_requests(expires_at)
    WHERE status IN ('ready', 'pending');

-- Track export downloads (for audit)
CREATE TABLE IF NOT EXISTS portability_downloads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id UUID NOT NULL,
    user_id UUID NOT NULL,
    download_ip VARCHAR(45),
    user_agent TEXT,
    downloaded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_download_request FOREIGN KEY (request_id)
        REFERENCES portability_requests(id) ON DELETE CASCADE,
    CONSTRAINT fk_download_user FOREIGN KEY (user_id)
        REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_downloads_request ON portability_downloads(request_id);
CREATE INDEX IF NOT EXISTS idx_downloads_user ON portability_downloads(user_id);

-- Function to automatically expire old export URLs
CREATE OR REPLACE FUNCTION expire_old_exports()
RETURNS void AS $$
BEGIN
    UPDATE portability_requests
    SET status = 'expired',
        export_url = NULL
    WHERE status = 'ready'
        AND expires_at < NOW()
        AND export_url IS NOT NULL;
END;
$$ LANGUAGE plpgsql;

-- Add comment for documentation
COMMENT ON TABLE portability_requests IS 'GDPR Article 20 - Right to Data Portability: User data export requests';
COMMENT ON TABLE portability_downloads IS 'Audit log of data portability exports downloaded by users';
COMMENT ON COLUMN portability_requests.format IS 'Export format: json (structured data), archive (files as zip), s3 (direct S3 access)';
COMMENT ON COLUMN portability_requests.export_url IS 'Pre-signed URL for downloading the export (expires after 7 days)';
COMMENT ON COLUMN portability_requests.transfer_destination IS 'Destination for direct transfer (e.g., another S3 endpoint)';
