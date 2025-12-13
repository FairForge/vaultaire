// internal/global/georouting_test.go
package global

import (
	"context"
	"testing"
	"time"
)

func TestNewGeoRouter(t *testing.T) {
	em := NewEdgeManager(nil)
	router := NewGeoRouter(em, nil, nil)

	if router == nil {
		t.Fatal("expected non-nil router")
	}
	if router.config == nil {
		t.Error("expected default config")
	}
}

func TestGeoRouterAddRule(t *testing.T) {
	em := NewEdgeManager(nil)
	router := NewGeoRouter(em, nil, nil)

	router.AddRule(&RoutingRule{
		ID:       "rule-1",
		Name:     "Test Rule",
		Priority: 10,
		Enabled:  true,
	})

	rules := router.GetRules()
	if len(rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(rules))
	}
}

func TestGeoRouterRemoveRule(t *testing.T) {
	em := NewEdgeManager(nil)
	router := NewGeoRouter(em, nil, nil)

	router.AddRule(&RoutingRule{ID: "rule-1", Priority: 10, Enabled: true})
	router.AddRule(&RoutingRule{ID: "rule-2", Priority: 20, Enabled: true})

	removed := router.RemoveRule("rule-1")
	if !removed {
		t.Error("expected rule to be removed")
	}

	rules := router.GetRules()
	if len(rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ID != "rule-2" {
		t.Error("wrong rule removed")
	}
}

func TestGeoRouterRemoveNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	router := NewGeoRouter(em, nil, nil)

	removed := router.RemoveRule("nonexistent")
	if removed {
		t.Error("should not remove nonexistent rule")
	}
}

func TestGeoRouterRulePriority(t *testing.T) {
	em := NewEdgeManager(nil)
	router := NewGeoRouter(em, nil, nil)

	router.AddRule(&RoutingRule{ID: "low", Priority: 10, Enabled: true})
	router.AddRule(&RoutingRule{ID: "high", Priority: 100, Enabled: true})
	router.AddRule(&RoutingRule{ID: "medium", Priority: 50, Enabled: true})

	rules := router.GetRules()
	if rules[0].ID != "high" {
		t.Error("expected high priority first")
	}
	if rules[1].ID != "medium" {
		t.Error("expected medium priority second")
	}
	if rules[2].ID != "low" {
		t.Error("expected low priority last")
	}
}

func TestGeoRouterRouteWithRule(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-east-1", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "eu-west-1", Enabled: true})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{
		CountryCode: "DE",
		Latitude:    50.0,
		Longitude:   8.0,
	})

	router := NewGeoRouter(em, geoIP, nil)
	router.AddRule(&RoutingRule{
		ID:       "eu-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "DE"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "eu-west-1",
		},
	})

	ctx := context.Background()
	result, err := router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})

	if err != nil {
		t.Fatalf("routing error: %v", err)
	}
	if result.LocationID != "eu-west-1" {
		t.Errorf("expected eu-west-1, got %s", result.LocationID)
	}
	if result.Rule == nil || result.Rule.ID != "eu-rule" {
		t.Error("expected rule to be set")
	}
}

func TestGeoRouterRouteWithGeoIP(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "us-east-1",
		Latitude:  39.0,
		Longitude: -77.0,
		Enabled:   true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "eu-west-1",
		Latitude:  53.0,
		Longitude: -6.0,
		Enabled:   true,
	})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("5.6.7.8", &GeoIPResult{
		CountryCode: "GB",
		Latitude:    51.5,
		Longitude:   -0.1,
	})

	router := NewGeoRouter(em, geoIP, nil)

	ctx := context.Background()
	result, err := router.Route(ctx, &RoutingRequest{IP: "5.6.7.8"})

	if err != nil {
		t.Fatalf("routing error: %v", err)
	}
	// Should route to nearest (eu-west-1 is closer to London)
	if result.LocationID != "eu-west-1" {
		t.Errorf("expected eu-west-1, got %s", result.LocationID)
	}
	if result.Reason != "nearest location" {
		t.Errorf("expected 'nearest location', got '%s'", result.Reason)
	}
}

