# Step 345: Consent Management (GDPR Article 7 & 8)

## Objective
Implement GDPR-compliant consent management system with granular controls, easy withdrawal, and complete audit trail.

## GDPR Requirements

### Article 7: Conditions for Consent
1. Freely given (no coercion)
2. Specific (per purpose)
3. Informed (clear information)
4. Unambiguous (affirmative action)
5. Withdrawable at any time
6. Burden of proof on controller

### Article 8: Child's Consent
1. Age verification
2. Parental consent for under 16 (or lower as per member state)
3. Reasonable efforts to verify

## Implementation Plan

### 1. Consent Types & Purposes
```go
// Consent purposes
- Marketing communications
- Analytics & performance
- Third-party data sharing
- Data processing for service delivery
- Cookies (essential, functional, analytics, marketing)

// Consent status
- granted
- denied
- withdrawn
- expired
2. Core Components
ConsentService

CreateConsent(userID, purpose, details)
WithdrawConsent(userID, purpose)
UpdateConsent(userID, purpose, granted)
GetConsentStatus(userID, purpose)
GetConsentHistory(userID)
CheckConsent(userID, purpose) bool

ConsentRecord

ID
UserID
Purpose (string enum)
Granted (bool)
GrantedAt
WithdrawnAt
Method (UI, API, import)
IPAddress
UserAgent
Version (terms version)
Metadata

ConsentAudit

Track all consent changes
Who, what, when, how
IP address and user agent
Terms version at time of consent

3. Database Schema
sql-- Consent purposes (categories)
CREATE TABLE consent_purposes (
    id UUID PRIMARY KEY,
    name VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    required BOOLEAN DEFAULT false,
    legal_basis VARCHAR(50),
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- User consents
CREATE TABLE user_consents (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    purpose_id UUID NOT NULL,
    granted BOOLEAN NOT NULL,
    granted_at TIMESTAMPTZ,
    withdrawn_at TIMESTAMPTZ,
    method VARCHAR(50),
    ip_address VARCHAR(45),
    user_agent TEXT,
    terms_version VARCHAR(20),
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    FOREIGN KEY (purpose_id) REFERENCES consent_purposes(id),
    UNIQUE(user_id, purpose_id)
);

-- Consent audit log
CREATE TABLE consent_audit (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    purpose_id UUID NOT NULL,
    action VARCHAR(50) NOT NULL,
    granted BOOLEAN,
    method VARCHAR(50),
    ip_address VARCHAR(45),
    user_agent TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Indexes
CREATE INDEX idx_user_consents_user ON user_consents(user_id);
CREATE INDEX idx_user_consents_purpose ON user_consents(purpose_id);
CREATE INDEX idx_consent_audit_user ON consent_audit(user_id);
CREATE INDEX idx_consent_audit_created ON consent_audit(created_at DESC);
4. API Endpoints
POST   /api/consent                    - Grant/update consent
DELETE /api/consent/:purpose           - Withdraw consent
GET    /api/consent                    - Get all user consents
GET    /api/consent/:purpose           - Get specific consent status
GET    /api/consent/history            - Get consent history
GET    /api/consent/purposes           - List available purposes
5. Features
Granular Consent

Per-purpose consent management
Required vs optional consents
Bundle management (e.g., "Accept all")

Easy Withdrawal

One-click withdrawal
Batch withdrawal
Effect takes immediate effect

Audit Trail

Every consent action logged
IP address and user agent captured
Terms version tracked
Metadata for additional context

Age Verification

Birth date capture
Age calculation
Parental consent flow (future)

6. Testing Strategy

Unit tests for ConsentService
API endpoint tests
Database tests
Race condition tests (concurrent consent updates)
Age verification tests

Success Criteria
✅ Granular consent per purpose
✅ Easy withdrawal mechanism
✅ Complete audit trail
✅ Age verification framework
✅ API for consent operations
✅ Database schema with indexes
✅ All tests passing
✅ Linter clean
Files to Create/Modify

internal/compliance/consent.go (new)
internal/compliance/consent_test.go (new)
internal/compliance/api.go (add handlers)
internal/compliance/api_test.go (add tests)
internal/compliance/types.go (add consent types)
internal/database/migrations/013_consent_management.sql (new)

Notes

Keep consents separate from user preferences
Withdrawal must be as easy as granting
Required consents must be clearly marked
Age verification for future enhancement
Consider cookie consent separately (future)
