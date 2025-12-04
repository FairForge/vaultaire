// internal/alerting/rules_test.go
package alerting

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAlertRuleConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &AlertRuleConfig{
			Name:      "high-error-rate",
			Condition: "error_rate > 5",
			Severity:  SeverityCritical,
			Duration:  5 * time.Minute,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &AlertRuleConfig{Condition: "x > 1"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects empty condition", func(t *testing.T) {
		config := &AlertRuleConfig{Name: "test"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "condition")
	})
}

func TestNewAlertManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewAlertManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestAlertManager_AddRule(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("adds rule", func(t *testing.T) {
		config := &AlertRuleConfig{
			Name:      "test-rule",
			Condition: "cpu > 80",
			Severity:  SeverityWarning,
		}

		rule, err := manager.AddRule(config)
		require.NoError(t, err)
		assert.Equal(t, "test-rule", rule.Name())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		config := &AlertRuleConfig{Name: "dup-rule", Condition: "x > 1", Severity: SeverityWarning}
		_, _ = manager.AddRule(config)
		_, err := manager.AddRule(config)
		assert.Error(t, err)
	})
}

func TestAlertRule_Evaluate(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("evaluates threshold condition", func(t *testing.T) {
		rule, _ := manager.AddRule(&AlertRuleConfig{
			Name:      "cpu-high",
			Condition: "cpu > 80",
			Severity:  SeverityWarning,
		})

		result := rule.Evaluate(map[string]float64{"cpu": 90})
		assert.True(t, result)

		result = rule.Evaluate(map[string]float64{"cpu": 70})
		assert.False(t, result)
	})

	t.Run("evaluates less than condition", func(t *testing.T) {
		rule, _ := manager.AddRule(&AlertRuleConfig{
			Name:      "disk-low",
			Condition: "disk_free < 10",
			Severity:  SeverityCritical,
		})

		result := rule.Evaluate(map[string]float64{"disk_free": 5})
		assert.True(t, result)
	})

	t.Run("evaluates equals condition", func(t *testing.T) {
		rule, _ := manager.AddRule(&AlertRuleConfig{
			Name:      "status-check",
			Condition: "status == 0",
			Severity:  SeverityCritical,
		})

		result := rule.Evaluate(map[string]float64{"status": 0})
		assert.True(t, result)
	})
}

func TestAlertRule_Duration(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("requires condition for duration", func(t *testing.T) {
		rule, _ := manager.AddRule(&AlertRuleConfig{
			Name:      "sustained-high",
			Condition: "cpu > 80",
			Severity:  SeverityWarning,
			Duration:  100 * time.Millisecond,
		})

		// First evaluation - starts pending
		rule.Evaluate(map[string]float64{"cpu": 90})
		assert.Equal(t, StatePending, rule.State())

		// Wait for duration
		time.Sleep(150 * time.Millisecond)

		// Second evaluation - now fires
		rule.Evaluate(map[string]float64{"cpu": 90})
		assert.Equal(t, StateFiring, rule.State())
	})
}

func TestAlertRule_States(t *testing.T) {
	manager := NewAlertManager(nil)
	rule, _ := manager.AddRule(&AlertRuleConfig{
		Name:      "state-test",
		Condition: "error > 0",
		Severity:  SeverityWarning,
	})

	t.Run("starts inactive", func(t *testing.T) {
		assert.Equal(t, StateInactive, rule.State())
	})

	t.Run("transitions to firing", func(t *testing.T) {
		rule.Evaluate(map[string]float64{"error": 1})
		assert.Equal(t, StateFiring, rule.State())
	})

	t.Run("transitions to resolved", func(t *testing.T) {
		rule.Evaluate(map[string]float64{"error": 0})
		assert.Equal(t, StateInactive, rule.State())
	})
}

func TestAlertManager_GetAlerts(t *testing.T) {
	manager := NewAlertManager(nil)

	rule, _ := manager.AddRule(&AlertRuleConfig{
		Name:      "alert-test",
		Condition: "errors > 0",
		Severity:  SeverityCritical,
	})

	rule.Evaluate(map[string]float64{"errors": 5})

	t.Run("returns active alerts", func(t *testing.T) {
		alerts := manager.GetAlerts(StateFiring)
		assert.Len(t, alerts, 1)
		assert.Equal(t, "alert-test", alerts[0].RuleName)
	})
}

func TestAlert(t *testing.T) {
	t.Run("contains alert details", func(t *testing.T) {
		alert := &Alert{
			ID:       "alert-123",
			RuleName: "high-cpu",
			Severity: SeverityCritical,
			State:    StateFiring,
			Message:  "CPU usage exceeded threshold",
			FiredAt:  time.Now(),
			Labels:   map[string]string{"host": "server-1"},
		}

		assert.Equal(t, "alert-123", alert.ID)
		assert.Equal(t, SeverityCritical, alert.Severity)
	})
}

