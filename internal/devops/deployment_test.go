// internal/devops/deployment_test.go
package devops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeploymentConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &DeploymentConfig{
			Name:        "web-app",
			Environment: "production",
			Strategy:    StrategyRolling,
			Replicas:    3,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &DeploymentConfig{Environment: "production"}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects invalid strategy", func(t *testing.T) {
		config := &DeploymentConfig{Name: "app", Environment: "prod", Strategy: "invalid"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewDeploymentManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewDeploymentManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestDeploymentManager_Deploy(t *testing.T) {
	manager := NewDeploymentManager(nil)

	manager.RegisterTarget("mock", &MockDeployTarget{})

	t.Run("creates deployment", func(t *testing.T) {
		ctx := context.Background()
		deployment, err := manager.Deploy(ctx, &DeploymentConfig{
			Name:        "test-app",
			Environment: "staging",
			Strategy:    StrategyRolling,
			Target:      "mock",
			Replicas:    2,
		})

		require.NoError(t, err)
		assert.NotEmpty(t, deployment.ID())
		assert.Contains(t, []string{DeployStatusPending, DeployStatusRunning, DeployStatusSuccess}, deployment.Status())
	})
}

func TestDeployment_Lifecycle(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "lifecycle-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "mock",
		Replicas:    3,
	})

	t.Run("transitions through states", func(t *testing.T) {
		_ = deployment.Wait(ctx)

		history := deployment.StatusHistory()
		assert.GreaterOrEqual(t, len(history), 2)
	})
}

func TestDeployment_RollingStrategy(t *testing.T) {
	manager := NewDeploymentManager(nil)

	target := &MockDeployTarget{}
	manager.RegisterTarget("mock", target)

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:           "rolling-app",
		Environment:    "production",
		Strategy:       StrategyRolling,
		Target:         "mock",
		Replicas:       3,
		MaxSurge:       1,
		MaxUnavailable: 1,
	})

	t.Run("deploys incrementally", func(t *testing.T) {
		_ = deployment.Wait(ctx)
		assert.Equal(t, DeployStatusSuccess, deployment.Status())
	})
}

func TestDeployment_RecreateStrategy(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "recreate-app",
		Environment: "production",
		Strategy:    StrategyRecreate,
		Target:      "mock",
		Replicas:    2,
	})

	t.Run("recreates all instances", func(t *testing.T) {
		_ = deployment.Wait(ctx)
		assert.Equal(t, DeployStatusSuccess, deployment.Status())
	})
}

func TestDeployment_Rollback(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()

	// First deployment
	dep1, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "rollback-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "mock",
		Version:     "v1.0.0",
	})
	_ = dep1.Wait(ctx)

	// Second deployment
	dep2, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "rollback-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "mock",
		Version:     "v2.0.0",
	})
	_ = dep2.Wait(ctx)

	t.Run("rolls back to previous version", func(t *testing.T) {
		rollback, err := manager.Rollback(ctx, "rollback-app", "production")
		require.NoError(t, err)
		_ = rollback.Wait(ctx)

		assert.Equal(t, "v1.0.0", rollback.Version())
	})
}

func TestDeployment_Cancel(t *testing.T) {
	manager := NewDeploymentManager(nil)

	slowTarget := &MockDeployTarget{delay: 100 * time.Millisecond}
	manager.RegisterTarget("slow", slowTarget)

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "cancel-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "slow",
		Replicas:    10,
	})

	t.Run("cancels in-progress deployment", func(t *testing.T) {
		time.Sleep(50 * time.Millisecond)
		err := deployment.Cancel()
		assert.NoError(t, err)
		assert.Equal(t, DeployStatusCanceled, deployment.Status())
	})
}

func TestDeployment_HealthChecks(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "health-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "mock",
		HealthCheck: &HealthCheckConfig{
			Path:     "/health",
			Interval: 5 * time.Second,
			Timeout:  2 * time.Second,
			Retries:  3,
		},
	})

	t.Run("runs health checks", func(t *testing.T) {
		_ = deployment.Wait(ctx)
		assert.Equal(t, DeployStatusSuccess, deployment.Status())
	})
}

