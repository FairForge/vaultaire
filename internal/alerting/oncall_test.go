// internal/alerting/oncall_test.go
package alerting

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotationConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &RotationConfig{
			Name:      "primary-oncall",
			Type:      RotationTypeWeekly,
			Users:     []string{"alice", "bob", "charlie"},
			StartTime: time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC),
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &RotationConfig{
			Type:  RotationTypeDaily,
			Users: []string{"alice"},
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects empty users", func(t *testing.T) {
		config := &RotationConfig{
			Name: "test",
			Type: RotationTypeDaily,
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user")
	})

	t.Run("rejects invalid type", func(t *testing.T) {
		config := &RotationConfig{
			Name:  "test",
			Type:  "invalid",
			Users: []string{"alice"},
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type")
	})
}

func TestNewOnCallManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewOnCallManager()
		assert.NotNil(t, manager)
	})
}

func TestOnCallManager_AddRotation(t *testing.T) {
	manager := NewOnCallManager()

	t.Run("adds rotation", func(t *testing.T) {
		config := &RotationConfig{
			Name:      "api-team",
			Type:      RotationTypeWeekly,
			Users:     []string{"alice", "bob"},
			StartTime: time.Now(),
		}

		rotation, err := manager.AddRotation(config)
		require.NoError(t, err)
		assert.Equal(t, "api-team", rotation.Name())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		config := &RotationConfig{
			Name:      "duplicate",
			Type:      RotationTypeDaily,
			Users:     []string{"alice"},
			StartTime: time.Now(),
		}
		_, _ = manager.AddRotation(config)
		_, err := manager.AddRotation(config)
		assert.Error(t, err)
	})
}

func TestRotation_CurrentOnCall(t *testing.T) {
	manager := NewOnCallManager()

	t.Run("daily rotation", func(t *testing.T) {
		// Start 2 days ago
		startTime := time.Now().Add(-48 * time.Hour)

		rotation, _ := manager.AddRotation(&RotationConfig{
			Name:      "daily-test",
			Type:      RotationTypeDaily,
			Users:     []string{"alice", "bob", "charlie"},
			StartTime: startTime,
		})

		// After 2 days, should be on 3rd user (index 2)
		current := rotation.CurrentOnCall()
		assert.Equal(t, "charlie", current)
	})

	t.Run("weekly rotation", func(t *testing.T) {
		// Start 1 week ago
		startTime := time.Now().Add(-7 * 24 * time.Hour)

		rotation, _ := manager.AddRotation(&RotationConfig{
			Name:      "weekly-test",
			Type:      RotationTypeWeekly,
			Users:     []string{"alice", "bob"},
			StartTime: startTime,
		})

		// After 1 week, should be on 2nd user
		current := rotation.CurrentOnCall()
		assert.Equal(t, "bob", current)
	})

	t.Run("custom duration rotation", func(t *testing.T) {
		startTime := time.Now().Add(-6 * time.Hour)

		rotation, _ := manager.AddRotation(&RotationConfig{
			Name:          "custom-test",
			Type:          RotationTypeCustom,
			Users:         []string{"alice", "bob", "charlie"},
			StartTime:     startTime,
			ShiftDuration: 2 * time.Hour,
		})

		// After 6 hours with 2-hour shifts, should be on 4th position (index 0, wraps)
		current := rotation.CurrentOnCall()
		assert.Equal(t, "alice", current)
	})
}

func TestRotation_Override(t *testing.T) {
	manager := NewOnCallManager()

	rotation, _ := manager.AddRotation(&RotationConfig{
		Name:      "override-test",
		Type:      RotationTypeDaily,
		Users:     []string{"alice", "bob"},
		StartTime: time.Now(),
	})

	t.Run("adds override", func(t *testing.T) {
		override, err := rotation.AddOverride(&OverrideConfig{
			User:    "charlie",
			StartAt: time.Now().Add(-time.Hour),
			EndAt:   time.Now().Add(time.Hour),
			Reason:  "Covering for alice",
		})

		require.NoError(t, err)
		assert.NotEmpty(t, override.ID)
	})

	t.Run("override takes precedence", func(t *testing.T) {
		// Current should be charlie due to override
		current := rotation.CurrentOnCall()
		assert.Equal(t, "charlie", current)
	})

	t.Run("returns to normal after override", func(t *testing.T) {
		// Add expired override
		rotation2, _ := manager.AddRotation(&RotationConfig{
			Name:      "expired-override-test",
			Type:      RotationTypeDaily,
			Users:     []string{"alice", "bob"},
			StartTime: time.Now(),
		})

		_, _ = rotation2.AddOverride(&OverrideConfig{
			User:    "charlie",
			StartAt: time.Now().Add(-2 * time.Hour),
			EndAt:   time.Now().Add(-1 * time.Hour),
			Reason:  "Past override",
		})

		// Should be alice since override expired
		current := rotation2.CurrentOnCall()
		assert.Equal(t, "alice", current)
	})
}

