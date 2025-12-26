// internal/devops/cdn_test.go
package devops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCDNManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewCDNManager(nil)
		assert.NotNil(t, manager)
		assert.Equal(t, CDNProviderNone, manager.config.Provider)
		assert.False(t, manager.config.Enabled)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &CDNConfig{
			Provider: CDNProviderCloudflare,
			Enabled:  true,
		}
		manager := NewCDNManager(config)
		assert.Equal(t, CDNProviderCloudflare, manager.config.Provider)
		assert.True(t, manager.config.Enabled)
	})
}

func TestCDNManager_Origins(t *testing.T) {
	manager := NewCDNManager(nil)

	t.Run("adds origin", func(t *testing.T) {
		err := manager.AddOrigin(&Origin{
			Name:    "primary",
			Address: "1.2.3.4",
		})
		assert.NoError(t, err)
	})

	t.Run("sets defaults", func(t *testing.T) {
		origin := manager.GetOrigin("primary")
		require.NotNil(t, origin)
		assert.Equal(t, 443, origin.Port)
		assert.Equal(t, 100, origin.Weight)
		assert.True(t, origin.Healthy)
	})

	t.Run("rejects nil origin", func(t *testing.T) {
		err := manager.AddOrigin(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.AddOrigin(&Origin{Address: "1.2.3.4"})
		assert.Error(t, err)
	})

	t.Run("rejects empty address", func(t *testing.T) {
		err := manager.AddOrigin(&Origin{Name: "test"})
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.AddOrigin(&Origin{Name: "dup", Address: "1.1.1.1"})
		err := manager.AddOrigin(&Origin{Name: "dup", Address: "2.2.2.2"})
		assert.Error(t, err)
	})

	t.Run("lists origins", func(t *testing.T) {
		origins := manager.ListOrigins()
		assert.GreaterOrEqual(t, len(origins), 1)
	})

	t.Run("removes origin", func(t *testing.T) {
		_ = manager.AddOrigin(&Origin{Name: "removeme", Address: "9.9.9.9"})
		err := manager.RemoveOrigin("removeme")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetOrigin("removeme"))
	})

	t.Run("errors removing unknown", func(t *testing.T) {
		err := manager.RemoveOrigin("unknown")
		assert.Error(t, err)
	})
}

func TestCDNManager_OriginHealth(t *testing.T) {
	manager := NewCDNManager(nil)
	_ = manager.AddOrigin(&Origin{Name: "health-test", Address: "1.2.3.4"})

	t.Run("sets origin unhealthy", func(t *testing.T) {
		err := manager.SetOriginHealth("health-test", false)
		assert.NoError(t, err)

		origin := manager.GetOrigin("health-test")
		assert.False(t, origin.Healthy)
	})

	t.Run("gets healthy origins only", func(t *testing.T) {
		_ = manager.AddOrigin(&Origin{Name: "healthy-one", Address: "2.2.2.2"})
		healthy := manager.GetHealthyOrigins()

		for _, o := range healthy {
			assert.True(t, o.Healthy)
		}
	})

	t.Run("errors for unknown origin", func(t *testing.T) {
		err := manager.SetOriginHealth("unknown", true)
		assert.Error(t, err)
	})
}

func TestCDNManager_CacheRules(t *testing.T) {
	manager := NewCDNManager(nil)

	t.Run("adds cache rule", func(t *testing.T) {
		err := manager.AddCacheRule(&CacheRule{
			Name:        "static",
			PathPattern: "/static/*",
			TTL:         24 * time.Hour,
		})
		assert.NoError(t, err)
	})

	t.Run("sets defaults", func(t *testing.T) {
		rule := manager.GetCacheRule("static")
		require.NotNil(t, rule)
		assert.Equal(t, CacheLevelStandard, rule.CacheLevel)
		assert.True(t, rule.Enabled)
	})

	t.Run("rejects nil rule", func(t *testing.T) {
		err := manager.AddCacheRule(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.AddCacheRule(&CacheRule{PathPattern: "/*"})
		assert.Error(t, err)
	})

	t.Run("rejects empty pattern", func(t *testing.T) {
		err := manager.AddCacheRule(&CacheRule{Name: "test"})
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.AddCacheRule(&CacheRule{Name: "dup", PathPattern: "/*"})
		err := manager.AddCacheRule(&CacheRule{Name: "dup", PathPattern: "/other/*"})
		assert.Error(t, err)
	})

	t.Run("lists rules", func(t *testing.T) {
		rules := manager.ListCacheRules()
		assert.GreaterOrEqual(t, len(rules), 1)
	})

	t.Run("removes rule", func(t *testing.T) {
		_ = manager.AddCacheRule(&CacheRule{Name: "removeme", PathPattern: "/*"})
		err := manager.RemoveCacheRule("removeme")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetCacheRule("removeme"))
	})
}

