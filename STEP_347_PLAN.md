# Step 347: Records of Processing Activities (ROPA) - GDPR Article 30

## Objective
Implement GDPR Article 30 compliant Records of Processing Activities (ROPA) to document all data processing operations, legal bases, data categories, and retention periods.

## GDPR Requirements

### Article 30: Records of Processing Activities
Organizations must maintain records of processing activities containing:

1. **Controller Information:**
   - Name and contact details of controller
   - Contact details of DPO (if applicable)
   - Purposes of processing

2. **Data Processing Details:**
   - Categories of data subjects (users, customers, employees, etc.)
   - Categories of personal data (email, name, payment info, etc.)
   - Categories of recipients (third parties, processors)
   - Transfers to third countries

3. **Legal & Technical:**
   - Legal basis for processing
   - Retention periods
   - Security measures description

4. **Documentation:**
   - Must be in writing (electronic form acceptable)
   - Available to supervisory authority on request
   - Regular reviews and updates

## Implementation Plan

### 1. Core Entities

**ProcessingActivity**
- Name, description, purpose
- Legal basis (consent, contract, legal obligation, etc.)
- Data controller information
- Data categories
- Data subject categories
- Recipients/processors
- Retention period
- Security measures
- Transfer details (if to third countries)
- Status (active, inactive, under review)

**DataCategory**
- Name (email, password, financial, health, etc.)
- Sensitivity level (low, medium, high)
- Special category (Article 9 data)

**DataSubjectCategory**
- Name (customers, employees, visitors, etc.)
- Description

**Recipient**
- Name, type (processor, controller, third party)
- Purpose of sharing
- Country (for transfer tracking)

### 2. Core Components

**ROPAService**
- CreateActivity(activity) - Register new processing
- UpdateActivity(activityID, updates) - Update existing
- GetActivity(activityID) - Retrieve details
- ListActivities(filters) - Query all activities
- DeleteActivity(activityID) - Mark as inactive
- ReviewActivity(activityID) - Mark as reviewed
- GenerateROPAReport() - Export full ROPA document
- ValidateCompliance(activityID) - Check completeness

**Legal Bases (Article 6.1)**
- consent
- contract
- legal_obligation
- vital_interests
- public_task
- legitimate_interests

**Special Category Processing (Article 9)**
- explicit_consent
- employment
- vital_interests
- legitimate_activities
- public_data
- legal_claims
- substantial_public_interest
- health_care
- public_health
- archiving

### 3. Database Schema
```sql
-- Processing activities
CREATE TABLE processing_activities (
    id UUID PRIMARY KEY,
    name VARCHAR(255),
    description TEXT,
    purpose TEXT,
    legal_basis VARCHAR(50),
    special_category_basis VARCHAR(100),
    controller_name VARCHAR(255),
    controller_contact VARCHAR(255),
    dpo_name VARCHAR(255),
    dpo_contact VARCHAR(255),
    retention_period VARCHAR(100),
    security_measures TEXT,
    status VARCHAR(20),
    last_reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

-- Data categories for each activity
CREATE TABLE activity_data_categories (
    id UUID PRIMARY KEY,
    activity_id UUID,
    category VARCHAR(100),
    sensitivity VARCHAR(20),
    is_special_category BOOLEAN,
    FOREIGN KEY (activity_id) REFERENCES processing_activities(id)
);

-- Data subject categories
CREATE TABLE activity_data_subjects (
    id UUID PRIMARY KEY,
    activity_id UUID,
    category VARCHAR(100),
    description TEXT,
    FOREIGN KEY (activity_id) REFERENCES processing_activities(id)
);

-- Recipients/processors
CREATE TABLE activity_recipients (
    id UUID PRIMARY KEY,
    activity_id UUID,
    name VARCHAR(255),
    type VARCHAR(50),
    purpose TEXT,
    country VARCHAR(100),
    FOREIGN KEY (activity_id) REFERENCES processing_activities(id)
);

-- Review history
CREATE TABLE activity_reviews (
    id UUID PRIMARY KEY,
    activity_id UUID,
    reviewed_by UUID,
    notes TEXT,
    reviewed_at TIMESTAMPTZ,
    FOREIGN KEY (activity_id) REFERENCES processing_activities(id)
);
4. API Endpoints
POST   /api/ropa/activities           - Create processing activity
GET    /api/ropa/activities/:id       - Get activity details
GET    /api/ropa/activities           - List all activities
PATCH  /api/ropa/activities/:id       - Update activity
DELETE /api/ropa/activities/:id       - Mark as inactive
POST   /api/ropa/activities/:id/review - Review activity
GET    /api/ropa/report               - Generate full ROPA report
GET    /api/ropa/compliance/:id       - Check compliance status
GET    /api/ropa/stats                - ROPA statistics
5. Features
Activity Templates

Pre-configured templates for common processing:

User registration
Email marketing
Payment processing
Analytics
Customer support



Compliance Validation

Check required fields
Validate legal basis
Check retention periods
Verify security measures documented
Ensure reviews are up-to-date (annually)

ROPA Report Generation

Export as PDF/JSON/CSV
Supervisory authority ready format
Complete documentation
Review history included

Review Reminders

Annual review tracking
Notification when review due
Review history audit trail

6. Testing Strategy

Unit tests for ROPAService
Activity CRUD tests
Compliance validation tests
Report generation tests
API endpoint tests
Concurrent access tests

Success Criteria
✅ Processing activity registration
✅ Full GDPR Article 30 compliance
✅ Legal basis validation
✅ Data category tracking
✅ Recipient/processor tracking
✅ Retention period documentation
✅ Security measures documentation
✅ Review tracking
✅ ROPA report generation
✅ Compliance validation
✅ All tests passing
✅ Linter clean
Files to Create/Modify
internal/compliance/ropa.go                    (new - service)
internal/compliance/ropa_test.go               (new - tests)
internal/compliance/api.go                     (add handlers)
internal/compliance/api_test.go                (add tests)
internal/compliance/types.go                   (add ROPA types)
internal/database/migrations/015_ropa.sql      (new)
Integration Points

User service (DPO, reviewers)
Audit log (activity changes)
Notification service (review reminders)
Export service (ROPA reports)

Example Processing Activities

User Registration

Purpose: Account creation
Legal basis: Contract
Data: email, name, password
Subjects: Website visitors, customers
Retention: Until account deletion + 30 days


Marketing Communications

Purpose: Promotional emails
Legal basis: Consent
Data: email, name, preferences
Subjects: Newsletter subscribers
Retention: Until consent withdrawn


Payment Processing

Purpose: Transaction processing
Legal basis: Contract
Data: payment info, billing address
Subjects: Customers
Recipients: Stripe (processor, USA)
Retention: 7 years (tax requirements)



Notes

ROPA required for organizations with 250+ employees OR regular/systematic processing
Vaultaire should maintain ROPA from day 1 as best practice
Must be available to supervisory authority on request
Should be reviewed annually
Links to other GDPR processes (consent, breaches, SARs)
