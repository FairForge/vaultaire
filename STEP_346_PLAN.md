# Step 346: Data Breach Notification (GDPR Article 33 & 34)

## Objective
Implement GDPR-compliant data breach notification system with 72-hour reporting, severity assessment, and affected user notification.

## GDPR Requirements

### Article 33: Notification of Breach to Supervisory Authority
1. Notification within 72 hours of becoming aware
2. Must include:
   - Nature of breach
   - Categories and number of affected data subjects
   - Categories and number of affected records
   - Likely consequences
   - Measures taken or proposed
3. Phased notification if full info not available
4. Documentation of all breaches

### Article 34: Communication to Data Subject
1. Notify affected individuals without undue delay if high risk
2. Must include:
   - Description of breach in clear language
   - Contact point for more information
   - Likely consequences
   - Measures taken or proposed
3. Exceptions when:
   - Encrypted data
   - Subsequent measures remove high risk
   - Communication requires disproportionate effort

## Implementation Plan

### 1. Breach Severity Classification
```go
// Breach severity levels
- low       // Minimal risk, no notification required
- medium    // Some risk, internal monitoring
- high      // Significant risk, notify authority
- critical  // Severe risk, notify authority + subjects
2. Core Components
BreachService

DetectBreach(ctx, details) - Record breach
AssessSeverity(breach) - Determine severity
NotifyAuthority(breach) - 72-hour notification
NotifySubjects(breach) - User notification
GetBreachStatus(breachID) - Status tracking
ListBreaches(filters) - Breach history
UpdateBreach(breachID, updates) - Status updates

BreachRecord

ID, DetectedAt, ReportedAt
BreachType (unauthorized_access, data_loss, etc.)
Severity, Status
AffectedUserCount, AffectedRecordCount
DataCategories
Description, RootCause
Consequences, Mitigation
NotifiedAuthority, NotifiedSubjects

BreachNotification

To authority or subjects
SentAt, Method, Status
Content, Recipients

3. Database Schema
sql-- Breach records
CREATE TABLE breach_records (
    id UUID PRIMARY KEY,
    breach_type VARCHAR(50),
    severity VARCHAR(20),
    status VARCHAR(20),
    detected_at TIMESTAMPTZ,
    reported_at TIMESTAMPTZ,
    affected_user_count INT,
    affected_record_count INT,
    data_categories JSONB,
    description TEXT,
    root_cause TEXT,
    consequences TEXT,
    mitigation TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

-- Affected users
CREATE TABLE breach_affected_users (
    id UUID PRIMARY KEY,
    breach_id UUID,
    user_id UUID,
    notified BOOLEAN,
    notified_at TIMESTAMPTZ,
    FOREIGN KEY (breach_id) REFERENCES breach_records(id)
);

-- Notifications
CREATE TABLE breach_notifications (
    id UUID PRIMARY KEY,
    breach_id UUID,
    notification_type VARCHAR(50), -- authority, subject
    recipient VARCHAR(255),
    sent_at TIMESTAMPTZ,
    method VARCHAR(50),
    status VARCHAR(50),
    content TEXT,
    FOREIGN KEY (breach_id) REFERENCES breach_records(id)
);
4. API Endpoints
POST   /api/breach                    - Report breach
GET    /api/breach/:id                - Get breach details
GET    /api/breach                    - List breaches
PATCH  /api/breach/:id                - Update breach
POST   /api/breach/:id/notify         - Send notifications
GET    /api/breach/:id/status         - Check 72-hour deadline
GET    /api/breach/stats              - Breach statistics
5. Features
72-Hour Deadline Tracking

Automatic countdown from detection
Alert when approaching deadline
Status tracking (detected, assessed, reported)

Severity Assessment

Affected user count
Data sensitivity
Potential consequences
Existing safeguards

Notification Templates

Authority notification (formal)
User notification (plain language)
Email, SMS, dashboard options

Breach Timeline

Detection → Assessment → Reporting → Mitigation
Full audit trail

6. Testing Strategy

Unit tests for BreachService
Severity assessment tests
72-hour deadline calculations
Notification tests
API endpoint tests

Success Criteria
✅ Breach detection and recording
✅ Severity assessment algorithm
✅ 72-hour deadline tracking
✅ Authority notification
✅ User notification (when required)
✅ Complete audit trail
✅ API for breach management
✅ All tests passing
✅ Linter clean
Files to Create/Modify
internal/compliance/breach.go              (new - service)
internal/compliance/breach_test.go         (new - tests)
internal/compliance/api.go                 (add handlers)
internal/compliance/api_test.go            (add tests)
internal/compliance/types.go               (add breach types)
internal/database/migrations/014_breach_notification.sql (new)
Notes

Breaches MUST be documented even if not reported
72-hour deadline is from "becoming aware"
High-risk breaches require subject notification
Encryption can exempt from subject notification
Keep records of all breaches (not just reported ones)
Authority contact: DPA (Data Protection Authority)

Integration Points

User service (affected users)
Email service (notifications)
Monitoring/alerting (detection)
Audit log (timeline)
