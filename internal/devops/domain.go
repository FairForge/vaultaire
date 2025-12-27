// internal/devops/domain.go
package devops

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DomainType represents the type of domain
type DomainType string

const (
	DomainTypePrimary   DomainType = "primary"
	DomainTypeAPI       DomainType = "api"
	DomainTypeDashboard DomainType = "dashboard"
	DomainTypeCDN       DomainType = "cdn"
	DomainTypeCustom    DomainType = "custom"
)

// DomainStatus represents the verification status
type DomainStatus string

const (
	DomainStatusPending  DomainStatus = "pending"
	DomainStatusVerified DomainStatus = "verified"
	DomainStatusFailed   DomainStatus = "failed"
	DomainStatusExpired  DomainStatus = "expired"
)

// Domain represents a configured domain
type Domain struct {
	Name         string       `json:"name"`
	Type         DomainType   `json:"type"`
	Status       DomainStatus `json:"status"`
	Environment  string       `json:"environment"`
	SSLEnabled   bool         `json:"ssl_enabled"`
	SSLCertPath  string       `json:"ssl_cert_path,omitempty"`
	SSLKeyPath   string       `json:"ssl_key_path,omitempty"`
	TargetIP     string       `json:"target_ip,omitempty"`
	TargetCNAME  string       `json:"target_cname,omitempty"`
	VerifiedAt   *time.Time   `json:"verified_at,omitempty"`
	LastChecked  *time.Time   `json:"last_checked,omitempty"`
	ErrorMessage string       `json:"error_message,omitempty"`
}

// DomainConfig holds domain configuration
type DomainConfig struct {
	PrimaryDomain   string   `json:"primary_domain"`
	APIDomain       string   `json:"api_domain"`
	DashboardDomain string   `json:"dashboard_domain"`
	CDNDomain       string   `json:"cdn_domain,omitempty"`
	AllowedOrigins  []string `json:"allowed_origins"`
	RedirectWWW     bool     `json:"redirect_www"`
	ForceHTTPS      bool     `json:"force_https"`
	HSTSEnabled     bool     `json:"hsts_enabled"`
	HSTSMaxAge      int      `json:"hsts_max_age"`
}

// DefaultDomainConfigs returns environment-specific domain configurations
var DefaultDomainConfigs = map[string]*DomainConfig{
	EnvTypeDevelopment: {
		PrimaryDomain:   "localhost",
		APIDomain:       "localhost",
		DashboardDomain: "localhost",
		AllowedOrigins:  []string{"*"},
		RedirectWWW:     false,
		ForceHTTPS:      false,
		HSTSEnabled:     false,
	},
	EnvTypeStaging: {
		PrimaryDomain:   "staging.stored.ge",
		APIDomain:       "staging.stored.ge",
		DashboardDomain: "staging-dashboard.stored.ge",
		AllowedOrigins: []string{
			"https://staging.stored.ge",
			"https://staging-dashboard.stored.ge",
		},
		RedirectWWW: true,
		ForceHTTPS:  true,
		HSTSEnabled: true,
		HSTSMaxAge:  86400, // 1 day for staging
	},
	EnvTypeProduction: {
		PrimaryDomain:   "stored.ge",
		APIDomain:       "api.stored.ge",
		DashboardDomain: "dashboard.stored.ge",
		CDNDomain:       "cdn.stored.ge",
		AllowedOrigins: []string{
			"https://stored.ge",
			"https://www.stored.ge",
			"https://api.stored.ge",
			"https://dashboard.stored.ge",
		},
		RedirectWWW: true,
		ForceHTTPS:  true,
		HSTSEnabled: true,
		HSTSMaxAge:  31536000, // 1 year
	},
}

// DomainManager manages domain configuration
type DomainManager struct {
	config  *DomainConfig
	domains map[string]*Domain
	mu      sync.RWMutex
}

// NewDomainManager creates a domain manager
func NewDomainManager(config *DomainConfig) *DomainManager {
	if config == nil {
		config = DefaultDomainConfigs[EnvTypeDevelopment]
	}
	return &DomainManager{
		config:  config,
		domains: make(map[string]*Domain),
	}
}

// GetConfig returns the domain configuration
func (m *DomainManager) GetConfig() *DomainConfig {
	return m.config
}

