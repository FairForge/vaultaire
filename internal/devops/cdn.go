// internal/devops/cdn.go
package devops

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// CDNProvider represents supported CDN providers
type CDNProvider string

const (
	CDNProviderCloudflare CDNProvider = "cloudflare"
	CDNProviderFastly     CDNProvider = "fastly"
	CDNProviderBunny      CDNProvider = "bunny"
	CDNProviderNone       CDNProvider = "none"
)

// CacheLevel represents caching aggressiveness
type CacheLevel string

const (
	CacheLevelBypass     CacheLevel = "bypass"
	CacheLevelStandard   CacheLevel = "standard"
	CacheLevelAggressive CacheLevel = "aggressive"
	CacheLevelEverything CacheLevel = "everything"
)

// CDNConfig configures CDN settings
type CDNConfig struct {
	Provider        CDNProvider   `json:"provider"`
	Enabled         bool          `json:"enabled"`
	ZoneID          string        `json:"zone_id,omitempty"`
	APIToken        string        `json:"api_token,omitempty"`
	DefaultTTL      time.Duration `json:"default_ttl"`
	MaxTTL          time.Duration `json:"max_ttl"`
	BrowserTTL      time.Duration `json:"browser_ttl"`
	CacheLevel      CacheLevel    `json:"cache_level"`
	MinifyJS        bool          `json:"minify_js"`
	MinifyCSS       bool          `json:"minify_css"`
	MinifyHTML      bool          `json:"minify_html"`
	AutoHTTPS       bool          `json:"auto_https"`
	HTTP2           bool          `json:"http2"`
	HTTP3           bool          `json:"http3"`
	Brotli          bool          `json:"brotli"`
	EarlyHints      bool          `json:"early_hints"`
	WebSockets      bool          `json:"websockets"`
	AlwaysOnline    bool          `json:"always_online"`
	DevelopmentMode bool          `json:"development_mode"`
}

// DefaultCDNConfigs returns environment-specific CDN configurations
var DefaultCDNConfigs = map[string]*CDNConfig{
	EnvTypeDevelopment: {
		Provider:        CDNProviderNone,
		Enabled:         false,
		DefaultTTL:      0,
		CacheLevel:      CacheLevelBypass,
		DevelopmentMode: true,
	},
	EnvTypeStaging: {
		Provider:        CDNProviderCloudflare,
		Enabled:         true,
		DefaultTTL:      1 * time.Hour,
		MaxTTL:          24 * time.Hour,
		BrowserTTL:      30 * time.Minute,
		CacheLevel:      CacheLevelStandard,
		MinifyJS:        true,
		MinifyCSS:       true,
		MinifyHTML:      false,
		AutoHTTPS:       true,
		HTTP2:           true,
		HTTP3:           true,
		Brotli:          true,
		WebSockets:      true,
		DevelopmentMode: false,
	},
	EnvTypeProduction: {
		Provider:        CDNProviderCloudflare,
		Enabled:         true,
		DefaultTTL:      4 * time.Hour,
		MaxTTL:          7 * 24 * time.Hour,
		BrowserTTL:      1 * time.Hour,
		CacheLevel:      CacheLevelAggressive,
		MinifyJS:        true,
		MinifyCSS:       true,
		MinifyHTML:      true,
		AutoHTTPS:       true,
		HTTP2:           true,
		HTTP3:           true,
		Brotli:          true,
		EarlyHints:      true,
		WebSockets:      true,
		AlwaysOnline:    true,
		DevelopmentMode: false,
	},
}

// CacheRule defines a custom caching rule
type CacheRule struct {
	Name        string        `json:"name"`
	PathPattern string        `json:"path_pattern"`
	CacheLevel  CacheLevel    `json:"cache_level"`
	TTL         time.Duration `json:"ttl"`
	Headers     []string      `json:"headers,omitempty"`
	Enabled     bool          `json:"enabled"`
}

// Origin represents a CDN origin server
type Origin struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Weight   int    `json:"weight"`
	Healthy  bool   `json:"healthy"`
	Primary  bool   `json:"primary"`
	Location string `json:"location"`
}

// CDNManager manages CDN configuration
type CDNManager struct {
	config  *CDNConfig
	rules   []*CacheRule
	origins []*Origin
	mu      sync.RWMutex
}

