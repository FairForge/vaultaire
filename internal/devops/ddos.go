// internal/devops/ddos.go
package devops

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ThreatLevel represents the severity of detected threats
type ThreatLevel string

const (
	ThreatLevelNone     ThreatLevel = "none"
	ThreatLevelLow      ThreatLevel = "low"
	ThreatLevelMedium   ThreatLevel = "medium"
	ThreatLevelHigh     ThreatLevel = "high"
	ThreatLevelCritical ThreatLevel = "critical"
)

// MitigationMode represents how to handle attacks
type MitigationMode string

const (
	MitigationModeOff        MitigationMode = "off"
	MitigationModeMonitor    MitigationMode = "monitor"
	MitigationModeChallenge  MitigationMode = "challenge"
	MitigationModeBlock      MitigationMode = "block"
	MitigationModeAggressive MitigationMode = "aggressive"
)

// DDoSConfig configures DDoS protection
type DDoSConfig struct {
	Enabled             bool           `json:"enabled"`
	Mode                MitigationMode `json:"mode"`
	RateLimit           int            `json:"rate_limit"`           // Requests per second per IP
	BurstLimit          int            `json:"burst_limit"`          // Max burst
	ConnectionLimit     int            `json:"connection_limit"`     // Max concurrent connections per IP
	BanDuration         time.Duration  `json:"ban_duration"`         // How long to ban offenders
	ChallengeThreshold  int            `json:"challenge_threshold"`  // Requests before challenge
	SuspiciousThreshold int            `json:"suspicious_threshold"` // Threshold for suspicious activity
	WhitelistEnabled    bool           `json:"whitelist_enabled"`
	GeoBlockingEnabled  bool           `json:"geo_blocking_enabled"`
	BlockedCountries    []string       `json:"blocked_countries"`
	SlowLorisProtection bool           `json:"slowloris_protection"`
	SYNFloodProtection  bool           `json:"syn_flood_protection"`
}

// DefaultDDoSConfigs returns environment-specific configurations
var DefaultDDoSConfigs = map[string]*DDoSConfig{
	EnvTypeDevelopment: {
		Enabled:            false,
		Mode:               MitigationModeMonitor,
		RateLimit:          1000,
		BurstLimit:         2000,
		ConnectionLimit:    100,
		BanDuration:        5 * time.Minute,
		WhitelistEnabled:   true,
		GeoBlockingEnabled: false,
	},
	EnvTypeStaging: {
		Enabled:             true,
		Mode:                MitigationModeChallenge,
		RateLimit:           100,
		BurstLimit:          200,
		ConnectionLimit:     50,
		BanDuration:         15 * time.Minute,
		ChallengeThreshold:  50,
		SuspiciousThreshold: 30,
		WhitelistEnabled:    true,
		GeoBlockingEnabled:  false,
		SlowLorisProtection: true,
		SYNFloodProtection:  true,
	},
	EnvTypeProduction: {
		Enabled:             true,
		Mode:                MitigationModeBlock,
		RateLimit:           50,
		BurstLimit:          100,
		ConnectionLimit:     30,
		BanDuration:         1 * time.Hour,
		ChallengeThreshold:  30,
		SuspiciousThreshold: 20,
		WhitelistEnabled:    true,
		GeoBlockingEnabled:  true,
		BlockedCountries:    []string{}, // Configure as needed
		SlowLorisProtection: true,
		SYNFloodProtection:  true,
	},
}

// IPReputation tracks an IP's behavior
type IPReputation struct {
	IP             string     `json:"ip"`
	RequestCount   int64      `json:"request_count"`
	BlockedCount   int64      `json:"blocked_count"`
	LastSeen       time.Time  `json:"last_seen"`
	FirstSeen      time.Time  `json:"first_seen"`
	ThreatScore    int        `json:"threat_score"`
	Banned         bool       `json:"banned"`
	BannedAt       *time.Time `json:"banned_at,omitempty"`
	BanExpires     *time.Time `json:"ban_expires,omitempty"`
	BanReason      string     `json:"ban_reason,omitempty"`
	Whitelisted    bool       `json:"whitelisted"`
	Country        string     `json:"country,omitempty"`
	ASN            string     `json:"asn,omitempty"`
	RequestsPerSec float64    `json:"requests_per_sec"`
}

// AttackEvent represents a detected attack
type AttackEvent struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	SourceIP    string      `json:"source_ip"`
	TargetPath  string      `json:"target_path"`
	ThreatLevel ThreatLevel `json:"threat_level"`
	DetectedAt  time.Time   `json:"detected_at"`
	Mitigated   bool        `json:"mitigated"`
	Details     string      `json:"details"`
}

// DDoSManager manages DDoS protection
type DDoSManager struct {
	config     *DDoSConfig
	ipData     map[string]*IPReputation
	whitelist  map[string]bool
	blacklist  map[string]bool
	attacks    []*AttackEvent
	attackChan chan *AttackEvent
	mu         sync.RWMutex
}

