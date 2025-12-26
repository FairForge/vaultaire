// internal/devops/firewall.go
package devops

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// FirewallAction represents what to do with matching traffic
type FirewallAction string

const (
	ActionAllow FirewallAction = "allow"
	ActionDeny  FirewallAction = "deny"
	ActionLog   FirewallAction = "log"
)

// Protocol represents network protocols
type Protocol string

const (
	ProtocolTCP  Protocol = "tcp"
	ProtocolUDP  Protocol = "udp"
	ProtocolICMP Protocol = "icmp"
	ProtocolAny  Protocol = "any"
)

// FirewallRule represents a firewall rule
type FirewallRule struct {
	Name        string         `json:"name"`
	Priority    int            `json:"priority"`
	Action      FirewallAction `json:"action"`
	Protocol    Protocol       `json:"protocol"`
	SourceCIDR  string         `json:"source_cidr"`
	DestPort    int            `json:"dest_port"`
	DestPorts   []int          `json:"dest_ports,omitempty"`
	Description string         `json:"description"`
	Enabled     bool           `json:"enabled"`
}

// FirewallConfig configures the firewall
type FirewallConfig struct {
	DefaultAction FirewallAction `json:"default_action"`
	LogDenied     bool           `json:"log_denied"`
	RateLimiting  bool           `json:"rate_limiting"`
}

// DefaultFirewallConfig returns sensible defaults
func DefaultFirewallConfig() *FirewallConfig {
	return &FirewallConfig{
		DefaultAction: ActionDeny,
		LogDenied:     true,
		RateLimiting:  true,
	}
}

// FirewallManager manages firewall rules
type FirewallManager struct {
	config *FirewallConfig
	rules  []*FirewallRule
	mu     sync.RWMutex
}

// NewFirewallManager creates a firewall manager
func NewFirewallManager(config *FirewallConfig) *FirewallManager {
	if config == nil {
		config = DefaultFirewallConfig()
	}
	return &FirewallManager{
		config: config,
		rules:  make([]*FirewallRule, 0),
	}
}

// GetConfig returns the firewall configuration
func (m *FirewallManager) GetConfig() *FirewallConfig {
	return m.config
}

