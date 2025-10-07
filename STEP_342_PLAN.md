# Step 342: Data Retention Policies

## Overview
Implement automated data retention policies to comply with GDPR Article 5(1)(e) - storage limitation principle. Data must not be kept longer than necessary.

## What We're Building

### 1. Retention Policy Engine
Automated cleanup of data based on configurable policies:
- Define retention periods per data type (files, logs, metadata)
- Automatic background cleanup jobs
- Grace period before permanent deletion
- Tenant-specific policy overrides

### 2. Legal Hold System
Prevent deletion during investigations:
- Place holds on user data
- Override retention policies
- Audit trail of all holds
- Automatic expiration

### 3. Policy Management
CRUD operations for policies:
- Create/update/delete policies
- Apply to existing data
- Policy templates (7 years audit, 30 days temp files)
- Dry-run mode to preview deletions

## Database Schema
```sql
-- Retention policies
CREATE TABLE retention_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    description TEXT,
    data_category VARCHAR(100) NOT NULL, -- files, audit_logs, user_data, backups
    retention_period INTERVAL NOT NULL,
    grace_period INTERVAL DEFAULT '7 days',
    action VARCHAR(50) DEFAULT 'delete', -- delete, archive, anonymize
    enabled BOOLEAN DEFAULT true,
    tenant_id VARCHAR(255), -- NULL means global policy
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Legal holds (prevent deletion)
CREATE TABLE legal_holds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    reason TEXT NOT NULL,
    case_number VARCHAR(100),
    created_by UUID NOT NULL,
    expires_at TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    status VARCHAR(50) DEFAULT 'active', -- active, expired, released
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Retention jobs (track cleanup operations)
CREATE TABLE retention_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id UUID REFERENCES retention_policies(id),
    started_at TIMESTAMPTZ DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    status VARCHAR(50) DEFAULT 'running', -- running, completed, failed
    items_scanned INTEGER DEFAULT 0,
    items_deleted INTEGER DEFAULT 0,
    items_skipped INTEGER DEFAULT 0, -- Due to legal hold
    error_message TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_retention_policy_category ON retention_policies(data_category, enabled);
CREATE INDEX idx_legal_holds_user ON legal_holds(user_id, status);
CREATE INDEX idx_retention_jobs_status ON retention_jobs(status, started_at);
API Endpoints
# Retention Policies
GET    /api/retention/policies           - List all policies
POST   /api/retention/policies           - Create policy
GET    /api/retention/policies/:id       - Get policy
PUT    /api/retention/policies/:id       - Update policy
DELETE /api/retention/policies/:id       - Delete policy
POST   /api/retention/policies/:id/run   - Manually trigger policy

# Legal Holds
POST   /api/retention/holds              - Create legal hold
GET    /api/retention/holds              - List active holds
DELETE /api/retention/holds/:id          - Release hold

# Jobs
GET    /api/retention/jobs               - List cleanup jobs
GET    /api/retention/jobs/:id           - Get job status
TDD Implementation Order
RED Phase

internal/retention/types.go - Data structures
internal/retention/policy_test.go - Policy tests
internal/retention/hold_test.go - Legal hold tests
internal/retention/cleanup_test.go - Cleanup job tests

GREEN Phase

internal/retention/policy.go - Policy management
internal/retention/hold.go - Legal hold system
internal/retention/cleanup.go - Cleanup engine
internal/retention/job.go - Background job runner

REFACTOR Phase

Add proper error handling
Add logging
Extract constants
Add API handlers

Success Criteria

 Policies can be created and managed
 Legal holds prevent deletion
 Cleanup jobs run automatically
 Dry-run mode works
 All operations logged to audit system
 Tests pass with >80% coverage
 Linter clean

Time Estimate
2-3 hours
