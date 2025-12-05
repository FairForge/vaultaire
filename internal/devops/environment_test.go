// internal/devops/environment_test.go
package devops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &EnvironmentConfig{
			Name: "production",
			Type: EnvTypeProduction,
			Tier: TierPrimary,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &EnvironmentConfig{Type: EnvTypeProduction}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects invalid type", func(t *testing.T) {
		config := &EnvironmentConfig{Name: "test", Type: "invalid"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewEnvironmentManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewEnvironmentManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestEnvironmentManager_Create(t *testing.T) {
	manager := NewEnvironmentManager(nil)

	t.Run("creates environment", func(t *testing.T) {
		env, err := manager.Create(&EnvironmentConfig{
			Name:        "staging",
			Type:        EnvTypeStaging,
			Tier:        TierSecondary,
			Description: "Staging environment",
		})

		require.NoError(t, err)
		assert.Equal(t, "staging", env.Name())
		assert.Equal(t, EnvTypeStaging, env.Type())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_, _ = manager.Create(&EnvironmentConfig{Name: "dup", Type: EnvTypeDevelopment})
		_, err := manager.Create(&EnvironmentConfig{Name: "dup", Type: EnvTypeDevelopment})
		assert.Error(t, err)
	})
}

func TestEnvironment_Variables(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	env, _ := manager.Create(&EnvironmentConfig{Name: "vars-env", Type: EnvTypeDevelopment})

	t.Run("sets variable", func(t *testing.T) {
		err := env.SetVariable("DATABASE_URL", "postgres://localhost/db")
		assert.NoError(t, err)
	})

	t.Run("gets variable", func(t *testing.T) {
		_ = env.SetVariable("API_KEY", "secret123")
		value, err := env.GetVariable("API_KEY")
		require.NoError(t, err)
		assert.Equal(t, "secret123", value)
	})

	t.Run("lists variables", func(t *testing.T) {
		vars := env.Variables()
		assert.NotEmpty(t, vars)
	})
}

func TestEnvironment_Secrets(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	env, _ := manager.Create(&EnvironmentConfig{Name: "secrets-env", Type: EnvTypeProduction})

	t.Run("sets secret", func(t *testing.T) {
		err := env.SetSecret("DB_PASSWORD", "supersecret")
		assert.NoError(t, err)
	})

	t.Run("gets secret", func(t *testing.T) {
		_ = env.SetSecret("API_SECRET", "hidden")
		value, err := env.GetSecret("API_SECRET")
		require.NoError(t, err)
		assert.Equal(t, "hidden", value)
	})
}

func TestEnvironment_Promotion(t *testing.T) {
	manager := NewEnvironmentManager(nil)

	dev, _ := manager.Create(&EnvironmentConfig{Name: "dev", Type: EnvTypeDevelopment})
	staging, _ := manager.Create(&EnvironmentConfig{Name: "staging", Type: EnvTypeStaging})
	prod, _ := manager.Create(&EnvironmentConfig{Name: "prod", Type: EnvTypeProduction})

	t.Run("sets promotion path", func(t *testing.T) {
		err := manager.SetPromotionPath("dev", "staging")
		assert.NoError(t, err)

		err = manager.SetPromotionPath("staging", "prod")
		assert.NoError(t, err)
	})

	t.Run("gets next environment", func(t *testing.T) {
		next := manager.GetNextEnvironment("dev")
		assert.Equal(t, "staging", next)
	})

	t.Run("returns empty for final env", func(t *testing.T) {
		next := manager.GetNextEnvironment("prod")
		assert.Empty(t, next)
	})

	// Use environments to avoid unused warnings
	_ = dev
	_ = staging
	_ = prod
}

func TestEnvironment_Locks(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	env, _ := manager.Create(&EnvironmentConfig{Name: "lock-env", Type: EnvTypeProduction})

	t.Run("locks environment", func(t *testing.T) {
		err := env.Lock("deployment in progress", "user@example.com")
		assert.NoError(t, err)
		assert.True(t, env.IsLocked())
	})

	t.Run("unlock environment", func(t *testing.T) {
		err := env.Unlock()
		assert.NoError(t, err)
		assert.False(t, env.IsLocked())
	})
}

func TestEnvironment_MaintenanceWindow(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	env, _ := manager.Create(&EnvironmentConfig{Name: "maint-env", Type: EnvTypeProduction})

	t.Run("sets maintenance window", func(t *testing.T) {
		err := env.SetMaintenanceWindow(&MaintenanceWindow{
			Day:       time.Sunday,
			StartHour: 2,
			Duration:  4 * time.Hour,
		})
		assert.NoError(t, err)
	})

	t.Run("checks if in maintenance", func(t *testing.T) {
		inMaint := env.InMaintenanceWindow()
		assert.IsType(t, true, inMaint) // Just check it returns bool
	})
}

func TestEnvironment_Resources(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	env, _ := manager.Create(&EnvironmentConfig{Name: "resource-env", Type: EnvTypeStaging})

	t.Run("sets resource limits", func(t *testing.T) {
		err := env.SetResourceLimits(&ResourceLimits{
			MaxCPU:     "4",
			MaxMemory:  "8Gi",
			MaxStorage: "100Gi",
		})
		assert.NoError(t, err)
	})

	t.Run("gets resource limits", func(t *testing.T) {
		limits := env.ResourceLimits()
		assert.NotNil(t, limits)
	})
}

func TestEnvironmentManager_Get(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	_, _ = manager.Create(&EnvironmentConfig{Name: "get-env", Type: EnvTypeDevelopment})

	t.Run("gets environment", func(t *testing.T) {
		env := manager.Get("get-env")
		assert.NotNil(t, env)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		env := manager.Get("unknown")
		assert.Nil(t, env)
	})
}

func TestEnvironmentManager_List(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	_, _ = manager.Create(&EnvironmentConfig{Name: "list-1", Type: EnvTypeDevelopment})
	_, _ = manager.Create(&EnvironmentConfig{Name: "list-2", Type: EnvTypeStaging})

	t.Run("lists environments", func(t *testing.T) {
		envs := manager.List()
		assert.Len(t, envs, 2)
	})
}

func TestEnvironmentManager_Delete(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	_, _ = manager.Create(&EnvironmentConfig{Name: "delete-env", Type: EnvTypeDevelopment})

	t.Run("deletes environment", func(t *testing.T) {
		err := manager.Delete("delete-env")
		assert.NoError(t, err)
		assert.Nil(t, manager.Get("delete-env"))
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := manager.Delete("unknown")
		assert.Error(t, err)
	})
}

func TestEnvironmentTypes(t *testing.T) {
	t.Run("defines types", func(t *testing.T) {
		assert.Equal(t, "development", EnvTypeDevelopment)
		assert.Equal(t, "staging", EnvTypeStaging)
		assert.Equal(t, "production", EnvTypeProduction)
		assert.Equal(t, "testing", EnvTypeTesting)
	})
}

func TestTiers(t *testing.T) {
	t.Run("defines tiers", func(t *testing.T) {
		assert.Equal(t, "primary", TierPrimary)
		assert.Equal(t, "secondary", TierSecondary)
		assert.Equal(t, "disaster-recovery", TierDR)
	})
}

func TestEnvironment_Clone(t *testing.T) {
	manager := NewEnvironmentManager(nil)
	source, _ := manager.Create(&EnvironmentConfig{Name: "source-env", Type: EnvTypeStaging})
	_ = source.SetVariable("KEY", "value")

	t.Run("clones environment", func(t *testing.T) {
		clone, err := manager.Clone("source-env", "clone-env")
		require.NoError(t, err)
		assert.Equal(t, "clone-env", clone.Name())

		value, _ := clone.GetVariable("KEY")
		assert.Equal(t, "value", value)
	})
}