// NewCDNManager creates a CDN manager
func NewCDNManager(config *CDNConfig) *CDNManager {
	if config == nil {
		config = DefaultCDNConfigs[EnvTypeDevelopment]
	}
	return &CDNManager{
		config:  config,
		rules:   make([]*CacheRule, 0),
		origins: make([]*Origin, 0),
	}
}

// GetConfig returns the CDN configuration
func (m *CDNManager) GetConfig() *CDNConfig {
	return m.config
}

// IsEnabled returns whether CDN is enabled
func (m *CDNManager) IsEnabled() bool {
	return m.config.Enabled
}

// AddOrigin adds an origin server
func (m *CDNManager) AddOrigin(origin *Origin) error {
	if origin == nil {
		return errors.New("cdn: origin is required")
	}
	if origin.Name == "" {
		return errors.New("cdn: origin name is required")
	}
	if origin.Address == "" {
		return errors.New("cdn: origin address is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicate
	for _, o := range m.origins {
		if o.Name == origin.Name {
			return fmt.Errorf("cdn: origin %s already exists", origin.Name)
		}
	}

	if origin.Port == 0 {
		origin.Port = 443
	}
	if origin.Weight == 0 {
		origin.Weight = 100
	}
	origin.Healthy = true

	m.origins = append(m.origins, origin)
	return nil
}

// GetOrigin returns an origin by name
func (m *CDNManager) GetOrigin(name string) *Origin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, o := range m.origins {
		if o.Name == name {
			return o
		}
	}
	return nil
}

// ListOrigins returns all origins
func (m *CDNManager) ListOrigins() []*Origin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	origins := make([]*Origin, len(m.origins))
	copy(origins, m.origins)
	return origins
}

// GetHealthyOrigins returns only healthy origins
func (m *CDNManager) GetHealthyOrigins() []*Origin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var healthy []*Origin
	for _, o := range m.origins {
		if o.Healthy {
			healthy = append(healthy, o)
		}
	}
	return healthy
}

// SetOriginHealth sets the health status of an origin
func (m *CDNManager) SetOriginHealth(name string, healthy bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, o := range m.origins {
		if o.Name == name {
			o.Healthy = healthy
			return nil
		}
	}
	return fmt.Errorf("cdn: origin %s not found", name)
}

