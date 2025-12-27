// internal/devops/firewall_test.go
package devops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFirewallManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewFirewallManager(nil)
		assert.NotNil(t, manager)
		assert.Equal(t, ActionDeny, manager.config.DefaultAction)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &FirewallConfig{DefaultAction: ActionAllow}
		manager := NewFirewallManager(config)
		assert.Equal(t, ActionAllow, manager.config.DefaultAction)
	})
}

func TestFirewallManager_AddRule(t *testing.T) {
	manager := NewFirewallManager(nil)

	t.Run("adds rule", func(t *testing.T) {
		err := manager.AddRule(&FirewallRule{
			Name:       "allow-https",
			Priority:   100,
			Action:     ActionAllow,
			Protocol:   ProtocolTCP,
			SourceCIDR: "any",
			DestPort:   443,
		})
		assert.NoError(t, err)
	})

	t.Run("sets defaults", func(t *testing.T) {
		_ = manager.AddRule(&FirewallRule{
			Name:     "test-defaults",
			Priority: 200,
			DestPort: 8080,
		})
		rule := manager.GetRule("test-defaults")
		require.NotNil(t, rule)
		assert.Equal(t, ProtocolTCP, rule.Protocol)
		assert.Equal(t, ActionAllow, rule.Action)
		assert.True(t, rule.Enabled)
	})

	t.Run("rejects nil rule", func(t *testing.T) {
		err := manager.AddRule(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.AddRule(&FirewallRule{DestPort: 443})
		assert.Error(t, err)
	})

	t.Run("rejects invalid CIDR", func(t *testing.T) {
		err := manager.AddRule(&FirewallRule{
			Name:       "bad-cidr",
			SourceCIDR: "not-a-cidr",
		})
		assert.Error(t, err)
	})

	t.Run("accepts valid CIDR", func(t *testing.T) {
		err := manager.AddRule(&FirewallRule{
			Name:       "good-cidr",
			SourceCIDR: "10.0.0.0/8",
			Priority:   300,
		})
		assert.NoError(t, err)
	})

	t.Run("accepts single IP", func(t *testing.T) {
		err := manager.AddRule(&FirewallRule{
			Name:       "single-ip",
			SourceCIDR: "192.168.1.1",
			Priority:   301,
		})
		assert.NoError(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.AddRule(&FirewallRule{Name: "dup", Priority: 400})
		err := manager.AddRule(&FirewallRule{Name: "dup", Priority: 401})
		assert.Error(t, err)
	})

	t.Run("maintains priority order", func(t *testing.T) {
		m := NewFirewallManager(nil)
		_ = m.AddRule(&FirewallRule{Name: "low", Priority: 300})
		_ = m.AddRule(&FirewallRule{Name: "high", Priority: 100})
		_ = m.AddRule(&FirewallRule{Name: "mid", Priority: 200})

		rules := m.ListRules()
		assert.Equal(t, "high", rules[0].Name)
		assert.Equal(t, "mid", rules[1].Name)
		assert.Equal(t, "low", rules[2].Name)
	})
}

func TestFirewallManager_GetRule(t *testing.T) {
	manager := NewFirewallManager(nil)
	_ = manager.AddRule(&FirewallRule{Name: "test-get", Priority: 100})

	t.Run("returns existing rule", func(t *testing.T) {
		rule := manager.GetRule("test-get")
		assert.NotNil(t, rule)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		rule := manager.GetRule("unknown")
		assert.Nil(t, rule)
	})
}

func TestFirewallManager_RemoveRule(t *testing.T) {
	manager := NewFirewallManager(nil)
	_ = manager.AddRule(&FirewallRule{Name: "to-remove", Priority: 100})

	t.Run("removes rule", func(t *testing.T) {
		err := manager.RemoveRule("to-remove")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetRule("to-remove"))
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := manager.RemoveRule("unknown")
		assert.Error(t, err)
	})
}

func TestFirewallManager_EnableDisableRule(t *testing.T) {
	manager := NewFirewallManager(nil)
	_ = manager.AddRule(&FirewallRule{Name: "toggle", Priority: 100})

	t.Run("disables rule", func(t *testing.T) {
		err := manager.DisableRule("toggle")
		assert.NoError(t, err)
		assert.False(t, manager.GetRule("toggle").Enabled)
	})

	t.Run("enables rule", func(t *testing.T) {
		err := manager.EnableRule("toggle")
		assert.NoError(t, err)
		assert.True(t, manager.GetRule("toggle").Enabled)
	})

	t.Run("errors for unknown", func(t *testing.T) {
		assert.Error(t, manager.EnableRule("unknown"))
		assert.Error(t, manager.DisableRule("unknown"))
	})
}

func TestFirewallManager_CheckAccess(t *testing.T) {
	manager := NewFirewallManager(&FirewallConfig{DefaultAction: ActionDeny})

	_ = manager.AddRule(&FirewallRule{
		Name:       "allow-https",
		Priority:   100,
		Action:     ActionAllow,
		Protocol:   ProtocolTCP,
		SourceCIDR: "any",
		DestPort:   443,
	})

	_ = manager.AddRule(&FirewallRule{
		Name:       "allow-internal",
		Priority:   200,
		Action:     ActionAllow,
		Protocol:   ProtocolTCP,
		SourceCIDR: "10.0.0.0/8",
		DestPort:   5432,
	})

	t.Run("allows matching rule", func(t *testing.T) {
		action := manager.CheckAccess("1.2.3.4", 443, ProtocolTCP)
		assert.Equal(t, ActionAllow, action)
	})

	t.Run("allows internal access", func(t *testing.T) {
		action := manager.CheckAccess("10.1.2.3", 5432, ProtocolTCP)
		assert.Equal(t, ActionAllow, action)
	})

	t.Run("denies external access to internal port", func(t *testing.T) {
		action := manager.CheckAccess("1.2.3.4", 5432, ProtocolTCP)
		assert.Equal(t, ActionDeny, action)
	})

	t.Run("denies unmatched traffic", func(t *testing.T) {
		action := manager.CheckAccess("1.2.3.4", 22, ProtocolTCP)
		assert.Equal(t, ActionDeny, action)
	})

	t.Run("denies invalid IP", func(t *testing.T) {
		action := manager.CheckAccess("not-an-ip", 443, ProtocolTCP)
		assert.Equal(t, ActionDeny, action)
	})

	t.Run("ignores disabled rules", func(t *testing.T) {
		_ = manager.DisableRule("allow-https")
		action := manager.CheckAccess("1.2.3.4", 443, ProtocolTCP)
		assert.Equal(t, ActionDeny, action)
		_ = manager.EnableRule("allow-https")
	})
}

func TestFirewallManager_SetupProductionFirewall(t *testing.T) {
	manager := NewFirewallManager(nil)

	err := manager.SetupProductionFirewall()
	require.NoError(t, err)

	t.Run("creates expected rules", func(t *testing.T) {
		rules := manager.ListRules()
		assert.GreaterOrEqual(t, len(rules), 8)
	})

	t.Run("allows HTTPS", func(t *testing.T) {
		action := manager.CheckAccess("1.2.3.4", 443, ProtocolTCP)
		assert.Equal(t, ActionAllow, action)
	})

	t.Run("allows SSH", func(t *testing.T) {
		action := manager.CheckAccess("1.2.3.4", 22, ProtocolTCP)
		assert.Equal(t, ActionAllow, action)
	})

	t.Run("denies postgres from external", func(t *testing.T) {
		action := manager.CheckAccess("1.2.3.4", 5432, ProtocolTCP)
		assert.Equal(t, ActionDeny, action)
	})

	t.Run("allows postgres from internal", func(t *testing.T) {
		action := manager.CheckAccess("10.0.0.5", 5432, ProtocolTCP)
		assert.Equal(t, ActionAllow, action)
	})
}

func TestFirewallManager_GenerateIPTables(t *testing.T) {
	manager := NewFirewallManager(nil)
	_ = manager.SetupProductionFirewall()

	commands := manager.GenerateIPTables()

	t.Run("generates commands", func(t *testing.T) {
		assert.NotEmpty(t, commands)
	})

	t.Run("starts with flush", func(t *testing.T) {
		assert.Equal(t, "iptables -F INPUT", commands[0])
	})

	t.Run("sets default policy", func(t *testing.T) {
		assert.Equal(t, "iptables -P INPUT DROP", commands[1])
	})

	t.Run("includes port rules", func(t *testing.T) {
		found := false
		for _, cmd := range commands {
			if contains(cmd, "--dport 443") {
				found = true
				break
			}
		}
		assert.True(t, found, "should include HTTPS rule")
	})
}

func TestFirewallManager_GenerateUFW(t *testing.T) {
	manager := NewFirewallManager(nil)
	_ = manager.SetupProductionFirewall()

	commands := manager.GenerateUFW()

	t.Run("generates commands", func(t *testing.T) {
		assert.NotEmpty(t, commands)
	})

	t.Run("resets first", func(t *testing.T) {
		assert.Equal(t, "ufw --force reset", commands[0])
	})

	t.Run("enables at end", func(t *testing.T) {
		assert.Equal(t, "ufw --force enable", commands[len(commands)-1])
	})

	t.Run("includes allow rules", func(t *testing.T) {
		found := false
		for _, cmd := range commands {
			if contains(cmd, "ufw allow 443") {
				found = true
				break
			}
		}
		assert.True(t, found, "should include HTTPS rule")
	})
}

func TestProtocolConstants(t *testing.T) {
	assert.Equal(t, Protocol("tcp"), ProtocolTCP)
	assert.Equal(t, Protocol("udp"), ProtocolUDP)
	assert.Equal(t, Protocol("icmp"), ProtocolICMP)
	assert.Equal(t, Protocol("any"), ProtocolAny)
}

func TestActionConstants(t *testing.T) {
	assert.Equal(t, FirewallAction("allow"), ActionAllow)
	assert.Equal(t, FirewallAction("deny"), ActionDeny)
	assert.Equal(t, FirewallAction("log"), ActionLog)
}

// helper
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
