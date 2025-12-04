// internal/auth/ldap_test.go
package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLDAPConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &LDAPConfig{
			Host:         "ldap.example.com",
			Port:         389,
			BaseDN:       "dc=example,dc=com",
			BindDN:       "cn=admin,dc=example,dc=com",
			BindPassword: "secret",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty host", func(t *testing.T) {
		config := &LDAPConfig{
			Port:   389,
			BaseDN: "dc=example,dc=com",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "host")
	})

	t.Run("rejects empty BaseDN", func(t *testing.T) {
		config := &LDAPConfig{
			Host: "ldap.example.com",
			Port: 389,
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "base DN")
	})

	t.Run("defaults port to 389", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			BaseDN: "dc=example,dc=com",
		}
		config.ApplyDefaults()
		assert.Equal(t, 389, config.Port)
	})

	t.Run("defaults TLS port to 636", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			BaseDN: "dc=example,dc=com",
			UseTLS: true,
		}
		config.ApplyDefaults()
		assert.Equal(t, 636, config.Port)
	})
}

func TestNewLDAPProvider(t *testing.T) {
	t.Run("creates provider with valid config", func(t *testing.T) {
		config := &LDAPConfig{
			Host:         "ldap.example.com",
			Port:         389,
			BaseDN:       "dc=example,dc=com",
			BindDN:       "cn=admin,dc=example,dc=com",
			BindPassword: "secret",
		}
		provider, err := NewLDAPProvider(config)
		require.NoError(t, err)
		assert.NotNil(t, provider)
	})

	t.Run("rejects nil config", func(t *testing.T) {
		provider, err := NewLDAPProvider(nil)
		assert.Error(t, err)
		assert.Nil(t, provider)
	})

	t.Run("rejects invalid config", func(t *testing.T) {
		config := &LDAPConfig{} // Missing required fields
		provider, err := NewLDAPProvider(config)
		assert.Error(t, err)
		assert.Nil(t, provider)
	})
}

func TestLDAPProvider_BuildUserDN(t *testing.T) {
	config := &LDAPConfig{
		Host:           "ldap.example.com",
		Port:           389,
		BaseDN:         "dc=example,dc=com",
		UserSearchBase: "ou=users",
		UserAttribute:  "uid",
	}
	provider, _ := NewLDAPProvider(config)

	t.Run("builds DN with user attribute", func(t *testing.T) {
		dn := provider.BuildUserDN("jsmith")
		assert.Equal(t, "uid=jsmith,ou=users,dc=example,dc=com", dn)
	})

	t.Run("uses cn as default attribute", func(t *testing.T) {
		config2 := &LDAPConfig{
			Host:           "ldap.example.com",
			Port:           389,
			BaseDN:         "dc=example,dc=com",
			UserSearchBase: "ou=users",
		}
		provider2, _ := NewLDAPProvider(config2)
		dn := provider2.BuildUserDN("jsmith")
		assert.Equal(t, "cn=jsmith,ou=users,dc=example,dc=com", dn)
	})
}

func TestLDAPProvider_BuildSearchFilter(t *testing.T) {
	t.Run("builds default filter", func(t *testing.T) {
		config := &LDAPConfig{
			Host:          "ldap.example.com",
			Port:          389,
			BaseDN:        "dc=example,dc=com",
			UserAttribute: "uid",
		}
		provider, _ := NewLDAPProvider(config)
		filter := provider.BuildSearchFilter("jsmith")
		assert.Equal(t, "(uid=jsmith)", filter)
	})

	t.Run("uses custom filter template", func(t *testing.T) {
		config := &LDAPConfig{
			Host:         "ldap.example.com",
			Port:         389,
			BaseDN:       "dc=example,dc=com",
			SearchFilter: "(&(objectClass=person)(sAMAccountName=%s))",
		}
		provider, _ := NewLDAPProvider(config)
		filter := provider.BuildSearchFilter("jsmith")
		assert.Equal(t, "(&(objectClass=person)(sAMAccountName=jsmith))", filter)
	})

	t.Run("escapes special characters", func(t *testing.T) {
		config := &LDAPConfig{
			Host:          "ldap.example.com",
			Port:          389,
			BaseDN:        "dc=example,dc=com",
			UserAttribute: "uid",
		}
		provider, _ := NewLDAPProvider(config)
		filter := provider.BuildSearchFilter("user*with(special)chars")
		assert.Contains(t, filter, "\\2a") // escaped *
		assert.Contains(t, filter, "\\28") // escaped (
		assert.Contains(t, filter, "\\29") // escaped )
	})
}