// AddRule adds a firewall rule
func (m *FirewallManager) AddRule(rule *FirewallRule) error {
	if rule == nil {
		return errors.New("firewall: rule is required")
	}
	if rule.Name == "" {
		return errors.New("firewall: rule name is required")
	}

	// Validate CIDR if provided
	if rule.SourceCIDR != "" && rule.SourceCIDR != "any" {
		if _, _, err := net.ParseCIDR(rule.SourceCIDR); err != nil {
			// Try parsing as single IP
			if ip := net.ParseIP(rule.SourceCIDR); ip == nil {
				return fmt.Errorf("firewall: invalid source CIDR: %s", rule.SourceCIDR)
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicate
	for _, r := range m.rules {
		if r.Name == rule.Name {
			return fmt.Errorf("firewall: rule %s already exists", rule.Name)
		}
	}

	if rule.Protocol == "" {
		rule.Protocol = ProtocolTCP
	}
	if rule.Action == "" {
		rule.Action = ActionAllow
	}
	rule.Enabled = true

	// Insert in priority order
	inserted := false
	for i, r := range m.rules {
		if rule.Priority < r.Priority {
			m.rules = append(m.rules[:i], append([]*FirewallRule{rule}, m.rules[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted {
		m.rules = append(m.rules, rule)
	}

	return nil
}

// GetRule returns a rule by name
func (m *FirewallManager) GetRule(name string) *FirewallRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, r := range m.rules {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// ListRules returns all rules in priority order
func (m *FirewallManager) ListRules() []*FirewallRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rules := make([]*FirewallRule, len(m.rules))
	copy(rules, m.rules)
	return rules
}

// RemoveRule removes a rule
func (m *FirewallManager) RemoveRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, r := range m.rules {
		if r.Name == name {
			m.rules = append(m.rules[:i], m.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("firewall: rule %s not found", name)
}

// EnableRule enables a rule
func (m *FirewallManager) EnableRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.rules {
		if r.Name == name {
			r.Enabled = true
			return nil
		}
	}
	return fmt.Errorf("firewall: rule %s not found", name)
}

// DisableRule disables a rule
func (m *FirewallManager) DisableRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.rules {
		if r.Name == name {
			r.Enabled = false
			return nil
		}
	}
	return fmt.Errorf("firewall: rule %s not found", name)
}

// CheckAccess checks if traffic should be allowed
func (m *FirewallManager) CheckAccess(sourceIP string, destPort int, protocol Protocol) FirewallAction {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ip := net.ParseIP(sourceIP)
	if ip == nil {
		return ActionDeny
	}

	for _, rule := range m.rules {
		if !rule.Enabled {
			continue
		}

		// Check protocol
		if rule.Protocol != ProtocolAny && rule.Protocol != protocol {
			continue
		}

		// Check port
		portMatch := false
		if rule.DestPort == 0 && len(rule.DestPorts) == 0 {
			portMatch = true // Any port
		} else if rule.DestPort == destPort {
			portMatch = true
		} else {
			for _, p := range rule.DestPorts {
				if p == destPort {
					portMatch = true
					break
				}
			}
		}
		if !portMatch {
			continue
		}

		// Check source CIDR
		if rule.SourceCIDR == "" || rule.SourceCIDR == "any" {
			return rule.Action
		}

		_, network, err := net.ParseCIDR(rule.SourceCIDR)
		if err != nil {
			// Try as single IP
			if rule.SourceCIDR == sourceIP {
				return rule.Action
			}
			continue
		}

		if network.Contains(ip) {
			return rule.Action
		}
	}

	return m.config.DefaultAction
}

// SetupProductionFirewall creates standard production rules
func (m *FirewallManager) SetupProductionFirewall() error {
	rules := []*FirewallRule{
		// Allow SSH from anywhere (consider restricting in production)
		{
			Name:        "allow-ssh",
			Priority:    100,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "any",
			DestPort:    22,
			Description: "Allow SSH access",
		},
		// Allow HTTP
		{
			Name:        "allow-http",
			Priority:    200,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "any",
			DestPort:    80,
			Description: "Allow HTTP for redirect",
		},
		// Allow HTTPS
		{
			Name:        "allow-https",
			Priority:    201,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "any",
			DestPort:    443,
			Description: "Allow HTTPS traffic",
		},
		// Allow S3 API port
		{
			Name:        "allow-s3-api",
			Priority:    300,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "any",
			DestPort:    8080,
			Description: "Allow S3 API traffic",
		},
		// Allow metrics (internal only)
		{
			Name:        "allow-metrics-internal",
			Priority:    400,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "10.0.0.0/8",
			DestPort:    9090,
			Description: "Allow Prometheus metrics from internal network",
		},
		// Allow PostgreSQL (internal only)
		{
			Name:        "allow-postgres-internal",
			Priority:    500,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "10.0.0.0/8",
			DestPort:    5432,
			Description: "Allow PostgreSQL from internal network",
		},
		// Allow Redis (internal only)
		{
			Name:        "allow-redis-internal",
			Priority:    501,
			Action:      ActionAllow,
			Protocol:    ProtocolTCP,
			SourceCIDR:  "10.0.0.0/8",
			DestPort:    6379,
			Description: "Allow Redis from internal network",
		},
		// Allow ICMP ping
		{
			Name:        "allow-icmp",
			Priority:    900,
			Action:      ActionAllow,
			Protocol:    ProtocolICMP,
			SourceCIDR:  "any",
			Description: "Allow ping",
		},
		// Deny all other inbound
		{
			Name:        "deny-all",
			Priority:    9999,
			Action:      ActionDeny,
			Protocol:    ProtocolAny,
			SourceCIDR:  "any",
			Description: "Deny all other traffic",
		},
	}

	for _, rule := range rules {
		if err := m.AddRule(rule); err != nil {
			return fmt.Errorf("firewall: failed to add rule %s: %w", rule.Name, err)
		}
	}

	return nil
}

// GenerateIPTables generates iptables commands
func (m *FirewallManager) GenerateIPTables() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var commands []string

	// Flush existing rules
	commands = append(commands, "iptables -F INPUT")
	commands = append(commands, "iptables -P INPUT DROP")

	// Allow established connections
	commands = append(commands, "iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT")

	// Allow loopback
	commands = append(commands, "iptables -A INPUT -i lo -j ACCEPT")

	for _, rule := range m.rules {
		if !rule.Enabled {
			continue
		}

		var cmd string
		var action string
		switch rule.Action {
		case ActionDeny:
			action = "DROP"
		case ActionLog:
			action = "LOG"
		default:
			action = "ACCEPT"
		}

		proto := string(rule.Protocol)
		if rule.Protocol == ProtocolAny {
			proto = "all"
		}

		source := ""
		if rule.SourceCIDR != "" && rule.SourceCIDR != "any" {
			source = fmt.Sprintf("-s %s", rule.SourceCIDR)
		}

		port := ""
		if rule.DestPort > 0 {
			port = fmt.Sprintf("--dport %d", rule.DestPort)
		}

		if rule.Protocol == ProtocolICMP {
			cmd = fmt.Sprintf("iptables -A INPUT -p icmp %s -j %s", source, action)
		} else if port != "" {
			cmd = fmt.Sprintf("iptables -A INPUT -p %s %s %s -j %s", proto, source, port, action)
		} else {
			cmd = fmt.Sprintf("iptables -A INPUT -p %s %s -j %s", proto, source, action)
		}

		commands = append(commands, cmd)
	}

	return commands
}

// GenerateUFW generates ufw commands
func (m *FirewallManager) GenerateUFW() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var commands []string

	commands = append(commands, "ufw --force reset")
	commands = append(commands, "ufw default deny incoming")
	commands = append(commands, "ufw default allow outgoing")

	for _, rule := range m.rules {
		if !rule.Enabled || rule.Action != ActionAllow {
			continue
		}

		var cmd string
		proto := string(rule.Protocol)

		if rule.Protocol == ProtocolICMP {
			continue // UFW handles ICMP differently
		}

		if rule.SourceCIDR != "" && rule.SourceCIDR != "any" {
			if rule.DestPort > 0 {
				cmd = fmt.Sprintf("ufw allow from %s to any port %d proto %s",
					rule.SourceCIDR, rule.DestPort, proto)
			} else {
				cmd = fmt.Sprintf("ufw allow from %s", rule.SourceCIDR)
			}
		} else {
			if rule.DestPort > 0 {
				cmd = fmt.Sprintf("ufw allow %d/%s", rule.DestPort, proto)
			}
		}

		if cmd != "" {
			commands = append(commands, cmd)
		}
	}

	commands = append(commands, "ufw --force enable")

	return commands
}
