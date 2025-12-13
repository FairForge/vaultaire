// internal/global/georouting.go
package global

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// GeoRouter handles geographic routing of requests
type GeoRouter struct {
	mu          sync.RWMutex
	edgeManager *EdgeManager
	geoIPLookup GeoIPLookup
	rules       []*RoutingRule
	defaultLoc  string
	config      *GeoRouterConfig
	metrics     *GeoRouterMetrics
}

// GeoRouterConfig configures the geo router
type GeoRouterConfig struct {
	EnableGeoIP           bool
	EnableLatencyRouting  bool
	EnableAffinityRouting bool
	AffinityTTL           time.Duration
	FallbackLocation      string
	MaxRoutingLatency     time.Duration
}

// DefaultGeoRouterConfig returns default configuration
func DefaultGeoRouterConfig() *GeoRouterConfig {
	return &GeoRouterConfig{
		EnableGeoIP:           true,
		EnableLatencyRouting:  true,
		EnableAffinityRouting: true,
		AffinityTTL:           24 * time.Hour,
		FallbackLocation:      "us-east-1",
		MaxRoutingLatency:     50 * time.Millisecond,
	}
}

// GeoIPLookup provides geographic lookup from IP addresses
type GeoIPLookup interface {
	Lookup(ip string) (*GeoIPResult, error)
}

// GeoIPResult contains geographic information for an IP
type GeoIPResult struct {
	IP          string
	Country     string
	CountryCode string
	Region      string
	City        string
	Latitude    float64
	Longitude   float64
	Timezone    string
	ISP         string
	ASN         string
}

// RoutingRule defines a routing rule
type RoutingRule struct {
	ID          string
	Name        string
	Priority    int
	Enabled     bool
	Conditions  []RoutingCondition
	Action      RoutingAction
	Description string
}

// RoutingCondition defines a condition for routing
type RoutingCondition struct {
	Type     ConditionType
	Operator ConditionOperator
	Value    string
	Values   []string
}

// ConditionType represents types of routing conditions
type ConditionType string

const (
	ConditionCountry ConditionType = "country"
	ConditionRegion  ConditionType = "region"
	ConditionCity    ConditionType = "city"
	ConditionASN     ConditionType = "asn"
	ConditionIP      ConditionType = "ip"
	ConditionCIDR    ConditionType = "cidr"
	ConditionHeader  ConditionType = "header"
	ConditionPath    ConditionType = "path"
	ConditionTenant  ConditionType = "tenant"
)

// ConditionOperator represents comparison operators
type ConditionOperator string

const (
	OperatorEquals     ConditionOperator = "eq"
	OperatorNotEquals  ConditionOperator = "ne"
	OperatorIn         ConditionOperator = "in"
	OperatorNotIn      ConditionOperator = "not_in"
	OperatorContains   ConditionOperator = "contains"
	OperatorStartsWith ConditionOperator = "starts_with"
	OperatorEndsWith   ConditionOperator = "ends_with"
	OperatorMatches    ConditionOperator = "matches"
)

// RoutingAction defines the action to take
type RoutingAction struct {
	Type       ActionType
	LocationID string
	Locations  []string
	Weight     map[string]int
	Headers    map[string]string
}

// ActionType represents types of routing actions
type ActionType string

const (
	ActionRoute    ActionType = "route"
	ActionRedirect ActionType = "redirect"
	ActionReject   ActionType = "reject"
	ActionWeight   ActionType = "weight"
)

// GeoRouterMetrics tracks routing metrics
type GeoRouterMetrics struct {
	mu             sync.RWMutex
	TotalRequests  int64
	GeoIPLookups   int64
	GeoIPErrors    int64
	RuleMatches    map[string]int64
	LocationRoutes map[string]int64
	FallbackRoutes int64
	AvgRoutingTime time.Duration
}

// RoutingRequest contains request information for routing
type RoutingRequest struct {
	IP        string
	Headers   map[string]string
	Path      string
	TenantID  string
	Latitude  float64
	Longitude float64
	Affinity  string
}

