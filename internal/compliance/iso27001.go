// internal/compliance/iso27001.go
// ISO/IEC 27001:2022 Information Security Management System (ISMS)
// Implements Annex A controls mapping and ISMS documentation framework
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// ISO 27001:2022 Annex A Control Categories
// ============================================================================

// ISOControlCategory represents ISO 27001:2022 Annex A categories
type ISOControlCategory string

const (
	ISOCategoryOrganizational ISOControlCategory = "A.5" // Organizational Controls (37)
	ISOCategoryPeople         ISOControlCategory = "A.6" // People Controls (8)
	ISOCategoryPhysical       ISOControlCategory = "A.7" // Physical Controls (14)
	ISOCategoryTechnological  ISOControlCategory = "A.8" // Technological Controls (34)
)

// ISOControlAttribute represents control attributes per ISO 27001:2022
type ISOControlAttribute string

const (
	// Control Types
	AttrPreventive ISOControlAttribute = "Preventive"
	AttrDetective  ISOControlAttribute = "Detective"
	AttrCorrective ISOControlAttribute = "Corrective"

	// Security Properties (CIA)
	AttrConfidentiality ISOControlAttribute = "Confidentiality"
	AttrIntegrity       ISOControlAttribute = "Integrity"
	AttrAvailability    ISOControlAttribute = "Availability"

	// Cybersecurity Concepts
	AttrIdentify ISOControlAttribute = "Identify"
	AttrProtect  ISOControlAttribute = "Protect"
	AttrDetect   ISOControlAttribute = "Detect"
	AttrRespond  ISOControlAttribute = "Respond"
	AttrRecover  ISOControlAttribute = "Recover"

	// Operational Capabilities
	AttrGovernance       ISOControlAttribute = "Governance"
	AttrAssetManagement  ISOControlAttribute = "Asset_management"
	AttrInfoProtection   ISOControlAttribute = "Information_protection"
	AttrHRSecurity       ISOControlAttribute = "HR_security"
	AttrPhysicalSecurity ISOControlAttribute = "Physical_security"
	AttrSystemSecurity   ISOControlAttribute = "System_security"
	AttrNetworkSecurity  ISOControlAttribute = "Network_security"
	AttrAppSecurity      ISOControlAttribute = "Application_security"
	AttrSecureConfig     ISOControlAttribute = "Secure_configuration"
	AttrIdentityAccess   ISOControlAttribute = "Identity_access"
	AttrThreatVuln       ISOControlAttribute = "Threat_vulnerability"
	AttrContinuity       ISOControlAttribute = "Continuity"
	AttrSupplierSecurity ISOControlAttribute = "Supplier_security"
	AttrLegalCompliance  ISOControlAttribute = "Legal_compliance"
	AttrInfoSecEvents    ISOControlAttribute = "InfoSec_events"
	AttrInfoSecAssurance ISOControlAttribute = "InfoSec_assurance"
)

// ============================================================================
// ISO 27001 Control Types
// ============================================================================

// ISOControl represents an ISO 27001:2022 Annex A control
type ISOControl struct {
	ID              string                `json:"id"`               // e.g., "A.5.1"
	Category        ISOControlCategory    `json:"category"`         // A.5, A.6, A.7, A.8
	Name            string                `json:"name"`             // Control name
	Description     string                `json:"description"`      // What the control requires
	Purpose         string                `json:"purpose"`          // Why it matters
	Guidance        string                `json:"guidance"`         // Implementation guidance
	ControlTypes    []ISOControlAttribute `json:"control_types"`    // Preventive, Detective, Corrective
	SecurityProps   []ISOControlAttribute `json:"security_props"`   // CIA triad
	CyberConcepts   []ISOControlAttribute `json:"cyber_concepts"`   // NIST CSF alignment
	OperationalCaps []ISOControlAttribute `json:"operational_caps"` // Operational capability
	Status          ControlStatus         `json:"status"`           // Implementation status
	Owner           string                `json:"owner"`            // Responsible party
	Evidence        []ISOEvidence         `json:"evidence"`         // Supporting evidence
	SOC2Mapping     []string              `json:"soc2_mapping"`     // Related SOC2 controls
	LastAudit       *time.Time            `json:"last_audit"`       // Last audit date
	NextAudit       *time.Time            `json:"next_audit"`       // Next scheduled audit
	Notes           string                `json:"notes"`            // Implementation notes
}