func TestLDAPProvider_AttributeMapping(t *testing.T) {
	t.Run("maps LDAP attributes to user fields", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
			AttributeMapping: AttributeMapping{
				Email:       "mail",
				DisplayName: "displayName",
				FirstName:   "givenName",
				LastName:    "sn",
				Groups:      "memberOf",
			},
		}
		provider, _ := NewLDAPProvider(config)

		attrs := map[string][]string{
			"mail":        {"jsmith@example.com"},
			"displayName": {"John Smith"},
			"givenName":   {"John"},
			"sn":          {"Smith"},
			"memberOf":    {"cn=developers,ou=groups,dc=example,dc=com", "cn=users,ou=groups,dc=example,dc=com"},
		}

		user := provider.MapAttributes(attrs)
		assert.Equal(t, "jsmith@example.com", user.Email)
		assert.Equal(t, "John Smith", user.DisplayName)
		assert.Equal(t, "John", user.FirstName)
		assert.Equal(t, "Smith", user.LastName)
		assert.Len(t, user.Groups, 2)
	})

	t.Run("handles missing attributes", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
			AttributeMapping: AttributeMapping{
				Email:       "mail",
				DisplayName: "displayName",
			},
		}
		provider, _ := NewLDAPProvider(config)

		attrs := map[string][]string{
			"mail": {"jsmith@example.com"},
			// displayName missing
		}

		user := provider.MapAttributes(attrs)
		assert.Equal(t, "jsmith@example.com", user.Email)
		assert.Empty(t, user.DisplayName)
	})
}

func TestLDAPProvider_GroupExtraction(t *testing.T) {
	config := &LDAPConfig{
		Host:   "ldap.example.com",
		Port:   389,
		BaseDN: "dc=example,dc=com",
	}
	provider, _ := NewLDAPProvider(config)

	t.Run("extracts CN from full DN", func(t *testing.T) {
		groups := []string{
			"cn=developers,ou=groups,dc=example,dc=com",
			"cn=admins,ou=groups,dc=example,dc=com",
		}
		extracted := provider.ExtractGroupNames(groups)
		assert.Contains(t, extracted, "developers")
		assert.Contains(t, extracted, "admins")
	})

	t.Run("handles simple group names", func(t *testing.T) {
		groups := []string{"developers", "admins"}
		extracted := provider.ExtractGroupNames(groups)
		assert.Contains(t, extracted, "developers")
		assert.Contains(t, extracted, "admins")
	})
}

func TestLDAPProvider_ConnectionPool(t *testing.T) {
	t.Run("respects max connections", func(t *testing.T) {
		config := &LDAPConfig{
			Host:           "ldap.example.com",
			Port:           389,
			BaseDN:         "dc=example,dc=com",
			MaxConnections: 5,
		}
		provider, _ := NewLDAPProvider(config)
		assert.Equal(t, 5, provider.MaxConnections())
	})

	t.Run("defaults to 10 connections", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewLDAPProvider(config)
		assert.Equal(t, 10, provider.MaxConnections())
	})
}

