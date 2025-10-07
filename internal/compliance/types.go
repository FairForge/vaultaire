package compliance

import (
	"time"

	"github.com/google/uuid"
)

// Legal bases for data processing (GDPR Article 6)
const (
	LegalBasisConsent            = "consent"
	LegalBasisContract           = "contract"
	LegalBasisLegalObligation    = "legal_obligation"
	LegalBasisVitalInterests     = "vital_interests"
	LegalBasisPublicTask         = "public_task"
	LegalBasisLegitimateInterest = "legitimate_interest"
)

// SAR statuses
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Deletion methods
const (
	DeletionMethodSoft      = "soft_delete"
	DeletionMethodHard      = "hard_delete"
	DeletionMethodAnonymize = "anonymize"
)

// ProcessingActivity represents a data processing activity (Article 30)
type ProcessingActivity struct {
	ID                   uuid.UUID
	ActivityName         string
	Purpose              string
	LegalBasis           string
	DataCategories       []string
	RetentionPeriod      time.Duration
	ThirdPartyProcessors []string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// SubjectAccessRequest represents a user data export request (Article 15)
type SubjectAccessRequest struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	RequestDate    time.Time
	CompletionDate *time.Time
	Status         string
	DataExportPath string
	FileCount      int
	TotalSizeBytes int64
	ErrorMessage   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// DeletionRequest represents a right to erasure request (Article 17)
type DeletionRequest struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	UserEmail      string
	RequestDate    time.Time
	CompletionDate *time.Time
	Status         string
	DeletionMethod string
	FilesDeleted   int
	ErrorMessage   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UserDataExport represents exported user data
type UserDataExport struct {
	UserID         uuid.UUID              `json:"user_id"`
	Email          string                 `json:"email"`
	Profile        map[string]interface{} `json:"profile"`
	Files          []FileMetadata         `json:"files"`
	AuditLogs      []AuditLogEntry        `json:"audit_logs"`
	BillingHistory []BillingRecord        `json:"billing_history"`
	ExportDate     time.Time              `json:"export_date"`
}

// FileMetadata for SAR export
type FileMetadata struct {
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
	LastModified time.Time `json:"last_modified"`
}

// AuditLogEntry for SAR export
type AuditLogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	IPAddress string    `json:"ip_address"`
	UserAgent string    `json:"user_agent"`
}

// BillingRecord for SAR export
type BillingRecord struct {
	Date        time.Time `json:"date"`
	Description string    `json:"description"`
	Amount      float64   `json:"amount"`
	Currency    string    `json:"currency"`
}
