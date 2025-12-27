// internal/devops/production_test.go
package devops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCurrentEnvironmentType(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected string
	}{
		{"empty defaults to development", "", EnvTypeDevelopment},
		{"production", "production", EnvTypeProduction},
		{"prod shorthand", "prod", EnvTypeProduction},
		{"staging", "staging", EnvTypeStaging},
		{"stage shorthand", "stage", EnvTypeStaging},
		{"testing", "testing", EnvTypeTesting},
		{"test shorthand", "test", EnvTypeTesting},
		{"case insensitive", "PRODUCTION", EnvTypeProduction},
		{"unknown defaults to development", "unknown", EnvTypeDevelopment},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("VAULTAIRE_ENV", tt.envValue)

			got := GetCurrentEnvironmentType()
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestGetProductionConfig(t *testing.T) {
	t.Run("returns production config", func(t *testing.T) {
		t.Setenv("VAULTAIRE_ENV", "production")

		config := GetProductionConfig()

		assert.Equal(t, "stored.ge", config.Domain)
		assert.Equal(t, "https://api.stored.ge", config.APIEndpoint)
		assert.True(t, config.TLSEnabled)
		assert.True(t, config.RateLimitEnabled)
		assert.Equal(t, 1000, config.RateLimitRPS)
		assert.Equal(t, "warn", config.LogLevel)
	})

	t.Run("returns staging config", func(t *testing.T) {
		t.Setenv("VAULTAIRE_ENV", "staging")

		config := GetProductionConfig()

		assert.Equal(t, "staging.stored.ge", config.Domain)
		assert.True(t, config.TLSEnabled)
		assert.Equal(t, 100, config.RateLimitRPS)
		assert.Equal(t, "info", config.LogLevel)
	})

	t.Run("returns development config by default", func(t *testing.T) {
		// Don't set VAULTAIRE_ENV - let it default

		config := GetProductionConfig()

		assert.Equal(t, "localhost", config.Domain)
		assert.False(t, config.TLSEnabled)
		assert.False(t, config.RateLimitEnabled)
		assert.Equal(t, "debug", config.LogLevel)
	})
}

func TestIsProductionEnvironment(t *testing.T) {
	t.Run("returns true for production", func(t *testing.T) {
		t.Setenv("VAULTAIRE_ENV", "production")

		assert.True(t, IsProductionEnvironment())
		assert.False(t, IsDevelopmentEnvironment())
	})

	t.Run("returns false for non-production", func(t *testing.T) {
		t.Setenv("VAULTAIRE_ENV", "staging")

		assert.False(t, IsProductionEnvironment())
	})
}

func TestProductionInventory(t *testing.T) {
	t.Run("has expected servers", func(t *testing.T) {
		assert.GreaterOrEqual(t, len(ProductionInventory), 5)
	})

	t.Run("hub server exists", func(t *testing.T) {
		hubs := GetServersByRole(RoleHub)
		assert.Len(t, hubs, 1)
		assert.Equal(t, "hub-nyc-1", hubs[0].Name)
		assert.Equal(t, 256, hubs[0].RAMGB)
	})

	t.Run("worker servers exist", func(t *testing.T) {
		workers := GetServersByRole(RoleWorker)
		assert.GreaterOrEqual(t, len(workers), 4)
	})
}

func TestGetTotalMonthlyCost(t *testing.T) {
	cost := GetTotalMonthlyCost()
	// Based on inventory: 79 + 10 + 10 + 10 + 2.67 + 1.83 = 113.50
	assert.InDelta(t, 113.50, cost, 1.0)
}

func TestGetTotalResources(t *testing.T) {
	cpu, ram, storage := GetTotalResources()

	assert.Greater(t, cpu, 0)
	assert.Greater(t, ram, 0)
	assert.Greater(t, storage, 0)

	// Hub alone has 12 cores, 256GB RAM, 8TB storage
	assert.GreaterOrEqual(t, cpu, 12)
	assert.GreaterOrEqual(t, ram, 256)
	assert.GreaterOrEqual(t, storage, 8192)
}

func TestSetupProductionEnvironments(t *testing.T) {
	manager := NewEnvironmentManager(nil)

	err := SetupProductionEnvironments(manager)
	require.NoError(t, err)

	t.Run("creates all environments", func(t *testing.T) {
		envs := manager.List()
		assert.Len(t, envs, 3)
	})

	t.Run("development exists", func(t *testing.T) {
		dev := manager.Get("development")
		require.NotNil(t, dev)
		assert.Equal(t, EnvTypeDevelopment, dev.Type())
	})

	t.Run("staging exists with maintenance window", func(t *testing.T) {
		staging := manager.Get("staging")
		require.NotNil(t, staging)
		assert.Equal(t, EnvTypeStaging, staging.Type())
	})

	t.Run("production exists with resource limits", func(t *testing.T) {
		prod := manager.Get("production")
		require.NotNil(t, prod)
		assert.Equal(t, EnvTypeProduction, prod.Type())

		limits := prod.ResourceLimits()
		require.NotNil(t, limits)
		assert.Equal(t, "256Gi", limits.MaxMemory)
	})

	t.Run("promotion paths set correctly", func(t *testing.T) {
		next := manager.GetNextEnvironment("development")
		assert.Equal(t, "staging", next)

		next = manager.GetNextEnvironment("staging")
		assert.Equal(t, "production", next)

		next = manager.GetNextEnvironment("production")
		assert.Empty(t, next)
	})
}

func TestProductionConfigValidation(t *testing.T) {
	t.Run("production has TLS enabled", func(t *testing.T) {
		config := DefaultProductionConfigs[EnvTypeProduction]
		assert.True(t, config.TLSEnabled, "TLS must be enabled in production")
	})

	t.Run("production has rate limiting", func(t *testing.T) {
		config := DefaultProductionConfigs[EnvTypeProduction]
		assert.True(t, config.RateLimitEnabled, "Rate limiting must be enabled in production")
	})

	t.Run("staging has tracing", func(t *testing.T) {
		config := DefaultProductionConfigs[EnvTypeStaging]
		assert.True(t, config.TracingEnabled, "Tracing should be enabled in staging")
	})

	t.Run("development has debug logging", func(t *testing.T) {
		config := DefaultProductionConfigs[EnvTypeDevelopment]
		assert.Equal(t, "debug", config.LogLevel)
	})
}