func TestGeoRouterRouteFallback(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "us-east-1", Enabled: true})

	config := &GeoRouterConfig{
		FallbackLocation: "us-east-1",
	}
	router := NewGeoRouter(em, nil, config)

	ctx := context.Background()
	result, err := router.Route(ctx, &RoutingRequest{})

	if err != nil {
		t.Fatalf("routing error: %v", err)
	}
	if result.LocationID != "us-east-1" {
		t.Errorf("expected us-east-1, got %s", result.LocationID)
	}
	if result.Reason != "fallback" {
		t.Errorf("expected 'fallback', got '%s'", result.Reason)
	}
}

func TestGeoRouterRouteReject(t *testing.T) {
	em := NewEdgeManager(nil)

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{CountryCode: "XX"})

	router := NewGeoRouter(em, geoIP, nil)
	router.AddRule(&RoutingRule{
		ID:       "block-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "XX"},
		},
		Action: RoutingAction{Type: ActionReject},
	})

	ctx := context.Background()
	_, err := router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})

	if err == nil {
		t.Error("expected rejection error")
	}
}

func TestGeoRouterConditionOperators(t *testing.T) {
	em := NewEdgeManager(nil)
	router := NewGeoRouter(em, nil, nil)

	tests := []struct {
		name     string
		value    string
		operator ConditionOperator
		target   string
		targets  []string
		expected bool
	}{
		{"equals match", "US", OperatorEquals, "US", nil, true},
		{"equals no match", "US", OperatorEquals, "UK", nil, false},
		{"equals case insensitive", "us", OperatorEquals, "US", nil, true},
		{"not equals match", "US", OperatorNotEquals, "UK", nil, true},
		{"not equals no match", "US", OperatorNotEquals, "US", nil, false},
		{"in match", "US", OperatorIn, "", []string{"US", "UK", "CA"}, true},
		{"in no match", "DE", OperatorIn, "", []string{"US", "UK", "CA"}, false},
		{"not in match", "DE", OperatorNotIn, "", []string{"US", "UK", "CA"}, true},
		{"not in no match", "US", OperatorNotIn, "", []string{"US", "UK", "CA"}, false},
		{"contains match", "hello world", OperatorContains, "world", nil, true},
		{"contains no match", "hello", OperatorContains, "world", nil, false},
		{"starts with match", "hello world", OperatorStartsWith, "hello", nil, true},
		{"starts with no match", "hello world", OperatorStartsWith, "world", nil, false},
		{"ends with match", "hello world", OperatorEndsWith, "world", nil, true},
		{"ends with no match", "hello world", OperatorEndsWith, "hello", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := router.compareValues(tt.value, tt.operator, tt.target, tt.targets)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestGeoRouterCIDRCondition(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "internal", Enabled: true})

	router := NewGeoRouter(em, nil, nil)
	router.AddRule(&RoutingRule{
		ID:       "internal-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCIDR, Values: []string{"10.0.0.0/8", "192.168.0.0/16"}},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "internal",
		},
	})

	ctx := context.Background()

	// Test matching IP
	result, _ := router.Route(ctx, &RoutingRequest{IP: "10.1.2.3"})
	if result.LocationID != "internal" {
		t.Error("expected internal routing for 10.x IP")
	}

	// Test non-matching IP
	router.SetDefaultLocation("external")
	_ = em.RegisterLocation(&EdgeLocation{ID: "external", Enabled: true})
	result, _ = router.Route(ctx, &RoutingRequest{IP: "8.8.8.8"})
	if result.LocationID == "internal" {
		t.Error("should not route 8.8.8.8 to internal")
	}
}

func TestGeoRouterWeightedRouting(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-a", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-b", Enabled: true})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{CountryCode: "US"})

	router := NewGeoRouter(em, geoIP, nil)
	router.AddRule(&RoutingRule{
		ID:       "weighted-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "US"},
		},
		Action: RoutingAction{
			Type:   ActionWeight,
			Weight: map[string]int{"loc-a": 70, "loc-b": 30},
		},
	})

	ctx := context.Background()
	result, err := router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})

	if err != nil {
		t.Fatalf("routing error: %v", err)
	}
	if result.LocationID != "loc-a" && result.LocationID != "loc-b" {
		t.Errorf("unexpected location: %s", result.LocationID)
	}
}

