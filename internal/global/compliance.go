// internal/global/compliance.go
package global

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ComplianceFramework represents a regulatory compliance framework
type ComplianceFramework string

const (
	ComplianceGDPR    ComplianceFramework = "GDPR"
	ComplianceHIPAA   ComplianceFramework = "HIPAA"
	CompliancePCIDSS  ComplianceFramework = "PCI-DSS"
	ComplianceSOC2    ComplianceFramework = "SOC2"
	ComplianceCCPA    ComplianceFramework = "CCPA"
	ComplianceFedRAMP ComplianceFramework = "FedRAMP"
	ComplianceLGPD    ComplianceFramework = "LGPD"
	CompliancePDPA    ComplianceFramework = "PDPA"
)

// DataResidencyRequirement defines where data must reside
type DataResidencyRequirement struct {
	Framework         ComplianceFramework
	AllowedRegions    []string
	AllowedCountries  []string
	DeniedRegions     []string
	DeniedCountries   []string
	RequireEncryption bool
	RequireAuditLog   bool
	RetentionDays     int
	Description       string
}

// ComplianceManager manages regional compliance rules
type ComplianceManager struct {
	mu                sync.RWMutex
	requirements      map[ComplianceFramework]*DataResidencyRequirement
	countryFrameworks map[string][]ComplianceFramework
	regionFrameworks  map[string][]ComplianceFramework
	edgeManager       *EdgeManager
}

// NewComplianceManager creates a new compliance manager
func NewComplianceManager(edgeManager *EdgeManager) *ComplianceManager {
	cm := &ComplianceManager{
		requirements:      make(map[ComplianceFramework]*DataResidencyRequirement),
		countryFrameworks: make(map[string][]ComplianceFramework),
		regionFrameworks:  make(map[string][]ComplianceFramework),
		edgeManager:       edgeManager,
	}
	cm.loadDefaultRequirements()
	return cm
}

func (cm *ComplianceManager) loadDefaultRequirements() {
	// GDPR - European Union
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:      ComplianceGDPR,
		AllowedRegions: []string{"eu-west", "eu-central", "eu-north"},
		AllowedCountries: []string{
			"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
			"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
			"PL", "PT", "RO", "SK", "SI", "ES", "SE",
			// EEA
			"IS", "LI", "NO",
			// Adequate countries
			"CH", "GB", "JP", "NZ", "IL", "AR", "UY",
		},
		RequireEncryption: true,
		RequireAuditLog:   true,
		RetentionDays:     0, // Varies by purpose
		Description:       "EU General Data Protection Regulation",
	})

	// HIPAA - United States Healthcare
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:         ComplianceHIPAA,
		AllowedCountries:  []string{"US"},
		RequireEncryption: true,
		RequireAuditLog:   true,
		RetentionDays:     2190, // 6 years
		Description:       "US Health Insurance Portability and Accountability Act",
	})

	// PCI-DSS - Payment Card Industry
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:         CompliancePCIDSS,
		RequireEncryption: true,
		RequireAuditLog:   true,
		RetentionDays:     365,
		Description:       "Payment Card Industry Data Security Standard",
	})

	// SOC2 - Service Organization Control
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:         ComplianceSOC2,
		RequireEncryption: true,
		RequireAuditLog:   true,
		Description:       "Service Organization Control 2",
	})

	// CCPA - California Consumer Privacy Act
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:        ComplianceCCPA,
		AllowedCountries: []string{"US"},
		RequireAuditLog:  true,
		Description:      "California Consumer Privacy Act",
	})

	// FedRAMP - US Federal
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:         ComplianceFedRAMP,
		AllowedCountries:  []string{"US"},
		AllowedRegions:    []string{"us-east", "us-west", "us-gov"},
		RequireEncryption: true,
		RequireAuditLog:   true,
		Description:       "Federal Risk and Authorization Management Program",
	})

	// LGPD - Brazil
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:         ComplianceLGPD,
		AllowedCountries:  []string{"BR"},
		AllowedRegions:    []string{"sa-east"},
		RequireEncryption: true,
		RequireAuditLog:   true,
		Description:       "Lei Geral de Proteção de Dados (Brazil)",
	})

	// PDPA - Singapore
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:        CompliancePDPA,
		AllowedCountries: []string{"SG"},
		AllowedRegions:   []string{"ap-southeast"},
		RequireAuditLog:  true,
		Description:      "Personal Data Protection Act (Singapore)",
	})

	// Map countries to frameworks
	euCountries := []string{
		"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
		"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
		"PL", "PT", "RO", "SK", "SI", "ES", "SE", "IS", "LI", "NO",
	}
	for _, country := range euCountries {
		cm.countryFrameworks[country] = append(cm.countryFrameworks[country], ComplianceGDPR)
	}
	cm.countryFrameworks["US"] = []ComplianceFramework{ComplianceHIPAA, ComplianceCCPA, ComplianceFedRAMP}
	cm.countryFrameworks["BR"] = []ComplianceFramework{ComplianceLGPD}
	cm.countryFrameworks["SG"] = []ComplianceFramework{CompliancePDPA}
}