func TestLDAPProvider_Timeout(t *testing.T) {
	t.Run("respects configured timeout", func(t *testing.T) {
		config := &LDAPConfig{
			Host:    "ldap.example.com",
			Port:    389,
			BaseDN:  "dc=example,dc=com",
			Timeout: 5 * time.Second,
		}
		provider, _ := NewLDAPProvider(config)
		assert.Equal(t, 5*time.Second, provider.Timeout())
	})

	t.Run("defaults to 30 seconds", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewLDAPProvider(config)
		assert.Equal(t, 30*time.Second, provider.Timeout())
	})
}

func TestLDAPProvider_TLSConfig(t *testing.T) {
	t.Run("configures TLS when enabled", func(t *testing.T) {
		config := &LDAPConfig{
			Host:               "ldap.example.com",
			Port:               636,
			BaseDN:             "dc=example,dc=com",
			UseTLS:             true,
			InsecureSkipVerify: false,
		}
		provider, _ := NewLDAPProvider(config)
		assert.True(t, provider.UsesTLS())
		assert.False(t, provider.SkipsVerify())
	})

	t.Run("supports StartTLS", func(t *testing.T) {
		config := &LDAPConfig{
			Host:        "ldap.example.com",
			Port:        389,
			BaseDN:      "dc=example,dc=com",
			UseStartTLS: true,
		}
		provider, _ := NewLDAPProvider(config)
		assert.True(t, provider.UsesStartTLS())
	})
}

func TestLDAPAuthResult(t *testing.T) {
	t.Run("successful auth result", func(t *testing.T) {
		result := &LDAPAuthResult{
			Success:     true,
			UserDN:      "uid=jsmith,ou=users,dc=example,dc=com",
			Username:    "jsmith",
			Email:       "jsmith@example.com",
			DisplayName: "John Smith",
			Groups:      []string{"developers", "users"},
		}
		assert.True(t, result.Success)
		assert.Empty(t, result.Error)
	})

	t.Run("failed auth result", func(t *testing.T) {
		result := &LDAPAuthResult{
			Success: false,
			Error:   "invalid credentials",
		}
		assert.False(t, result.Success)
		assert.Equal(t, "invalid credentials", result.Error)
	})
}

func TestLDAPProvider_Authenticate(t *testing.T) {
	t.Run("returns error for empty username", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewLDAPProvider(config)

		result, err := provider.Authenticate(context.Background(), "", "password")
		assert.Error(t, err)
		assert.Nil(t, result)
	})

	t.Run("returns error for empty password", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewLDAPProvider(config)

		result, err := provider.Authenticate(context.Background(), "jsmith", "")
		assert.Error(t, err)
		assert.Nil(t, result)
	})
}

func TestLDAPProvider_ProviderInfo(t *testing.T) {
	t.Run("returns provider information", func(t *testing.T) {
		config := &LDAPConfig{
			Host:   "ldap.example.com",
			Port:   389,
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewLDAPProvider(config)

		info := provider.Info()
		assert.Equal(t, "ldap", info.Type)
		assert.Equal(t, "ldap.example.com", info.Host)
		assert.Equal(t, 389, info.Port)
	})
}

func TestDefaultLDAPConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultLDAPConfig()
		assert.Equal(t, 389, config.Port)
		assert.Equal(t, "cn", config.UserAttribute)
		assert.Equal(t, 30*time.Second, config.Timeout)
		assert.Equal(t, 10, config.MaxConnections)
	})
}

func TestLDAPEscaping(t *testing.T) {
	t.Run("escapes all special characters", func(t *testing.T) {
		input := "user*name(with)special\\chars"
		escaped := EscapeLDAPFilter(input)
		assert.NotContains(t, escaped, "*")
		assert.NotContains(t, escaped, "(")
		assert.NotContains(t, escaped, ")")
		assert.Contains(t, escaped, "\\2a")
		assert.Contains(t, escaped, "\\28")
		assert.Contains(t, escaped, "\\29")
		assert.Contains(t, escaped, "\\5c")
	})

	t.Run("handles null bytes", func(t *testing.T) {
		input := "user\x00name"
		escaped := EscapeLDAPFilter(input)
		assert.Contains(t, escaped, "\\00")
	})
}