// ISOEvidence represents evidence for ISO 27001 compliance
type ISOEvidence struct {
	ID          uuid.UUID    `json:"id"`
	ControlID   string       `json:"control_id"`
	Type        EvidenceType `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	DocumentRef string       `json:"document_ref"` // ISMS document reference
	Location    string       `json:"location"`
	CollectedAt time.Time    `json:"collected_at"`
	ValidUntil  *time.Time   `json:"valid_until"`
}

// ============================================================================
// ISMS Documentation Types
// ============================================================================

// ISMSDocument represents an ISMS document
type ISMSDocument struct {
	ID           uuid.UUID  `json:"id"`
	DocNumber    string     `json:"doc_number"` // e.g., "ISMS-POL-001"
	Title        string     `json:"title"`
	Type         string     `json:"type"` // Policy, Procedure, Standard, Guideline
	Version      string     `json:"version"`
	Status       string     `json:"status"` // Draft, Approved, Superseded
	Owner        string     `json:"owner"`
	ApprovedBy   string     `json:"approved_by"`
	ApprovedDate *time.Time `json:"approved_date"`
	ReviewDate   *time.Time `json:"review_date"`
	Controls     []string   `json:"controls"` // Controls this document addresses
	Content      string     `json:"content"`  // Document content or path
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ISMSRiskAssessment represents a risk assessment record
type ISMSRiskAssessment struct {
	ID              uuid.UUID  `json:"id"`
	AssetID         string     `json:"asset_id"`
	AssetName       string     `json:"asset_name"`
	ThreatSource    string     `json:"threat_source"`
	Vulnerability   string     `json:"vulnerability"`
	ExistingControl string     `json:"existing_control"`
	Likelihood      int        `json:"likelihood"` // 1-5
	Impact          int        `json:"impact"`     // 1-5
	RiskScore       int        `json:"risk_score"` // Likelihood × Impact
	RiskLevel       string     `json:"risk_level"` // Low, Medium, High, Critical
	Treatment       string     `json:"treatment"`  // Accept, Mitigate, Transfer, Avoid
	TreatmentPlan   string     `json:"treatment_plan"`
	ResidualRisk    int        `json:"residual_risk"`
	Owner           string     `json:"owner"`
	DueDate         *time.Time `json:"due_date"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// StatementOfApplicability represents the SoA
type StatementOfApplicability struct {
	ID           uuid.UUID         `json:"id"`
	Version      string            `json:"version"`
	ApprovedBy   string            `json:"approved_by"`
	ApprovedDate *time.Time        `json:"approved_date"`
	Controls     []SoAControlEntry `json:"controls"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// SoAControlEntry represents a control entry in the SoA
type SoAControlEntry struct {
	ControlID     string `json:"control_id"`
	ControlName   string `json:"control_name"`
	Applicable    bool   `json:"applicable"`
	Justification string `json:"justification"` // Why applicable or not
	Implemented   bool   `json:"implemented"`
	Evidence      string `json:"evidence"`
}

// ============================================================================
// ISO 27001 Service
// ============================================================================

// ISO27001Service manages ISO 27001 compliance
type ISO27001Service struct {
	controls  map[string]*ISOControl
	documents map[string]*ISMSDocument
	risks     map[string]*ISMSRiskAssessment
	soa       *StatementOfApplicability
	mu        sync.RWMutex
}

// NewISO27001Service creates a new ISO 27001 service
func NewISO27001Service() *ISO27001Service {
	s := &ISO27001Service{
		controls:  make(map[string]*ISOControl),
		documents: make(map[string]*ISMSDocument),
		risks:     make(map[string]*ISMSRiskAssessment),
	}
	s.initializeControls()
	return s
}

// initializeControls sets up Annex A controls relevant to cloud storage
func (s *ISO27001Service) initializeControls() {
	// A.5 - Organizational Controls
	s.addControl(&ISOControl{
		ID:              "A.5.1",
		Category:        ISOCategoryOrganizational,
		Name:            "Policies for information security",
		Description:     "Information security policy and topic-specific policies shall be defined, approved, published, communicated, and reviewed",
		Purpose:         "Ensure management direction and support for information security",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrIdentify},
		OperationalCaps: []ISOControlAttribute{AttrGovernance},
		Status:          ControlStatusImplemented,
		Owner:           "Security Team",
		SOC2Mapping:     []string{"CC1.1", "CC2.1"},
	})

	s.addControl(&ISOControl{
		ID:              "A.5.2",
		Category:        ISOCategoryOrganizational,
		Name:            "Information security roles and responsibilities",
		Description:     "Information security roles and responsibilities shall be defined and allocated",
		Purpose:         "Establish clear accountability for information security",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrIdentify},
		OperationalCaps: []ISOControlAttribute{AttrGovernance},
		Status:          ControlStatusImplemented,
		Owner:           "Executive Team",
		SOC2Mapping:     []string{"CC1.2"},
	})

	s.addControl(&ISOControl{
		ID:              "A.5.7",
		Category:        ISOCategoryOrganizational,
		Name:            "Threat intelligence",
		Description:     "Information relating to information security threats shall be collected and analyzed",
		Purpose:         "Provide awareness of threat environment",
		ControlTypes:    []ISOControlAttribute{AttrPreventive, AttrDetective},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrIdentify, AttrDetect},
		OperationalCaps: []ISOControlAttribute{AttrThreatVuln},
		Status:          ControlStatusPartial,
		Owner:           "Security Team",
		SOC2Mapping:     []string{"CC3.1", "CC7.1"},
	})

	s.addControl(&ISOControl{
		ID:              "A.5.10",
		Category:        ISOCategoryOrganizational,
		Name:            "Acceptable use of information",
		Description:     "Rules for acceptable use of information and assets shall be identified, documented, and implemented",
		Purpose:         "Ensure proper use of information",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrInfoProtection},
		Status:          ControlStatusImplemented,
		Owner:           "Security Team",
		SOC2Mapping:     []string{"CC5.1"},
	})

	s.addControl(&ISOControl{
		ID:              "A.5.15",
		Category:        ISOCategoryOrganizational,
		Name:            "Access control",
		Description:     "Rules to control physical and logical access shall be established based on business and security requirements",
		Purpose:         "Ensure authorized access and prevent unauthorized access",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrIdentityAccess},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.1", "CC6.2", "CC6.3"},
		Notes:           "Implemented via internal/rbac and internal/auth packages",
	})

	s.addControl(&ISOControl{
		ID:              "A.5.17",
		Category:        ISOCategoryOrganizational,
		Name:            "Authentication information",
		Description:     "Allocation and management of authentication information shall be controlled",
		Purpose:         "Ensure secure authentication",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrIdentityAccess},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.1"},
		Notes:           "HMAC-SHA256 for S3 API, API keys for REST",
	})

	s.addControl(&ISOControl{
		ID:              "A.5.23",
		Category:        ISOCategoryOrganizational,
		Name:            "Information security for cloud services",
		Description:     "Processes for acquisition, use, management, and exit from cloud services shall be established",
		Purpose:         "Ensure security of cloud services",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrIdentify, AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrSupplierSecurity},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC9.2"},
		Notes:           "Multi-backend architecture: Lyve, Geyser, Quotaless",
	})

	s.addControl(&ISOControl{
		ID:              "A.5.24",
		Category:        ISOCategoryOrganizational,
		Name:            "Information security incident management planning",
		Description:     "Management responsibilities and procedures shall be established for incident response",
		Purpose:         "Ensure consistent and effective incident response",
		ControlTypes:    []ISOControlAttribute{AttrCorrective},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrRespond},
		OperationalCaps: []ISOControlAttribute{AttrInfoSecEvents},
		Status:          ControlStatusImplemented,
		Owner:           "Security Team",
		SOC2Mapping:     []string{"CC7.3", "CC7.4"},
		Notes:           "Breach notification via internal/compliance/breach.go",
	})

	s.addControl(&ISOControl{
		ID:              "A.5.28",
		Category:        ISOCategoryOrganizational,
		Name:            "Collection of evidence",
		Description:     "Procedures for identification, collection, acquisition, and preservation of evidence shall be established",
		Purpose:         "Support legal/disciplinary actions",
		ControlTypes:    []ISOControlAttribute{AttrDetective},
		SecurityProps:   []ISOControlAttribute{AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrRespond},
		OperationalCaps: []ISOControlAttribute{AttrInfoSecEvents},
		Status:          ControlStatusImplemented,
		Owner:           "Security Team",
		SOC2Mapping:     []string{"CC7.2"},
		Notes:           "Implemented via internal/audit package with SHA256 hashing",
	})

	s.addControl(&ISOControl{
		ID:              "A.5.31",
		Category:        ISOCategoryOrganizational,
		Name:            "Legal, statutory, regulatory, and contractual requirements",
		Description:     "Legal and contractual requirements relevant to information security shall be identified and documented",
		Purpose:         "Ensure legal compliance",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrIdentify},
		OperationalCaps: []ISOControlAttribute{AttrLegalCompliance},
		Status:          ControlStatusImplemented,
		Owner:           "Legal Team",
		SOC2Mapping:     []string{"CC2.1"},
		Notes:           "GDPR compliance via internal/compliance/gdpr.go",
	})

	s.addControl(&ISOControl{
		ID:              "A.5.34",
		Category:        ISOCategoryOrganizational,
		Name:            "Privacy and protection of PII",
		Description:     "Privacy and protection of PII shall be ensured as required by applicable laws",
		Purpose:         "Ensure privacy compliance",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrLegalCompliance},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"P1.1", "P3.1", "P4.1", "P6.1"},
		Notes:           "GDPR Articles 15, 17, 20 via internal/compliance",
	})

	// A.6 - People Controls
	s.addControl(&ISOControl{
		ID:              "A.6.3",
		Category:        ISOCategoryPeople,
		Name:            "Information security awareness, education, and training",
		Description:     "Personnel shall receive appropriate security awareness education and training",
		Purpose:         "Ensure personnel can perform security duties",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrHRSecurity},
		Status:          ControlStatusPartial,
		Owner:           "HR Team",
		SOC2Mapping:     []string{"CC1.1"},
		Notes:           "Solo founder - formal training program needed at scale",
	})

	// A.7 - Physical Controls
	s.addControl(&ISOControl{
		ID:              "A.7.1",
		Category:        ISOCategoryPhysical,
		Name:            "Physical security perimeters",
		Description:     "Security perimeters shall be defined and used to protect sensitive information",
		Purpose:         "Prevent unauthorized physical access",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrPhysicalSecurity},
		Status:          ControlStatusImplemented,
		Owner:           "Operations",
		SOC2Mapping:     []string{"CC6.4"},
		Notes:           "Physical security handled by datacenter providers (ReliableSite, Terabit)",
	})

	// A.8 - Technological Controls
	s.addControl(&ISOControl{
		ID:              "A.8.1",
		Category:        ISOCategoryTechnological,
		Name:            "User endpoint devices",
		Description:     "Information stored on, processed by, or accessible via endpoint devices shall be protected",
		Purpose:         "Protect endpoint data",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrAssetManagement},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.8"},
		Notes:           "Client-side encryption option for zero-knowledge",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.2",
		Category:        ISOCategoryTechnological,
		Name:            "Privileged access rights",
		Description:     "Allocation and use of privileged access rights shall be restricted and managed",
		Purpose:         "Prevent unauthorized privileged access",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrIdentityAccess},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.1", "CC6.2"},
		Notes:           "RBAC via internal/rbac package",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.5",
		Category:        ISOCategoryTechnological,
		Name:            "Secure authentication",
		Description:     "Secure authentication technologies and procedures shall be implemented",
		Purpose:         "Verify user identity",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrIdentityAccess},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.1"},
		Notes:           "AWS Signature V4 compatible authentication",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.9",
		Category:        ISOCategoryTechnological,
		Name:            "Configuration management",
		Description:     "Configurations including security configurations shall be established, documented, and managed",
		Purpose:         "Ensure secure system configurations",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrSecureConfig},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC8.1"},
		Notes:           "Git-based configuration management",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.12",
		Category:        ISOCategoryTechnological,
		Name:            "Data leakage prevention",
		Description:     "Data leakage prevention measures shall be applied to prevent unauthorized disclosure",
		Purpose:         "Prevent data breaches",
		ControlTypes:    []ISOControlAttribute{AttrPreventive, AttrDetective},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality},
		CyberConcepts:   []ISOControlAttribute{AttrProtect, AttrDetect},
		OperationalCaps: []ISOControlAttribute{AttrInfoProtection},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"C1.1"},
		Notes:           "Tenant isolation with t-{tenantID} prefix",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.15",
		Category:        ISOCategoryTechnological,
		Name:            "Logging",
		Description:     "Logs that record activities, exceptions, faults, and security events shall be produced and protected",
		Purpose:         "Enable security monitoring and forensics",
		ControlTypes:    []ISOControlAttribute{AttrDetective},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrDetect},
		OperationalCaps: []ISOControlAttribute{AttrInfoSecEvents},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC7.2"},
		Notes:           "Comprehensive audit logging via internal/audit",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.16",
		Category:        ISOCategoryTechnological,
		Name:            "Monitoring activities",
		Description:     "Networks, systems, and applications shall be monitored for anomalous behavior",
		Purpose:         "Detect security events",
		ControlTypes:    []ISOControlAttribute{AttrDetective},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrDetect},
		OperationalCaps: []ISOControlAttribute{AttrInfoSecEvents},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC4.1", "CC7.2"},
		Notes:           "Metrics and alerting via internal/gateway/metrics",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.20",
		Category:        ISOCategoryTechnological,
		Name:            "Networks security",
		Description:     "Networks and network devices shall be secured, managed, and controlled",
		Purpose:         "Protect network infrastructure",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrNetworkSecurity},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.6"},
		Notes:           "TLS 1.3, firewall rules on all servers",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.24",
		Category:        ISOCategoryTechnological,
		Name:            "Use of cryptography",
		Description:     "Rules for effective use of cryptography shall be defined and implemented",
		Purpose:         "Protect data confidentiality and integrity",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrInfoProtection},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC6.7"},
		Notes:           "AES-256-GCM at rest, TLS 1.3 in transit, client-side encryption option",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.25",
		Category:        ISOCategoryTechnological,
		Name:            "Secure development life cycle",
		Description:     "Rules for secure development shall be established and applied",
		Purpose:         "Ensure secure software development",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrAppSecurity},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC8.1"},
		Notes:           "TDD, code review, linting, pre-commit hooks",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.28",
		Category:        ISOCategoryTechnological,
		Name:            "Secure coding",
		Description:     "Secure coding principles shall be applied to software development",
		Purpose:         "Prevent security vulnerabilities",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrConfidentiality, AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrAppSecurity},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC8.1"},
		Notes:           "Go's memory safety, input validation, error handling standards",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.31",
		Category:        ISOCategoryTechnological,
		Name:            "Separation of development, test, and production environments",
		Description:     "Development, testing, and production environments shall be separated",
		Purpose:         "Prevent unauthorized changes to production",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrSecureConfig},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC8.1"},
		Notes:           "Separate dev server (MaximumSettings) from production (ReliableSite)",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.32",
		Category:        ISOCategoryTechnological,
		Name:            "Change management",
		Description:     "Changes to systems shall be subject to change management procedures",
		Purpose:         "Ensure controlled changes",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrIntegrity, AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrSecureConfig},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC8.1"},
		Notes:           "GitHub PR workflow with branch protection",
	})

	s.addControl(&ISOControl{
		ID:              "A.8.34",
		Category:        ISOCategoryTechnological,
		Name:            "Protection of information systems during audit testing",
		Description:     "Audit tests shall be planned to minimize impact on operational systems",
		Purpose:         "Ensure audit testing doesn't disrupt operations",
		ControlTypes:    []ISOControlAttribute{AttrPreventive},
		SecurityProps:   []ISOControlAttribute{AttrAvailability},
		CyberConcepts:   []ISOControlAttribute{AttrProtect},
		OperationalCaps: []ISOControlAttribute{AttrInfoSecAssurance},
		Status:          ControlStatusImplemented,
		Owner:           "Engineering Team",
		SOC2Mapping:     []string{"CC4.1"},
		Notes:           "Separate test environment, read-only audit access",
	})
}

func (s *ISO27001Service) addControl(c *ISOControl) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controls[c.ID] = c
}

// ============================================================================
// ISO 27001 Service Methods
// ============================================================================

// GetControl returns a specific control
func (s *ISO27001Service) GetControl(id string) (*ISOControl, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	control, exists := s.controls[id]
	if !exists {
		return nil, fmt.Errorf("ISO control %s: %w", id, ErrNotFound)
	}
	return control, nil
}

// ListControls returns all controls, optionally filtered by category
func (s *ISO27001Service) ListControls(category *ISOControlCategory) []*ISOControl {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*ISOControl
	for _, c := range s.controls {
		if category == nil || c.Category == *category {
			result = append(result, c)
		}
	}
	return result
}

// ListControlsBySOC2 returns ISO controls mapped to a SOC2 control
func (s *ISO27001Service) ListControlsBySOC2(soc2ControlID string) []*ISOControl {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*ISOControl
	for _, c := range s.controls {
		for _, mapping := range c.SOC2Mapping {
			if mapping == soc2ControlID {
				result = append(result, c)
				break
			}
		}
	}
	return result
}

// UpdateControlStatus updates a control's status
func (s *ISO27001Service) UpdateControlStatus(ctx context.Context, controlID string, status ControlStatus, notes string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	control, exists := s.controls[controlID]
	if !exists {
		return fmt.Errorf("ISO control %s: %w", controlID, ErrNotFound)
	}

	control.Status = status
	if notes != "" {
		control.Notes = notes
	}
	now := time.Now()
	control.LastAudit = &now

	return nil
}

// AddEvidence adds evidence to a control
func (s *ISO27001Service) AddEvidence(ctx context.Context, controlID string, evidence ISOEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	control, exists := s.controls[controlID]
	if !exists {
		return fmt.Errorf("ISO control %s: %w", controlID, ErrNotFound)
	}

	evidence.ID = uuid.New()
	evidence.ControlID = controlID
	evidence.CollectedAt = time.Now()

	control.Evidence = append(control.Evidence, evidence)
	return nil
}

// ============================================================================
// Statement of Applicability (SoA)
// ============================================================================

// GenerateSoA generates the Statement of Applicability
func (s *ISO27001Service) GenerateSoA(ctx context.Context, approver string) (*StatementOfApplicability, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	soa := &StatementOfApplicability{
		ID:           uuid.New(),
		Version:      "1.0",
		ApprovedBy:   approver,
		ApprovedDate: &now,
		Controls:     make([]SoAControlEntry, 0, len(s.controls)),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	for _, control := range s.controls {
		entry := SoAControlEntry{
			ControlID:     control.ID,
			ControlName:   control.Name,
			Applicable:    true, // All initialized controls are applicable
			Justification: control.Purpose,
			Implemented: control.Status == ControlStatusImplemented ||
				control.Status == ControlStatusTested ||
				control.Status == ControlStatusEffective,
			Evidence: control.Notes,
		}
		soa.Controls = append(soa.Controls, entry)
	}

	s.mu.RUnlock()
	s.mu.Lock()
	s.soa = soa
	s.mu.Unlock()
	s.mu.RLock()

	return soa, nil
}

// ============================================================================
// Risk Assessment
// ============================================================================

// CreateRiskAssessment creates a new risk assessment
func (s *ISO27001Service) CreateRiskAssessment(ctx context.Context, assessment *ISMSRiskAssessment) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	assessment.ID = uuid.New()
	assessment.RiskScore = assessment.Likelihood * assessment.Impact
	assessment.RiskLevel = calculateRiskLevel(assessment.RiskScore)
	assessment.CreatedAt = time.Now()
	assessment.UpdatedAt = time.Now()
	assessment.Status = "Open"

	s.risks[assessment.ID.String()] = assessment
	return nil
}

// ListRisks returns all risk assessments
func (s *ISO27001Service) ListRisks(status *string) []*ISMSRiskAssessment {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*ISMSRiskAssessment
	for _, r := range s.risks {
		if status == nil || r.Status == *status {
			result = append(result, r)
		}
	}
	return result
}

func calculateRiskLevel(score int) string {
	switch {
	case score >= 20:
		return "Critical"
	case score >= 12:
		return "High"
	case score >= 6:
		return "Medium"
	default:
		return "Low"
	}
}

// ============================================================================
// Compliance Report Generation
// ============================================================================

// ISO27001Report represents an ISO 27001 compliance report
type ISO27001Report struct {
	GeneratedAt       time.Time                 `json:"generated_at"`
	TotalControls     int                       `json:"total_controls"`
	ImplementedCount  int                       `json:"implemented_count"`
	PartialCount      int                       `json:"partial_count"`
	NotImplemented    int                       `json:"not_implemented_count"`
	CompliancePercent float64                   `json:"compliance_percent"`
	ByCategory        map[string]CategoryStats  `json:"by_category"`
	SOC2Overlap       int                       `json:"soc2_overlap_count"`
	GapCount          int                       `json:"gap_count"`
	RiskCount         int                       `json:"risk_count"`
	SoA               *StatementOfApplicability `json:"soa,omitempty"`
}

// CategoryStats represents stats for a control category
type CategoryStats struct {
	Total       int     `json:"total"`
	Implemented int     `json:"implemented"`
	Percent     float64 `json:"percent"`
}

// GenerateReport generates an ISO 27001 compliance report
func (s *ISO27001Service) GenerateReport(ctx context.Context, includeSoA bool) (*ISO27001Report, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	report := &ISO27001Report{
		GeneratedAt: time.Now(),
		ByCategory:  make(map[string]CategoryStats),
	}

	categoryStats := make(map[ISOControlCategory]*CategoryStats)

	for _, control := range s.controls {
		report.TotalControls++

		// Initialize category stats if needed
		if _, exists := categoryStats[control.Category]; !exists {
			categoryStats[control.Category] = &CategoryStats{}
		}
		categoryStats[control.Category].Total++

		// Count by status
		switch control.Status {
		case ControlStatusImplemented, ControlStatusTested, ControlStatusEffective:
			report.ImplementedCount++
			categoryStats[control.Category].Implemented++
		case ControlStatusPartial:
			report.PartialCount++
			report.GapCount++
		case ControlStatusNotImplemented:
			report.NotImplemented++
			report.GapCount++
		}

		// Count SOC2 mappings
		if len(control.SOC2Mapping) > 0 {
			report.SOC2Overlap++
		}
	}

	// Calculate percentages
	if report.TotalControls > 0 {
		report.CompliancePercent = float64(report.ImplementedCount) / float64(report.TotalControls) * 100
	}

	for cat, stats := range categoryStats {
		if stats.Total > 0 {
			stats.Percent = float64(stats.Implemented) / float64(stats.Total) * 100
		}
		report.ByCategory[string(cat)] = *stats
	}

	report.RiskCount = len(s.risks)

	// Include SoA if requested
	if includeSoA && s.soa != nil {
		report.SoA = s.soa
	}

	return report, nil
}

// ExportToJSON exports report to JSON
func (s *ISO27001Service) ExportToJSON(report *ISO27001Report) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
