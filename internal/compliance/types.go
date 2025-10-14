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

// ============================================================================
// GDPR Request Types (Articles 15, 17, 20)
// ============================================================================

const (
	RequestTypeAccess   = "access"
	RequestTypeDeletion = "deletion"
	RequestTypeExport   = "export"
)

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// SubjectAccessRequest represents a GDPR Article 15 request
type SubjectAccessRequest struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	RequestDate time.Time
	Status      string
	DataPackage []byte
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// ============================================================================
// Deletion (Article 17 - Right to Erasure)
// ============================================================================

const (
	DeletionMethodSoft = "soft"
	DeletionMethodHard = "hard"
)

const (
	DeletionStatusPending    = "pending"
	DeletionStatusInProgress = "in_progress"
	DeletionStatusCompleted  = "completed"
	DeletionStatusFailed     = "failed"
	DeletionStatusCancelled  = "cancelled"
)

const (
	DeletionScopeAll        = "all"
	DeletionScopeContainers = "containers"
	DeletionScopeSpecific   = "specific"
)

const (
	ProofTypeFileDeleted     = "file_deleted"
	ProofTypeBackupDeleted   = "backup_deleted"
	ProofTypeMetadataCleared = "metadata_cleared"
	ProofTypeVersionDeleted  = "version_deleted"
)

type DeletionRequest struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	RequestedBy     uuid.UUID
	UserEmail       string
	RequestDate     time.Time
	Reason          string
	Scope           string
	ContainerFilter []string
	IncludeBackups  bool
	PreserveAudit   bool
	DeletionMethod  string
	ScheduledFor    *time.Time
	StartedAt       *time.Time
	CompletedAt     *time.Time
	Status          string
	ItemsDeleted    int64
	BytesDeleted    int64
	ProofHash       string
	ErrorMessage    string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

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

type DeletionCertificate struct {
	RequestID       uuid.UUID
	UserID          uuid.UUID
	CompletedAt     time.Time
	ItemsDeleted    int64
	BytesDeleted    int64
	Proofs          []DeletionProof
	CertificateHash string
}

// ============================================================================
// Data Portability (Article 20)
// ============================================================================

type PortabilityRequest struct {
	ID                  uuid.UUID              `json:"id"`
	UserID              uuid.UUID              `json:"user_id"`
	RequestType         string                 `json:"request_type"`
	Status              string                 `json:"status"`
	Format              string                 `json:"format"`
	ExportURL           string                 `json:"export_url,omitempty"`
	ExpiresAt           time.Time              `json:"expires_at"`
	TransferDestination string                 `json:"transfer_destination,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	CompletedAt         time.Time              `json:"completed_at,omitempty"`
	Metadata            map[string]interface{} `json:"metadata,omitempty"`
}

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

type PersonalData struct {
	UserID    uuid.UUID `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type APIKeyExport struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Masked    string    `json:"masked_key"`
	CreatedAt time.Time `json:"created_at"`
}

type FileMetadataExport struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	Uploaded    time.Time `json:"uploaded"`
	Modified    time.Time `json:"modified"`
	Container   string    `json:"container"`
	ContentType string    `json:"content_type,omitempty"`
}

type ContainerExport struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	FileCount int       `json:"file_count"`
	TotalSize int64     `json:"total_size"`
}

type S3Credentials struct {
	AccessKey  string    `json:"access_key"`
	SecretKey  string    `json:"secret_key"`
	Endpoint   string    `json:"endpoint"`
	Bucket     string    `json:"bucket"`
	Region     string    `json:"region,omitempty"`
	ValidUntil time.Time `json:"valid_until"`
}

type UsageRecord struct {
	Date          time.Time `json:"date"`
	BytesStored   int64     `json:"bytes_stored"`
	BytesTransfer int64     `json:"bytes_transfer"`
	APIRequests   int64     `json:"api_requests"`
}

// ============================================================================
// Consent Management (Articles 7 & 8)
// ============================================================================

const (
	ConsentPurposeMarketing       = "marketing"
	ConsentPurposeAnalytics       = "analytics"
	ConsentPurposeDataSharing     = "data_sharing"
	ConsentPurposeService         = "service"
	ConsentPurposeCookies         = "cookies"
	ConsentPurposeResearch        = "research"
	ConsentPurposeThirdParty      = "third_party"
	ConsentPurposePersonalization = "personalization"
)