func TestGeoRouterHeaderCondition(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "premium", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "standard", Enabled: true})

	router := NewGeoRouter(em, nil, nil)
	router.SetDefaultLocation("standard")
	router.AddRule(&RoutingRule{
		ID:       "premium-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionHeader, Operator: OperatorEquals, Value: "X-Tier:premium"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "premium",
		},
	})

	ctx := context.Background()

	// Test with premium header
	result, _ := router.Route(ctx, &RoutingRequest{
		Headers: map[string]string{"X-Tier": "premium"},
	})
	if result.LocationID != "premium" {
		t.Errorf("expected premium, got %s", result.LocationID)
	}

	// Test without header
	result, _ = router.Route(ctx, &RoutingRequest{
		Headers: map[string]string{},
	})
	if result.LocationID != "standard" {
		t.Errorf("expected standard, got %s", result.LocationID)
	}
}

func TestGeoRouterTenantCondition(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "dedicated", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "shared", Enabled: true})

	router := NewGeoRouter(em, nil, nil)
	router.SetDefaultLocation("shared")
	router.AddRule(&RoutingRule{
		ID:       "dedicated-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionTenant, Operator: OperatorIn, Values: []string{"tenant-vip-1", "tenant-vip-2"}},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "dedicated",
		},
	})

	ctx := context.Background()

	result, _ := router.Route(ctx, &RoutingRequest{TenantID: "tenant-vip-1"})
	if result.LocationID != "dedicated" {
		t.Errorf("expected dedicated, got %s", result.LocationID)
	}

	result, _ = router.Route(ctx, &RoutingRequest{TenantID: "tenant-regular"})
	if result.LocationID != "shared" {
		t.Errorf("expected shared, got %s", result.LocationID)
	}
}

func TestGeoRouterPathCondition(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "api", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "static", Enabled: true})

	router := NewGeoRouter(em, nil, nil)
	router.SetDefaultLocation("static")
	router.AddRule(&RoutingRule{
		ID:       "api-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionPath, Operator: OperatorStartsWith, Value: "/api/"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "api",
		},
	})

	ctx := context.Background()

	result, _ := router.Route(ctx, &RoutingRequest{Path: "/api/v1/users"})
	if result.LocationID != "api" {
		t.Errorf("expected api, got %s", result.LocationID)
	}

	result, _ = router.Route(ctx, &RoutingRequest{Path: "/static/image.png"})
	if result.LocationID != "static" {
		t.Errorf("expected static, got %s", result.LocationID)
	}
}

func TestGeoRouterMetrics(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{CountryCode: "US"})

	router := NewGeoRouter(em, geoIP, nil)
	router.SetDefaultLocation("loc-1")
	router.AddRule(&RoutingRule{
		ID:       "test-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "US"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "loc-1",
		},
	})

	ctx := context.Background()

	// Make several requests
	for i := 0; i < 5; i++ {
		_, _ = router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})
	}
	// Make fallback request
	_, _ = router.Route(ctx, &RoutingRequest{})

	metrics := router.GetMetrics()
	if metrics.TotalRequests != 6 {
		t.Errorf("expected 6 requests, got %d", metrics.TotalRequests)
	}
	if metrics.GeoIPLookups != 5 {
		t.Errorf("expected 5 GeoIP lookups, got %d", metrics.GeoIPLookups)
	}
	if metrics.RuleMatches["test-rule"] != 5 {
		t.Errorf("expected 5 rule matches, got %d", metrics.RuleMatches["test-rule"])
	}
	if metrics.FallbackRoutes != 1 {
		t.Errorf("expected 1 fallback, got %d", metrics.FallbackRoutes)
	}
}

func TestGeoRouterDisabledRule(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "target", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "fallback", Enabled: true})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{CountryCode: "US"})

	router := NewGeoRouter(em, geoIP, nil)
	router.SetDefaultLocation("fallback")
	router.AddRule(&RoutingRule{
		ID:       "disabled-rule",
		Priority: 100,
		Enabled:  false, // Disabled
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "US"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "target",
		},
	})

	ctx := context.Background()
	result, _ := router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})

	if result.LocationID != "fallback" {
		t.Errorf("expected fallback (rule disabled), got %s", result.LocationID)
	}
}

