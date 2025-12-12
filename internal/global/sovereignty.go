// internal/global/sovereignty.go
package global

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SovereigntyRule defines data sovereignty requirements
type SovereigntyRule struct {
	ID              string
	Name            string
	Description     string
	Enabled         bool
	Priority        int
	SourceCountries []string // Where data originates
	TargetCountries []string // Where data can be stored
	TargetRegions   []string // Allowed regions
	DeniedCountries []string // Explicitly denied
	DeniedRegions   []string // Explicitly denied regions
	RequireLocal    bool     // Data must stay in source country
	AllowTransfer   bool     // Allow cross-border transfer
	TransferRules   []TransferRule
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TransferRule defines conditions for cross-border data transfer
type TransferRule struct {
	FromCountry     string
	ToCountries     []string
	ToRegions       []string
	RequireConsent  bool
	RequireContract bool
	RequireApproval bool
	MaxRetention    time.Duration
}

// SovereigntyManager manages data sovereignty rules
type SovereigntyManager struct {
	mu            sync.RWMutex
	rules         map[string]*SovereigntyRule
	countryRules  map[string][]*SovereigntyRule
	edgeManager   *EdgeManager
	complianceMgr *ComplianceManager
}

// NewSovereigntyManager creates a new sovereignty manager
func NewSovereigntyManager(edgeManager *EdgeManager, complianceMgr *ComplianceManager) *SovereigntyManager {
	sm := &SovereigntyManager{
		rules:         make(map[string]*SovereigntyRule),
		countryRules:  make(map[string][]*SovereigntyRule),
		edgeManager:   edgeManager,
		complianceMgr: complianceMgr,
	}
	sm.loadDefaultRules()
	return sm
}

func (sm *SovereigntyManager) loadDefaultRules() {
	// EU Data Sovereignty
	sm.AddRule(&SovereigntyRule{
		ID:          "eu-sovereignty",
		Name:        "EU Data Sovereignty",
		Description: "EU citizen data must remain within EU/EEA or adequate countries",
		Enabled:     true,
		Priority:    100,
		SourceCountries: []string{
			"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
			"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
			"PL", "PT", "RO", "SK", "SI", "ES", "SE", "IS", "LI", "NO",
		},
		TargetCountries: []string{
			// EU + EEA
			"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
			"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
			"PL", "PT", "RO", "SK", "SI", "ES", "SE", "IS", "LI", "NO",
			// Adequacy decisions
			"CH", "GB", "JP", "NZ", "IL", "AR", "UY", "KR", "CA",
			// EU-US Data Privacy Framework (2023)
			"US",
		},
		TargetRegions: []string{"eu-west", "eu-central", "eu-north"},
		AllowTransfer: true,
	})

	// Russia Data Localization
	sm.AddRule(&SovereigntyRule{
		ID:              "russia-localization",
		Name:            "Russia Data Localization",
		Description:     "Russian citizen data must be stored in Russia",
		Enabled:         true,
		Priority:        100,
		SourceCountries: []string{"RU"},
		TargetCountries: []string{"RU"},
		RequireLocal:    true,
		AllowTransfer:   false,
	})

	// China Data Localization
	sm.AddRule(&SovereigntyRule{
		ID:              "china-localization",
		Name:            "China Data Localization",
		Description:     "Important data and personal info must be stored in China",
		Enabled:         true,
		Priority:        100,
		SourceCountries: []string{"CN"},
		TargetCountries: []string{"CN"},
		RequireLocal:    true,
		AllowTransfer:   false,
	})

	// Australia Data Sovereignty
	sm.AddRule(&SovereigntyRule{
		ID:              "australia-sovereignty",
		Name:            "Australia Data Sovereignty",
		Description:     "Australian government data must stay in Australia",
		Enabled:         true,
		Priority:        90,
		SourceCountries: []string{"AU"},
		TargetCountries: []string{"AU"},
		TargetRegions:   []string{"ap-southeast"},
		RequireLocal:    false,
		AllowTransfer:   true,
	})

	// Brazil LGPD
	sm.AddRule(&SovereigntyRule{
		ID:              "brazil-lgpd",
		Name:            "Brazil LGPD Data Sovereignty",
		Description:     "Brazilian personal data under LGPD",
		Enabled:         true,
		Priority:        90,
		SourceCountries: []string{"BR"},
		TargetCountries: []string{"BR"},
		TargetRegions:   []string{"sa-east"},
		AllowTransfer:   true,
		TransferRules: []TransferRule{
			{
				FromCountry:     "BR",
				ToCountries:     []string{"US", "EU"},
				RequireConsent:  true,
				RequireContract: true,
			},
		},
	})

	// India Data Localization
	sm.AddRule(&SovereigntyRule{
		ID:              "india-localization",
		Name:            "India Data Localization",
		Description:     "Certain categories of Indian data must be stored locally",
		Enabled:         true,
		Priority:        80,
		SourceCountries: []string{"IN"},
		TargetCountries: []string{"IN"},
		TargetRegions:   []string{"ap-south"},
		AllowTransfer:   true,
	})

	// Build country index
	for _, rule := range sm.rules {
		for _, country := range rule.SourceCountries {
			sm.countryRules[country] = append(sm.countryRules[country], rule)
		}
	}
}

// AddRule adds a sovereignty rule
func (sm *SovereigntyManager) AddRule(rule *SovereigntyRule) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = now
	}
	rule.UpdatedAt = now

	sm.rules[rule.ID] = rule

	// Update country index
	for _, country := range rule.SourceCountries {
		sm.countryRules[country] = append(sm.countryRules[country], rule)
	}
}

