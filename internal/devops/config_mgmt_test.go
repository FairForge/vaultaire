// internal/devops/config_mgmt_test.go
package devops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigSpec_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		spec := &ConfigSpec{
			Name:    "app-config",
			Version: "1.0.0",
			Entries: map[string]ConfigEntry{
				"database.host": {Value: "localhost", Type: ConfigTypeString},
			},
		}
		err := spec.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		spec := &ConfigSpec{Version: "1.0.0"}
		err := spec.Validate()
		assert.Error(t, err)
	})
}

func TestNewConfigManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewConfigManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestConfigManager_Set(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("sets config value", func(t *testing.T) {
		err := manager.Set("database.host", "localhost")
		assert.NoError(t, err)

		value, err := manager.Get("database.host")
		require.NoError(t, err)
		assert.Equal(t, "localhost", value)
	})

	t.Run("overwrites existing value", func(t *testing.T) {
		_ = manager.Set("app.port", "8080")
		_ = manager.Set("app.port", "9090")

		value, _ := manager.Get("app.port")
		assert.Equal(t, "9090", value)
	})
}

func TestConfigManager_GetWithDefault(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("returns default for missing key", func(t *testing.T) {
		value := manager.GetWithDefault("missing.key", "default")
		assert.Equal(t, "default", value)
	})

	t.Run("returns actual value when exists", func(t *testing.T) {
		_ = manager.Set("exists.key", "actual")
		value := manager.GetWithDefault("exists.key", "default")
		assert.Equal(t, "actual", value)
	})
}

func TestConfigManager_Delete(t *testing.T) {
	manager := NewConfigManager(nil)
	_ = manager.Set("to.delete", "value")

	t.Run("deletes config", func(t *testing.T) {
		err := manager.Delete("to.delete")
		assert.NoError(t, err)

		_, err = manager.Get("to.delete")
		assert.Error(t, err)
	})
}

func TestConfigManager_Environments(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("manages environments", func(t *testing.T) {
		err := manager.CreateEnvironment("production")
		assert.NoError(t, err)

		err = manager.SetForEnv("production", "database.host", "prod-db.example.com")
		assert.NoError(t, err)

		value, err := manager.GetForEnv("production", "database.host")
		require.NoError(t, err)
		assert.Equal(t, "prod-db.example.com", value)
	})

	t.Run("inherits from default", func(t *testing.T) {
		_ = manager.Set("app.name", "myapp")
		_ = manager.CreateEnvironment("staging")

		value, err := manager.GetForEnv("staging", "app.name")
		require.NoError(t, err)
		assert.Equal(t, "myapp", value)
	})
}

func TestConfigManager_Secrets(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("stores secret", func(t *testing.T) {
		err := manager.SetSecret("database.password", "secret123")
		assert.NoError(t, err)
	})

	t.Run("retrieves secret", func(t *testing.T) {
		_ = manager.SetSecret("api.key", "abc123")
		value, err := manager.GetSecret("api.key")
		require.NoError(t, err)
		assert.Equal(t, "abc123", value)
	})

	t.Run("masks secret in export", func(t *testing.T) {
		_ = manager.SetSecret("masked.secret", "sensitive")
		export := manager.Export(false)
		assert.NotContains(t, export, "sensitive")
		assert.Contains(t, export, "***")
	})
}

func TestConfigManager_Versioning(t *testing.T) {
	manager := NewConfigManager(nil)
	_ = manager.Set("versioned.key", "v1")

	t.Run("tracks versions", func(t *testing.T) {
		_ = manager.Set("versioned.key", "v2")
		_ = manager.Set("versioned.key", "v3")

		history := manager.History("versioned.key")
		assert.Len(t, history, 3)
	})

	t.Run("rollback to version", func(t *testing.T) {
		history := manager.History("versioned.key")
		err := manager.Rollback("versioned.key", history[0].Version)
		assert.NoError(t, err)

		value, _ := manager.Get("versioned.key")
		assert.Equal(t, "v1", value)
	})
}

