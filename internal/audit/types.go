package audit

import (
	"time"

	"github.com/google/uuid"
)

// EventType represents the type of audit event
type EventType string

const (
	// Authentication events
	EventTypeLogin          EventType = "auth.login"
	EventTypeLogout         EventType = "auth.logout"
	EventTypeLoginFailed    EventType = "auth.login_failed"
	EventTypeMFAEnabled     EventType = "auth.mfa_enabled"
	EventTypeMFADisabled    EventType = "auth.mfa_disabled"
	EventTypePasswordChange EventType = "auth.password_change"

	// API Key events
	EventTypeAPIKeyCreated EventType = "apikey.created"
	EventTypeAPIKeyRevoked EventType = "apikey.revoked"
	EventTypeAPIKeyUsed    EventType = "apikey.used"

	// File operations
	EventTypeFileUpload   EventType = "file.upload"
	EventTypeFileDownload EventType = "file.download"
	EventTypeFileDelete   EventType = "file.delete"
	EventTypeFileList     EventType = "file.list"

	// Bucket operations
	EventTypeBucketCreate EventType = "bucket.create"
	EventTypeBucketDelete EventType = "bucket.delete"
	EventTypeBucketList   EventType = "bucket.list"

	// RBAC events
	EventTypeRoleAssigned      EventType = "rbac.role_assigned"
	EventTypeRoleRevoked       EventType = "rbac.role_revoked"
	EventTypePermissionGranted EventType = "rbac.permission_granted"
	EventTypePermissionRevoked EventType = "rbac.permission_revoked"

	// Admin events
	EventTypeUserCreated   EventType = "admin.user_created"
	EventTypeUserDeleted   EventType = "admin.user_deleted"
	EventTypeUserModified  EventType = "admin.user_modified"
	EventTypeQuotaModified EventType = "admin.quota_modified"

	// Security events
	EventTypeSecurityAlert      EventType = "security.alert"
	EventTypeAccessDenied       EventType = "security.access_denied"
	EventTypeSuspiciousActivity EventType = "security.suspicious"
)

// Result represents the result of an operation
type Result string

const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
	ResultDenied  Result = "denied"
)

// Severity represents the severity of an audit event
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// AuditEvent represents a single audit event
type AuditEvent struct {
	ID          uuid.UUID         `json:"id" db:"id"`
	Timestamp   time.Time         `json:"timestamp" db:"timestamp"`
	UserID      uuid.UUID         `json:"user_id" db:"user_id"`
	TenantID    string            `json:"tenant_id" db:"tenant_id"`
	EventType   EventType         `json:"event_type" db:"event_type"`
	Action      string            `json:"action" db:"action"`
	Resource    string            `json:"resource" db:"resource"`
	Result      Result            `json:"result" db:"result"`
	Severity    Severity          `json:"severity" db:"severity"`
	IP          string            `json:"ip,omitempty" db:"ip"`
	UserAgent   string            `json:"user_agent,omitempty" db:"user_agent"`
	Duration    time.Duration     `json:"duration,omitempty" db:"duration_ms"`
	ErrorMsg    string            `json:"error_msg,omitempty" db:"error_msg"`
	Metadata    map[string]string `json:"metadata,omitempty" db:"metadata"`
	PerformedBy uuid.UUID         `json:"performed_by,omitempty" db:"performed_by"`
}

// AuditQuery defines parameters for querying audit logs
type AuditQuery struct {
	UserID    *uuid.UUID `json:"user_id,omitempty"`
	TenantID  *string    `json:"tenant_id,omitempty"`
	EventType *EventType `json:"event_type,omitempty"`
	Resource  *string    `json:"resource,omitempty"`
	Result    *Result    `json:"result,omitempty"`
	Severity  *Severity  `json:"severity,omitempty"`
	StartTime *time.Time `json:"start_time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	Limit     int        `json:"limit"`
	Offset    int        `json:"offset"`
}

// Additional event types for compliance
const (
	EventTypeDataExport     EventType = "data.export"
	EventTypeDataDeletion   EventType = "data.deletion"
	EventTypeConsentGiven   EventType = "consent.given"
	EventTypeConsentRevoked EventType = "consent.revoked"
)

// SearchFilters represents search filter parameters
type SearchFilters struct {
	TenantID  string
	UserID    *uuid.UUID
	EventType EventType
	Result    Result
	Severity  Severity
	StartTime *time.Time
	EndTime   *time.Time
}