// AddRequirement adds a compliance requirement
func (cm *ComplianceManager) AddRequirement(req *DataResidencyRequirement) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.requirements[req.Framework] = req
}

// GetRequirement returns a specific compliance requirement
func (cm *ComplianceManager) GetRequirement(framework ComplianceFramework) (*DataResidencyRequirement, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	req, ok := cm.requirements[framework]
	return req, ok
}

// GetRequirements returns all compliance requirements
func (cm *ComplianceManager) GetRequirements() map[ComplianceFramework]*DataResidencyRequirement {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	result := make(map[ComplianceFramework]*DataResidencyRequirement)
	for k, v := range cm.requirements {
		result[k] = v
	}
	return result
}

// GetFrameworksForCountry returns applicable frameworks for a country
func (cm *ComplianceManager) GetFrameworksForCountry(countryCode string) []ComplianceFramework {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.countryFrameworks[countryCode]
}

// ValidateLocation checks if a location is compliant for given frameworks
func (cm *ComplianceManager) ValidateLocation(locationID string, frameworks []ComplianceFramework) *ComplianceValidation {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	validation := &ComplianceValidation{
		LocationID: locationID,
		Timestamp:  time.Now(),
		Results:    make(map[ComplianceFramework]*FrameworkValidation),
	}

	loc, ok := cm.edgeManager.GetLocation(locationID)
	if !ok {
		validation.Valid = false
		validation.Error = fmt.Sprintf("location not found: %s", locationID)
		return validation
	}

	allValid := true
	for _, framework := range frameworks {
		fv := cm.validateFramework(loc, framework)
		validation.Results[framework] = fv
		if !fv.Valid {
			allValid = false
		}
	}

	validation.Valid = allValid
	return validation
}

func (cm *ComplianceManager) validateFramework(loc *EdgeLocation, framework ComplianceFramework) *FrameworkValidation {
	req, ok := cm.requirements[framework]
	if !ok {
		return &FrameworkValidation{
			Framework: framework,
			Valid:     false,
			Reason:    "unknown framework",
		}
	}

	fv := &FrameworkValidation{
		Framework: framework,
		Valid:     true,
	}

	// Check allowed regions
	if len(req.AllowedRegions) > 0 {
		regionAllowed := false
		for _, region := range req.AllowedRegions {
			if loc.Region == region {
				regionAllowed = true
				break
			}
		}
		if !regionAllowed {
			fv.Valid = false
			fv.Reason = fmt.Sprintf("region %s not allowed for %s", loc.Region, framework)
			return fv
		}
	}

	// Check allowed countries
	if len(req.AllowedCountries) > 0 {
		countryAllowed := false
		for _, country := range req.AllowedCountries {
			if loc.Country == country {
				countryAllowed = true
				break
			}
		}
		if !countryAllowed {
			fv.Valid = false
			fv.Reason = fmt.Sprintf("country %s not allowed for %s", loc.Country, framework)
			return fv
		}
	}

	// Check denied regions
	for _, region := range req.DeniedRegions {
		if loc.Region == region {
			fv.Valid = false
			fv.Reason = fmt.Sprintf("region %s denied for %s", loc.Region, framework)
			return fv
		}
	}

	// Check denied countries
	for _, country := range req.DeniedCountries {
		if loc.Country == country {
			fv.Valid = false
			fv.Reason = fmt.Sprintf("country %s denied for %s", loc.Country, framework)
			return fv
		}
	}

	fv.Reason = "compliant"
	return fv
}