func TestConfigManager_Validation(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("validates with schema", func(t *testing.T) {
		schema := &ConfigSchema{
			"port": {Type: ConfigTypeInt, Min: 1, Max: 65535},
		}

		manager.SetSchema(schema)
		err := manager.Set("port", "8080")
		assert.NoError(t, err)
	})

	t.Run("rejects invalid value", func(t *testing.T) {
		schema := &ConfigSchema{
			"port": {Type: ConfigTypeInt, Min: 1, Max: 65535},
		}

		manager.SetSchema(schema)
		err := manager.Set("port", "invalid")
		assert.Error(t, err)
	})
}

func TestConfigManager_Watch(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("notifies on change", func(t *testing.T) {
		changed := make(chan string, 1)

		manager.Watch("watched.key", func(key, value string) {
			changed <- value
		})

		_ = manager.Set("watched.key", "new-value")

		select {
		case v := <-changed:
			assert.Equal(t, "new-value", v)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("watch callback not called")
		}
	})
}

func TestConfigManager_Import(t *testing.T) {
	manager := NewConfigManager(nil)

	t.Run("imports from JSON", func(t *testing.T) {
		jsonData := `{"app":{"name":"myapp","port":"8080"}}`
		err := manager.ImportJSON(jsonData)
		assert.NoError(t, err)

		value, _ := manager.Get("app.name")
		assert.Equal(t, "myapp", value)
	})

	t.Run("imports from YAML", func(t *testing.T) {
		yamlData := "app:\n  name: myapp\n  port: 8080"
		err := manager.ImportYAML(yamlData)
		assert.NoError(t, err)
	})

	t.Run("imports from env", func(t *testing.T) {
		t.Setenv("APP_DATABASE_HOST", "localhost")
		err := manager.ImportEnv("APP_")
		assert.NoError(t, err)

		value, _ := manager.Get("database.host")
		assert.Equal(t, "localhost", value)
	})
}

func TestConfigManager_Export(t *testing.T) {
	manager := NewConfigManager(nil)
	_ = manager.Set("export.key", "value")

	t.Run("exports as string", func(t *testing.T) {
		export := manager.Export(true)
		assert.Contains(t, export, "export.key")
	})

	t.Run("exports as JSON", func(t *testing.T) {
		json, err := manager.ExportJSON()
		require.NoError(t, err)
		assert.Contains(t, json, "export")
	})

	t.Run("exports as YAML", func(t *testing.T) {
		yaml, err := manager.ExportYAML()
		require.NoError(t, err)
		assert.Contains(t, yaml, "export")
	})
}

func TestConfigTypes(t *testing.T) {
	t.Run("defines types", func(t *testing.T) {
		assert.Equal(t, "string", ConfigTypeString)
		assert.Equal(t, "int", ConfigTypeInt)
		assert.Equal(t, "bool", ConfigTypeBool)
		assert.Equal(t, "float", ConfigTypeFloat)
		assert.Equal(t, "duration", ConfigTypeDuration)
	})
}

func TestConfigEntry(t *testing.T) {
	t.Run("creates entry", func(t *testing.T) {
		entry := ConfigEntry{
			Value:       "localhost",
			Type:        ConfigTypeString,
			Description: "Database host",
			Secret:      false,
		}
		assert.Equal(t, "localhost", entry.Value)
	})
}

func TestConfigManager_Keys(t *testing.T) {
	manager := NewConfigManager(nil)
	_ = manager.Set("key1", "value1")
	_ = manager.Set("key2", "value2")

	t.Run("lists all keys", func(t *testing.T) {
		keys := manager.Keys()
		assert.Len(t, keys, 2)
	})
}

func TestConfigManager_Clear(t *testing.T) {
	manager := NewConfigManager(nil)
	_ = manager.Set("key1", "value1")

	t.Run("clears all config", func(t *testing.T) {
		manager.Clear()
		keys := manager.Keys()
		assert.Empty(t, keys)
	})
}
