package compliance

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Common errors
var (
	ErrNotFound = errors.New("not found")
)

// GDPR Request Types
const (
	RequestTypeAccess   = "access"
	RequestTypeDeletion = "deletion"
	RequestTypeExport   = "export"
)

// Request statuses (backwards compatibility)
const (
	StatusPending    = "pending"
	StatusProcessing = "processing" // Added for portability
	StatusReady      = "ready"      // Added for portability
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
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

// PortabilityRequest represents a data export/transfer request
type PortabilityRequest struct {
	ID                  uuid.UUID              `json:"id"`
	UserID              uuid.UUID              `json:"user_id"`
	RequestType         string                 `json:"request_type"` // 'export', 'transfer'
	Status              string                 `json:"status"`       // 'pending', 'processing', 'ready', 'completed', 'failed'
	Format              string                 `json:"format"`       // 'json', 'archive', 's3'
	ExportURL           string                 `json:"export_url,omitempty"`
	ExpiresAt           time.Time              `json:"expires_at"`
	TransferDestination string                 `json:"transfer_destination,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	CompletedAt         time.Time              `json:"completed_at,omitempty"`
	Metadata            map[string]interface{} `json:"metadata,omitempty"`
}

// UserDataExport represents the complete export of a user's data
type UserDataExport struct {
	ExportDate   time.Time            `json:"export_date"`
	Format       string               `json:"format"`
	Version      string               `json:"version"`
	PersonalData *PersonalData        `json:"personal_data"`
	APIKeys      []APIKeyExport       `json:"api_keys"`
	UsageRecords []UsageRecord        `json:"usage_records"`
	Files        []FileMetadataExport `json:"files"`
	Containers   []ContainerExport    `json:"containers"`
}

// PersonalData represents user's personal information
type PersonalData struct {
	UserID    uuid.UUID `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKeyExport represents an API key (masked)
type APIKeyExport struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Masked    string    `json:"masked_key"`
	CreatedAt time.Time `json:"created_at"`
}

// FileMetadataExport represents file metadata
type FileMetadataExport struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	Uploaded    time.Time `json:"uploaded"`
	Modified    time.Time `json:"modified"`
	Container   string    `json:"container"`
	ContentType string    `json:"content_type,omitempty"`
}

// ContainerExport represents a container's metadata
type ContainerExport struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	FileCount int       `json:"file_count"`
	TotalSize int64     `json:"total_size"`
}

// S3Credentials represents temporary S3 access credentials
type S3Credentials struct {
	AccessKey  string    `json:"access_key"`
	SecretKey  string    `json:"secret_key"`
	Endpoint   string    `json:"endpoint"`
	Bucket     string    `json:"bucket"`
	Region     string    `json:"region,omitempty"`
	ValidUntil time.Time `json:"valid_until"`
}

// UsageRecord represents a usage record for export
type UsageRecord struct {
	Date          time.Time `json:"date"`
	BytesStored   int64     `json:"bytes_stored"`
	BytesTransfer int64     `json:"bytes_transfer"`
	APIRequests   int64     `json:"api_requests"`
}

// PortabilityDatabase interface for portability operations
type PortabilityDatabase interface {
	CreatePortabilityRequest(ctx context.Context, req *PortabilityRequest) error
	GetPortabilityRequest(ctx context.Context, id uuid.UUID) (*PortabilityRequest, error)
	UpdatePortabilityRequest(ctx context.Context, req *PortabilityRequest) error
	ListPortabilityRequests(ctx context.Context, userID uuid.UUID) ([]*PortabilityRequest, error)
	GetUser(ctx context.Context, userID uuid.UUID) (*User, error)
	ListAPIKeys(ctx context.Context, userID uuid.UUID) ([]*APIKey, error)
	GetUsageRecords(ctx context.Context, userID uuid.UUID) ([]UsageRecord, error)
	ListFiles(ctx context.Context, userID uuid.UUID) ([]*FileMetadata, error)
	ListContainers(ctx context.Context, userID uuid.UUID) ([]*Container, error)
}

// User represents a user (placeholder - adjust to your actual user model)
type User struct {
	ID        uuid.UUID
	Email     string
	Name      string
	CreatedAt time.Time
}

// APIKey represents an API key (placeholder)
type APIKey struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	Key       string
	CreatedAt time.Time
}

// FileMetadata represents file metadata (placeholder)
type FileMetadata struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Path        string
	Size        int64
	Container   string
	ContentType string
	CreatedAt   time.Time
	ModifiedAt  time.Time
}

// Container represents a storage container (placeholder)
type Container struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	CreatedAt time.Time
}