// AddDomain adds a domain to manage
func (m *DomainManager) AddDomain(domain *Domain) error {
	if domain == nil {
		return errors.New("domain: domain is required")
	}
	if domain.Name == "" {
		return errors.New("domain: name is required")
	}
	if !IsValidDomain(domain.Name) {
		return fmt.Errorf("domain: invalid domain name: %s", domain.Name)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.domains[domain.Name]; exists {
		return fmt.Errorf("domain: %s already exists", domain.Name)
	}

	if domain.Status == "" {
		domain.Status = DomainStatusPending
	}

	m.domains[domain.Name] = domain
	return nil
}

// GetDomain returns a domain by name
func (m *DomainManager) GetDomain(name string) *Domain {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.domains[name]
}

// ListDomains returns all managed domains
func (m *DomainManager) ListDomains() []*Domain {
	m.mu.RLock()
	defer m.mu.RUnlock()

	domains := make([]*Domain, 0, len(m.domains))
	for _, d := range m.domains {
		domains = append(domains, d)
	}
	return domains
}

// RemoveDomain removes a domain
func (m *DomainManager) RemoveDomain(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.domains[name]; !exists {
		return fmt.Errorf("domain: %s not found", name)
	}

	delete(m.domains, name)
	return nil
}

// VerifyDomain checks DNS configuration for a domain
func (m *DomainManager) VerifyDomain(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	domain, exists := m.domains[name]
	if !exists {
		return fmt.Errorf("domain: %s not found", name)
	}

	now := time.Now()
	domain.LastChecked = &now

	// Check DNS resolution
	ips, err := net.LookupIP(name)
	if err != nil {
		domain.Status = DomainStatusFailed
		domain.ErrorMessage = fmt.Sprintf("DNS lookup failed: %v", err)
		return fmt.Errorf("domain: DNS verification failed for %s: %w", name, err)
	}

	if len(ips) == 0 {
		domain.Status = DomainStatusFailed
		domain.ErrorMessage = "no DNS records found"
		return fmt.Errorf("domain: no DNS records found for %s", name)
	}

	// If target IP is specified, verify it matches
	if domain.TargetIP != "" {
		found := false
		for _, ip := range ips {
			if ip.String() == domain.TargetIP {
				found = true
				break
			}
		}
		if !found {
			domain.Status = DomainStatusFailed
			domain.ErrorMessage = fmt.Sprintf("DNS does not point to expected IP %s", domain.TargetIP)
			return fmt.Errorf("domain: %s does not point to expected IP %s", name, domain.TargetIP)
		}
	}

	domain.Status = DomainStatusVerified
	domain.VerifiedAt = &now
	domain.ErrorMessage = ""

	return nil
}

// SetupProductionDomains creates the standard domain set for production
func (m *DomainManager) SetupProductionDomains(targetIP string) error {
	domains := []*Domain{
		{
			Name:        m.config.PrimaryDomain,
			Type:        DomainTypePrimary,
			Environment: EnvTypeProduction,
			SSLEnabled:  true,
			TargetIP:    targetIP,
		},
		{
			Name:        "www." + m.config.PrimaryDomain,
			Type:        DomainTypePrimary,
			Environment: EnvTypeProduction,
			SSLEnabled:  true,
			TargetIP:    targetIP,
		},
		{
			Name:        m.config.APIDomain,
			Type:        DomainTypeAPI,
			Environment: EnvTypeProduction,
			SSLEnabled:  true,
			TargetIP:    targetIP,
		},
		{
			Name:        m.config.DashboardDomain,
			Type:        DomainTypeDashboard,
			Environment: EnvTypeProduction,
			SSLEnabled:  true,
			TargetIP:    targetIP,
		},
	}

	// Add CDN domain if configured
	if m.config.CDNDomain != "" {
		domains = append(domains, &Domain{
			Name:        m.config.CDNDomain,
			Type:        DomainTypeCDN,
			Environment: EnvTypeProduction,
			SSLEnabled:  true,
			TargetIP:    targetIP,
		})
	}

	for _, d := range domains {
		if err := m.AddDomain(d); err != nil {
			return fmt.Errorf("domain: failed to add %s: %w", d.Name, err)
		}
	}

	return nil
}

// IsAllowedOrigin checks if an origin is in the allowed list
func (m *DomainManager) IsAllowedOrigin(origin string) bool {
	for _, allowed := range m.config.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}

// GetCORSHeaders returns CORS headers for a given origin
func (m *DomainManager) GetCORSHeaders(origin string) map[string]string {
	headers := make(map[string]string)

	if m.IsAllowedOrigin(origin) {
		headers["Access-Control-Allow-Origin"] = origin
		headers["Access-Control-Allow-Methods"] = "GET, POST, PUT, DELETE, OPTIONS"
		headers["Access-Control-Allow-Headers"] = "Authorization, Content-Type, X-Request-ID"
		headers["Access-Control-Max-Age"] = "86400"

		if m.config.AllowedOrigins[0] != "*" {
			headers["Access-Control-Allow-Credentials"] = "true"
		}
	}

	return headers
}

// GetSecurityHeaders returns security headers for responses
func (m *DomainManager) GetSecurityHeaders() map[string]string {
	headers := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"X-XSS-Protection":       "1; mode=block",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}

	if m.config.HSTSEnabled && m.config.HSTSMaxAge > 0 {
		headers["Strict-Transport-Security"] = fmt.Sprintf(
			"max-age=%d; includeSubDomains; preload",
			m.config.HSTSMaxAge,
		)
	}

	return headers
}

// IsValidDomain validates a domain name
func IsValidDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}

	// Allow localhost for development
	if domain == "localhost" {
		return true
	}

	// Domain regex pattern
	pattern := `^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`
	matched, _ := regexp.MatchString(pattern, domain)
	return matched
}

// NormalizeDomain normalizes a domain name
func NormalizeDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimSuffix(domain, "/")
	return domain
}

// GetDomainConfigForEnvironment returns domain config for an environment
func GetDomainConfigForEnvironment(envType string) *DomainConfig {
	if config, ok := DefaultDomainConfigs[envType]; ok {
		return config
	}
	return DefaultDomainConfigs[EnvTypeDevelopment]
}
