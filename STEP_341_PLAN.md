# Step 341: GDPR Compliance Tools

## Overview
Implement GDPR compliance infrastructure to enable EU operations and provide users with their data rights.

## What We're Building

### 1. Data Inventory Service
Track what personal data we store and where:
- User profile data (email, name, preferences)
- Storage metadata (file names, timestamps)
- Audit logs (access history)
- Billing data (payment info)

### 2. Subject Access Request (SAR) Handler
Enable users to request their data:
- API endpoint: `POST /api/compliance/sar`
- Export all user data in JSON format
- Include data from all sources (DB, storage, audit logs)
- Generate within 30 days (GDPR requirement)

### 3. Data Processing Records
Document how we process personal data:
- Purpose of processing
- Legal basis (consent, contract, legitimate interest)
- Data retention periods
- Third-party processors

### 4. Right to Be Forgotten
Enable data deletion:
- API endpoint: `DELETE /api/compliance/user-data`
- Remove user from all systems
- Preserve anonymized audit logs for compliance

## Database Schema
```sql
-- Track data processing activities
CREATE TABLE gdpr_processing_activities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    activity_name VARCHAR(255) NOT NULL,
    purpose TEXT NOT NULL,
    legal_basis VARCHAR(50) NOT NULL, -- consent, contract, legitimate_interest
    data_categories TEXT[] NOT NULL,  -- email, name, files, etc
    retention_period INTERVAL NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Track subject access requests
CREATE TABLE gdpr_subject_access_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    request_date TIMESTAMPTZ DEFAULT NOW(),
    completion_date TIMESTAMPTZ,
    status VARCHAR(50) DEFAULT 'pending', -- pending, processing, completed, failed
    data_export_path TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Track deletion requests
CREATE TABLE gdpr_deletion_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    request_date TIMESTAMPTZ DEFAULT NOW(),
    completion_date TIMESTAMPTZ,
    status VARCHAR(50) DEFAULT 'pending',
    deletion_method VARCHAR(50), -- soft_delete, hard_delete, anonymize
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_sar_user ON gdpr_subject_access_requests(user_id);
CREATE INDEX idx_sar_status ON gdpr_subject_access_requests(status);
CREATE INDEX idx_deletion_user ON gdpr_deletion_requests(user_id);
TDD Implementation Order
RED Phase (Write Tests First)

internal/compliance/gdpr_test.go - Core GDPR service tests
internal/compliance/sar_test.go - Subject access request tests
internal/compliance/deletion_test.go - Right to be forgotten tests

GREEN Phase (Implement)

internal/compliance/gdpr.go - Core service
internal/compliance/sar.go - SAR handler
internal/compliance/deletion.go - Deletion handler
internal/compliance/inventory.go - Data inventory

REFACTOR Phase

Add proper error wrapping
Add structured logging
Extract magic numbers to constants
Add godoc comments

API Endpoints
POST   /api/compliance/sar              - Request data export
GET    /api/compliance/sar/:id          - Check SAR status
DELETE /api/compliance/user-data        - Request deletion
GET    /api/compliance/data-inventory   - View data categories (admin)
GET    /api/compliance/processing       - View processing activities (admin)
Integration Points

Audit System: Log all compliance requests
User Service: Fetch user data for SARs
Storage Engine: Enumerate user files
Billing: Export payment history
RBAC: Require appropriate permissions

Success Criteria

 Users can request their data via API
 SAR completes within 30 days
 Data export includes all sources
 Deletion removes all personal data
 Audit logs preserved (anonymized)
 All operations logged to audit system
 Tests pass with >80% coverage

Time Estimate
2-3 hours total