func TestGeoRouterMultipleConditions(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "special", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "default", Enabled: true})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{CountryCode: "US", City: "New York"})
	geoIP.Add("5.6.7.8", &GeoIPResult{CountryCode: "US", City: "Los Angeles"})

	router := NewGeoRouter(em, geoIP, nil)
	router.SetDefaultLocation("default")
	router.AddRule(&RoutingRule{
		ID:       "multi-condition",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "US"},
			{Type: ConditionCity, Operator: OperatorEquals, Value: "New York"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "special",
		},
	})

	ctx := context.Background()

	// Both conditions match
	result, _ := router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})
	if result.LocationID != "special" {
		t.Errorf("expected special, got %s", result.LocationID)
	}

	// Only country matches
	result, _ = router.Route(ctx, &RoutingRequest{IP: "5.6.7.8"})
	if result.LocationID != "default" {
		t.Errorf("expected default, got %s", result.LocationID)
	}
}

func TestSimpleGeoIPLookup(t *testing.T) {
	lookup := NewSimpleGeoIPLookup()
	lookup.Add("1.2.3.4", &GeoIPResult{
		CountryCode: "US",
		City:        "New York",
	})

	result, err := lookup.Lookup("1.2.3.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CountryCode != "US" {
		t.Errorf("expected US, got %s", result.CountryCode)
	}

	_, err = lookup.Lookup("9.9.9.9")
	if err == nil {
		t.Error("expected error for unknown IP")
	}
}

func TestCommonRoutingRules(t *testing.T) {
	rules := CommonRoutingRules()

	if len(rules) == 0 {
		t.Fatal("expected common rules")
	}

	// Check EU GDPR rule exists
	foundGDPR := false
	for _, rule := range rules {
		if rule.ID == "eu-gdpr" {
			foundGDPR = true
			if rule.Action.LocationID != "eu-west-1" {
				t.Error("EU rule should route to eu-west-1")
			}
		}
	}
	if !foundGDPR {
		t.Error("expected EU GDPR rule")
	}
}

func TestDefaultGeoRouterConfig(t *testing.T) {
	config := DefaultGeoRouterConfig()

	if !config.EnableGeoIP {
		t.Error("expected GeoIP enabled by default")
	}
	if config.FallbackLocation == "" {
		t.Error("expected fallback location")
	}
	if config.AffinityTTL != 24*time.Hour {
		t.Errorf("unexpected affinity TTL: %v", config.AffinityTTL)
	}
}

func TestGeoRouterWithExplicitCoordinates(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "tokyo",
		Latitude:  35.68,
		Longitude: 139.65,
		Enabled:   true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "sydney",
		Latitude:  -33.87,
		Longitude: 151.21,
		Enabled:   true,
	})

	router := NewGeoRouter(em, nil, nil)

	ctx := context.Background()
	// Request from near Tokyo
	result, _ := router.Route(ctx, &RoutingRequest{
		Latitude:  35.0,
		Longitude: 140.0,
	})

	if result.LocationID != "tokyo" {
		t.Errorf("expected tokyo, got %s", result.LocationID)
	}
}

func TestGeoRouterActionHeaders(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})

	geoIP := NewSimpleGeoIPLookup()
	geoIP.Add("1.2.3.4", &GeoIPResult{CountryCode: "US"})

	router := NewGeoRouter(em, geoIP, nil)
	router.AddRule(&RoutingRule{
		ID:       "header-rule",
		Priority: 100,
		Enabled:  true,
		Conditions: []RoutingCondition{
			{Type: ConditionCountry, Operator: OperatorEquals, Value: "US"},
		},
		Action: RoutingAction{
			Type:       ActionRoute,
			LocationID: "loc-1",
			Headers:    map[string]string{"X-Region": "us", "X-Cache": "hit"},
		},
	})

	ctx := context.Background()
	result, _ := router.Route(ctx, &RoutingRequest{IP: "1.2.3.4"})

	if result.Headers["X-Region"] != "us" {
		t.Error("expected X-Region header")
	}
	if result.Headers["X-Cache"] != "hit" {
		t.Error("expected X-Cache header")
	}
}
