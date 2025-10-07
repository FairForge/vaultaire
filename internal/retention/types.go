package retention

import (
	"time"

	"github.com/google/uuid"
)

// Data categories
const (
	CategoryFiles     = "files"
	CategoryAuditLogs = "audit_logs"
	CategoryUserData  = "user_data"
	CategoryBackups   = "backups"
	CategoryTempFiles = "temp_files"
)

// Retention actions
const (
	ActionDelete    = "delete"
	ActionArchive   = "archive"
	ActionAnonymize = "anonymize"
)

// Legal hold statuses
const (
	HoldStatusActive   = "active"
	HoldStatusExpired  = "expired"
	HoldStatusReleased = "released"
)

// Job statuses
const (
	JobStatusRunning   = "running"
	JobStatusCompleted = "completed"
	JobStatusFailed    = "failed"
)

// RetentionPolicy defines how long data should be retained (backend-aware)
type RetentionPolicy struct {
	ID              uuid.UUID
	Name            string
	Description     string
	DataCategory    string
	RetentionPeriod time.Duration
	GracePeriod     time.Duration
	Action          string
	Enabled         bool

	// Scope
	TenantID      string // Empty = global
	BackendID     string // Empty = all backends
	ContainerName string // Empty = all containers

	// Backend feature toggles
	UseBackendObjectLock bool // Use Lyve/S3 Object Lock
	UseBackendVersioning bool // Use S3 versioning
	UseBackendLifecycle  bool // Use S3 lifecycle rules

	CreatedAt time.Time
	UpdatedAt time.Time
}

// LegalHold prevents deletion (backend-aware)
type LegalHold struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Reason     string
	CaseNumber string
	CreatedBy  uuid.UUID
	ExpiresAt  *time.Time
	ReleasedAt *time.Time
	Status     string

	// Backend-specific
	BackendID       string // Empty = all backends
	ApplyObjectLock bool   // Use S3 Object Lock if available

	CreatedAt time.Time
}

// BackendCapabilities tracks what features a backend supports
type BackendCapabilities struct {
	BackendID               string
	SupportsObjectLock      bool
	SupportsVersioning      bool
	SupportsLifecycle       bool
	SupportsLegalHold       bool
	SupportsRetentionPeriod bool
	LastChecked             time.Time
}

// RetentionJob tracks execution
type RetentionJob struct {
	ID           uuid.UUID
	PolicyID     *uuid.UUID
	BackendID    string // Which backend was processed
	StartedAt    time.Time
	CompletedAt  *time.Time
	Status       string
	ItemsScanned int
	ItemsDeleted int
	ItemsSkipped int
	DryRun       bool
	ErrorMessage string
	CreatedAt    time.Time
}

// CleanupResult summarizes what happened
type CleanupResult struct {
	ItemsScanned int
	ItemsDeleted int
	ItemsSkipped int
	Errors       []error
}
