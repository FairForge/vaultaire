// internal/devops/canary_test.go
package devops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanaryConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &CanaryConfig{
			Name:        "web-app",
			Environment: "production",
			Steps: []CanaryStep{
				{Weight: 10, Duration: 5 * time.Minute},
				{Weight: 50, Duration: 10 * time.Minute},
				{Weight: 100, Duration: 0},
			},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &CanaryConfig{Environment: "prod"}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects empty steps", func(t *testing.T) {
		config := &CanaryConfig{Name: "app", Environment: "prod"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewCanaryManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewCanaryManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestCanaryManager_Deploy(t *testing.T) {
	manager := NewCanaryManager(nil)
	manager.RegisterTarget("mock", &MockCanaryTarget{})

	ctx := context.Background()

	t.Run("creates canary deployment", func(t *testing.T) {
		deploy, err := manager.Deploy(ctx, &CanaryConfig{
			Name:        "canary-app",
			Environment: "production",
			Target:      "mock",
			Version:     "v2.0.0",
			Steps: []CanaryStep{
				{Weight: 10, Duration: 10 * time.Millisecond},
				{Weight: 100, Duration: 0},
			},
		})

		require.NoError(t, err)
		assert.NotEmpty(t, deploy.ID())
	})
}

func TestCanary_ProgressiveRollout(t *testing.T) {
	manager := NewCanaryManager(nil)
	manager.RegisterTarget("mock", &MockCanaryTarget{})

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name:        "progressive-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
		Steps: []CanaryStep{
			{Weight: 10, Duration: 20 * time.Millisecond},
			{Weight: 50, Duration: 20 * time.Millisecond},
			{Weight: 100, Duration: 0},
		},
	})

	t.Run("progresses through steps", func(t *testing.T) {
		_ = deploy.Wait(ctx)

		history := deploy.StepHistory()
		assert.GreaterOrEqual(t, len(history), 3)
	})
}

func TestCanary_Metrics(t *testing.T) {
	manager := NewCanaryManager(nil)

	target := &MockCanaryTarget{
		errorRate:  0.01,
		latencyP99: 100 * time.Millisecond,
	}
	manager.RegisterTarget("mock", target)

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name:        "metrics-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
		Steps: []CanaryStep{
			{Weight: 10, Duration: 10 * time.Millisecond},
			{Weight: 100, Duration: 0},
		},
		Analysis: &CanaryAnalysis{
			MaxErrorRate:  0.05,
			MaxLatencyP99: 200 * time.Millisecond,
		},
	})

	t.Run("collects metrics during rollout", func(t *testing.T) {
		_ = deploy.Wait(ctx)

		metrics := deploy.Metrics()
		assert.NotNil(t, metrics)
	})
}

func TestCanary_AutoRollback(t *testing.T) {
	manager := NewCanaryManager(nil)

	target := &MockCanaryTarget{
		errorRate: 0.15, // High error rate triggers rollback
	}
	manager.RegisterTarget("mock", target)

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name:        "rollback-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
		Steps: []CanaryStep{
			{Weight: 10, Duration: 10 * time.Millisecond},
			{Weight: 100, Duration: 0},
		},
		Analysis: &CanaryAnalysis{
			MaxErrorRate: 0.05,
		},
	})

	t.Run("auto rollback on high error rate", func(t *testing.T) {
		_ = deploy.Wait(ctx)
		assert.Equal(t, CanaryStatusRolledBack, deploy.Status())
	})
}

func TestCanary_ManualPromotion(t *testing.T) {
	manager := NewCanaryManager(nil)
	manager.RegisterTarget("mock", &MockCanaryTarget{})

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name:        "manual-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
		Steps: []CanaryStep{
			{Weight: 10, Duration: time.Hour, ManualApproval: true},
			{Weight: 100, Duration: 0},
		},
	})

	time.Sleep(20 * time.Millisecond)

	t.Run("waits for manual approval", func(t *testing.T) {
		assert.Equal(t, CanaryStatusPaused, deploy.Status())
	})

	t.Run("promotes on approval", func(t *testing.T) {
		err := deploy.Promote()
		assert.NoError(t, err)

		_ = deploy.Wait(ctx)
		assert.Equal(t, CanaryStatusComplete, deploy.Status())
	})
}