const (
	ConsentMethodUI     = "ui"
	ConsentMethodAPI    = "api"
	ConsentMethodImport = "import"
	ConsentMethodAdmin  = "admin"
)

const (
	ConsentActionGrant    = "grant"
	ConsentActionWithdraw = "withdraw"
	ConsentActionUpdate   = "update"
)

type ConsentPurpose struct {
	ID          uuid.UUID
	Name        string
	Description string
	Required    bool
	LegalBasis  string
	CreatedAt   time.Time
}

type ConsentRecord struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	PurposeID    uuid.UUID
	Purpose      string
	Granted      bool
	GrantedAt    *time.Time
	WithdrawnAt  *time.Time
	Method       string
	IPAddress    string
	UserAgent    string
	TermsVersion string
	Metadata     map[string]interface{}
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type ConsentAuditEntry struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	PurposeID uuid.UUID
	Purpose   string
	Action    string
	Granted   bool
	Method    string
	IPAddress string
	UserAgent string
	Metadata  map[string]interface{}
	CreatedAt time.Time
}

type ConsentRequest struct {
	UserID       uuid.UUID
	Purpose      string
	Granted      bool
	Method       string
	IPAddress    string
	UserAgent    string
	TermsVersion string
	Metadata     map[string]interface{}
}

type ConsentStatus struct {
	UserID    uuid.UUID
	Consents  map[string]*ConsentRecord
	UpdatedAt time.Time
}

// ============================================================================
// Breach Notification (Articles 33 & 34)
// ============================================================================

const (
	BreachTypeUnauthorizedAccess = "unauthorized_access"
	BreachTypeDataLoss           = "data_loss"
	BreachTypeDataLeakage        = "data_leakage"
	BreachTypeRansomware         = "ransomware"
	BreachTypePhishing           = "phishing"
	BreachTypeInsiderThreat      = "insider_threat"
	BreachTypeSystemFailure      = "system_failure"
	BreachTypeThirdParty         = "third_party"
)

const (
	BreachSeverityLow      = "low"
	BreachSeverityMedium   = "medium"
	BreachSeverityHigh     = "high"
	BreachSeverityCritical = "critical"
)

const (
	BreachStatusDetected  = "detected"
	BreachStatusAssessed  = "assessed"
	BreachStatusReported  = "reported"
	BreachStatusMitigated = "mitigated"
	BreachStatusClosed    = "closed"
)

const (
	NotificationTypeAuthority = "authority"
	NotificationTypeSubject   = "subject"
)

const (
	NotificationMethodEmail     = "email"
	NotificationMethodSMS       = "sms"
	NotificationMethodDashboard = "dashboard"
	NotificationMethodPost      = "post"
)