// RoutingResult contains the routing decision
type RoutingResult struct {
	LocationID  string
	Location    *EdgeLocation
	Rule        *RoutingRule
	GeoIP       *GeoIPResult
	RoutingTime time.Duration
	Reason      string
	Headers     map[string]string
}

// NewGeoRouter creates a new geo router
func NewGeoRouter(edgeManager *EdgeManager, geoIP GeoIPLookup, config *GeoRouterConfig) *GeoRouter {
	if config == nil {
		config = DefaultGeoRouterConfig()
	}
	return &GeoRouter{
		edgeManager: edgeManager,
		geoIPLookup: geoIP,
		rules:       make([]*RoutingRule, 0),
		defaultLoc:  config.FallbackLocation,
		config:      config,
		metrics: &GeoRouterMetrics{
			RuleMatches:    make(map[string]int64),
			LocationRoutes: make(map[string]int64),
		},
	}
}

// AddRule adds a routing rule
func (r *GeoRouter) AddRule(rule *RoutingRule) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.rules = append(r.rules, rule)
	r.sortRules()
}

// RemoveRule removes a routing rule
func (r *GeoRouter) RemoveRule(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, rule := range r.rules {
		if rule.ID == id {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return true
		}
	}
	return false
}

// GetRules returns all routing rules
func (r *GeoRouter) GetRules() []*RoutingRule {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*RoutingRule, len(r.rules))
	copy(result, r.rules)
	return result
}

func (r *GeoRouter) sortRules() {
	// Sort by priority (higher first)
	for i := 0; i < len(r.rules)-1; i++ {
		for j := i + 1; j < len(r.rules); j++ {
			if r.rules[j].Priority > r.rules[i].Priority {
				r.rules[i], r.rules[j] = r.rules[j], r.rules[i]
			}
		}
	}
}

// Route determines the best location for a request
func (r *GeoRouter) Route(ctx context.Context, req *RoutingRequest) (*RoutingResult, error) {
	start := time.Now()
	result := &RoutingResult{
		Headers: make(map[string]string),
	}

	r.incrementRequests()

	// Get GeoIP info if enabled and IP provided
	var geoIP *GeoIPResult
	if r.config.EnableGeoIP && req.IP != "" && r.geoIPLookup != nil {
		var err error
		geoIP, err = r.geoIPLookup.Lookup(req.IP)
		if err != nil {
			r.incrementGeoIPErrors()
		} else {
			r.incrementGeoIPLookups()
			result.GeoIP = geoIP
			// Use GeoIP coordinates if not provided
			if req.Latitude == 0 && req.Longitude == 0 && geoIP != nil {
				req.Latitude = geoIP.Latitude
				req.Longitude = geoIP.Longitude
			}
		}
	}

	// Check routing rules
	r.mu.RLock()
	rules := make([]*RoutingRule, len(r.rules))
	copy(rules, r.rules)
	r.mu.RUnlock()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}

		if r.matchesRule(rule, req, geoIP) {
			r.incrementRuleMatch(rule.ID)

			switch rule.Action.Type {
			case ActionRoute:
				result.LocationID = rule.Action.LocationID
				result.Rule = rule
				result.Reason = fmt.Sprintf("matched rule: %s", rule.Name)
				if rule.Action.Headers != nil {
					for k, v := range rule.Action.Headers {
						result.Headers[k] = v
					}
				}
			case ActionWeight:
				result.LocationID = r.selectWeighted(rule.Action.Weight)
				result.Rule = rule
				result.Reason = fmt.Sprintf("weighted rule: %s", rule.Name)
			case ActionReject:
				return nil, fmt.Errorf("request rejected by rule: %s", rule.Name)
			}

			if result.LocationID != "" {
				break
			}
		}
	}

	// If no rule matched, use geo-based routing
	if result.LocationID == "" {
		if req.Latitude != 0 || req.Longitude != 0 {
			// Find nearest location
			loc := r.edgeManager.FindNearestLocation(req.Latitude, req.Longitude)
			if loc != nil {
				result.LocationID = loc.ID
				result.Location = loc
				result.Reason = "nearest location"
			}
		}
	}

	// Fallback to default
	if result.LocationID == "" {
		result.LocationID = r.defaultLoc
		result.Reason = "fallback"
		r.incrementFallback()
	}

	// Get location details if not already set
	if result.Location == nil {
		loc, _ := r.edgeManager.GetLocation(result.LocationID)
		result.Location = loc
	}

	r.incrementLocationRoute(result.LocationID)

	result.RoutingTime = time.Since(start)
	return result, nil
}