func TestDeployment_Hooks(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	preDeployCalled := false
	postDeployCalled := false

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "hooks-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "mock",
		Hooks: &DeployHooks{
			PreDeploy:  func() error { preDeployCalled = true; return nil },
			PostDeploy: func() error { postDeployCalled = true; return nil },
		},
	})

	t.Run("executes hooks", func(t *testing.T) {
		_ = deployment.Wait(ctx)
		assert.True(t, preDeployCalled)
		assert.True(t, postDeployCalled)
	})
}

func TestDeploymentManager_List(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()
	_, _ = manager.Deploy(ctx, &DeploymentConfig{Name: "app1", Environment: "prod", Strategy: StrategyRolling, Target: "mock"})
	_, _ = manager.Deploy(ctx, &DeploymentConfig{Name: "app2", Environment: "prod", Strategy: StrategyRolling, Target: "mock"})

	t.Run("lists deployments", func(t *testing.T) {
		deployments := manager.List()
		assert.Len(t, deployments, 2)
	})
}

func TestDeploymentManager_GetHistory(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()
	dep1, _ := manager.Deploy(ctx, &DeploymentConfig{Name: "history-app", Environment: "prod", Strategy: StrategyRolling, Target: "mock", Version: "v1"})
	_ = dep1.Wait(ctx)

	dep2, _ := manager.Deploy(ctx, &DeploymentConfig{Name: "history-app", Environment: "prod", Strategy: StrategyRolling, Target: "mock", Version: "v2"})
	_ = dep2.Wait(ctx)

	t.Run("returns deployment history", func(t *testing.T) {
		history := manager.GetHistory("history-app", "prod")
		assert.Len(t, history, 2)
	})
}

func TestStrategies(t *testing.T) {
	t.Run("defines strategies", func(t *testing.T) {
		assert.Equal(t, "rolling", StrategyRolling)
		assert.Equal(t, "recreate", StrategyRecreate)
		assert.Equal(t, "blue-green", StrategyBlueGreen)
		assert.Equal(t, "canary", StrategyCanary)
	})
}

func TestDeployStatuses(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "pending", DeployStatusPending)
		assert.Equal(t, "running", DeployStatusRunning)
		assert.Equal(t, "success", DeployStatusSuccess)
		assert.Equal(t, "failed", DeployStatusFailed)
		assert.Equal(t, "canceled", DeployStatusCanceled)
		assert.Equal(t, "rollback", DeployStatusRollback)
	})
}

func TestDeployment_Metrics(t *testing.T) {
	manager := NewDeploymentManager(nil)
	manager.RegisterTarget("mock", &MockDeployTarget{})

	ctx := context.Background()
	deployment, _ := manager.Deploy(ctx, &DeploymentConfig{
		Name:        "metrics-app",
		Environment: "production",
		Strategy:    StrategyRolling,
		Target:      "mock",
	})

	_ = deployment.Wait(ctx)

	t.Run("tracks metrics", func(t *testing.T) {
		metrics := deployment.Metrics()
		assert.NotNil(t, metrics)
		assert.Greater(t, metrics.Duration, time.Duration(0))
	})
}

// MockDeployTarget for testing
type MockDeployTarget struct {
	delay time.Duration
}

func (m *MockDeployTarget) Deploy(ctx context.Context, config *DeploymentConfig) error {
	if m.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.delay):
		}
	}
	return nil
}

func (m *MockDeployTarget) Rollback(ctx context.Context, config *DeploymentConfig) error {
	return nil
}

func (m *MockDeployTarget) Status(name, env string) (string, error) {
	return DeployStatusSuccess, nil
}

func (m *MockDeployTarget) Scale(name, env string, replicas int) error {
	return nil
}
