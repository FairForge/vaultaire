// internal/global/compliance_test.go
package global

import (
	"context"
	"testing"
	"time"
)

func TestNewComplianceManager(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	if cm == nil {
		t.Fatal("expected non-nil manager")
	}

	// Should have default requirements loaded
	req, ok := cm.GetRequirement(ComplianceGDPR)
	if !ok {
		t.Error("expected GDPR requirement")
	}
	if req.Framework != ComplianceGDPR {
		t.Error("expected GDPR framework")
	}
}

func TestComplianceManagerGetRequirements(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	reqs := cm.GetRequirements()
	if len(reqs) == 0 {
		t.Error("expected default requirements")
	}

	// Check some known frameworks
	frameworks := []ComplianceFramework{ComplianceGDPR, ComplianceHIPAA, CompliancePCIDSS, ComplianceSOC2}
	for _, f := range frameworks {
		if _, ok := reqs[f]; !ok {
			t.Errorf("expected %s requirement", f)
		}
	}
}

func TestComplianceManagerAddRequirement(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	cm.AddRequirement(&DataResidencyRequirement{
		Framework:        "CUSTOM",
		AllowedCountries: []string{"XX"},
		Description:      "Custom requirement",
	})

	req, ok := cm.GetRequirement("CUSTOM")
	if !ok {
		t.Error("expected custom requirement")
	}
	if req.Description != "Custom requirement" {
		t.Error("requirement not saved correctly")
	}
}

func TestComplianceManagerGetFrameworksForCountry(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	// US should have HIPAA, CCPA, FedRAMP
	usFrameworks := cm.GetFrameworksForCountry("US")
	if len(usFrameworks) == 0 {
		t.Error("expected US frameworks")
	}

	hasHIPAA := false
	for _, f := range usFrameworks {
		if f == ComplianceHIPAA {
			hasHIPAA = true
		}
	}
	if !hasHIPAA {
		t.Error("expected HIPAA for US")
	}

	// DE should have GDPR
	deFrameworks := cm.GetFrameworksForCountry("DE")
	hasGDPR := false
	for _, f := range deFrameworks {
		if f == ComplianceGDPR {
			hasGDPR = true
		}
	}
	if !hasGDPR {
		t.Error("expected GDPR for DE")
	}
}

func TestComplianceManagerValidateLocation(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "eu-west-1",
		Region:  "eu-west",
		Country: "IE",
		Enabled: true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "us-east-1",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})

	cm := NewComplianceManager(em)

	// EU location should be GDPR compliant
	validation := cm.ValidateLocation("eu-west-1", []ComplianceFramework{ComplianceGDPR})
	if !validation.Valid {
		t.Errorf("expected eu-west-1 to be GDPR compliant: %v", validation.Results[ComplianceGDPR])
	}

	// US location should NOT be GDPR compliant (US not in allowed list for GDPR)
	validation = cm.ValidateLocation("us-east-1", []ComplianceFramework{ComplianceGDPR})
	if validation.Valid {
		t.Error("expected us-east-1 to NOT be GDPR compliant")
	}

	// US location should be HIPAA compliant
	validation = cm.ValidateLocation("us-east-1", []ComplianceFramework{ComplianceHIPAA})
	if !validation.Valid {
		t.Errorf("expected us-east-1 to be HIPAA compliant: %v", validation.Results[ComplianceHIPAA])
	}
}

func TestComplianceManagerValidateLocationNotFound(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	validation := cm.ValidateLocation("nonexistent", []ComplianceFramework{ComplianceGDPR})
	if validation.Valid {
		t.Error("expected invalid for nonexistent location")
	}
	if validation.Error == "" {
		t.Error("expected error message")
	}
}

func TestComplianceManagerValidateUnknownFramework(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "loc-1",
		Region:  "test",
		Country: "XX",
		Enabled: true,
	})

	cm := NewComplianceManager(em)

	validation := cm.ValidateLocation("loc-1", []ComplianceFramework{"UNKNOWN"})
	if validation.Valid {
		t.Error("expected invalid for unknown framework")
	}
}

func TestComplianceManagerGetCompliantLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "eu-west-1",
		Region:  "eu-west",
		Country: "IE",
		Enabled: true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "eu-central-1",
		Region:  "eu-central",
		Country: "DE",
		Enabled: true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "us-east-1",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})

	cm := NewComplianceManager(em)

	// Get GDPR compliant locations
	compliant := cm.GetCompliantLocations([]ComplianceFramework{ComplianceGDPR})
	if len(compliant) != 2 {
		t.Errorf("expected 2 GDPR compliant locations, got %d", len(compliant))
	}

	// Get HIPAA compliant locations
	compliant = cm.GetCompliantLocations([]ComplianceFramework{ComplianceHIPAA})
	if len(compliant) != 1 {
		t.Errorf("expected 1 HIPAA compliant location, got %d", len(compliant))
	}
}