func (r *GeoRouter) matchesRule(rule *RoutingRule, req *RoutingRequest, geoIP *GeoIPResult) bool {
	for _, cond := range rule.Conditions {
		if !r.matchesCondition(cond, req, geoIP) {
			return false
		}
	}
	return len(rule.Conditions) > 0
}

func (r *GeoRouter) matchesCondition(cond RoutingCondition, req *RoutingRequest, geoIP *GeoIPResult) bool {
	var value string

	switch cond.Type {
	case ConditionCountry:
		if geoIP != nil {
			value = geoIP.CountryCode
		}
	case ConditionRegion:
		if geoIP != nil {
			value = geoIP.Region
		}
	case ConditionCity:
		if geoIP != nil {
			value = geoIP.City
		}
	case ConditionASN:
		if geoIP != nil {
			value = geoIP.ASN
		}
	case ConditionIP:
		value = req.IP
	case ConditionCIDR:
		return r.matchesCIDR(req.IP, cond.Value, cond.Values)
	case ConditionHeader:
		if req.Headers != nil {
			// Value format: "HeaderName:expectedValue"
			parts := strings.SplitN(cond.Value, ":", 2)
			if len(parts) == 2 {
				value = req.Headers[parts[0]]
				cond.Value = parts[1]
			}
		}
	case ConditionPath:
		value = req.Path
	case ConditionTenant:
		value = req.TenantID
	default:
		return false
	}

	return r.compareValues(value, cond.Operator, cond.Value, cond.Values)
}

func (r *GeoRouter) compareValues(value string, op ConditionOperator, target string, targets []string) bool {
	switch op {
	case OperatorEquals:
		return strings.EqualFold(value, target)
	case OperatorNotEquals:
		return !strings.EqualFold(value, target)
	case OperatorIn:
		for _, t := range targets {
			if strings.EqualFold(value, t) {
				return true
			}
		}
		return false
	case OperatorNotIn:
		for _, t := range targets {
			if strings.EqualFold(value, t) {
				return false
			}
		}
		return true
	case OperatorContains:
		return strings.Contains(strings.ToLower(value), strings.ToLower(target))
	case OperatorStartsWith:
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(target))
	case OperatorEndsWith:
		return strings.HasSuffix(strings.ToLower(value), strings.ToLower(target))
	default:
		return false
	}
}

func (r *GeoRouter) matchesCIDR(ip string, cidr string, cidrs []string) bool {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	checkCIDRs := cidrs
	if cidr != "" {
		checkCIDRs = append(checkCIDRs, cidr)
	}

	for _, c := range checkCIDRs {
		_, network, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if network.Contains(parsedIP) {
			return true
		}
	}
	return false
}

func (r *GeoRouter) selectWeighted(weights map[string]int) string {
	if len(weights) == 0 {
		return ""
	}

	total := 0
	for _, w := range weights {
		total += w
	}

	if total == 0 {
		// Return first location if all weights are 0
		for loc := range weights {
			return loc
		}
	}

	// Simple weighted selection based on time
	target := int(time.Now().UnixNano() % int64(total))
	current := 0

	for loc, w := range weights {
		current += w
		if target < current {
			return loc
		}
	}

	// Fallback to first
	for loc := range weights {
		return loc
	}
	return ""
}

// SetDefaultLocation sets the default fallback location
func (r *GeoRouter) SetDefaultLocation(locationID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultLoc = locationID
}

