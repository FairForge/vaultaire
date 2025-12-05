// internal/devops/bluegreen_test.go
package devops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlueGreenConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &BlueGreenConfig{
			Name:        "web-app",
			Environment: "production",
			Replicas:    3,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &BlueGreenConfig{Environment: "prod"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewBlueGreenManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewBlueGreenManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestBlueGreenManager_Deploy(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{})

	ctx := context.Background()

	t.Run("creates blue environment first", func(t *testing.T) {
		deploy, err := manager.Deploy(ctx, &BlueGreenConfig{
			Name:        "first-app",
			Environment: "production",
			Target:      "mock",
			Version:     "v1.0.0",
		})

		require.NoError(t, err)
		assert.Equal(t, SlotBlue, deploy.InactiveSlot())
	})

	t.Run("deploys to green on second deploy", func(t *testing.T) {
		// First deployment
		_, _ = manager.Deploy(ctx, &BlueGreenConfig{
			Name:        "second-app",
			Environment: "production",
			Target:      "mock",
			Version:     "v1.0.0",
		})

		// Second deployment goes to inactive slot
		deploy, err := manager.Deploy(ctx, &BlueGreenConfig{
			Name:        "second-app",
			Environment: "production",
			Target:      "mock",
			Version:     "v2.0.0",
		})

		require.NoError(t, err)
		assert.Equal(t, SlotGreen, deploy.InactiveSlot())
	})
}

func TestBlueGreen_Switch(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{})

	ctx := context.Background()

	// Deploy v1 to blue
	deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "switch-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v1.0.0",
	})
	_ = deploy.Wait(ctx)

	// Deploy v2 to green
	deploy, _ = manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "switch-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
	})
	_ = deploy.Wait(ctx)

	t.Run("switches traffic to green", func(t *testing.T) {
		err := manager.Switch(ctx, "switch-app", "production")
		assert.NoError(t, err)

		state := manager.GetState("switch-app", "production")
		assert.Equal(t, SlotGreen, state.ActiveSlot)
	})
}

func TestBlueGreen_Rollback(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{})

	ctx := context.Background()

	// Deploy v1 to blue
	deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "rollback-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v1.0.0",
	})
	_ = deploy.Wait(ctx)

	// Deploy v2 to green and switch
	deploy, _ = manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "rollback-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
	})
	_ = deploy.Wait(ctx)
	_ = manager.Switch(ctx, "rollback-app", "production")

	t.Run("rolls back to blue", func(t *testing.T) {
		err := manager.Rollback(ctx, "rollback-app", "production")
		assert.NoError(t, err)

		state := manager.GetState("rollback-app", "production")
		assert.Equal(t, SlotBlue, state.ActiveSlot)
		assert.Equal(t, "v1.0.0", state.BlueVersion)
	})
}

func TestBlueGreen_HealthCheck(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{healthy: true})

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "health-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v1.0.0",
		HealthCheck: &BlueGreenHealthCheck{
			Endpoint: "/health",
			Interval: 100 * time.Millisecond,
			Timeout:  50 * time.Millisecond,
		},
	})

	t.Run("waits for healthy before completing", func(t *testing.T) {
		err := deploy.Wait(ctx)
		assert.NoError(t, err)
		assert.Equal(t, BlueGreenStatusReady, deploy.Status())
	})
}

func TestBlueGreen_AutoSwitch(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{healthy: true})

	ctx := context.Background()

	// First deploy to blue
	deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "auto-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v1.0.0",
	})
	_ = deploy.Wait(ctx)

	t.Run("auto-switches when enabled", func(t *testing.T) {
		deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
			Name:        "auto-app",
			Environment: "production",
			Target:      "mock",
			Version:     "v2.0.0",
			AutoSwitch:  true,
		})
		_ = deploy.Wait(ctx)

		state := manager.GetState("auto-app", "production")
		assert.Equal(t, SlotGreen, state.ActiveSlot)
	})
}

func TestBlueGreen_Cleanup(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{})

	ctx := context.Background()

	// Deploy to both slots
	deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "cleanup-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v1.0.0",
	})
	_ = deploy.Wait(ctx)

	deploy, _ = manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "cleanup-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
	})
	_ = deploy.Wait(ctx)

	t.Run("cleans up inactive slot", func(t *testing.T) {
		err := manager.Cleanup(ctx, "cleanup-app", "production")
		assert.NoError(t, err)
	})
}

func TestBlueGreenState(t *testing.T) {
	t.Run("tracks state", func(t *testing.T) {
		state := &BlueGreenState{
			ActiveSlot:   SlotBlue,
			BlueVersion:  "v1.0.0",
			GreenVersion: "v2.0.0",
			BlueReady:    true,
			GreenReady:   true,
		}

		assert.Equal(t, SlotBlue, state.ActiveSlot)
		assert.True(t, state.BlueReady)
	})
}

func TestSlots(t *testing.T) {
	t.Run("defines slots", func(t *testing.T) {
		assert.Equal(t, "blue", SlotBlue)
		assert.Equal(t, "green", SlotGreen)
	})
}

func TestBlueGreenStatuses(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "pending", BlueGreenStatusPending)
		assert.Equal(t, "deploying", BlueGreenStatusDeploying)
		assert.Equal(t, "ready", BlueGreenStatusReady)
		assert.Equal(t, "switching", BlueGreenStatusSwitching)
		assert.Equal(t, "failed", BlueGreenStatusFailed)
	})
}

func TestBlueGreenManager_GetState(t *testing.T) {
	manager := NewBlueGreenManager(nil)
	manager.RegisterTarget("mock", &MockBlueGreenTarget{})

	ctx := context.Background()
	deploy, _ := manager.Deploy(ctx, &BlueGreenConfig{
		Name:        "state-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v1.0.0",
	})
	_ = deploy.Wait(ctx)

	t.Run("returns state", func(t *testing.T) {
		state := manager.GetState("state-app", "production")
		assert.NotNil(t, state)
		assert.Equal(t, "v1.0.0", state.BlueVersion)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		state := manager.GetState("unknown", "production")
		assert.Nil(t, state)
	})
}

// MockBlueGreenTarget for testing
type MockBlueGreenTarget struct {
	healthy bool
}

func (m *MockBlueGreenTarget) DeployToSlot(ctx context.Context, config *BlueGreenConfig, slot string) error {
	return nil
}

func (m *MockBlueGreenTarget) SwitchTraffic(ctx context.Context, name, env, slot string) error {
	return nil
}

func (m *MockBlueGreenTarget) CheckHealth(ctx context.Context, name, env, slot string) (bool, error) {
	return m.healthy, nil
}

func (m *MockBlueGreenTarget) CleanupSlot(ctx context.Context, name, env, slot string) error {
	return nil
}