// RemoveOrigin removes an origin
func (m *CDNManager) RemoveOrigin(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, o := range m.origins {
		if o.Name == name {
			m.origins = append(m.origins[:i], m.origins[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("cdn: origin %s not found", name)
}

// AddCacheRule adds a cache rule
func (m *CDNManager) AddCacheRule(rule *CacheRule) error {
	if rule == nil {
		return errors.New("cdn: rule is required")
	}
	if rule.Name == "" {
		return errors.New("cdn: rule name is required")
	}
	if rule.PathPattern == "" {
		return errors.New("cdn: rule path pattern is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicate
	for _, r := range m.rules {
		if r.Name == rule.Name {
			return fmt.Errorf("cdn: rule %s already exists", rule.Name)
		}
	}

	if rule.CacheLevel == "" {
		rule.CacheLevel = CacheLevelStandard
	}
	rule.Enabled = true

	m.rules = append(m.rules, rule)
	return nil
}

// GetCacheRule returns a cache rule by name
func (m *CDNManager) GetCacheRule(name string) *CacheRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, r := range m.rules {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// ListCacheRules returns all cache rules
func (m *CDNManager) ListCacheRules() []*CacheRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rules := make([]*CacheRule, len(m.rules))
	copy(rules, m.rules)
	return rules
}

// RemoveCacheRule removes a cache rule
func (m *CDNManager) RemoveCacheRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, r := range m.rules {
		if r.Name == name {
			m.rules = append(m.rules[:i], m.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("cdn: rule %s not found", name)
}

// GetCacheHeaders returns appropriate cache headers for a path
func (m *CDNManager) GetCacheHeaders(path string) map[string]string {
	headers := make(map[string]string)

	if !m.config.Enabled {
		headers["Cache-Control"] = "no-store"
		return headers
	}

	// Check for matching rules
	m.mu.RLock()
	var matchedRule *CacheRule
	for _, r := range m.rules {
		if r.Enabled && pathMatches(path, r.PathPattern) {
			matchedRule = r
			break
		}
	}
	m.mu.RUnlock()

	var ttl time.Duration
	var cacheLevel CacheLevel

	if matchedRule != nil {
		ttl = matchedRule.TTL
		cacheLevel = matchedRule.CacheLevel
	} else {
		ttl = m.config.DefaultTTL
		cacheLevel = m.config.CacheLevel
	}

	switch cacheLevel {
	case CacheLevelBypass:
		headers["Cache-Control"] = "no-store"
	case CacheLevelStandard:
		headers["Cache-Control"] = fmt.Sprintf("public, max-age=%d", int(ttl.Seconds()))
	case CacheLevelAggressive:
		headers["Cache-Control"] = fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d",
			int(ttl.Seconds()), int(ttl.Seconds()/2))
	case CacheLevelEverything:
		headers["Cache-Control"] = fmt.Sprintf("public, max-age=%d, immutable",
			int(m.config.MaxTTL.Seconds()))
	}

	return headers
}

// SetupProductionCDN configures CDN for production with two origins
func (m *CDNManager) SetupProductionCDN(nycIP, laIP string) error {
	// Add origins
	origins := []*Origin{
		{
			Name:     "nyc-hub",
			Address:  nycIP,
			Port:     443,
			Weight:   100,
			Primary:  true,
			Location: "us-east",
		},
		{
			Name:     "la-worker",
			Address:  laIP,
			Port:     443,
			Weight:   100,
			Primary:  false,
			Location: "us-west",
		},
	}

	for _, o := range origins {
		if err := m.AddOrigin(o); err != nil {
			return err
		}
	}

	// Add cache rules for stored.ge
	rules := []*CacheRule{
		{
			Name:        "api-no-cache",
			PathPattern: "/api/*",
			CacheLevel:  CacheLevelBypass,
			TTL:         0,
		},
		{
			Name:        "health-short-cache",
			PathPattern: "/health",
			CacheLevel:  CacheLevelStandard,
			TTL:         10 * time.Second,
		},
		{
			Name:        "static-assets",
			PathPattern: "/static/*",
			CacheLevel:  CacheLevelEverything,
			TTL:         30 * 24 * time.Hour,
		},
		{
			Name:        "dashboard-assets",
			PathPattern: "/dashboard/assets/*",
			CacheLevel:  CacheLevelAggressive,
			TTL:         24 * time.Hour,
		},
		{
			Name:        "object-downloads",
			PathPattern: "/v1/objects/*",
			CacheLevel:  CacheLevelBypass, // S3 objects have their own caching
			TTL:         0,
		},
	}

	for _, r := range rules {
		if err := m.AddCacheRule(r); err != nil {
			return err
		}
	}

	return nil
}

// GenerateCloudflareConfig generates Cloudflare-compatible configuration
func (m *CDNManager) GenerateCloudflareConfig() map[string]interface{} {
	config := map[string]interface{}{
		"ssl":               "full_strict",
		"always_use_https":  m.config.AutoHTTPS,
		"http2":             m.config.HTTP2,
		"http3":             m.config.HTTP3,
		"brotli":            m.config.Brotli,
		"early_hints":       m.config.EarlyHints,
		"websockets":        m.config.WebSockets,
		"always_online":     m.config.AlwaysOnline,
		"development_mode":  m.config.DevelopmentMode,
		"browser_cache_ttl": int(m.config.BrowserTTL.Seconds()),
	}

	if m.config.MinifyJS || m.config.MinifyCSS || m.config.MinifyHTML {
		config["minify"] = map[string]bool{
			"js":   m.config.MinifyJS,
			"css":  m.config.MinifyCSS,
			"html": m.config.MinifyHTML,
		}
	}

	return config
}

// pathMatches checks if a path matches a pattern (simple glob)
func pathMatches(path, pattern string) bool {
	if pattern == "*" {
		return true
	}

	// Handle trailing wildcard
	if len(pattern) > 0 && pattern[len(pattern)-1] == '*' {
		prefix := pattern[:len(pattern)-1]
		return len(path) >= len(prefix) && path[:len(prefix)] == prefix
	}

	return path == pattern
}

// GetCDNConfigForEnvironment returns CDN config for an environment
func GetCDNConfigForEnvironment(envType string) *CDNConfig {
	if config, ok := DefaultCDNConfigs[envType]; ok {
		return config
	}
	return DefaultCDNConfigs[EnvTypeDevelopment]
}
