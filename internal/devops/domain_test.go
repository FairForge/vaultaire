// internal/devops/domain_test.go
package devops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDomainManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewDomainManager(nil)
		assert.NotNil(t, manager)
		assert.Equal(t, "localhost", manager.config.PrimaryDomain)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &DomainConfig{
			PrimaryDomain: "example.com",
		}
		manager := NewDomainManager(config)
		assert.Equal(t, "example.com", manager.config.PrimaryDomain)
	})
}

func TestDomainManager_AddDomain(t *testing.T) {
	manager := NewDomainManager(nil)

	t.Run("adds valid domain", func(t *testing.T) {
		err := manager.AddDomain(&Domain{
			Name: "test.example.com",
			Type: DomainTypePrimary,
		})
		assert.NoError(t, err)
	})

	t.Run("rejects nil domain", func(t *testing.T) {
		err := manager.AddDomain(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.AddDomain(&Domain{Name: ""})
		assert.Error(t, err)
	})

	t.Run("rejects invalid domain", func(t *testing.T) {
		err := manager.AddDomain(&Domain{Name: "not a valid domain!"})
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.AddDomain(&Domain{Name: "dup.example.com"})
		err := manager.AddDomain(&Domain{Name: "dup.example.com"})
		assert.Error(t, err)
	})

	t.Run("sets default status", func(t *testing.T) {
		_ = manager.AddDomain(&Domain{Name: "status.example.com"})
		domain := manager.GetDomain("status.example.com")
		assert.Equal(t, DomainStatusPending, domain.Status)
	})
}

func TestDomainManager_GetDomain(t *testing.T) {
	manager := NewDomainManager(nil)
	_ = manager.AddDomain(&Domain{Name: "get.example.com"})

	t.Run("returns existing domain", func(t *testing.T) {
		domain := manager.GetDomain("get.example.com")
		assert.NotNil(t, domain)
		assert.Equal(t, "get.example.com", domain.Name)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		domain := manager.GetDomain("unknown.example.com")
		assert.Nil(t, domain)
	})
}

func TestDomainManager_ListDomains(t *testing.T) {
	manager := NewDomainManager(nil)
	_ = manager.AddDomain(&Domain{Name: "list1.example.com"})
	_ = manager.AddDomain(&Domain{Name: "list2.example.com"})

	domains := manager.ListDomains()
	assert.Len(t, domains, 2)
}

func TestDomainManager_RemoveDomain(t *testing.T) {
	manager := NewDomainManager(nil)
	_ = manager.AddDomain(&Domain{Name: "remove.example.com"})

	t.Run("removes existing domain", func(t *testing.T) {
		err := manager.RemoveDomain("remove.example.com")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetDomain("remove.example.com"))
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := manager.RemoveDomain("unknown.example.com")
		assert.Error(t, err)
	})
}

func TestDomainManager_SetupProductionDomains(t *testing.T) {
	config := DefaultDomainConfigs[EnvTypeProduction]
	manager := NewDomainManager(config)

	err := manager.SetupProductionDomains("1.2.3.4")
	require.NoError(t, err)

	t.Run("creates primary domain", func(t *testing.T) {
		domain := manager.GetDomain("stored.ge")
		require.NotNil(t, domain)
		assert.Equal(t, DomainTypePrimary, domain.Type)
		assert.Equal(t, "1.2.3.4", domain.TargetIP)
	})

	t.Run("creates www domain", func(t *testing.T) {
		domain := manager.GetDomain("www.stored.ge")
		require.NotNil(t, domain)
	})

	t.Run("creates api domain", func(t *testing.T) {
		domain := manager.GetDomain("api.stored.ge")
		require.NotNil(t, domain)
		assert.Equal(t, DomainTypeAPI, domain.Type)
	})

	t.Run("creates dashboard domain", func(t *testing.T) {
		domain := manager.GetDomain("dashboard.stored.ge")
		require.NotNil(t, domain)
		assert.Equal(t, DomainTypeDashboard, domain.Type)
	})

	t.Run("creates cdn domain", func(t *testing.T) {
		domain := manager.GetDomain("cdn.stored.ge")
		require.NotNil(t, domain)
		assert.Equal(t, DomainTypeCDN, domain.Type)
	})
}