// NewDDoSManager creates a DDoS protection manager
func NewDDoSManager(config *DDoSConfig) *DDoSManager {
	if config == nil {
		config = DefaultDDoSConfigs[EnvTypeDevelopment]
	}
	return &DDoSManager{
		config:     config,
		ipData:     make(map[string]*IPReputation),
		whitelist:  make(map[string]bool),
		blacklist:  make(map[string]bool),
		attacks:    make([]*AttackEvent, 0),
		attackChan: make(chan *AttackEvent, 1000),
	}
}

// GetConfig returns the configuration
func (m *DDoSManager) GetConfig() *DDoSConfig {
	return m.config
}

// IsEnabled returns whether protection is enabled
func (m *DDoSManager) IsEnabled() bool {
	return m.config.Enabled
}

// AddToWhitelist adds an IP or CIDR to the whitelist
func (m *DDoSManager) AddToWhitelist(ipOrCIDR string) error {
	if ipOrCIDR == "" {
		return errors.New("ddos: IP or CIDR is required")
	}

	// Validate
	if ip := net.ParseIP(ipOrCIDR); ip == nil {
		if _, _, err := net.ParseCIDR(ipOrCIDR); err != nil {
			return fmt.Errorf("ddos: invalid IP or CIDR: %s", ipOrCIDR)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.whitelist[ipOrCIDR] = true
	return nil
}

// RemoveFromWhitelist removes an IP from the whitelist
func (m *DDoSManager) RemoveFromWhitelist(ipOrCIDR string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.whitelist, ipOrCIDR)
}

// IsWhitelisted checks if an IP is whitelisted
func (m *DDoSManager) IsWhitelisted(ip string) bool {
	if !m.config.WhitelistEnabled {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Direct match
	if m.whitelist[ip] {
		return true
	}

	// Check CIDR ranges
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	for entry := range m.whitelist {
		if _, network, err := net.ParseCIDR(entry); err == nil {
			if network.Contains(parsedIP) {
				return true
			}
		}
	}

	return false
}

// AddToBlacklist permanently blocks an IP
func (m *DDoSManager) AddToBlacklist(ip, reason string) error {
	if ip == "" {
		return errors.New("ddos: IP is required")
	}

	if net.ParseIP(ip) == nil {
		return fmt.Errorf("ddos: invalid IP: %s", ip)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.blacklist[ip] = true

	// Update IP reputation
	rep := m.getOrCreateIPReputation(ip)
	rep.Banned = true
	now := time.Now()
	rep.BannedAt = &now
	rep.BanReason = reason

	return nil
}

// RemoveFromBlacklist removes an IP from the blacklist
func (m *DDoSManager) RemoveFromBlacklist(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.blacklist, ip)

	if rep, exists := m.ipData[ip]; exists {
		rep.Banned = false
		rep.BannedAt = nil
		rep.BanExpires = nil
		rep.BanReason = ""
	}
}

// IsBlacklisted checks if an IP is blacklisted
func (m *DDoSManager) IsBlacklisted(ip string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.blacklist[ip]
}

// RecordRequest records a request from an IP
func (m *DDoSManager) RecordRequest(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rep := m.getOrCreateIPReputation(ip)
	rep.RequestCount++
	rep.LastSeen = time.Now()
}

// CheckRequest checks if a request should be allowed
func (m *DDoSManager) CheckRequest(ip string) (allowed bool, reason string) {
	if !m.config.Enabled {
		return true, ""
	}

	// Whitelist bypass
	if m.IsWhitelisted(ip) {
		return true, ""
	}

	// Blacklist check
	if m.IsBlacklisted(ip) {
		return false, "blacklisted"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rep := m.getOrCreateIPReputation(ip)

	// Check if currently banned
	if rep.Banned {
		if rep.BanExpires != nil && time.Now().After(*rep.BanExpires) {
			// Ban expired
			rep.Banned = false
			rep.BannedAt = nil
			rep.BanExpires = nil
			rep.BanReason = ""
		} else {
			return false, rep.BanReason
		}
	}

	// Check threat score
	if rep.ThreatScore > 100 {
		m.banIP(rep, "high threat score")
		return false, "high threat score"
	}

	return true, ""
}

// BanIP temporarily bans an IP
func (m *DDoSManager) BanIP(ip, reason string, duration time.Duration) error {
	if ip == "" {
		return errors.New("ddos: IP is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rep := m.getOrCreateIPReputation(ip)
	m.banIPWithDuration(rep, reason, duration)

	return nil
}

// UnbanIP removes a temporary ban
func (m *DDoSManager) UnbanIP(ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rep, exists := m.ipData[ip]
	if !exists {
		return fmt.Errorf("ddos: no data for IP %s", ip)
	}

	rep.Banned = false
	rep.BannedAt = nil
	rep.BanExpires = nil
	rep.BanReason = ""

	return nil
}

// GetIPReputation returns reputation data for an IP
func (m *DDoSManager) GetIPReputation(ip string) *IPReputation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if rep, exists := m.ipData[ip]; exists {
		// Return a copy
		copy := *rep
		return &copy
	}
	return nil
}

// IncreaseThreatScore increases an IP's threat score
func (m *DDoSManager) IncreaseThreatScore(ip string, amount int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rep := m.getOrCreateIPReputation(ip)
	rep.ThreatScore += amount

	// Auto-ban at high threat levels
	if rep.ThreatScore > 100 && !rep.Banned {
		m.banIP(rep, "automatic: high threat score")
	}
}

// RecordAttack records a detected attack
func (m *DDoSManager) RecordAttack(event *AttackEvent) {
	if event == nil {
		return
	}

	m.mu.Lock()
	m.attacks = append(m.attacks, event)
	m.mu.Unlock()

	// Non-blocking send to channel
	select {
	case m.attackChan <- event:
	default:
	}
}

// GetRecentAttacks returns recent attack events
func (m *DDoSManager) GetRecentAttacks(limit int) []*AttackEvent {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > len(m.attacks) {
		limit = len(m.attacks)
	}

	// Return most recent
	start := len(m.attacks) - limit
	if start < 0 {
		start = 0
	}

	result := make([]*AttackEvent, limit)
	copy(result, m.attacks[start:])
	return result
}

// GetAttackChannel returns channel for attack notifications
func (m *DDoSManager) GetAttackChannel() <-chan *AttackEvent {
	return m.attackChan
}

// GetCurrentThreatLevel assesses overall threat level
func (m *DDoSManager) GetCurrentThreatLevel() ThreatLevel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Count recent attacks
	recentAttacks := 0
	cutoff := time.Now().Add(-5 * time.Minute)

	for _, attack := range m.attacks {
		if attack.DetectedAt.After(cutoff) {
			recentAttacks++
		}
	}

	// Count banned IPs
	bannedIPs := 0
	for _, rep := range m.ipData {
		if rep.Banned {
			bannedIPs++
		}
	}

	// Assess threat level
	if recentAttacks > 100 || bannedIPs > 50 {
		return ThreatLevelCritical
	}
	if recentAttacks > 50 || bannedIPs > 25 {
		return ThreatLevelHigh
	}
	if recentAttacks > 20 || bannedIPs > 10 {
		return ThreatLevelMedium
	}
	if recentAttacks > 5 || bannedIPs > 3 {
		return ThreatLevelLow
	}

	return ThreatLevelNone
}

// SetupProductionProtection configures production-grade protection
func (m *DDoSManager) SetupProductionProtection() error {
	// Add common whitelisted ranges
	trustedRanges := []string{
		"127.0.0.1",      // Localhost
		"10.0.0.0/8",     // Private network
		"172.16.0.0/12",  // Private network
		"192.168.0.0/16", // Private network
	}

	for _, r := range trustedRanges {
		if err := m.AddToWhitelist(r); err != nil {
			return fmt.Errorf("ddos: failed to whitelist %s: %w", r, err)
		}
	}

	return nil
}

// GetStats returns protection statistics
func (m *DDoSManager) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	bannedCount := 0
	totalRequests := int64(0)
	totalBlocked := int64(0)

	for _, rep := range m.ipData {
		if rep.Banned {
			bannedCount++
		}
		totalRequests += rep.RequestCount
		totalBlocked += rep.BlockedCount
	}

	return map[string]interface{}{
		"enabled":         m.config.Enabled,
		"mode":            m.config.Mode,
		"tracked_ips":     len(m.ipData),
		"banned_ips":      bannedCount,
		"whitelisted_ips": len(m.whitelist),
		"blacklisted_ips": len(m.blacklist),
		"total_requests":  totalRequests,
		"total_blocked":   totalBlocked,
		"recent_attacks":  len(m.attacks),
		"current_threat":  m.GetCurrentThreatLevel(),
	}
}

// Helper methods

func (m *DDoSManager) getOrCreateIPReputation(ip string) *IPReputation {
	rep, exists := m.ipData[ip]
	if !exists {
		now := time.Now()
		rep = &IPReputation{
			IP:        ip,
			FirstSeen: now,
			LastSeen:  now,
		}
		m.ipData[ip] = rep
	}
	return rep
}

func (m *DDoSManager) banIP(rep *IPReputation, reason string) {
	m.banIPWithDuration(rep, reason, m.config.BanDuration)
}

func (m *DDoSManager) banIPWithDuration(rep *IPReputation, reason string, duration time.Duration) {
	now := time.Now()
	expires := now.Add(duration)
	rep.Banned = true
	rep.BannedAt = &now
	rep.BanExpires = &expires
	rep.BanReason = reason
	rep.BlockedCount++
}

// GetDDoSConfigForEnvironment returns config for an environment
func GetDDoSConfigForEnvironment(envType string) *DDoSConfig {
	if config, ok := DefaultDDoSConfigs[envType]; ok {
		return config
	}
	return DefaultDDoSConfigs[EnvTypeDevelopment]
}