func TestCanary_Abort(t *testing.T) {
	manager := NewCanaryManager(nil)
	manager.RegisterTarget("mock", &MockCanaryTarget{})

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name:        "abort-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
		Steps: []CanaryStep{
			{Weight: 10, Duration: time.Hour},
			{Weight: 100, Duration: 0},
		},
	})

	time.Sleep(20 * time.Millisecond)

	t.Run("aborts canary", func(t *testing.T) {
		err := deploy.Abort()
		assert.NoError(t, err)
		assert.Equal(t, CanaryStatusAborted, deploy.Status())
	})
}

func TestCanary_TrafficSplit(t *testing.T) {
	manager := NewCanaryManager(nil)
	target := &MockCanaryTarget{}
	manager.RegisterTarget("mock", target)

	ctx := context.Background()

	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name:        "split-app",
		Environment: "production",
		Target:      "mock",
		Version:     "v2.0.0",
		Steps: []CanaryStep{
			{Weight: 25, Duration: 10 * time.Millisecond},
			{Weight: 100, Duration: 0},
		},
	})

	t.Run("updates traffic weight", func(t *testing.T) {
		time.Sleep(5 * time.Millisecond)
		weight := deploy.CurrentWeight()
		assert.Greater(t, weight, 0)
	})

	_ = deploy.Wait(ctx)
}

func TestCanaryStatuses(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "pending", CanaryStatusPending)
		assert.Equal(t, "running", CanaryStatusRunning)
		assert.Equal(t, "paused", CanaryStatusPaused)
		assert.Equal(t, "complete", CanaryStatusComplete)
		assert.Equal(t, "rolled_back", CanaryStatusRolledBack)
		assert.Equal(t, "aborted", CanaryStatusAborted)
	})
}

func TestCanaryStep(t *testing.T) {
	t.Run("creates step", func(t *testing.T) {
		step := CanaryStep{
			Weight:         25,
			Duration:       10 * time.Minute,
			ManualApproval: true,
		}
		assert.Equal(t, 25, step.Weight)
		assert.True(t, step.ManualApproval)
	})
}

func TestCanaryManager_List(t *testing.T) {
	manager := NewCanaryManager(nil)
	manager.RegisterTarget("mock", &MockCanaryTarget{})

	ctx := context.Background()
	_, _ = manager.Deploy(ctx, &CanaryConfig{
		Name: "list-app-1", Environment: "prod", Target: "mock", Version: "v1",
		Steps: []CanaryStep{{Weight: 100}},
	})
	_, _ = manager.Deploy(ctx, &CanaryConfig{
		Name: "list-app-2", Environment: "prod", Target: "mock", Version: "v1",
		Steps: []CanaryStep{{Weight: 100}},
	})

	t.Run("lists canaries", func(t *testing.T) {
		canaries := manager.List()
		assert.Len(t, canaries, 2)
	})
}

func TestCanaryManager_Get(t *testing.T) {
	manager := NewCanaryManager(nil)
	manager.RegisterTarget("mock", &MockCanaryTarget{})

	ctx := context.Background()
	deploy, _ := manager.Deploy(ctx, &CanaryConfig{
		Name: "get-app", Environment: "prod", Target: "mock", Version: "v1",
		Steps: []CanaryStep{{Weight: 100}},
	})

	t.Run("gets canary by ID", func(t *testing.T) {
		found := manager.Get(deploy.ID())
		assert.NotNil(t, found)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		found := manager.Get("unknown")
		assert.Nil(t, found)
	})
}

// MockCanaryTarget for testing
type MockCanaryTarget struct {
	errorRate  float64
	latencyP99 time.Duration
}

func (m *MockCanaryTarget) SetWeight(ctx context.Context, name, env string, weight int) error {
	return nil
}

func (m *MockCanaryTarget) GetMetrics(ctx context.Context, name, env string) (*CanaryMetrics, error) {
	return &CanaryMetrics{
		ErrorRate:    m.errorRate,
		LatencyP99:   m.latencyP99,
		RequestCount: 1000,
	}, nil
}

func (m *MockCanaryTarget) Rollback(ctx context.Context, name, env string) error {
	return nil
}

func (m *MockCanaryTarget) Promote(ctx context.Context, name, env string) error {
	return nil
}
