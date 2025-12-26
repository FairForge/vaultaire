// internal/devops/alerting_test.go
package devops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAlertManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewAlertManager(nil)
		assert.NotNil(t, manager)
		assert.False(t, manager.config.Enabled)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &AlertingConfig{Enabled: true}
		manager := NewAlertManager(config)
		assert.True(t, manager.config.Enabled)
	})
}

func TestAlertManager_Rules(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("adds rule", func(t *testing.T) {
		err := manager.AddRule(&AlertRule{
			Name:      "test_rule",
			Threshold: 100,
		})
		assert.NoError(t, err)
	})

	t.Run("sets defaults", func(t *testing.T) {
		rule := manager.GetRule("test_rule")
		require.NotNil(t, rule)
		assert.Equal(t, AlertSeverityWarning, rule.Severity)
		assert.Equal(t, 5*time.Minute, rule.Duration)
		assert.True(t, rule.Enabled)
	})

	t.Run("rejects nil rule", func(t *testing.T) {
		err := manager.AddRule(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.AddRule(&AlertRule{})
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.AddRule(&AlertRule{Name: "dup"})
		err := manager.AddRule(&AlertRule{Name: "dup"})
		assert.Error(t, err)
	})

	t.Run("lists rules", func(t *testing.T) {
		rules := manager.ListRules()
		assert.GreaterOrEqual(t, len(rules), 1)
	})

	t.Run("enables/disables rule", func(t *testing.T) {
		_ = manager.DisableRule("test_rule")
		assert.False(t, manager.GetRule("test_rule").Enabled)

		_ = manager.EnableRule("test_rule")
		assert.True(t, manager.GetRule("test_rule").Enabled)
	})

	t.Run("removes rule", func(t *testing.T) {
		_ = manager.AddRule(&AlertRule{Name: "to_remove"})
		err := manager.RemoveRule("to_remove")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetRule("to_remove"))
	})
}

func TestAlertManager_FireAlert(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})
	_ = manager.AddRule(&AlertRule{
		Name:      "error_rate",
		Threshold: 0.05,
		Cooldown:  1 * time.Millisecond, // Short for testing
	})

	t.Run("fires alert", func(t *testing.T) {
		alert, err := manager.FireAlert("error_rate", "High error rate detected", 0.10, nil)
		require.NoError(t, err)
		require.NotNil(t, alert)

		assert.Equal(t, AlertStateFiring, alert.State)
		assert.Equal(t, "error_rate", alert.RuleName)
		assert.Equal(t, 0.10, alert.Value)
	})

	t.Run("errors for unknown rule", func(t *testing.T) {
		_, err := manager.FireAlert("unknown", "test", 0, nil)
		assert.Error(t, err)
	})

	t.Run("errors for disabled rule", func(t *testing.T) {
		_ = manager.AddRule(&AlertRule{Name: "disabled_rule"})
		_ = manager.DisableRule("disabled_rule")

		_, err := manager.FireAlert("disabled_rule", "test", 0, nil)
		assert.Error(t, err)
	})

	t.Run("respects cooldown", func(t *testing.T) {
		_ = manager.AddRule(&AlertRule{
			Name:     "cooldown_test",
			Cooldown: 1 * time.Hour,
		})

		alert1, _ := manager.FireAlert("cooldown_test", "first", 1, nil)
		require.NotNil(t, alert1)

		alert2, _ := manager.FireAlert("cooldown_test", "second", 2, nil)
		assert.Nil(t, alert2) // Should be nil due to cooldown
	})
}

func TestAlertManager_ResolveAlert(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})
	_ = manager.AddRule(&AlertRule{Name: "resolve_test", Cooldown: 1 * time.Millisecond})

	alert, _ := manager.FireAlert("resolve_test", "test", 1, nil)

	t.Run("resolves alert", func(t *testing.T) {
		err := manager.ResolveAlert(alert.ID)
		assert.NoError(t, err)

		resolved := manager.GetAlert(alert.ID)
		assert.Equal(t, AlertStateResolved, resolved.State)
		assert.NotNil(t, resolved.ResolvedAt)
	})

	t.Run("errors for unknown alert", func(t *testing.T) {
		err := manager.ResolveAlert("unknown")
		assert.Error(t, err)
	})
}

func TestAlertManager_AcknowledgeAlert(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})
	_ = manager.AddRule(&AlertRule{Name: "ack_test", Cooldown: 1 * time.Millisecond})

	alert, _ := manager.FireAlert("ack_test", "test", 1, nil)

	t.Run("acknowledges alert", func(t *testing.T) {
		err := manager.AcknowledgeAlert(alert.ID, "oncall@example.com")
		assert.NoError(t, err)

		acked := manager.GetAlert(alert.ID)
		assert.NotNil(t, acked.AckedAt)
		assert.Equal(t, "oncall@example.com", acked.AckedBy)
	})
}