// ComplianceValidation contains validation results
type ComplianceValidation struct {
	LocationID string
	Valid      bool
	Error      string
	Timestamp  time.Time
	Results    map[ComplianceFramework]*FrameworkValidation
}

// FrameworkValidation contains validation for a single framework
type FrameworkValidation struct {
	Framework ComplianceFramework
	Valid     bool
	Reason    string
}

// GetCompliantLocations returns locations compliant with given frameworks
func (cm *ComplianceManager) GetCompliantLocations(frameworks []ComplianceFramework) []*EdgeLocation {
	locations := cm.edgeManager.GetEnabledLocations()
	var compliant []*EdgeLocation

	for _, loc := range locations {
		validation := cm.ValidateLocation(loc.ID, frameworks)
		if validation.Valid {
			compliant = append(compliant, loc)
		}
	}

	return compliant
}

// FindCompliantLocation finds the nearest compliant location
func (cm *ComplianceManager) FindCompliantLocation(lat, lon float64, frameworks []ComplianceFramework) *EdgeLocation {
	compliant := cm.GetCompliantLocations(frameworks)
	if len(compliant) == 0 {
		return nil
	}

	var nearest *EdgeLocation
	minDist := float64(1e18)

	for _, loc := range compliant {
		dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		if dist < minDist {
			minDist = dist
			nearest = loc
		}
	}

	return nearest
}

// TenantCompliance represents compliance settings for a tenant
type TenantCompliance struct {
	TenantID          string
	Frameworks        []ComplianceFramework
	AllowedRegions    []string
	AllowedCountries  []string
	RequireEncryption bool
	RequireAuditLog   bool
	DataRetentionDays int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TenantComplianceManager manages per-tenant compliance
type TenantComplianceManager struct {
	mu         sync.RWMutex
	tenants    map[string]*TenantCompliance
	compliance *ComplianceManager
}

// NewTenantComplianceManager creates a new tenant compliance manager
func NewTenantComplianceManager(cm *ComplianceManager) *TenantComplianceManager {
	return &TenantComplianceManager{
		tenants:    make(map[string]*TenantCompliance),
		compliance: cm,
	}
}

// SetTenantCompliance sets compliance requirements for a tenant
func (tcm *TenantComplianceManager) SetTenantCompliance(tc *TenantCompliance) {
	tcm.mu.Lock()
	defer tcm.mu.Unlock()

	tc.UpdatedAt = time.Now()
	if tc.CreatedAt.IsZero() {
		tc.CreatedAt = tc.UpdatedAt
	}
	tcm.tenants[tc.TenantID] = tc
}

// GetTenantCompliance returns compliance settings for a tenant
func (tcm *TenantComplianceManager) GetTenantCompliance(tenantID string) (*TenantCompliance, bool) {
	tcm.mu.RLock()
	defer tcm.mu.RUnlock()
	tc, ok := tcm.tenants[tenantID]
	return tc, ok
}

// RemoveTenantCompliance removes compliance settings for a tenant
func (tcm *TenantComplianceManager) RemoveTenantCompliance(tenantID string) {
	tcm.mu.Lock()
	defer tcm.mu.Unlock()
	delete(tcm.tenants, tenantID)
}

// ValidateTenantLocation validates if a location is compliant for a tenant
func (tcm *TenantComplianceManager) ValidateTenantLocation(tenantID, locationID string) *ComplianceValidation {
	tcm.mu.RLock()
	tc, ok := tcm.tenants[tenantID]
	tcm.mu.RUnlock()

	if !ok {
		return &ComplianceValidation{
			LocationID: locationID,
			Valid:      true,
			Timestamp:  time.Now(),
		}
	}

	return tcm.compliance.ValidateLocation(locationID, tc.Frameworks)
}

// GetTenantCompliantLocations returns compliant locations for a tenant
func (tcm *TenantComplianceManager) GetTenantCompliantLocations(tenantID string) []*EdgeLocation {
	tcm.mu.RLock()
	tc, ok := tcm.tenants[tenantID]
	tcm.mu.RUnlock()

	if !ok {
		return tcm.compliance.edgeManager.GetEnabledLocations()
	}

	return tcm.compliance.GetCompliantLocations(tc.Frameworks)
}

// ComplianceAuditLog represents an audit log entry
type ComplianceAuditLog struct {
	ID         string
	TenantID   string
	Action     string
	Resource   string
	LocationID string
	UserID     string
	IPAddress  string
	Timestamp  time.Time
	Frameworks []ComplianceFramework
	Details    map[string]string
	Success    bool
	ErrorMsg   string
}

// ComplianceAuditor handles compliance audit logging
type ComplianceAuditor struct {
	mu   sync.Mutex
	logs []*ComplianceAuditLog
}

// NewComplianceAuditor creates a new compliance auditor
func NewComplianceAuditor() *ComplianceAuditor {
	return &ComplianceAuditor{
		logs: make([]*ComplianceAuditLog, 0),
	}
}

// Log records an audit entry
func (ca *ComplianceAuditor) Log(ctx context.Context, entry *ComplianceAuditLog) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	ca.logs = append(ca.logs, entry)
}