func TestCDNManager_GetCacheHeaders(t *testing.T) {
	t.Run("disabled CDN returns no-store", func(t *testing.T) {
		manager := NewCDNManager(&CDNConfig{Enabled: false})
		headers := manager.GetCacheHeaders("/anything")
		assert.Equal(t, "no-store", headers["Cache-Control"])
	})

	t.Run("bypass level returns no-store", func(t *testing.T) {
		manager := NewCDNManager(&CDNConfig{
			Enabled:    true,
			CacheLevel: CacheLevelBypass,
		})
		headers := manager.GetCacheHeaders("/anything")
		assert.Equal(t, "no-store", headers["Cache-Control"])
	})

	t.Run("standard level returns max-age", func(t *testing.T) {
		manager := NewCDNManager(&CDNConfig{
			Enabled:    true,
			CacheLevel: CacheLevelStandard,
			DefaultTTL: 1 * time.Hour,
		})
		headers := manager.GetCacheHeaders("/anything")
		assert.Contains(t, headers["Cache-Control"], "max-age=3600")
	})

	t.Run("aggressive level includes stale-while-revalidate", func(t *testing.T) {
		manager := NewCDNManager(&CDNConfig{
			Enabled:    true,
			CacheLevel: CacheLevelAggressive,
			DefaultTTL: 1 * time.Hour,
		})
		headers := manager.GetCacheHeaders("/anything")
		assert.Contains(t, headers["Cache-Control"], "stale-while-revalidate")
	})

	t.Run("everything level includes immutable", func(t *testing.T) {
		manager := NewCDNManager(&CDNConfig{
			Enabled:    true,
			CacheLevel: CacheLevelEverything,
			MaxTTL:     7 * 24 * time.Hour,
		})
		headers := manager.GetCacheHeaders("/anything")
		assert.Contains(t, headers["Cache-Control"], "immutable")
	})

	t.Run("matches cache rules", func(t *testing.T) {
		manager := NewCDNManager(&CDNConfig{
			Enabled:    true,
			CacheLevel: CacheLevelStandard,
			DefaultTTL: 1 * time.Hour,
		})
		_ = manager.AddCacheRule(&CacheRule{
			Name:        "api",
			PathPattern: "/api/*",
			CacheLevel:  CacheLevelBypass,
		})

		headers := manager.GetCacheHeaders("/api/users")
		assert.Equal(t, "no-store", headers["Cache-Control"])
	})
}

func TestCDNManager_SetupProductionCDN(t *testing.T) {
	manager := NewCDNManager(DefaultCDNConfigs[EnvTypeProduction])

	err := manager.SetupProductionCDN("1.2.3.4", "5.6.7.8")
	require.NoError(t, err)

	t.Run("creates both origins", func(t *testing.T) {
		origins := manager.ListOrigins()
		assert.Len(t, origins, 2)
	})

	t.Run("NYC is primary", func(t *testing.T) {
		nyc := manager.GetOrigin("nyc-hub")
		require.NotNil(t, nyc)
		assert.True(t, nyc.Primary)
		assert.Equal(t, "1.2.3.4", nyc.Address)
	})

	t.Run("LA is secondary", func(t *testing.T) {
		la := manager.GetOrigin("la-worker")
		require.NotNil(t, la)
		assert.False(t, la.Primary)
		assert.Equal(t, "5.6.7.8", la.Address)
	})

	t.Run("creates cache rules", func(t *testing.T) {
		rules := manager.ListCacheRules()
		assert.GreaterOrEqual(t, len(rules), 4)
	})

	t.Run("API has no-cache rule", func(t *testing.T) {
		rule := manager.GetCacheRule("api-no-cache")
		require.NotNil(t, rule)
		assert.Equal(t, CacheLevelBypass, rule.CacheLevel)
	})

	t.Run("static assets cached long-term", func(t *testing.T) {
		rule := manager.GetCacheRule("static-assets")
		require.NotNil(t, rule)
		assert.Equal(t, CacheLevelEverything, rule.CacheLevel)
	})
}

func TestCDNManager_GenerateCloudflareConfig(t *testing.T) {
	manager := NewCDNManager(DefaultCDNConfigs[EnvTypeProduction])
	config := manager.GenerateCloudflareConfig()

	assert.Equal(t, "full_strict", config["ssl"])
	assert.Equal(t, true, config["always_use_https"])
	assert.Equal(t, true, config["http2"])
	assert.Equal(t, true, config["http3"])
	assert.Equal(t, true, config["brotli"])

	minify := config["minify"].(map[string]bool)
	assert.True(t, minify["js"])
	assert.True(t, minify["css"])
}

func TestPathMatches(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		matches bool
	}{
		{"/api/users", "/api/*", true},
		{"/api/users/123", "/api/*", true},
		{"/static/style.css", "/api/*", false},
		{"/health", "/health", true},
		{"/healthcheck", "/health", false},
		{"/anything", "*", true},
		{"/static/js/app.js", "/static/*", true},
	}

	for _, tt := range tests {
		t.Run(tt.path+"_"+tt.pattern, func(t *testing.T) {
			assert.Equal(t, tt.matches, pathMatches(tt.path, tt.pattern))
		})
	}
}

func TestDefaultCDNConfigs(t *testing.T) {
	t.Run("production has aggressive caching", func(t *testing.T) {
		config := DefaultCDNConfigs[EnvTypeProduction]
		assert.Equal(t, CacheLevelAggressive, config.CacheLevel)
		assert.True(t, config.Brotli)
		assert.True(t, config.HTTP3)
	})

	t.Run("development disables CDN", func(t *testing.T) {
		config := DefaultCDNConfigs[EnvTypeDevelopment]
		assert.False(t, config.Enabled)
		assert.True(t, config.DevelopmentMode)
	})
}

func TestGetCDNConfigForEnvironment(t *testing.T) {
	t.Run("returns production config", func(t *testing.T) {
		config := GetCDNConfigForEnvironment(EnvTypeProduction)
		assert.True(t, config.Enabled)
	})

	t.Run("returns development for unknown", func(t *testing.T) {
		config := GetCDNConfigForEnvironment("unknown")
		assert.False(t, config.Enabled)
	})
}
