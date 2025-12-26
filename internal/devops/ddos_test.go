// internal/devops/ddos_test.go
package devops

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDDoSManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewDDoSManager(nil)
		assert.NotNil(t, manager)
		assert.False(t, manager.config.Enabled)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &DDoSConfig{Enabled: true, RateLimit: 100}
		manager := NewDDoSManager(config)
		assert.True(t, manager.config.Enabled)
		assert.Equal(t, 100, manager.config.RateLimit)
	})
}

func TestDDoSManager_Whitelist(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{
		Enabled:          true,
		WhitelistEnabled: true,
	})

	t.Run("adds IP to whitelist", func(t *testing.T) {
		err := manager.AddToWhitelist("1.2.3.4")
		assert.NoError(t, err)
		assert.True(t, manager.IsWhitelisted("1.2.3.4"))
	})

	t.Run("adds CIDR to whitelist", func(t *testing.T) {
		err := manager.AddToWhitelist("10.0.0.0/8")
		assert.NoError(t, err)
		assert.True(t, manager.IsWhitelisted("10.1.2.3"))
	})

	t.Run("rejects invalid entry", func(t *testing.T) {
		err := manager.AddToWhitelist("not-valid")
		assert.Error(t, err)
	})

	t.Run("rejects empty entry", func(t *testing.T) {
		err := manager.AddToWhitelist("")
		assert.Error(t, err)
	})

	t.Run("removes from whitelist", func(t *testing.T) {
		_ = manager.AddToWhitelist("9.9.9.9")
		manager.RemoveFromWhitelist("9.9.9.9")
		assert.False(t, manager.IsWhitelisted("9.9.9.9"))
	})
}

func TestDDoSManager_Blacklist(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{Enabled: true})

	t.Run("adds IP to blacklist", func(t *testing.T) {
		err := manager.AddToBlacklist("5.6.7.8", "malicious activity")
		assert.NoError(t, err)
		assert.True(t, manager.IsBlacklisted("5.6.7.8"))
	})

	t.Run("rejects invalid IP", func(t *testing.T) {
		err := manager.AddToBlacklist("not-an-ip", "test")
		assert.Error(t, err)
	})

	t.Run("rejects empty IP", func(t *testing.T) {
		err := manager.AddToBlacklist("", "test")
		assert.Error(t, err)
	})

	t.Run("removes from blacklist", func(t *testing.T) {
		_ = manager.AddToBlacklist("8.8.8.8", "test")
		manager.RemoveFromBlacklist("8.8.8.8")
		assert.False(t, manager.IsBlacklisted("8.8.8.8"))
	})
}

func TestDDoSManager_CheckRequest(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{
		Enabled:          true,
		WhitelistEnabled: true,
		BanDuration:      1 * time.Hour,
	})

	t.Run("allows when disabled", func(t *testing.T) {
		m := NewDDoSManager(&DDoSConfig{Enabled: false})
		allowed, _ := m.CheckRequest("1.2.3.4")
		assert.True(t, allowed)
	})

	t.Run("allows whitelisted IP", func(t *testing.T) {
		_ = manager.AddToWhitelist("10.0.0.1")
		allowed, _ := manager.CheckRequest("10.0.0.1")
		assert.True(t, allowed)
	})

	t.Run("blocks blacklisted IP", func(t *testing.T) {
		_ = manager.AddToBlacklist("6.6.6.6", "evil")
		allowed, reason := manager.CheckRequest("6.6.6.6")
		assert.False(t, allowed)
		assert.Equal(t, "blacklisted", reason)
	})

	t.Run("allows normal request", func(t *testing.T) {
		allowed, _ := manager.CheckRequest("1.2.3.4")
		assert.True(t, allowed)
	})
}

func TestDDoSManager_BanIP(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{
		Enabled:     true,
		BanDuration: 1 * time.Hour,
	})

	t.Run("bans IP", func(t *testing.T) {
		err := manager.BanIP("7.7.7.7", "too many requests", 1*time.Hour)
		assert.NoError(t, err)

		allowed, reason := manager.CheckRequest("7.7.7.7")
		assert.False(t, allowed)
		assert.Equal(t, "too many requests", reason)
	})

	t.Run("unbans IP", func(t *testing.T) {
		_ = manager.BanIP("8.8.8.8", "test", 1*time.Hour)
		err := manager.UnbanIP("8.8.8.8")
		assert.NoError(t, err)

		allowed, _ := manager.CheckRequest("8.8.8.8")
		assert.True(t, allowed)
	})

	t.Run("errors for empty IP", func(t *testing.T) {
		err := manager.BanIP("", "test", 1*time.Hour)
		assert.Error(t, err)
	})

	t.Run("errors unbanning unknown IP", func(t *testing.T) {
		err := manager.UnbanIP("unknown")
		assert.Error(t, err)
	})
}

func TestDDoSManager_ThreatScore(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{
		Enabled:     true,
		BanDuration: 1 * time.Hour,
	})

	t.Run("increases threat score", func(t *testing.T) {
		manager.IncreaseThreatScore("9.9.9.9", 50)
		rep := manager.GetIPReputation("9.9.9.9")
		require.NotNil(t, rep)
		assert.Equal(t, 50, rep.ThreatScore)
	})

	t.Run("auto-bans at high score", func(t *testing.T) {
		manager.IncreaseThreatScore("bad.actor", 150)
		rep := manager.GetIPReputation("bad.actor")
		require.NotNil(t, rep)
		assert.True(t, rep.Banned)
	})
}

