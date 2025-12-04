// internal/alerting/escalation_test.go
package alerting

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEscalationPolicyConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &EscalationPolicyConfig{
			Name: "default-policy",
			Steps: []EscalationStep{
				{Delay: 0, Targets: []string{"team-oncall"}},
				{Delay: 15 * time.Minute, Targets: []string{"team-lead"}},
			},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &EscalationPolicyConfig{
			Steps: []EscalationStep{{Targets: []string{"user"}}},
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects empty steps", func(t *testing.T) {
		config := &EscalationPolicyConfig{Name: "test"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "step")
	})

	t.Run("rejects step without targets", func(t *testing.T) {
		config := &EscalationPolicyConfig{
			Name:  "test",
			Steps: []EscalationStep{{Delay: 0}},
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "target")
	})
}

func TestNewEscalationManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewEscalationManager()
		assert.NotNil(t, manager)
	})
}

func TestEscalationManager_AddPolicy(t *testing.T) {
	manager := NewEscalationManager()

	t.Run("adds policy", func(t *testing.T) {
		config := &EscalationPolicyConfig{
			Name: "api-alerts",
			Steps: []EscalationStep{
				{Delay: 0, Targets: []string{"api-team"}},
			},
		}

		policy, err := manager.AddPolicy(config)
		require.NoError(t, err)
		assert.Equal(t, "api-alerts", policy.Name())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		config := &EscalationPolicyConfig{
			Name:  "duplicate",
			Steps: []EscalationStep{{Targets: []string{"user"}}},
		}
		_, _ = manager.AddPolicy(config)
		_, err := manager.AddPolicy(config)
		assert.Error(t, err)
	})
}

func TestEscalationPolicy_Escalate(t *testing.T) {
	manager := NewEscalationManager()

	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name: "multi-step",
		Steps: []EscalationStep{
			{Delay: 0, Targets: []string{"level1"}},
			{Delay: 50 * time.Millisecond, Targets: []string{"level2"}},
			{Delay: 100 * time.Millisecond, Targets: []string{"level3"}},
		},
	})

	t.Run("starts at first step", func(t *testing.T) {
		incident := policy.CreateIncident("alert-123")
		targets := incident.CurrentTargets()
		assert.Equal(t, []string{"level1"}, targets)
		assert.Equal(t, 0, incident.CurrentStep())
	})

	t.Run("escalates after delay", func(t *testing.T) {
		incident := policy.CreateIncident("alert-456")

		time.Sleep(60 * time.Millisecond)
		incident.CheckEscalation()

		assert.Equal(t, 1, incident.CurrentStep())
		assert.Equal(t, []string{"level2"}, incident.CurrentTargets())
	})

	t.Run("escalates to final step", func(t *testing.T) {
		incident := policy.CreateIncident("alert-789")

		time.Sleep(60 * time.Millisecond)
		incident.CheckEscalation()
		time.Sleep(60 * time.Millisecond)
		incident.CheckEscalation()

		assert.Equal(t, 2, incident.CurrentStep())
		assert.Equal(t, []string{"level3"}, incident.CurrentTargets())
	})
}

func TestIncident_Acknowledge(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "ack-test",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	t.Run("acknowledges incident", func(t *testing.T) {
		incident := policy.CreateIncident("alert-1")
		err := incident.Acknowledge("user@example.com")

		assert.NoError(t, err)
		assert.True(t, incident.IsAcknowledged())
		assert.Equal(t, "user@example.com", incident.AcknowledgedBy())
	})

	t.Run("stops escalation when acknowledged", func(t *testing.T) {
		policy2, _ := manager.AddPolicy(&EscalationPolicyConfig{
			Name: "ack-stop-test",
			Steps: []EscalationStep{
				{Delay: 0, Targets: []string{"level1"}},
				{Delay: 10 * time.Millisecond, Targets: []string{"level2"}},
			},
		})

		incident := policy2.CreateIncident("alert-2")
		_ = incident.Acknowledge("user@example.com")

		time.Sleep(20 * time.Millisecond)
		incident.CheckEscalation()

		// Should still be at step 0 because acknowledged
		assert.Equal(t, 0, incident.CurrentStep())
	})
}

func TestIncident_Resolve(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "resolve-test",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	t.Run("resolves incident", func(t *testing.T) {
		incident := policy.CreateIncident("alert-1")
		err := incident.Resolve("user@example.com", "Fixed the issue")

		assert.NoError(t, err)
		assert.True(t, incident.IsResolved())
		assert.Equal(t, "Fixed the issue", incident.Resolution())
	})
}