func TestDomainManager_IsAllowedOrigin(t *testing.T) {
	t.Run("development allows all", func(t *testing.T) {
		manager := NewDomainManager(DefaultDomainConfigs[EnvTypeDevelopment])
		assert.True(t, manager.IsAllowedOrigin("http://localhost:3000"))
		assert.True(t, manager.IsAllowedOrigin("https://anything.com"))
	})

	t.Run("production restricts origins", func(t *testing.T) {
		manager := NewDomainManager(DefaultDomainConfigs[EnvTypeProduction])
		assert.True(t, manager.IsAllowedOrigin("https://stored.ge"))
		assert.True(t, manager.IsAllowedOrigin("https://dashboard.stored.ge"))
		assert.False(t, manager.IsAllowedOrigin("https://evil.com"))
	})
}

func TestDomainManager_GetCORSHeaders(t *testing.T) {
	manager := NewDomainManager(DefaultDomainConfigs[EnvTypeProduction])

	t.Run("returns headers for allowed origin", func(t *testing.T) {
		headers := manager.GetCORSHeaders("https://stored.ge")
		assert.Equal(t, "https://stored.ge", headers["Access-Control-Allow-Origin"])
		assert.Contains(t, headers["Access-Control-Allow-Methods"], "GET")
	})

	t.Run("returns empty for disallowed origin", func(t *testing.T) {
		headers := manager.GetCORSHeaders("https://evil.com")
		assert.Empty(t, headers)
	})
}

func TestDomainManager_GetSecurityHeaders(t *testing.T) {
	t.Run("returns security headers", func(t *testing.T) {
		manager := NewDomainManager(DefaultDomainConfigs[EnvTypeProduction])
		headers := manager.GetSecurityHeaders()

		assert.Equal(t, "nosniff", headers["X-Content-Type-Options"])
		assert.Equal(t, "DENY", headers["X-Frame-Options"])
		assert.Contains(t, headers["Strict-Transport-Security"], "max-age=31536000")
	})

	t.Run("skips HSTS when disabled", func(t *testing.T) {
		manager := NewDomainManager(DefaultDomainConfigs[EnvTypeDevelopment])
		headers := manager.GetSecurityHeaders()

		_, hasHSTS := headers["Strict-Transport-Security"]
		assert.False(t, hasHSTS)
	})
}

func TestIsValidDomain(t *testing.T) {
	tests := []struct {
		domain string
		valid  bool
	}{
		{"example.com", true},
		{"sub.example.com", true},
		{"deep.sub.example.com", true},
		{"localhost", true},
		{"example.co.uk", true},
		{"", false},
		{"not valid", false},
		{"example", false},
		{"-example.com", false},
		{"example-.com", false},
		{"exam ple.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			assert.Equal(t, tt.valid, IsValidDomain(tt.domain))
		})
	}
}

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Example.Com", "example.com"},
		{"  example.com  ", "example.com"},
		{"http://example.com", "example.com"},
		{"https://example.com", "example.com"},
		{"example.com/", "example.com"},
		{"https://example.com/path", "example.com/path"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, NormalizeDomain(tt.input))
		})
	}
}

func TestGetDomainConfigForEnvironment(t *testing.T) {
	t.Run("returns production config", func(t *testing.T) {
		config := GetDomainConfigForEnvironment(EnvTypeProduction)
		assert.Equal(t, "stored.ge", config.PrimaryDomain)
	})

	t.Run("returns staging config", func(t *testing.T) {
		config := GetDomainConfigForEnvironment(EnvTypeStaging)
		assert.Equal(t, "staging.stored.ge", config.PrimaryDomain)
	})

	t.Run("returns development for unknown", func(t *testing.T) {
		config := GetDomainConfigForEnvironment("unknown")
		assert.Equal(t, "localhost", config.PrimaryDomain)
	})
}

func TestDefaultDomainConfigs(t *testing.T) {
	t.Run("production has HTTPS forced", func(t *testing.T) {
		config := DefaultDomainConfigs[EnvTypeProduction]
		assert.True(t, config.ForceHTTPS)
		assert.True(t, config.HSTSEnabled)
	})

	t.Run("development allows all origins", func(t *testing.T) {
		config := DefaultDomainConfigs[EnvTypeDevelopment]
		assert.Contains(t, config.AllowedOrigins, "*")
		assert.False(t, config.ForceHTTPS)
	})
}
