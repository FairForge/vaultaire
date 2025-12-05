// internal/devops/rollback_test.go
package devops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRollbackConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &RollbackConfig{
			Name:        "web-app",
			Environment: "production",
			Strategy:    RollbackStrategyImmediate,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &RollbackConfig{Environment: "prod"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewRollbackManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewRollbackManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestRollbackManager_RecordRelease(t *testing.T) {
	manager := NewRollbackManager(nil)

	t.Run("records release", func(t *testing.T) {
		err := manager.RecordRelease(&ReleaseRecord{
			Name:        "app",
			Environment: "production",
			Version:     "v1.0.0",
			Artifact:    "app:v1.0.0",
			Timestamp:   time.Now(),
		})
		assert.NoError(t, err)
	})
}

func TestRollbackManager_Rollback(t *testing.T) {
	manager := NewRollbackManager(nil)
	manager.RegisterExecutor("mock", &MockRollbackExecutor{})

	// Record releases
	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "rollback-app", Environment: "production",
		Version: "v1.0.0", Artifact: "app:v1.0.0", Executor: "mock",
		Timestamp: time.Now().Add(-time.Hour),
	})
	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "rollback-app", Environment: "production",
		Version: "v2.0.0", Artifact: "app:v2.0.0", Executor: "mock",
		Timestamp: time.Now(),
	})

	ctx := context.Background()

	t.Run("rolls back to previous version", func(t *testing.T) {
		result, err := manager.Rollback(ctx, &RollbackConfig{
			Name:        "rollback-app",
			Environment: "production",
			Strategy:    RollbackStrategyImmediate,
		})

		require.NoError(t, err)
		assert.Equal(t, "v1.0.0", result.TargetVersion)
		assert.Equal(t, RollbackStatusSuccess, result.Status)
	})
}

func TestRollbackManager_RollbackToVersion(t *testing.T) {
	manager := NewRollbackManager(nil)
	manager.RegisterExecutor("mock", &MockRollbackExecutor{})

	// Record multiple releases
	for i, v := range []string{"v1.0.0", "v1.1.0", "v2.0.0", "v2.1.0"} {
		_ = manager.RecordRelease(&ReleaseRecord{
			Name: "version-app", Environment: "production",
			Version: v, Artifact: "app:" + v, Executor: "mock",
			Timestamp: time.Now().Add(time.Duration(i) * time.Hour),
		})
	}

	ctx := context.Background()

	t.Run("rolls back to specific version", func(t *testing.T) {
		result, err := manager.RollbackToVersion(ctx, &RollbackConfig{
			Name:          "version-app",
			Environment:   "production",
			TargetVersion: "v1.1.0",
			Strategy:      RollbackStrategyImmediate,
		})

		require.NoError(t, err)
		assert.Equal(t, "v1.1.0", result.TargetVersion)
	})
}

func TestRollbackManager_GradualRollback(t *testing.T) {
	manager := NewRollbackManager(nil)
	manager.RegisterExecutor("mock", &MockRollbackExecutor{})

	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "gradual-app", Environment: "production",
		Version: "v1.0.0", Artifact: "app:v1.0.0", Executor: "mock",
	})
	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "gradual-app", Environment: "production",
		Version: "v2.0.0", Artifact: "app:v2.0.0", Executor: "mock",
	})

	ctx := context.Background()

	t.Run("performs gradual rollback", func(t *testing.T) {
		result, err := manager.Rollback(ctx, &RollbackConfig{
			Name:        "gradual-app",
			Environment: "production",
			Strategy:    RollbackStrategyGradual,
			Steps:       []int{25, 50, 75, 100},
			StepDelay:   10 * time.Millisecond,
		})

		require.NoError(t, err)
		assert.Equal(t, RollbackStatusSuccess, result.Status)
	})
}

func TestRollbackManager_AutoRollback(t *testing.T) {
	manager := NewRollbackManager(nil)
	manager.RegisterExecutor("mock", &MockRollbackExecutor{})

	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "auto-app", Environment: "production",
		Version: "v1.0.0", Artifact: "app:v1.0.0", Executor: "mock",
	})

	t.Run("configures auto-rollback triggers", func(t *testing.T) {
		err := manager.ConfigureAutoRollback("auto-app", "production", &AutoRollbackConfig{
			Enabled:    true,
			ErrorRate:  0.05,
			LatencyP99: 500 * time.Millisecond,
			Window:     5 * time.Minute,
		})
		assert.NoError(t, err)
	})
}

