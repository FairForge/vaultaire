// internal/global/sovereignty_test.go
package global

import (
	"context"
	"testing"
	"time"
)

func TestNewSovereigntyManager(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	if sm == nil {
		t.Fatal("expected non-nil manager")
	}

	// Should have default rules
	rules := sm.GetRules()
	if len(rules) == 0 {
		t.Error("expected default rules")
	}
}

func TestSovereigntyManagerGetRule(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	rule, ok := sm.GetRule("eu-sovereignty")
	if !ok {
		t.Fatal("expected EU sovereignty rule")
	}
	if rule.Name != "EU Data Sovereignty" {
		t.Errorf("unexpected rule name: %s", rule.Name)
	}
}

func TestSovereigntyManagerAddRule(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	sm.AddRule(&SovereigntyRule{
		ID:              "custom-rule",
		Name:            "Custom Rule",
		Enabled:         true,
		SourceCountries: []string{"XX"},
		TargetCountries: []string{"YY"},
	})

	rule, ok := sm.GetRule("custom-rule")
	if !ok {
		t.Fatal("expected custom rule")
	}
	if rule.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestSovereigntyManagerRemoveRule(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	sm.AddRule(&SovereigntyRule{
		ID:              "temp-rule",
		SourceCountries: []string{"XX"},
	})

	removed := sm.RemoveRule("temp-rule")
	if !removed {
		t.Error("expected rule to be removed")
	}

	_, ok := sm.GetRule("temp-rule")
	if ok {
		t.Error("rule should not exist after removal")
	}

	// Remove nonexistent
	removed = sm.RemoveRule("nonexistent")
	if removed {
		t.Error("should not remove nonexistent rule")
	}
}

func TestSovereigntyManagerGetRulesForCountry(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	// Germany should have EU sovereignty rules
	rules := sm.GetRulesForCountry("DE")
	if len(rules) == 0 {
		t.Error("expected rules for DE")
	}

	// Russia should have localization rules
	rules = sm.GetRulesForCountry("RU")
	if len(rules) == 0 {
		t.Error("expected rules for RU")
	}

	hasLocalization := false
	for _, r := range rules {
		if r.ID == "russia-localization" {
			hasLocalization = true
		}
	}
	if !hasLocalization {
		t.Error("expected Russia localization rule")
	}
}

func TestSovereigntyManagerCheckDataTransfer(t *testing.T) {
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
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "ru-central-1",
		Region:  "ru-central",
		Country: "RU",
		Enabled: true,
	})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	ctx := context.Background()

	// German data to EU - should be allowed
	check := sm.CheckDataTransfer(ctx, "DE", "eu-west-1")
	if !check.Allowed {
		t.Errorf("expected DE->EU allowed: %s", check.Reason)
	}

	// German data to US - should be allowed (US in adequacy list)
	check = sm.CheckDataTransfer(ctx, "DE", "us-east-1")
	if !check.Allowed {
		t.Errorf("expected DE->US allowed: %s", check.Reason)
	}

	// Russian data to EU - should NOT be allowed (require local)
	check = sm.CheckDataTransfer(ctx, "RU", "eu-west-1")
	if check.Allowed {
		t.Error("expected RU->EU NOT allowed")
	}

	// Russian data to Russia - should be allowed
	check = sm.CheckDataTransfer(ctx, "RU", "ru-central-1")
	if !check.Allowed {
		t.Errorf("expected RU->RU allowed: %s", check.Reason)
	}
}

func TestSovereigntyManagerCheckTransferLocationNotFound(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	check := sm.CheckDataTransfer(context.Background(), "DE", "nonexistent")
	if check.Allowed {
		t.Error("expected not allowed for nonexistent location")
	}
	if check.Reason != "location not found" {
		t.Errorf("unexpected reason: %s", check.Reason)
	}
}

func TestSovereigntyManagerGetAllowedLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "eu-west-1", Region: "eu-west", Country: "IE", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-east-1", Region: "us-east", Country: "US", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "cn-north-1", Region: "cn-north", Country: "CN", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	// German data should be allowed in EU and US (adequacy)
	allowed := sm.GetAllowedLocations("DE")
	if len(allowed) < 2 {
		t.Errorf("expected at least 2 allowed locations for DE, got %d", len(allowed))
	}

	// Chinese data should only be allowed in China
	allowed = sm.GetAllowedLocations("CN")
	if len(allowed) != 1 {
		t.Errorf("expected 1 allowed location for CN, got %d", len(allowed))
	}
	if allowed[0].Country != "CN" {
		t.Error("expected CN location for Chinese data")
	}
}