func TestDDoSManager_RecordRequest(t *testing.T) {
	manager := NewDDoSManager(nil)

	manager.RecordRequest("1.2.3.4")
	manager.RecordRequest("1.2.3.4")
	manager.RecordRequest("1.2.3.4")

	rep := manager.GetIPReputation("1.2.3.4")
	require.NotNil(t, rep)
	assert.Equal(t, int64(3), rep.RequestCount)
}

func TestDDoSManager_RecordAttack(t *testing.T) {
	manager := NewDDoSManager(nil)

	event := &AttackEvent{
		ID:          "attack-1",
		Type:        "syn_flood",
		SourceIP:    "1.2.3.4",
		ThreatLevel: ThreatLevelHigh,
		DetectedAt:  time.Now(),
	}

	manager.RecordAttack(event)

	attacks := manager.GetRecentAttacks(10)
	assert.Len(t, attacks, 1)
	assert.Equal(t, "attack-1", attacks[0].ID)
}

func TestDDoSManager_GetCurrentThreatLevel(t *testing.T) {
	t.Run("returns none when quiet", func(t *testing.T) {
		manager := NewDDoSManager(nil)
		assert.Equal(t, ThreatLevelNone, manager.GetCurrentThreatLevel())
	})

	t.Run("increases with attacks", func(t *testing.T) {
		manager := NewDDoSManager(nil)

		// Add many recent attacks
		for i := 0; i < 25; i++ {
			manager.RecordAttack(&AttackEvent{
				ID:         fmt.Sprintf("attack-%d", i),
				DetectedAt: time.Now(),
			})
		}

		level := manager.GetCurrentThreatLevel()
		assert.NotEqual(t, ThreatLevelNone, level)
	})
}

func TestDDoSManager_SetupProductionProtection(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{
		Enabled:          true,
		WhitelistEnabled: true,
	})

	err := manager.SetupProductionProtection()
	require.NoError(t, err)

	t.Run("whitelists localhost", func(t *testing.T) {
		assert.True(t, manager.IsWhitelisted("127.0.0.1"))
	})

	t.Run("whitelists private networks", func(t *testing.T) {
		assert.True(t, manager.IsWhitelisted("10.1.2.3"))
		assert.True(t, manager.IsWhitelisted("172.16.1.1"))
		assert.True(t, manager.IsWhitelisted("192.168.1.1"))
	})
}

func TestDDoSManager_GetStats(t *testing.T) {
	manager := NewDDoSManager(&DDoSConfig{Enabled: true})

	_ = manager.AddToWhitelist("1.1.1.1")
	_ = manager.AddToBlacklist("2.2.2.2", "test")
	manager.RecordRequest("3.3.3.3")

	stats := manager.GetStats()

	assert.True(t, stats["enabled"].(bool))
	assert.Equal(t, 1, stats["whitelisted_ips"])
	assert.Equal(t, 1, stats["blacklisted_ips"])
	assert.GreaterOrEqual(t, stats["tracked_ips"], 1)
}

func TestThreatLevelConstants(t *testing.T) {
	assert.Equal(t, ThreatLevel("none"), ThreatLevelNone)
	assert.Equal(t, ThreatLevel("low"), ThreatLevelLow)
	assert.Equal(t, ThreatLevel("medium"), ThreatLevelMedium)
	assert.Equal(t, ThreatLevel("high"), ThreatLevelHigh)
	assert.Equal(t, ThreatLevel("critical"), ThreatLevelCritical)
}

func TestMitigationModeConstants(t *testing.T) {
	assert.Equal(t, MitigationMode("off"), MitigationModeOff)
	assert.Equal(t, MitigationMode("monitor"), MitigationModeMonitor)
	assert.Equal(t, MitigationMode("challenge"), MitigationModeChallenge)
	assert.Equal(t, MitigationMode("block"), MitigationModeBlock)
	assert.Equal(t, MitigationMode("aggressive"), MitigationModeAggressive)
}

func TestGetDDoSConfigForEnvironment(t *testing.T) {
	t.Run("returns production config", func(t *testing.T) {
		config := GetDDoSConfigForEnvironment(EnvTypeProduction)
		assert.True(t, config.Enabled)
		assert.Equal(t, MitigationModeBlock, config.Mode)
	})

	t.Run("returns development for unknown", func(t *testing.T) {
		config := GetDDoSConfigForEnvironment("unknown")
		assert.False(t, config.Enabled)
	})
}

func TestDefaultDDoSConfigs(t *testing.T) {
	t.Run("production has strict settings", func(t *testing.T) {
		config := DefaultDDoSConfigs[EnvTypeProduction]
		assert.True(t, config.Enabled)
		assert.True(t, config.SlowLorisProtection)
		assert.True(t, config.SYNFloodProtection)
		assert.Equal(t, 1*time.Hour, config.BanDuration)
	})

	t.Run("development is permissive", func(t *testing.T) {
		config := DefaultDDoSConfigs[EnvTypeDevelopment]
		assert.False(t, config.Enabled)
		assert.Equal(t, MitigationModeMonitor, config.Mode)
	})
}
