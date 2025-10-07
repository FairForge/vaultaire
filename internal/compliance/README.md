# GDPR Compliance Package

This package implements GDPR (General Data Protection Regulation) compliance features for Vaultaire.

## Features

### Article 15: Right of Access (Subject Access Requests)
Users can request all personal data held about them:
- `POST /api/compliance/sar` - Create a new SAR
- `GET /api/compliance/sar/:id` - Check SAR status
- Data export includes: profile, files, audit logs, billing history

### Article 17: Right to Erasure (Right to be Forgotten)
Users can request deletion of their data:
- `DELETE /api/compliance/user-data` - Request data deletion
- Supports soft delete, hard delete, and anonymization
- Preserves anonymized audit logs for compliance

### Article 30: Records of Processing Activities
Documentation of how personal data is processed:
- `GET /api/compliance/processing-activities` - List all activities
- Includes purpose, legal basis, retention periods

### Data Transparency
Users can see what data categories are stored:
- `GET /api/compliance/data-inventory` - View data categories

## Database Schema

Run migration `009_gdpr_compliance.sql` to create required tables:
- `gdpr_subject_access_requests` - Track SARs
- `gdpr_deletion_requests` - Track deletion requests
- `gdpr_processing_activities` - Article 30 records
- `gdpr_breach_log` - Data breach notifications

## Usage
```go
// Initialize service
gdprService := compliance.NewGDPRService(db, logger)
handler := compliance.NewAPIHandler(gdprService, logger)

// Add routes
server.setupComplianceRoutes(handler)

// Process SARs asynchronously
go func() {
    for sarID := range sarQueue {
        gdprService.ProcessSubjectAccessRequest(ctx, sarID)
    }
}()
Compliance Notes

SARs must be completed within 30 days (GDPR requirement)
Audit logs are preserved anonymously even after deletion
All operations are logged for compliance
Data exports are stored securely (0600 permissions)

Testing
bashgo test ./internal/compliance -v -cover
Current coverage: 39.1%