func TestRotation_Schedule(t *testing.T) {
	manager := NewOnCallManager()

	startTime := time.Now()
	rotation, _ := manager.AddRotation(&RotationConfig{
		Name:      "schedule-test",
		Type:      RotationTypeDaily,
		Users:     []string{"alice", "bob", "charlie"},
		StartTime: startTime,
	})

	t.Run("returns schedule for time range", func(t *testing.T) {
		schedule := rotation.Schedule(startTime, startTime.Add(5*24*time.Hour))

		assert.Len(t, schedule, 5)
		assert.Equal(t, "alice", schedule[0].User)
		assert.Equal(t, "bob", schedule[1].User)
		assert.Equal(t, "charlie", schedule[2].User)
	})
}

func TestRotation_NextHandoff(t *testing.T) {
	manager := NewOnCallManager()

	startTime := time.Now()
	rotation, _ := manager.AddRotation(&RotationConfig{
		Name:      "handoff-test",
		Type:      RotationTypeDaily,
		Users:     []string{"alice", "bob"},
		StartTime: startTime,
	})

	t.Run("returns next handoff time", func(t *testing.T) {
		handoff := rotation.NextHandoff()
		assert.True(t, handoff.After(time.Now()))
		assert.True(t, handoff.Before(time.Now().Add(25*time.Hour)))
	})
}

func TestOnCallManager_WhoIsOnCall(t *testing.T) {
	manager := NewOnCallManager()

	_, _ = manager.AddRotation(&RotationConfig{
		Name:      "primary",
		Type:      RotationTypeDaily,
		Users:     []string{"alice", "bob"},
		StartTime: time.Now(),
		Layer:     1,
	})

	_, _ = manager.AddRotation(&RotationConfig{
		Name:      "secondary",
		Type:      RotationTypeDaily,
		Users:     []string{"charlie", "david"},
		StartTime: time.Now(),
		Layer:     2,
	})

	t.Run("returns all on-call users", func(t *testing.T) {
		oncall := manager.WhoIsOnCall()
		assert.Len(t, oncall, 2)
	})

	t.Run("returns by layer", func(t *testing.T) {
		primary := manager.WhoIsOnCallByLayer(1)
		assert.Equal(t, "alice", primary)
	})
}

func TestOnCallManager_GetRotation(t *testing.T) {
	manager := NewOnCallManager()
	_, _ = manager.AddRotation(&RotationConfig{
		Name:      "get-test",
		Type:      RotationTypeDaily,
		Users:     []string{"alice"},
		StartTime: time.Now(),
	})

	t.Run("gets rotation by name", func(t *testing.T) {
		rotation := manager.GetRotation("get-test")
		assert.NotNil(t, rotation)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		rotation := manager.GetRotation("unknown")
		assert.Nil(t, rotation)
	})
}

func TestOnCallManager_ListRotations(t *testing.T) {
	manager := NewOnCallManager()
	_, _ = manager.AddRotation(&RotationConfig{Name: "r1", Type: RotationTypeDaily, Users: []string{"a"}, StartTime: time.Now()})
	_, _ = manager.AddRotation(&RotationConfig{Name: "r2", Type: RotationTypeWeekly, Users: []string{"b"}, StartTime: time.Now()})

	t.Run("lists rotations", func(t *testing.T) {
		rotations := manager.ListRotations()
		assert.Len(t, rotations, 2)
	})
}

func TestRotationTypes(t *testing.T) {
	t.Run("defines types", func(t *testing.T) {
		assert.Equal(t, "daily", RotationTypeDaily)
		assert.Equal(t, "weekly", RotationTypeWeekly)
		assert.Equal(t, "custom", RotationTypeCustom)
	})
}

func TestShiftEntry(t *testing.T) {
	t.Run("creates entry", func(t *testing.T) {
		entry := ShiftEntry{
			User:    "alice",
			StartAt: time.Now(),
			EndAt:   time.Now().Add(24 * time.Hour),
		}
		assert.Equal(t, "alice", entry.User)
	})
}

func TestOverride(t *testing.T) {
	t.Run("creates override", func(t *testing.T) {
		override := Override{
			ID:      "override-1",
			User:    "bob",
			StartAt: time.Now(),
			EndAt:   time.Now().Add(4 * time.Hour),
			Reason:  "Covering shift",
		}
		assert.Equal(t, "bob", override.User)
	})
}

func TestOnCallManager_RemoveRotation(t *testing.T) {
	manager := NewOnCallManager()
	_, _ = manager.AddRotation(&RotationConfig{
		Name:      "to-remove",
		Type:      RotationTypeDaily,
		Users:     []string{"alice"},
		StartTime: time.Now(),
	})

	t.Run("removes rotation", func(t *testing.T) {
		err := manager.RemoveRotation("to-remove")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetRotation("to-remove"))
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := manager.RemoveRotation("unknown")
		assert.Error(t, err)
	})
}

func TestRotation_RemoveOverride(t *testing.T) {
	manager := NewOnCallManager()
	rotation, _ := manager.AddRotation(&RotationConfig{
		Name:      "remove-override-test",
		Type:      RotationTypeDaily,
		Users:     []string{"alice"},
		StartTime: time.Now(),
	})

	override, _ := rotation.AddOverride(&OverrideConfig{
		User:    "bob",
		StartAt: time.Now(),
		EndAt:   time.Now().Add(time.Hour),
	})

	t.Run("removes override", func(t *testing.T) {
		err := rotation.RemoveOverride(override.ID)
		assert.NoError(t, err)
	})
}