func TestComplianceManagerFindCompliantLocation(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "eu-west-1",
		Region:    "eu-west",
		Country:   "IE",
		Latitude:  53.35,
		Longitude: -6.26,
		Enabled:   true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "eu-central-1",
		Region:    "eu-central",
		Country:   "DE",
		Latitude:  50.11,
		Longitude: 8.68,
		Enabled:   true,
	})

	cm := NewComplianceManager(em)

	// Find nearest GDPR compliant location to Paris
	loc := cm.FindCompliantLocation(48.86, 2.35, []ComplianceFramework{ComplianceGDPR})
	if loc == nil {
		t.Fatal("expected to find location")
	}
	// Frankfurt is closer to Paris than Dublin
	if loc.ID != "eu-central-1" {
		t.Errorf("expected eu-central-1, got %s", loc.ID)
	}
}

func TestComplianceManagerFindCompliantLocationNoMatch(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "us-east-1",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})

	cm := NewComplianceManager(em)

	// Try to find LGPD compliant (Brazil only)
	loc := cm.FindCompliantLocation(0, 0, []ComplianceFramework{ComplianceLGPD})
	if loc != nil {
		t.Error("expected no compliant location")
	}
}

func TestTenantComplianceManager(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	tcm := NewTenantComplianceManager(cm)

	tc := &TenantCompliance{
		TenantID:   "tenant-1",
		Frameworks: []ComplianceFramework{ComplianceGDPR, ComplianceSOC2},
	}
	tcm.SetTenantCompliance(tc)

	retrieved, ok := tcm.GetTenantCompliance("tenant-1")
	if !ok {
		t.Fatal("expected tenant compliance")
	}
	if len(retrieved.Frameworks) != 2 {
		t.Errorf("expected 2 frameworks, got %d", len(retrieved.Frameworks))
	}
	if retrieved.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestTenantComplianceManagerRemove(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	tcm := NewTenantComplianceManager(cm)

	tcm.SetTenantCompliance(&TenantCompliance{TenantID: "tenant-1"})
	tcm.RemoveTenantCompliance("tenant-1")

	_, ok := tcm.GetTenantCompliance("tenant-1")
	if ok {
		t.Error("expected tenant to be removed")
	}
}

func TestTenantComplianceManagerValidateLocation(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "eu-west-1",
		Region:  "eu-west",
		Country: "IE",
		Enabled: true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "us-east-1",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})

	cm := NewComplianceManager(em)
	tcm := NewTenantComplianceManager(cm)

	tcm.SetTenantCompliance(&TenantCompliance{
		TenantID:   "gdpr-tenant",
		Frameworks: []ComplianceFramework{ComplianceGDPR},
	})

	// EU location should be valid for GDPR tenant
	validation := tcm.ValidateTenantLocation("gdpr-tenant", "eu-west-1")
	if !validation.Valid {
		t.Error("expected EU location valid for GDPR tenant")
	}

	// US location should be invalid for GDPR tenant
	validation = tcm.ValidateTenantLocation("gdpr-tenant", "us-east-1")
	if validation.Valid {
		t.Error("expected US location invalid for GDPR tenant")
	}
}

func TestTenantComplianceManagerNoCompliance(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})

	cm := NewComplianceManager(em)
	tcm := NewTenantComplianceManager(cm)

	// Tenant without compliance settings should be valid for any location
	validation := tcm.ValidateTenantLocation("unknown-tenant", "loc-1")
	if !validation.Valid {
		t.Error("expected valid for tenant without compliance")
	}
}

func TestTenantComplianceManagerGetCompliantLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "eu-west-1", Region: "eu-west", Country: "IE", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-east-1", Region: "us-east", Country: "US", Enabled: true})

	cm := NewComplianceManager(em)
	tcm := NewTenantComplianceManager(cm)

	tcm.SetTenantCompliance(&TenantCompliance{
		TenantID:   "gdpr-tenant",
		Frameworks: []ComplianceFramework{ComplianceGDPR},
	})

	// GDPR tenant should only get EU locations
	locs := tcm.GetTenantCompliantLocations("gdpr-tenant")
	if len(locs) != 1 {
		t.Errorf("expected 1 location, got %d", len(locs))
	}

	// Unknown tenant should get all locations
	locs = tcm.GetTenantCompliantLocations("unknown-tenant")
	if len(locs) != 2 {
		t.Errorf("expected 2 locations, got %d", len(locs))
	}
}

func TestComplianceAuditor(t *testing.T) {
	auditor := NewComplianceAuditor()

	ctx := context.Background()
	auditor.Log(ctx, &ComplianceAuditLog{
		TenantID:   "tenant-1",
		Action:     "upload",
		Resource:   "file.txt",
		LocationID: "eu-west-1",
		Success:    true,
	})

	if auditor.GetLogCount() != 1 {
		t.Errorf("expected 1 log, got %d", auditor.GetLogCount())
	}

	logs := auditor.GetLogs("tenant-1", time.Time{}, 10)
	if len(logs) != 1 {
		t.Errorf("expected 1 log, got %d", len(logs))
	}
	if logs[0].Action != "upload" {
		t.Error("wrong log entry")
	}
}