func TestSovereigntyManagerFindNearestAllowedLocation(t *testing.T) {
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
	sm := NewSovereigntyManager(em, cm)

	// Find nearest for German data from Paris
	loc := sm.FindNearestAllowedLocation("DE", 48.86, 2.35)
	if loc == nil {
		t.Fatal("expected to find location")
	}
	// Frankfurt should be closer to Paris than Dublin
	if loc.ID != "eu-central-1" {
		t.Errorf("expected eu-central-1, got %s", loc.ID)
	}
}

func TestSovereigntyManagerFindNearestNoAllowed(t *testing.T) {
	em := NewEdgeManager(nil)
	// No Chinese locations registered
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-east-1", Region: "us-east", Country: "US", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	loc := sm.FindNearestAllowedLocation("CN", 39.9, 116.4)
	if loc != nil {
		t.Error("expected no allowed location for CN")
	}
}

func TestSovereigntyManagerDeniedCountries(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "denied-loc", Region: "test", Country: "XX", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	sm.AddRule(&SovereigntyRule{
		ID:              "deny-rule",
		Enabled:         true,
		SourceCountries: []string{"YY"},
		DeniedCountries: []string{"XX"},
	})

	check := sm.CheckDataTransfer(context.Background(), "YY", "denied-loc")
	if check.Allowed {
		t.Error("expected transfer to denied country to be blocked")
	}
}

func TestTenantSovereigntyManager(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	policy := &DataResidencyPolicy{
		TenantID:         "tenant-1",
		SourceCountry:    "DE",
		AllowedCountries: []string{"DE", "IE", "NL"},
	}
	tsm.SetPolicy(policy)

	retrieved, ok := tsm.GetPolicy("tenant-1")
	if !ok {
		t.Fatal("expected policy")
	}
	if len(retrieved.AllowedCountries) != 3 {
		t.Error("policy not saved correctly")
	}
}

func TestTenantSovereigntyManagerRemovePolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	tsm.SetPolicy(&DataResidencyPolicy{TenantID: "tenant-1"})
	tsm.RemovePolicy("tenant-1")

	_, ok := tsm.GetPolicy("tenant-1")
	if ok {
		t.Error("expected policy to be removed")
	}
}

func TestTenantSovereigntyManagerCheckTransfer(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "ie-loc", Region: "eu-west", Country: "IE", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-loc", Region: "us-east", Country: "US", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	tsm.SetPolicy(&DataResidencyPolicy{
		TenantID:         "strict-tenant",
		SourceCountry:    "DE",
		AllowedCountries: []string{"IE", "DE"},
	})

	// Ireland should be allowed
	check := tsm.CheckTenantTransfer("strict-tenant", "ie-loc")
	if !check.Allowed {
		t.Errorf("expected IE allowed: %s", check.Reason)
	}

	// US should NOT be allowed by tenant policy
	check = tsm.CheckTenantTransfer("strict-tenant", "us-loc")
	if check.Allowed {
		t.Error("expected US NOT allowed by tenant policy")
	}
}

func TestTenantSovereigntyManagerNoPolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	// Tenant without policy should be allowed anywhere
	check := tsm.CheckTenantTransfer("no-policy-tenant", "loc-1")
	if !check.Allowed {
		t.Error("expected allowed for tenant without policy")
	}
}

func TestTenantSovereigntyManagerDeniedCountries(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-loc", Region: "us-east", Country: "US", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	tsm.SetPolicy(&DataResidencyPolicy{
		TenantID:        "deny-us-tenant",
		SourceCountry:   "DE",
		DeniedCountries: []string{"US"},
	})

	check := tsm.CheckTenantTransfer("deny-us-tenant", "us-loc")
	if check.Allowed {
		t.Error("expected US denied")
	}
}