func TestAlertManager_Silence(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("creates silence", func(t *testing.T) {
		silence, err := manager.CreateSilence(&SilenceConfig{
			Matchers:  map[string]string{"alertname": "test-alert"},
			StartsAt:  time.Now(),
			EndsAt:    time.Now().Add(time.Hour),
			CreatedBy: "admin",
			Comment:   "Maintenance window",
		})

		require.NoError(t, err)
		assert.NotEmpty(t, silence.ID)
	})

	t.Run("silences matching alerts", func(t *testing.T) {
		_, _ = manager.CreateSilence(&SilenceConfig{
			Matchers: map[string]string{"alertname": "silenced-rule"},
			StartsAt: time.Now(),
			EndsAt:   time.Now().Add(time.Hour),
		})

		rule, _ := manager.AddRule(&AlertRuleConfig{
			Name:      "silenced-rule",
			Condition: "x > 0",
			Severity:  SeverityWarning,
		})

		rule.Evaluate(map[string]float64{"x": 1})

		alerts := manager.GetAlerts(StateFiring)
		// Alert should be silenced
		for _, a := range alerts {
			if a.RuleName == "silenced-rule" {
				assert.True(t, a.Silenced)
			}
		}
	})
}

func TestAlertManager_Inhibit(t *testing.T) {
	manager := NewAlertManager(nil)

	t.Run("adds inhibition rule", func(t *testing.T) {
		err := manager.AddInhibition(&InhibitionRule{
			SourceMatch: map[string]string{"severity": "critical"},
			TargetMatch: map[string]string{"severity": "warning"},
			Equal:       []string{"alertname"},
		})
		assert.NoError(t, err)
	})
}

func TestAlertManager_Notify(t *testing.T) {
	manager := NewAlertManager(nil)

	notified := false
	manager.OnAlert(func(alert *Alert) {
		notified = true
	})

	rule, _ := manager.AddRule(&AlertRuleConfig{
		Name:      "notify-test",
		Condition: "x > 0",
		Severity:  SeverityWarning,
	})

	t.Run("notifies on alert", func(t *testing.T) {
		rule.Evaluate(map[string]float64{"x": 1})
		assert.True(t, notified)
	})
}

func TestAlertManager_RunRules(t *testing.T) {
	manager := NewAlertManager(nil)

	_, _ = manager.AddRule(&AlertRuleConfig{
		Name:      "run-test-1",
		Condition: "metric_a > 50",
		Severity:  SeverityWarning,
	})

	_, _ = manager.AddRule(&AlertRuleConfig{
		Name:      "run-test-2",
		Condition: "metric_b > 100",
		Severity:  SeverityCritical,
	})

	t.Run("evaluates all rules", func(t *testing.T) {
		metrics := map[string]float64{
			"metric_a": 60,
			"metric_b": 80,
		}

		manager.EvaluateAll(metrics)

		alerts := manager.GetAlerts(StateFiring)
		assert.Len(t, alerts, 1)
		assert.Equal(t, "run-test-1", alerts[0].RuleName)
	})
}

func TestSeverities(t *testing.T) {
	t.Run("defines severities", func(t *testing.T) {
		assert.Equal(t, "info", SeverityInfo)
		assert.Equal(t, "warning", SeverityWarning)
		assert.Equal(t, "critical", SeverityCritical)
		assert.Equal(t, "page", SeverityPage)
	})
}

func TestStates(t *testing.T) {
	t.Run("defines states", func(t *testing.T) {
		assert.Equal(t, "inactive", StateInactive)
		assert.Equal(t, "pending", StatePending)
		assert.Equal(t, "firing", StateFiring)
	})
}

func TestAlertManager_ListRules(t *testing.T) {
	manager := NewAlertManager(nil)

	_, _ = manager.AddRule(&AlertRuleConfig{Name: "rule-1", Condition: "x > 1", Severity: SeverityWarning})
	_, _ = manager.AddRule(&AlertRuleConfig{Name: "rule-2", Condition: "y > 2", Severity: SeverityCritical})

	t.Run("lists all rules", func(t *testing.T) {
		rules := manager.ListRules()
		assert.Len(t, rules, 2)
	})
}

func TestAlertManager_RemoveRule(t *testing.T) {
	manager := NewAlertManager(nil)

	_, _ = manager.AddRule(&AlertRuleConfig{Name: "to-remove", Condition: "x > 1", Severity: SeverityWarning})

	t.Run("removes rule", func(t *testing.T) {
		err := manager.RemoveRule("to-remove")
		assert.NoError(t, err)

		rules := manager.ListRules()
		assert.Empty(t, rules)
	})
}

func TestAlertManager_GetRule(t *testing.T) {
	manager := NewAlertManager(nil)

	_, _ = manager.AddRule(&AlertRuleConfig{Name: "get-test", Condition: "x > 1", Severity: SeverityWarning})

	t.Run("gets rule by name", func(t *testing.T) {
		rule := manager.GetRule("get-test")
		assert.NotNil(t, rule)
		assert.Equal(t, "get-test", rule.Name())
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		rule := manager.GetRule("unknown")
		assert.Nil(t, rule)
	})
}

func TestAlertManager_Start(t *testing.T) {
	manager := NewAlertManager(&AlertManagerConfig{
		EvaluationInterval: 50 * time.Millisecond,
	})

	_, _ = manager.AddRule(&AlertRuleConfig{
		Name:      "periodic-test",
		Condition: "x > 0",
		Severity:  SeverityWarning,
	})

	manager.SetMetricsProvider(func() map[string]float64 {
		return map[string]float64{"x": 1}
	})

	t.Run("runs periodic evaluation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		go manager.Start(ctx)
		time.Sleep(100 * time.Millisecond)

		alerts := manager.GetAlerts(StateFiring)
		assert.NotEmpty(t, alerts)
	})
}
