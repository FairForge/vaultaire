# Step 344: Data Portability (GDPR Article 20)

## Overview
Implement GDPR Article 20 - Right to Data Portability
Allow users to export all their data in machine-readable formats.

## Requirements

### Export Formats:
1. Personal Data Export (JSON)
   - User profile
   - API keys
   - Usage history
   - Billing records
   - Audit logs

2. File Data Export
   - Direct S3 export (native format)
   - Archive export (tar.gz/zip)
   - Metadata export (JSON manifest)

3. Transfer Capability
   - Generate temporary access URLs
   - S3-compatible transfer
   - Direct transfer to another provider

## Database Schema
```sql
-- Track data portability requests
CREATE TABLE portability_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    request_type VARCHAR(50) NOT NULL, -- 'export', 'transfer'
    status VARCHAR(50) NOT NULL, -- 'pending', 'processing', 'ready', 'completed', 'failed'
    format VARCHAR(50), -- 'json', 'archive', 's3'
    export_url TEXT, -- Pre-signed URL for download
    expires_at TIMESTAMPTZ,
    transfer_destination TEXT, -- For direct transfers
    created_at TIMESTAMPTZ DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    metadata JSONB
);

CREATE INDEX idx_portability_user ON portability_requests(user_id);
CREATE INDEX idx_portability_status ON portability_requests(status);
Implementation
Phase 1: Personal Data Export

Export user profile
Export API keys (masked)
Export usage records
Export billing history
Export audit logs

Phase 2: File Data Export

List all user's files
Generate manifest
Create archive (optional)
Generate download URLs

Phase 3: Transfer Capability

Generate S3 credentials for migration
Support direct S3-to-S3 transfer
Provide migration tools

Phase 4: API & UI

POST /api/portability/export
GET /api/portability/requests/:id
Dashboard UI for requesting exports
Email notifications

Testing

Test export generation
Test download URLs
Test transfer capability
Test expiration
Load test with large datasets

Timeline

Day 1: Database schema + basic export
Day 2: File export + manifest
Day 3: API endpoints + tests
Day 4: UI + integration
Day 5: Testing + documentation