type BreachRecord struct {
	ID                  uuid.UUID
	BreachType          string
	Severity            string
	Status              string
	DetectedAt          time.Time
	ReportedAt          *time.Time
	AffectedUserCount   int
	AffectedRecordCount int
	DataCategories      []string
	Description         string
	RootCause           string
	Consequences        string
	Mitigation          string
	NotifiedAuthority   bool
	NotifiedSubjects    bool
	AuthorityNotifiedAt *time.Time
	SubjectsNotifiedAt  *time.Time
	DeadlineAt          time.Time
	Metadata            map[string]interface{}
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type BreachAffectedUser struct {
	ID         uuid.UUID
	BreachID   uuid.UUID
	UserID     uuid.UUID
	Notified   bool
	NotifiedAt *time.Time
	Method     string
	CreatedAt  time.Time
}

type BreachNotification struct {
	ID               uuid.UUID
	BreachID         uuid.UUID
	NotificationType string
	Recipient        string
	SentAt           time.Time
	Method           string
	Status           string
	Content          string
	Metadata         map[string]interface{}
	CreatedAt        time.Time
}

type BreachRequest struct {
	BreachType          string      `json:"breach_type"`
	Description         string      `json:"description"`
	RootCause           string      `json:"root_cause"`
	DetectedAt          *time.Time  `json:"detected_at,omitempty"`
	AffectedUserCount   int         `json:"affected_user_count"`
	AffectedRecordCount int         `json:"affected_record_count"`
	DataCategories      []string    `json:"data_categories"`
	AffectedUserIDs     []uuid.UUID `json:"affected_user_ids,omitempty"`
}

type BreachAssessment struct {
	BreachID              uuid.UUID
	Severity              string
	RequiresAuthority     bool
	RequiresSubjects      bool
	RiskLevel             int
	DataSensitivity       int
	AffectedUserCount     int
	HasSafeguards         bool
	SafeguardsDescription string
	AssessedAt            time.Time
}

type BreachStats struct {
	TotalBreaches       int
	BreachesByType      map[string]int
	BreachesBySeverity  map[string]int
	BreachesByStatus    map[string]int
	WithinDeadline      int
	MissedDeadline      int
	AverageResponseTime time.Duration
}

// ============================================================================
// ROPA - Records of Processing Activities (Article 30)
// ============================================================================

const (
	LegalBasisConsent             = "consent"
	LegalBasisContract            = "contract"
	LegalBasisLegalObligation     = "legal_obligation"
	LegalBasisVitalInterests      = "vital_interests"
	LegalBasisPublicTask          = "public_task"
	LegalBasisLegitimateInterests = "legitimate_interests"
)

const (
	SpecialBasisExplicitConsent           = "explicit_consent"
	SpecialBasisEmployment                = "employment"
	SpecialBasisVitalInterests            = "vital_interests"
	SpecialBasisLegitimateActivities      = "legitimate_activities"
	SpecialBasisPublicData                = "public_data"
	SpecialBasisLegalClaims               = "legal_claims"
	SpecialBasisSubstantialPublicInterest = "substantial_public_interest"
	SpecialBasisHealthCare                = "health_care"
	SpecialBasisPublicHealth              = "public_health"
	SpecialBasisArchiving                 = "archiving"
)

const (
	ActivityStatusActive      = "active"
	ActivityStatusInactive    = "inactive"
	ActivityStatusUnderReview = "under_review"
	ActivityStatusDeprecated  = "deprecated"
)

const (
	SensitivityLow      = "low"
	SensitivityMedium   = "medium"
	SensitivityHigh     = "high"
	SensitivityCritical = "critical"
)

type ProcessingActivity struct {
	ID                    uuid.UUID
	Name                  string
	Description           string
	Purpose               string
	LegalBasis            string
	SpecialCategoryBasis  string
	ControllerName        string
	ControllerContact     string
	DPOName               string
	DPOContact            string
	DataCategories        []DataCategory
	DataSubjectCategories []DataSubjectCategory
	Recipients            []Recipient
	RetentionPeriod       string
	SecurityMeasures      string
	TransferDetails       string
	Status                string
	LastReviewedAt        *time.Time
	ReviewedBy            *uuid.UUID
	Metadata              map[string]interface{}
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type DataCategory struct {
	ID                uuid.UUID
	ActivityID        uuid.UUID
	Category          string
	Sensitivity       string
	IsSpecialCategory bool
	Description       string
}

type DataSubjectCategory struct {
	ID          uuid.UUID
	ActivityID  uuid.UUID
	Category    string
	Description string
}

type Recipient struct {
	ID         uuid.UUID
	ActivityID uuid.UUID
	Name       string
	Type       string
	Purpose    string
	Country    string
	Safeguards string
}

type ActivityReview struct {
	ID         uuid.UUID
	ActivityID uuid.UUID
	ReviewedBy uuid.UUID
	Notes      string
	ReviewedAt time.Time
}

type ProcessingActivityRequest struct {
	Name                  string
	Description           string
	Purpose               string
	LegalBasis            string
	SpecialCategoryBasis  string
	ControllerName        string
	ControllerContact     string
	DPOName               string
	DPOContact            string
	DataCategories        []string
	DataSubjectCategories []string
	Recipients            []RecipientRequest
	RetentionPeriod       string
	SecurityMeasures      string
	TransferDetails       string
}

type RecipientRequest struct {
	Name       string
	Type       string
	Purpose    string
	Country    string
	Safeguards string
}

type ROPAReport struct {
	GeneratedAt      time.Time
	OrganizationName string
	Activities       []*ProcessingActivity
	TotalActivities  int
	LastReviewDate   time.Time
	NextReviewDue    time.Time
}

type ComplianceCheck struct {
	ActivityID   uuid.UUID
	ActivityName string
	IsCompliant  bool
	Issues       []string
	Warnings     []string
	CheckedAt    time.Time
}

type ROPAStats struct {
	TotalActivities         int
	ActiveActivities        int
	InactiveActivities      int
	ActivitiesNeedingReview int
	ActivitiesByLegalBasis  map[string]int
	ActivitiesByStatus      map[string]int
	LastReviewDate          *time.Time
}

// ============================================================================
// Data Inventory (Article 30 supporting data)
// ============================================================================

type DataInventoryItem struct {
	UserID    uuid.UUID
	DataType  string
	Location  string
	Purpose   string
	Retention string
	CreatedAt time.Time
}

// ============================================================================
// Placeholder types (to be replaced with actual implementations)
// ============================================================================

type User struct {
	ID        uuid.UUID
	Email     string
	Name      string
	CreatedAt time.Time
}

type APIKey struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	Key       string
	CreatedAt time.Time
}

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

type Container struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	CreatedAt time.Time
}

