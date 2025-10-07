package compliance

import (
	"time"

	"github.com/google/uuid"
)

// GDPR Request Types
const (
	RequestTypeAccess   = "access"
	RequestTypeDeletion = "deletion"
	RequestTypeExport   = "export"
)

// Request statuses (backwards compatibility)
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Deletion methods (backwards compatibility)
const (
	DeletionMethodSoft = "soft" // Mark as deleted
	DeletionMethodHard = "hard" // Permanent deletion
)

// Deletion request statuses
const (
	DeletionStatusPending    = "pending"
	DeletionStatusInProgress = "in_progress"
	DeletionStatusCompleted  = "completed"
	DeletionStatusFailed     = "failed"
	DeletionStatusCancelled  = "cancelled"
)

// Deletion scopes
const (
	DeletionScopeAll        = "all"        // Delete everything
	DeletionScopeContainers = "containers" // Delete specific containers
	DeletionScopeSpecific   = "specific"   // Delete specific files
)

// Proof types
const (
	ProofTypeFileDeleted     = "file_deleted"
	ProofTypeBackupDeleted   = "backup_deleted"
	ProofTypeMetadataCleared = "metadata_cleared"
	ProofTypeVersionDeleted  = "version_deleted"
)

// SubjectAccessRequest represents a GDPR Article 15 request
type SubjectAccessRequest struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	RequestDate time.Time
	Status      string
	DataPackage []byte // JSON data package
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// DeletionRequest represents a GDPR Article 17 deletion request
type DeletionRequest struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	RequestedBy     uuid.UUID
	UserEmail       string // For backwards compatibility with existing code
	RequestDate     time.Time
	Reason          string
	Scope           string
	ContainerFilter []string
	IncludeBackups  bool
	PreserveAudit   bool
	DeletionMethod  string // For backwards compatibility

	ScheduledFor *time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Status       string

	ItemsDeleted int64
	BytesDeleted int64
	ProofHash    string
	ErrorMessage string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeletionProof provides cryptographic evidence of deletion
type DeletionProof struct {
	ID        uuid.UUID
	RequestID uuid.UUID
	BackendID string
	Container string
	Artifact  string
	DeletedAt time.Time
	ProofType string
	ProofData map[string]interface{}
	CreatedAt time.Time
}

// DeletionCertificate is the final proof document
type DeletionCertificate struct {
	RequestID       uuid.UUID
	UserID          uuid.UUID
	CompletedAt     time.Time
	ItemsDeleted    int64
	BytesDeleted    int64
	Proofs          []DeletionProof
	CertificateHash string // SHA-256 of all proofs
}

// ProcessingActivity represents GDPR Article 30 processing record
type ProcessingActivity struct {
	ID          uuid.UUID
	Name        string
	Purpose     string
	DataTypes   []string
	LegalBasis  string
	Retention   string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DataInventoryItem represents a piece of stored user data
type DataInventoryItem struct {
	UserID    uuid.UUID
	DataType  string
	Location  string
	Purpose   string
	Retention string
	CreatedAt time.Time
}