func TestRollbackManager_GetHistory(t *testing.T) {
	manager := NewRollbackManager(nil)
	manager.RegisterExecutor("mock", &MockRollbackExecutor{})

	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "history-app", Environment: "production",
		Version: "v1.0.0", Executor: "mock",
	})
	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "history-app", Environment: "production",
		Version: "v2.0.0", Executor: "mock",
	})

	ctx := context.Background()
	_, _ = manager.Rollback(ctx, &RollbackConfig{
		Name: "history-app", Environment: "production",
		Strategy: RollbackStrategyImmediate,
	})

	t.Run("returns rollback history", func(t *testing.T) {
		history := manager.GetRollbackHistory("history-app", "production")
		assert.Len(t, history, 1)
	})
}

func TestRollbackManager_GetReleases(t *testing.T) {
	manager := NewRollbackManager(nil)

	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "releases-app", Environment: "production", Version: "v1.0.0",
	})
	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "releases-app", Environment: "production", Version: "v2.0.0",
	})

	t.Run("returns release history", func(t *testing.T) {
		releases := manager.GetReleases("releases-app", "production")
		assert.Len(t, releases, 2)
	})
}

func TestRollbackManager_Validate(t *testing.T) {
	manager := NewRollbackManager(nil)
	executor := &MockRollbackExecutor{validateResult: true}
	manager.RegisterExecutor("mock", executor)

	_ = manager.RecordRelease(&ReleaseRecord{
		Name: "validate-app", Environment: "production",
		Version: "v1.0.0", Artifact: "app:v1.0.0", Executor: "mock",
	})

	ctx := context.Background()

	t.Run("validates rollback target", func(t *testing.T) {
		valid, err := manager.ValidateRollback(ctx, "validate-app", "production", "v1.0.0")
		require.NoError(t, err)
		assert.True(t, valid)
	})
}

func TestRollbackStrategies(t *testing.T) {
	t.Run("defines strategies", func(t *testing.T) {
		assert.Equal(t, "immediate", RollbackStrategyImmediate)
		assert.Equal(t, "gradual", RollbackStrategyGradual)
		assert.Equal(t, "blue-green", RollbackStrategyBlueGreen)
	})
}

func TestRollbackStatuses(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "pending", RollbackStatusPending)
		assert.Equal(t, "running", RollbackStatusRunning)
		assert.Equal(t, "success", RollbackStatusSuccess)
		assert.Equal(t, "failed", RollbackStatusFailed)
	})
}

func TestReleaseRecord(t *testing.T) {
	t.Run("creates record", func(t *testing.T) {
		record := &ReleaseRecord{
			Name:        "app",
			Environment: "production",
			Version:     "v1.0.0",
			Artifact:    "app:v1.0.0",
			Timestamp:   time.Now(),
			Metadata:    map[string]string{"commit": "abc123"},
		}
		assert.Equal(t, "v1.0.0", record.Version)
	})
}

func TestRollbackResult(t *testing.T) {
	t.Run("creates result", func(t *testing.T) {
		result := &RollbackResult{
			FromVersion:   "v2.0.0",
			TargetVersion: "v1.0.0",
			Status:        RollbackStatusSuccess,
			Duration:      5 * time.Second,
		}
		assert.Equal(t, RollbackStatusSuccess, result.Status)
	})
}

// MockRollbackExecutor for testing
type MockRollbackExecutor struct {
	validateResult bool
}

func (m *MockRollbackExecutor) Deploy(ctx context.Context, artifact string) error {
	return nil
}

func (m *MockRollbackExecutor) SetWeight(ctx context.Context, name, env string, weight int) error {
	return nil
}

func (m *MockRollbackExecutor) Validate(ctx context.Context, artifact string) (bool, error) {
	return m.validateResult, nil
}

func (m *MockRollbackExecutor) GetCurrentVersion(ctx context.Context, name, env string) (string, error) {
	return "v2.0.0", nil
}