func TestComplianceAuditorFiltering(t *testing.T) {
	auditor := NewComplianceAuditor()
	ctx := context.Background()

	// Log entries for different tenants
	auditor.Log(ctx, &ComplianceAuditLog{TenantID: "tenant-1", Action: "action-1"})
	auditor.Log(ctx, &ComplianceAuditLog{TenantID: "tenant-2", Action: "action-2"})
	auditor.Log(ctx, &ComplianceAuditLog{TenantID: "tenant-1", Action: "action-3"})

	// Filter by tenant
	logs := auditor.GetLogs("tenant-1", time.Time{}, 10)
	if len(logs) != 2 {
		t.Errorf("expected 2 logs for tenant-1, got %d", len(logs))
	}

	logs = auditor.GetLogs("tenant-2", time.Time{}, 10)
	if len(logs) != 1 {
		t.Errorf("expected 1 log for tenant-2, got %d", len(logs))
	}
}

func TestComplianceAuditorLimit(t *testing.T) {
	auditor := NewComplianceAuditor()
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		auditor.Log(ctx, &ComplianceAuditLog{TenantID: "tenant-1"})
	}

	logs := auditor.GetLogs("", time.Time{}, 5)
	if len(logs) != 5 {
		t.Errorf("expected 5 logs (limit), got %d", len(logs))
	}
}

func TestComplianceAuditorSinceFilter(t *testing.T) {
	auditor := NewComplianceAuditor()
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	auditor.Log(ctx, &ComplianceAuditLog{TenantID: "tenant-1", Timestamp: past})
	auditor.Log(ctx, &ComplianceAuditLog{TenantID: "tenant-1"}) // now

	// Filter since 30 minutes ago
	since := time.Now().Add(-30 * time.Minute)
	logs := auditor.GetLogs("", since, 10)
	if len(logs) != 1 {
		t.Errorf("expected 1 log since filter, got %d", len(logs))
	}
}

func TestDefaultClassificationPolicies(t *testing.T) {
	policies := DefaultClassificationPolicies()

	if len(policies) != 4 {
		t.Errorf("expected 4 policies, got %d", len(policies))
	}

	// Public should not require encryption
	public := policies[ClassificationPublic]
	if public.RequireEncryption {
		t.Error("public should not require encryption")
	}

	// Restricted should require encryption
	restricted := policies[ClassificationRestricted]
	if !restricted.RequireEncryption {
		t.Error("restricted should require encryption")
	}
	if restricted.AllowExport {
		t.Error("restricted should not allow export")
	}
}

func TestGDPRRequirement(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	gdpr, ok := cm.GetRequirement(ComplianceGDPR)
	if !ok {
		t.Fatal("expected GDPR requirement")
	}

	// Check EU countries are allowed
	euCountries := []string{"DE", "FR", "IE", "NL", "ES"}
	for _, country := range euCountries {
		found := false
		for _, allowed := range gdpr.AllowedCountries {
			if allowed == country {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s to be allowed for GDPR", country)
		}
	}

	if !gdpr.RequireEncryption {
		t.Error("GDPR should require encryption")
	}
	if !gdpr.RequireAuditLog {
		t.Error("GDPR should require audit log")
	}
}

func TestHIPAARequirement(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)

	hipaa, ok := cm.GetRequirement(ComplianceHIPAA)
	if !ok {
		t.Fatal("expected HIPAA requirement")
	}

	// HIPAA should only allow US
	if len(hipaa.AllowedCountries) != 1 || hipaa.AllowedCountries[0] != "US" {
		t.Error("HIPAA should only allow US")
	}

	if hipaa.RetentionDays != 2190 {
		t.Errorf("HIPAA retention should be 6 years (2190 days), got %d", hipaa.RetentionDays)
	}
}

func TestMultipleFrameworkValidation(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "us-east-1",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})

	cm := NewComplianceManager(em)

	// US location should be compliant with HIPAA + SOC2
	validation := cm.ValidateLocation("us-east-1", []ComplianceFramework{ComplianceHIPAA, ComplianceSOC2})
	if !validation.Valid {
		t.Error("expected US location valid for HIPAA + SOC2")
	}

	// US location should NOT be compliant with GDPR + HIPAA
	validation = cm.ValidateLocation("us-east-1", []ComplianceFramework{ComplianceGDPR, ComplianceHIPAA})
	if validation.Valid {
		t.Error("expected US location invalid for GDPR + HIPAA")
	}
}

func TestDeniedRegionsCountries(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "denied-loc",
		Region:  "denied-region",
		Country: "XX",
		Enabled: true,
	})

	cm := NewComplianceManager(em)
	cm.AddRequirement(&DataResidencyRequirement{
		Framework:       "CUSTOM",
		DeniedRegions:   []string{"denied-region"},
		DeniedCountries: []string{"YY"},
	})

	validation := cm.ValidateLocation("denied-loc", []ComplianceFramework{"CUSTOM"})
	if validation.Valid {
		t.Error("expected location in denied region to be invalid")
	}
}