func TestTenantSovereigntyManagerAllowedRegions(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "eu-loc", Region: "eu-west", Country: "IE", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "ap-loc", Region: "ap-southeast", Country: "SG", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	tsm.SetPolicy(&DataResidencyPolicy{
		TenantID:       "eu-only-tenant",
		SourceCountry:  "DE",
		AllowedRegions: []string{"eu-west", "eu-central"},
	})

	// EU region allowed
	check := tsm.CheckTenantTransfer("eu-only-tenant", "eu-loc")
	if !check.Allowed {
		t.Errorf("expected EU region allowed: %s", check.Reason)
	}

	// AP region not allowed
	check = tsm.CheckTenantTransfer("eu-only-tenant", "ap-loc")
	if check.Allowed {
		t.Error("expected AP region NOT allowed")
	}
}

func TestTenantSovereigntyManagerGetAllowedLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "eu-loc", Region: "eu-west", Country: "IE", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-loc", Region: "us-east", Country: "US", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)
	tsm := NewTenantSovereigntyManager(sm)

	tsm.SetPolicy(&DataResidencyPolicy{
		TenantID:         "restricted-tenant",
		SourceCountry:    "DE",
		AllowedCountries: []string{"IE"},
	})

	allowed := tsm.GetTenantAllowedLocations("restricted-tenant")
	if len(allowed) != 1 {
		t.Errorf("expected 1 allowed location, got %d", len(allowed))
	}
	if allowed[0].ID != "eu-loc" {
		t.Error("expected eu-loc")
	}
}

func TestCommonTransferAgreements(t *testing.T) {
	agreements := CommonTransferAgreements()

	if len(agreements) == 0 {
		t.Fatal("expected agreements")
	}

	// Check for EU-US DPF
	foundDPF := false
	for _, a := range agreements {
		if a.ID == "eu-us-dpf" {
			foundDPF = true
			if a.Type != AgreementAdequacy {
				t.Error("expected adequacy type for DPF")
			}
		}
	}
	if !foundDPF {
		t.Error("expected EU-US DPF agreement")
	}
}

func TestAgreementTypes(t *testing.T) {
	types := []AgreementType{
		AgreementSCC,
		AgreementBCR,
		AgreementAdequacy,
		AgreementConsent,
		AgreementDerogation,
	}

	for _, at := range types {
		if at == "" {
			t.Error("agreement type should not be empty")
		}
	}
}

func TestSovereigntyRuleTimestamps(t *testing.T) {
	em := NewEdgeManager(nil)
	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	before := time.Now()
	sm.AddRule(&SovereigntyRule{
		ID:   "timestamp-test",
		Name: "Test",
	})
	after := time.Now()

	rule, _ := sm.GetRule("timestamp-test")
	if rule.CreatedAt.Before(before) || rule.CreatedAt.After(after) {
		t.Error("CreatedAt should be between before and after")
	}
	if rule.UpdatedAt.Before(before) || rule.UpdatedAt.After(after) {
		t.Error("UpdatedAt should be between before and after")
	}
}

func TestRequireLocalRule(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "cn-loc", Region: "cn-north", Country: "CN", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-loc", Region: "us-east", Country: "US", Enabled: true})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	// China requires local storage
	check := sm.CheckDataTransfer(context.Background(), "CN", "cn-loc")
	if !check.Allowed {
		t.Errorf("expected CN->CN allowed: %s", check.Reason)
	}

	check = sm.CheckDataTransfer(context.Background(), "CN", "us-loc")
	if check.Allowed {
		t.Error("expected CN->US NOT allowed (require local)")
	}
}

func TestSovereigntyCheckFields(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "eu-west-1",
		Region:  "eu-west",
		Country: "IE",
		Enabled: true,
	})

	cm := NewComplianceManager(em)
	sm := NewSovereigntyManager(em, cm)

	check := sm.CheckDataTransfer(context.Background(), "DE", "eu-west-1")

	if check.SourceCountry != "DE" {
		t.Errorf("expected source DE, got %s", check.SourceCountry)
	}
	if check.TargetCountry != "IE" {
		t.Errorf("expected target IE, got %s", check.TargetCountry)
	}
	if check.TargetRegion != "eu-west" {
		t.Errorf("expected region eu-west, got %s", check.TargetRegion)
	}
	if check.Timestamp.IsZero() {
		t.Error("expected timestamp")
	}
}
