# Step 343: Right to Deletion (GDPR Article 17)

## Overview
Implement GDPR Article 17 "Right to Erasure" - users can request complete deletion of their data with cryptographic proof of deletion.

## What We're Building

### 1. Deletion Request System
- User-initiated deletion requests
- Admin-initiated deletion (for compliance)
- Scheduled deletion (after retention period)
- Cascading deletion (all user data)

### 2. Verification & Proof
- Cryptographic proof of deletion
- Deletion certificates
- Audit trail of what was deleted
- Compliance reports

### 3. Deletion Jobs
- Background deletion workers
- Safe multi-backend deletion
- Verify deletion across all backends
- Handle deletion failures gracefully

## Features

### Immediate Deletion
```go
request := &DeletionRequest{
    UserID:      userID,
    Reason:      "User requested (Article 17)",
    Immediate:   true,  // Delete ASAP
    IncludeBackups: true,  // Delete backups too
}
Scheduled Deletion
gorequest := &DeletionRequest{
    UserID:      userID,
    ScheduledFor: time.Now().Add(30 * 24 * time.Hour), // 30-day grace
    Reason:      "Account closure",
}
Selective Deletion
gorequest := &DeletionRequest{
    UserID:      userID,
    Scope:       []string{"container-name"},  // Only this container
    PreserveAudit: true,  // Keep audit logs
}
Database Schema
sql-- Deletion requests
CREATE TABLE deletion_requests (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    requested_by UUID NOT NULL,
    reason TEXT NOT NULL,
    scope VARCHAR(50) DEFAULT 'all', -- all, containers, specific
    container_filter TEXT[], -- NULL = all containers
    scheduled_for TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    status VARCHAR(50) DEFAULT 'pending',
    proof_hash VARCHAR(64), -- SHA-256 of deletion proof
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Deletion proof (what was deleted)
CREATE TABLE deletion_proofs (
    id UUID PRIMARY KEY,
    request_id UUID REFERENCES deletion_requests(id),
    backend_id VARCHAR(255),
    container VARCHAR(255),
    artifact VARCHAR(512),
    deleted_at TIMESTAMPTZ,
    proof_type VARCHAR(50), -- file_deleted, backup_deleted, metadata_cleared
    proof_data JSONB, -- Details of deletion
    created_at TIMESTAMPTZ DEFAULT NOW()
);
API Endpoints
POST   /api/deletion/request          - Create deletion request
GET    /api/deletion/requests          - List user's requests
GET    /api/deletion/requests/:id      - Get request status
GET    /api/deletion/proof/:id         - Get deletion certificate
DELETE /api/deletion/requests/:id      - Cancel pending request
TDD Implementation
RED Phase

internal/compliance/deletion_test.go - Deletion tests
Test immediate deletion
Test scheduled deletion
Test selective deletion
Test deletion proof generation

GREEN Phase

internal/compliance/deletion.go - Deletion service
Implement deletion requests
Implement deletion jobs
Implement proof generation
Add API handlers

Success Criteria

 User can request data deletion
 Deletion cascades across all backends
 Cryptographic proof generated
 Legal holds prevent deletion
 Audit trail maintained
 Tests pass with >80% coverage

Time Estimate
1-2 hours