func TestAlertManager_ListAlerts(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})
	_ = manager.AddRule(&AlertRule{Name: "list_test", Cooldown: 1 * time.Millisecond})

	time.Sleep(2 * time.Millisecond)
	_, _ = manager.FireAlert("list_test", "alert 1", 1, nil)
	time.Sleep(2 * time.Millisecond)
	alert2, _ := manager.FireAlert("list_test", "alert 2", 2, nil)
	_ = manager.ResolveAlert(alert2.ID)

	t.Run("lists all alerts", func(t *testing.T) {
		alerts := manager.ListAlerts()
		assert.Len(t, alerts, 2)
	})

	t.Run("lists active alerts only", func(t *testing.T) {
		active := manager.ListActiveAlerts()
		assert.Len(t, active, 1)
	})
}

func TestAlertManager_ListAlertsBySeverity(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})
	_ = manager.AddRule(&AlertRule{Name: "warning", Severity: AlertSeverityWarning, Cooldown: 1 * time.Millisecond})
	_ = manager.AddRule(&AlertRule{Name: "critical", Severity: AlertSeverityCritical, Cooldown: 1 * time.Millisecond})

	time.Sleep(2 * time.Millisecond)
	_, _ = manager.FireAlert("warning", "warn", 1, nil)
	time.Sleep(2 * time.Millisecond)
	_, _ = manager.FireAlert("critical", "crit", 1, nil)

	warnings := manager.ListAlertsBySeverity(AlertSeverityWarning)
	assert.Len(t, warnings, 1)

	criticals := manager.ListAlertsBySeverity(AlertSeverityCritical)
	assert.Len(t, criticals, 1)
}

func TestAlertManager_Silences(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})

	t.Run("adds silence", func(t *testing.T) {
		err := manager.AddSilence(&Silence{
			Matchers:  map[string]string{"env": "staging"},
			StartsAt:  time.Now().Add(-1 * time.Hour),
			EndsAt:    time.Now().Add(1 * time.Hour),
			CreatedBy: "admin",
			Comment:   "Maintenance",
		})
		assert.NoError(t, err)
	})

	t.Run("rejects nil silence", func(t *testing.T) {
		err := manager.AddSilence(nil)
		assert.Error(t, err)
	})

	t.Run("rejects invalid time range", func(t *testing.T) {
		err := manager.AddSilence(&Silence{
			StartsAt: time.Now().Add(1 * time.Hour),
			EndsAt:   time.Now(),
		})
		assert.Error(t, err)
	})

	t.Run("lists active silences", func(t *testing.T) {
		silences := manager.ListActiveSilences()
		assert.Len(t, silences, 1)
	})

	t.Run("suppresses matching alerts", func(t *testing.T) {
		_ = manager.AddRule(&AlertRule{Name: "silenced", Cooldown: 1 * time.Millisecond})

		alert, _ := manager.FireAlert("silenced", "test", 1, map[string]string{"env": "staging"})
		assert.Nil(t, alert) // Should be silenced
	})

	t.Run("allows non-matching alerts", func(t *testing.T) {
		_ = manager.AddRule(&AlertRule{Name: "not_silenced", Cooldown: 1 * time.Millisecond})

		alert, _ := manager.FireAlert("not_silenced", "test", 1, map[string]string{"env": "production"})
		assert.NotNil(t, alert)
	})
}