func TestIncident_Timeline(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "timeline-test",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	t.Run("tracks timeline", func(t *testing.T) {
		incident := policy.CreateIncident("alert-1")
		_ = incident.Acknowledge("user1")
		_ = incident.Resolve("user2", "Fixed")

		timeline := incident.Timeline()
		assert.GreaterOrEqual(t, len(timeline), 3) // created, ack, resolved
	})
}

func TestIncident_Reassign(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "reassign-test",
		Steps: []EscalationStep{{Targets: []string{"user1"}}},
	})

	t.Run("reassigns to new target", func(t *testing.T) {
		incident := policy.CreateIncident("alert-1")
		err := incident.Reassign("user2", "Need database expertise")

		assert.NoError(t, err)
		targets := incident.CurrentTargets()
		assert.Contains(t, targets, "user2")
	})
}

func TestIncident_Snooze(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name: "snooze-test",
		Steps: []EscalationStep{
			{Delay: 0, Targets: []string{"level1"}},
			{Delay: 10 * time.Millisecond, Targets: []string{"level2"}},
		},
	})

	t.Run("snoozes escalation", func(t *testing.T) {
		incident := policy.CreateIncident("alert-1")
		incident.Snooze(100 * time.Millisecond)

		time.Sleep(20 * time.Millisecond)
		incident.CheckEscalation()

		// Should not escalate while snoozed
		assert.Equal(t, 0, incident.CurrentStep())
	})
}

func TestIncident_Priority(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "priority-test",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	t.Run("sets priority", func(t *testing.T) {
		incident := policy.CreateIncident("alert-1")
		incident.SetPriority(PriorityHigh)

		assert.Equal(t, PriorityHigh, incident.Priority())
	})
}

func TestEscalationManager_GetPolicy(t *testing.T) {
	manager := NewEscalationManager()
	_, _ = manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "get-test",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	t.Run("gets policy by name", func(t *testing.T) {
		policy := manager.GetPolicy("get-test")
		assert.NotNil(t, policy)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		policy := manager.GetPolicy("unknown")
		assert.Nil(t, policy)
	})
}

func TestEscalationManager_ListPolicies(t *testing.T) {
	manager := NewEscalationManager()
	_, _ = manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "policy-1",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})
	_, _ = manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "policy-2",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	t.Run("lists policies", func(t *testing.T) {
		policies := manager.ListPolicies()
		assert.Len(t, policies, 2)
	})
}

func TestEscalationManager_GetIncidents(t *testing.T) {
	manager := NewEscalationManager()
	policy, _ := manager.AddPolicy(&EscalationPolicyConfig{
		Name:  "incidents-test",
		Steps: []EscalationStep{{Targets: []string{"user"}}},
	})

	_ = policy.CreateIncident("alert-1")
	_ = policy.CreateIncident("alert-2")

	t.Run("gets all incidents", func(t *testing.T) {
		incidents := manager.GetIncidents()
		assert.Len(t, incidents, 2)
	})

	t.Run("gets active incidents", func(t *testing.T) {
		incidents := manager.GetActiveIncidents()
		assert.Len(t, incidents, 2)
	})
}

func TestPriorities(t *testing.T) {
	t.Run("defines priorities", func(t *testing.T) {
		assert.Equal(t, 1, PriorityLow)
		assert.Equal(t, 2, PriorityMedium)
		assert.Equal(t, 3, PriorityHigh)
		assert.Equal(t, 4, PriorityCritical)
	})
}

func TestIncidentStatus(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "triggered", IncidentTriggered)
		assert.Equal(t, "acknowledged", IncidentAcknowledged)
		assert.Equal(t, "resolved", IncidentResolved)
	})
}

func TestEscalationStep(t *testing.T) {
	t.Run("creates step", func(t *testing.T) {
		step := EscalationStep{
			Delay:   5 * time.Minute,
			Targets: []string{"team-a", "team-b"},
		}
		assert.Equal(t, 5*time.Minute, step.Delay)
		assert.Len(t, step.Targets, 2)
	})
}

func TestTimelineEntry(t *testing.T) {
	t.Run("creates entry", func(t *testing.T) {
		entry := TimelineEntry{
			Timestamp: time.Now(),
			Action:    "acknowledged",
			Actor:     "user@example.com",
			Details:   "Taking ownership",
		}
		assert.Equal(t, "acknowledged", entry.Action)
	})
}