// GetRule returns a specific rule
func (sm *SovereigntyManager) GetRule(id string) (*SovereigntyRule, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	rule, ok := sm.rules[id]
	return rule, ok
}

// GetRules returns all rules
func (sm *SovereigntyManager) GetRules() []*SovereigntyRule {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	rules := make([]*SovereigntyRule, 0, len(sm.rules))
	for _, rule := range sm.rules {
		rules = append(rules, rule)
	}
	return rules
}

// RemoveRule removes a rule
func (sm *SovereigntyManager) RemoveRule(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	rule, ok := sm.rules[id]
	if !ok {
		return false
	}

	// Remove from country index
	for _, country := range rule.SourceCountries {
		rules := sm.countryRules[country]
		for i, r := range rules {
			if r.ID == id {
				sm.countryRules[country] = append(rules[:i], rules[i+1:]...)
				break
			}
		}
	}

	delete(sm.rules, id)
	return true
}

// GetRulesForCountry returns applicable rules for a source country
func (sm *SovereigntyManager) GetRulesForCountry(country string) []*SovereigntyRule {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	rules := sm.countryRules[country]
	result := make([]*SovereigntyRule, 0, len(rules))
	for _, r := range rules {
		if r.Enabled {
			result = append(result, r)
		}
	}
	return result
}

// SovereigntyCheck contains the result of a sovereignty check
type SovereigntyCheck struct {
	Allowed       bool
	SourceCountry string
	TargetCountry string
	TargetRegion  string
	LocationID    string
	ViolatedRules []*SovereigntyRule
	AllowedRules  []*SovereigntyRule
	Reason        string
	Timestamp     time.Time
}

// CheckDataTransfer checks if data can be transferred to a location
func (sm *SovereigntyManager) CheckDataTransfer(ctx context.Context, sourceCountry, locationID string) *SovereigntyCheck {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	check := &SovereigntyCheck{
		Allowed:       true,
		SourceCountry: sourceCountry,
		LocationID:    locationID,
		Timestamp:     time.Now(),
	}

	loc, ok := sm.edgeManager.GetLocation(locationID)
	if !ok {
		check.Allowed = false
		check.Reason = "location not found"
		return check
	}

	check.TargetCountry = loc.Country
	check.TargetRegion = loc.Region

	rules := sm.countryRules[sourceCountry]
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}

		if sm.ruleAllowsLocation(rule, loc) {
			check.AllowedRules = append(check.AllowedRules, rule)
		} else {
			check.ViolatedRules = append(check.ViolatedRules, rule)
			check.Allowed = false
		}
	}

	if !check.Allowed && len(check.ViolatedRules) > 0 {
		check.Reason = fmt.Sprintf("violates rule: %s", check.ViolatedRules[0].Name)
	} else if check.Allowed {
		check.Reason = "transfer allowed"
	}

	return check
}

func (sm *SovereigntyManager) ruleAllowsLocation(rule *SovereigntyRule, loc *EdgeLocation) bool {
	// Check denied lists first
	for _, denied := range rule.DeniedCountries {
		if loc.Country == denied {
			return false
		}
	}
	for _, denied := range rule.DeniedRegions {
		if loc.Region == denied {
			return false
		}
	}

	// If require local, check if target is in source countries
	if rule.RequireLocal {
		for _, source := range rule.SourceCountries {
			if loc.Country == source {
				return true
			}
		}
		return false
	}

	// Check allowed targets - use OR logic (country allowed OR region allowed)
	countryAllowed := len(rule.TargetCountries) == 0 // If no countries specified, don't restrict
	regionAllowed := len(rule.TargetRegions) == 0    // If no regions specified, don't restrict

	for _, target := range rule.TargetCountries {
		if loc.Country == target {
			countryAllowed = true
			break
		}
	}

	for _, target := range rule.TargetRegions {
		if loc.Region == target {
			regionAllowed = true
			break
		}
	}

	// If both are specified, either one matching is sufficient
	if len(rule.TargetCountries) > 0 && len(rule.TargetRegions) > 0 {
		return countryAllowed || regionAllowed
	}

	// If only one is specified, that one must match
	return countryAllowed && regionAllowed
}