func TestAlertManager_Notifications(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("configures notification", func(t *testing.T) {
		err := manager.ConfigureNotification(&NotificationConfig{
			Channel:  ChannelSlack,
			Endpoint: "https://hooks.slack.com/xxx",
		})
		assert.NoError(t, err)
	})

	t.Run("rejects nil config", func(t *testing.T) {
		err := manager.ConfigureNotification(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty endpoint", func(t *testing.T) {
		err := manager.ConfigureNotification(&NotificationConfig{
			Channel: ChannelWebhook,
		})
		assert.Error(t, err)
	})

	t.Run("lists configured channels", func(t *testing.T) {
		channels := manager.ListNotificationChannels()
		assert.Contains(t, channels, ChannelSlack)
	})
}

func TestAlertManager_SetupProductionAlerting(t *testing.T) {
	manager := NewAlertManager(nil)

	err := manager.SetupProductionAlerting()
	require.NoError(t, err)

	t.Run("creates expected rules", func(t *testing.T) {
		rules := manager.ListRules()
		assert.GreaterOrEqual(t, len(rules), 8)
	})

	t.Run("has error rate rule", func(t *testing.T) {
		rule := manager.GetRule("high_error_rate")
		require.NotNil(t, rule)
		assert.Equal(t, AlertSeverityCritical, rule.Severity)
	})

	t.Run("has DDoS rule", func(t *testing.T) {
		rule := manager.GetRule("ddos_attack_detected")
		require.NotNil(t, rule)
		assert.Equal(t, AlertSeverityFatal, rule.Severity)
	})
}

func TestAlertManager_GetStats(t *testing.T) {
	manager := NewAlertManager(&AlertingConfig{Enabled: true})
	_ = manager.AddRule(&AlertRule{Name: "stats_test", Cooldown: 1 * time.Millisecond})
	_, _ = manager.FireAlert("stats_test", "test", 1, nil)

	stats := manager.GetStats()

	assert.Equal(t, 1, stats["total_rules"])
	assert.Equal(t, 1, stats["total_alerts"])
	assert.Equal(t, 1, stats["active_alerts"])
}

func TestAlert_Duration(t *testing.T) {
	t.Run("active alert", func(t *testing.T) {
		alert := &Alert{FiredAt: time.Now().Add(-1 * time.Hour)}
		duration := alert.Duration()
		assert.InDelta(t, 1*time.Hour, duration, float64(1*time.Second))
	})

	t.Run("resolved alert", func(t *testing.T) {
		fired := time.Now().Add(-1 * time.Hour)
		resolved := time.Now().Add(-30 * time.Minute)
		alert := &Alert{FiredAt: fired, ResolvedAt: &resolved}
		duration := alert.Duration()
		assert.InDelta(t, 30*time.Minute, duration, float64(1*time.Second))
	})
}

func TestSilence_IsActive(t *testing.T) {
	t.Run("active silence", func(t *testing.T) {
		silence := &Silence{
			StartsAt: time.Now().Add(-1 * time.Hour),
			EndsAt:   time.Now().Add(1 * time.Hour),
		}
		assert.True(t, silence.IsActive())
	})

	t.Run("expired silence", func(t *testing.T) {
		silence := &Silence{
			StartsAt: time.Now().Add(-2 * time.Hour),
			EndsAt:   time.Now().Add(-1 * time.Hour),
		}
		assert.False(t, silence.IsActive())
	})

	t.Run("future silence", func(t *testing.T) {
		silence := &Silence{
			StartsAt: time.Now().Add(1 * time.Hour),
			EndsAt:   time.Now().Add(2 * time.Hour),
		}
		assert.False(t, silence.IsActive())
	})
}

func TestAlertSeverityConstants(t *testing.T) {
	assert.Equal(t, AlertSeverity("info"), AlertSeverityInfo)
	assert.Equal(t, AlertSeverity("warning"), AlertSeverityWarning)
	assert.Equal(t, AlertSeverity("critical"), AlertSeverityCritical)
	assert.Equal(t, AlertSeverity("fatal"), AlertSeverityFatal)
}

func TestAlertStateConstants(t *testing.T) {
	assert.Equal(t, AlertState("pending"), AlertStatePending)
	assert.Equal(t, AlertState("firing"), AlertStateFiring)
	assert.Equal(t, AlertState("resolved"), AlertStateResolved)
	assert.Equal(t, AlertState("silenced"), AlertStateSilenced)
}

func TestNotificationChannelConstants(t *testing.T) {
	assert.Equal(t, NotificationChannel("email"), ChannelEmail)
	assert.Equal(t, NotificationChannel("slack"), ChannelSlack)
	assert.Equal(t, NotificationChannel("pagerduty"), ChannelPagerDuty)
	assert.Equal(t, NotificationChannel("webhook"), ChannelWebhook)
}

func TestDefaultAlertingConfigs(t *testing.T) {
	t.Run("production has short intervals", func(t *testing.T) {
		config := DefaultAlertingConfigs[EnvTypeProduction]
		assert.True(t, config.Enabled)
		assert.Equal(t, 15*time.Second, config.EvaluationInterval)
	})

	t.Run("development is disabled", func(t *testing.T) {
		config := DefaultAlertingConfigs[EnvTypeDevelopment]
		assert.False(t, config.Enabled)
	})
}

func TestGetAlertingConfigForEnvironment(t *testing.T) {
	t.Run("returns production config", func(t *testing.T) {
		config := GetAlertingConfigForEnvironment(EnvTypeProduction)
		assert.True(t, config.Enabled)
	})

	t.Run("returns development for unknown", func(t *testing.T) {
		config := GetAlertingConfigForEnvironment("unknown")
		assert.False(t, config.Enabled)
	})
}