// ============================================================================
// Database Interfaces
// ============================================================================

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

type ConsentDatabase interface {
	CreateConsentPurpose(ctx context.Context, purpose *ConsentPurpose) error
	GetConsentPurpose(ctx context.Context, name string) (*ConsentPurpose, error)
	ListConsentPurposes(ctx context.Context) ([]*ConsentPurpose, error)
	CreateConsent(ctx context.Context, consent *ConsentRecord) error
	UpdateConsent(ctx context.Context, consent *ConsentRecord) error
	GetConsent(ctx context.Context, userID uuid.UUID, purpose string) (*ConsentRecord, error)
	ListUserConsents(ctx context.Context, userID uuid.UUID) ([]*ConsentRecord, error)
	CreateConsentAudit(ctx context.Context, entry *ConsentAuditEntry) error
	GetConsentHistory(ctx context.Context, userID uuid.UUID) ([]*ConsentAuditEntry, error)
}

type BreachDatabase interface {
	CreateBreach(ctx context.Context, breach *BreachRecord) error
	GetBreach(ctx context.Context, breachID uuid.UUID) (*BreachRecord, error)
	UpdateBreach(ctx context.Context, breach *BreachRecord) error
	ListBreaches(ctx context.Context, filters map[string]interface{}) ([]*BreachRecord, error)
	AddAffectedUsers(ctx context.Context, breachID uuid.UUID, userIDs []uuid.UUID) error
	GetAffectedUsers(ctx context.Context, breachID uuid.UUID) ([]*BreachAffectedUser, error)
	UpdateAffectedUser(ctx context.Context, affected *BreachAffectedUser) error
	CreateNotification(ctx context.Context, notification *BreachNotification) error
	GetNotifications(ctx context.Context, breachID uuid.UUID) ([]*BreachNotification, error)
	GetBreachStats(ctx context.Context) (*BreachStats, error)
}

type ROPADatabase interface {
	CreateActivity(ctx context.Context, activity *ProcessingActivity) error
	GetActivity(ctx context.Context, activityID uuid.UUID) (*ProcessingActivity, error)
	UpdateActivity(ctx context.Context, activity *ProcessingActivity) error
	DeleteActivity(ctx context.Context, activityID uuid.UUID) error
	ListActivities(ctx context.Context, filters map[string]interface{}) ([]*ProcessingActivity, error)
	AddDataCategories(ctx context.Context, activityID uuid.UUID, categories []DataCategory) error
	GetDataCategories(ctx context.Context, activityID uuid.UUID) ([]DataCategory, error)
	AddDataSubjects(ctx context.Context, activityID uuid.UUID, subjects []DataSubjectCategory) error
	GetDataSubjects(ctx context.Context, activityID uuid.UUID) ([]DataSubjectCategory, error)
	AddRecipients(ctx context.Context, activityID uuid.UUID, recipients []Recipient) error
	GetRecipients(ctx context.Context, activityID uuid.UUID) ([]Recipient, error)
	CreateReview(ctx context.Context, review *ActivityReview) error
	GetReviews(ctx context.Context, activityID uuid.UUID) ([]*ActivityReview, error)
	GetROPAStats(ctx context.Context) (*ROPAStats, error)
}