// GetLogs returns audit logs with optional filtering
func (ca *ComplianceAuditor) GetLogs(tenantID string, since time.Time, limit int) []*ComplianceAuditLog {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	var result []*ComplianceAuditLog
	for i := len(ca.logs) - 1; i >= 0 && len(result) < limit; i-- {
		log := ca.logs[i]
		if tenantID != "" && log.TenantID != tenantID {
			continue
		}
		if !since.IsZero() && log.Timestamp.Before(since) {
			continue
		}
		result = append(result, log)
	}
	return result
}

// GetLogCount returns the total number of logs
func (ca *ComplianceAuditor) GetLogCount() int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return len(ca.logs)
}

// DataClassification represents sensitivity level of data
type DataClassification string

const (
	ClassificationPublic       DataClassification = "public"
	ClassificationInternal     DataClassification = "internal"
	ClassificationConfidential DataClassification = "confidential"
	ClassificationRestricted   DataClassification = "restricted"
)

// DataClassificationPolicy defines policies for data classifications
type DataClassificationPolicy struct {
	Classification    DataClassification
	RequireEncryption bool
	AllowedFrameworks []ComplianceFramework
	RetentionDays     int
	AllowExport       bool
	RequireApproval   bool
}

// DefaultClassificationPolicies returns default data classification policies
func DefaultClassificationPolicies() map[DataClassification]*DataClassificationPolicy {
	return map[DataClassification]*DataClassificationPolicy{
		ClassificationPublic: {
			Classification:    ClassificationPublic,
			RequireEncryption: false,
			AllowExport:       true,
			RequireApproval:   false,
		},
		ClassificationInternal: {
			Classification:    ClassificationInternal,
			RequireEncryption: true,
			AllowExport:       true,
			RequireApproval:   false,
		},
		ClassificationConfidential: {
			Classification:    ClassificationConfidential,
			RequireEncryption: true,
			AllowExport:       false,
			RequireApproval:   true,
			RetentionDays:     2555, // 7 years
		},
		ClassificationRestricted: {
			Classification:    ClassificationRestricted,
			RequireEncryption: true,
			AllowedFrameworks: []ComplianceFramework{ComplianceHIPAA, CompliancePCIDSS},
			AllowExport:       false,
			RequireApproval:   true,
			RetentionDays:     2555,
		},
	}
}