// GetMetrics returns routing metrics
func (r *GeoRouter) GetMetrics() *GeoRouterMetrics {
	r.metrics.mu.RLock()
	defer r.metrics.mu.RUnlock()

	// Return a copy
	copy := &GeoRouterMetrics{
		TotalRequests:  r.metrics.TotalRequests,
		GeoIPLookups:   r.metrics.GeoIPLookups,
		GeoIPErrors:    r.metrics.GeoIPErrors,
		FallbackRoutes: r.metrics.FallbackRoutes,
		AvgRoutingTime: r.metrics.AvgRoutingTime,
		RuleMatches:    make(map[string]int64),
		LocationRoutes: make(map[string]int64),
	}
	for k, v := range r.metrics.RuleMatches {
		copy.RuleMatches[k] = v
	}
	for k, v := range r.metrics.LocationRoutes {
		copy.LocationRoutes[k] = v
	}
	return copy
}

func (r *GeoRouter) incrementRequests() {
	r.metrics.mu.Lock()
	r.metrics.TotalRequests++
	r.metrics.mu.Unlock()
}

func (r *GeoRouter) incrementGeoIPLookups() {
	r.metrics.mu.Lock()
	r.metrics.GeoIPLookups++
	r.metrics.mu.Unlock()
}

func (r *GeoRouter) incrementGeoIPErrors() {
	r.metrics.mu.Lock()
	r.metrics.GeoIPErrors++
	r.metrics.mu.Unlock()
}

func (r *GeoRouter) incrementRuleMatch(ruleID string) {
	r.metrics.mu.Lock()
	r.metrics.RuleMatches[ruleID]++
	r.metrics.mu.Unlock()
}

func (r *GeoRouter) incrementLocationRoute(locationID string) {
	r.metrics.mu.Lock()
	r.metrics.LocationRoutes[locationID]++
	r.metrics.mu.Unlock()
}

func (r *GeoRouter) incrementFallback() {
	r.metrics.mu.Lock()
	r.metrics.FallbackRoutes++
	r.metrics.mu.Unlock()
}

// SimpleGeoIPLookup provides a basic GeoIP implementation for testing
type SimpleGeoIPLookup struct {
	data map[string]*GeoIPResult
}

// NewSimpleGeoIPLookup creates a simple GeoIP lookup
func NewSimpleGeoIPLookup() *SimpleGeoIPLookup {
	return &SimpleGeoIPLookup{
		data: make(map[string]*GeoIPResult),
	}
}

// Add adds a GeoIP entry
func (s *SimpleGeoIPLookup) Add(ip string, result *GeoIPResult) {
	s.data[ip] = result
}

// Lookup looks up an IP address
func (s *SimpleGeoIPLookup) Lookup(ip string) (*GeoIPResult, error) {
	if result, ok := s.data[ip]; ok {
		return result, nil
	}
	return nil, fmt.Errorf("IP not found: %s", ip)
}

// CommonRoutingRules returns commonly used routing rules
func CommonRoutingRules() []*RoutingRule {
	return []*RoutingRule{
		{
			ID:       "eu-gdpr",
			Name:     "EU GDPR Compliance",
			Priority: 100,
			Enabled:  true,
			Conditions: []RoutingCondition{
				{Type: ConditionCountry, Operator: OperatorIn, Values: []string{
					"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
					"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
					"PL", "PT", "RO", "SK", "SI", "ES", "SE",
				}},
			},
			Action: RoutingAction{
				Type:       ActionRoute,
				LocationID: "eu-west-1",
			},
			Description: "Route EU users to EU data centers for GDPR compliance",
		},
		{
			ID:       "china-routing",
			Name:     "China Routing",
			Priority: 90,
			Enabled:  true,
			Conditions: []RoutingCondition{
				{Type: ConditionCountry, Operator: OperatorEquals, Value: "CN"},
			},
			Action: RoutingAction{
				Type:       ActionRoute,
				LocationID: "ap-northeast-1",
			},
			Description: "Route China traffic to nearest compliant region",
		},
		{
			ID:       "latam-routing",
			Name:     "Latin America Routing",
			Priority: 80,
			Enabled:  true,
			Conditions: []RoutingCondition{
				{Type: ConditionCountry, Operator: OperatorIn, Values: []string{
					"BR", "AR", "CL", "CO", "PE", "VE", "EC", "BO", "PY", "UY",
				}},
			},
			Action: RoutingAction{
				Type:       ActionRoute,
				LocationID: "sa-east-1",
			},
			Description: "Route Latin America traffic to São Paulo",
		},
	}
}