// GetAllowedLocations returns locations where data from a country can be stored
func (sm *SovereigntyManager) GetAllowedLocations(sourceCountry string) []*EdgeLocation {
	locations := sm.edgeManager.GetEnabledLocations()
	var allowed []*EdgeLocation

	for _, loc := range locations {
		check := sm.CheckDataTransfer(context.Background(), sourceCountry, loc.ID)
		if check.Allowed {
			allowed = append(allowed, loc)
		}
	}

	return allowed
}

// FindNearestAllowedLocation finds the nearest location where data can be stored
func (sm *SovereigntyManager) FindNearestAllowedLocation(sourceCountry string, lat, lon float64) *EdgeLocation {
	allowed := sm.GetAllowedLocations(sourceCountry)
	if len(allowed) == 0 {
		return nil
	}

	var nearest *EdgeLocation
	minDist := float64(1e18)

	for _, loc := range allowed {
		dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		if dist < minDist {
			minDist = dist
			nearest = loc
		}
	}

	return nearest
}

// DataResidencyPolicy defines where tenant data must reside
type DataResidencyPolicy struct {
	TenantID         string
	SourceCountry    string
	AllowedCountries []string
	AllowedRegions   []string
	DeniedCountries  []string
	PreferLocal      bool
	Frameworks       []ComplianceFramework
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TenantSovereigntyManager manages per-tenant data residency
type TenantSovereigntyManager struct {
	mu          sync.RWMutex
	policies    map[string]*DataResidencyPolicy
	sovereignty *SovereigntyManager
}

// NewTenantSovereigntyManager creates a new tenant sovereignty manager
func NewTenantSovereigntyManager(sm *SovereigntyManager) *TenantSovereigntyManager {
	return &TenantSovereigntyManager{
		policies:    make(map[string]*DataResidencyPolicy),
		sovereignty: sm,
	}
}

// SetPolicy sets data residency policy for a tenant
func (tsm *TenantSovereigntyManager) SetPolicy(policy *DataResidencyPolicy) {
	tsm.mu.Lock()
	defer tsm.mu.Unlock()

	now := time.Now()
	if policy.CreatedAt.IsZero() {
		policy.CreatedAt = now
	}
	policy.UpdatedAt = now
	tsm.policies[policy.TenantID] = policy
}

// GetPolicy returns policy for a tenant
func (tsm *TenantSovereigntyManager) GetPolicy(tenantID string) (*DataResidencyPolicy, bool) {
	tsm.mu.RLock()
	defer tsm.mu.RUnlock()
	policy, ok := tsm.policies[tenantID]
	return policy, ok
}

// RemovePolicy removes a tenant's policy
func (tsm *TenantSovereigntyManager) RemovePolicy(tenantID string) {
	tsm.mu.Lock()
	defer tsm.mu.Unlock()
	delete(tsm.policies, tenantID)
}

// CheckTenantTransfer checks if a tenant's data can go to a location
func (tsm *TenantSovereigntyManager) CheckTenantTransfer(tenantID, locationID string) *SovereigntyCheck {
	tsm.mu.RLock()
	policy, hasPolicy := tsm.policies[tenantID]
	tsm.mu.RUnlock()

	// If no policy, use default sovereignty rules
	if !hasPolicy {
		return &SovereigntyCheck{
			Allowed:   true,
			Reason:    "no policy defined",
			Timestamp: time.Now(),
		}
	}

	// First check country-level sovereignty
	sovereigntyCheck := tsm.sovereignty.CheckDataTransfer(context.Background(), policy.SourceCountry, locationID)
	if !sovereigntyCheck.Allowed {
		return sovereigntyCheck
	}

	// Then check tenant-specific policy
	loc, ok := tsm.sovereignty.edgeManager.GetLocation(locationID)
	if !ok {
		return &SovereigntyCheck{
			Allowed:   false,
			Reason:    "location not found",
			Timestamp: time.Now(),
		}
	}

	check := &SovereigntyCheck{
		Allowed:       true,
		SourceCountry: policy.SourceCountry,
		TargetCountry: loc.Country,
		TargetRegion:  loc.Region,
		LocationID:    locationID,
		Timestamp:     time.Now(),
	}

	// Check denied countries
	for _, denied := range policy.DeniedCountries {
		if loc.Country == denied {
			check.Allowed = false
			check.Reason = fmt.Sprintf("country %s denied by tenant policy", denied)
			return check
		}
	}

	// Check allowed countries
	if len(policy.AllowedCountries) > 0 {
		allowed := false
		for _, country := range policy.AllowedCountries {
			if loc.Country == country {
				allowed = true
				break
			}
		}
		if !allowed {
			check.Allowed = false
			check.Reason = fmt.Sprintf("country %s not in allowed list", loc.Country)
			return check
		}
	}

	// Check allowed regions
	if len(policy.AllowedRegions) > 0 {
		allowed := false
		for _, region := range policy.AllowedRegions {
			if loc.Region == region {
				allowed = true
				break
			}
		}
		if !allowed {
			check.Allowed = false
			check.Reason = fmt.Sprintf("region %s not in allowed list", loc.Region)
			return check
		}
	}

	check.Reason = "transfer allowed by tenant policy"
	return check
}

// GetTenantAllowedLocations returns allowed locations for a tenant
func (tsm *TenantSovereigntyManager) GetTenantAllowedLocations(tenantID string) []*EdgeLocation {
	locations := tsm.sovereignty.edgeManager.GetEnabledLocations()
	var allowed []*EdgeLocation

	for _, loc := range locations {
		check := tsm.CheckTenantTransfer(tenantID, loc.ID)
		if check.Allowed {
			allowed = append(allowed, loc)
		}
	}

	return allowed
}

// DataTransferAgreement represents a cross-border transfer agreement
type DataTransferAgreement struct {
	ID             string
	Name           string
	Type           AgreementType
	FromCountries  []string
	ToCountries    []string
	EffectiveDate  time.Time
	ExpirationDate time.Time
	RequireConsent bool
	RequireNotice  bool
	Frameworks     []ComplianceFramework
	Description    string
}

// AgreementType represents types of data transfer agreements
type AgreementType string

const (
	AgreementSCC        AgreementType = "SCC"        // Standard Contractual Clauses
	AgreementBCR        AgreementType = "BCR"        // Binding Corporate Rules
	AgreementAdequacy   AgreementType = "ADEQUACY"   // Adequacy Decision
	AgreementConsent    AgreementType = "CONSENT"    // Explicit Consent
	AgreementDerogation AgreementType = "DEROGATION" // Specific Derogation
)

// CommonTransferAgreements returns standard transfer agreements
func CommonTransferAgreements() []*DataTransferAgreement {
	return []*DataTransferAgreement{
		{
			ID:            "eu-us-dpf",
			Name:          "EU-US Data Privacy Framework",
			Type:          AgreementAdequacy,
			FromCountries: []string{"EU"},
			ToCountries:   []string{"US"},
			EffectiveDate: time.Date(2023, 7, 10, 0, 0, 0, 0, time.UTC),
			Frameworks:    []ComplianceFramework{ComplianceGDPR},
			Description:   "EU-US Data Privacy Framework adequacy decision",
		},
		{
			ID:            "eu-uk-adequacy",
			Name:          "EU-UK Adequacy Decision",
			Type:          AgreementAdequacy,
			FromCountries: []string{"EU"},
			ToCountries:   []string{"GB"},
			EffectiveDate: time.Date(2021, 6, 28, 0, 0, 0, 0, time.UTC),
			Frameworks:    []ComplianceFramework{ComplianceGDPR},
			Description:   "EU adequacy decision for UK post-Brexit",
		},
		{
			ID:            "eu-jp-adequacy",
			Name:          "EU-Japan Adequacy Decision",
			Type:          AgreementAdequacy,
			FromCountries: []string{"EU"},
			ToCountries:   []string{"JP"},
			EffectiveDate: time.Date(2019, 1, 23, 0, 0, 0, 0, time.UTC),
			Frameworks:    []ComplianceFramework{ComplianceGDPR},
			Description:   "EU adequacy decision for Japan",
		},
		{
			ID:             "eu-scc-2021",
			Name:           "EU Standard Contractual Clauses 2021",
			Type:           AgreementSCC,
			FromCountries:  []string{"EU"},
			ToCountries:    []string{}, // Any country with SCC
			EffectiveDate:  time.Date(2021, 6, 4, 0, 0, 0, 0, time.UTC),
			RequireConsent: false,
			RequireNotice:  true,
			Frameworks:     []ComplianceFramework{ComplianceGDPR},
			Description:    "Updated SCCs for international transfers",
		},
	}
}
