// internal/auth/activedirectory_test.go
package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestADConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &ADConfig{
			Domain:       "example.com",
			Host:         "dc.example.com",
			BaseDN:       "dc=example,dc=com",
			BindUser:     "admin@example.com",
			BindPassword: "secret",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty domain", func(t *testing.T) {
		config := &ADConfig{
			Host:   "dc.example.com",
			BaseDN: "dc=example,dc=com",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "domain")
	})

	t.Run("auto-generates BaseDN from domain", func(t *testing.T) {
		config := &ADConfig{
			Domain: "corp.example.com",
			Host:   "dc.corp.example.com",
		}
		config.ApplyDefaults()
		assert.Equal(t, "dc=corp,dc=example,dc=com", config.BaseDN)
	})
}

func TestNewADProvider(t *testing.T) {
	t.Run("creates provider with valid config", func(t *testing.T) {
		config := &ADConfig{
			Domain:       "example.com",
			Host:         "dc.example.com",
			BaseDN:       "dc=example,dc=com",
			BindUser:     "admin@example.com",
			BindPassword: "secret",
		}
		provider, err := NewADProvider(config)
		require.NoError(t, err)
		assert.NotNil(t, provider)
	})

	t.Run("rejects nil config", func(t *testing.T) {
		provider, err := NewADProvider(nil)
		assert.Error(t, err)
		assert.Nil(t, provider)
	})
}

func TestADProvider_BuildUPN(t *testing.T) {
	config := &ADConfig{
		Domain: "example.com",
		Host:   "dc.example.com",
		BaseDN: "dc=example,dc=com",
	}
	provider, _ := NewADProvider(config)

	t.Run("builds UPN from username", func(t *testing.T) {
		upn := provider.BuildUPN("jsmith")
		assert.Equal(t, "jsmith@example.com", upn)
	})

	t.Run("preserves existing UPN", func(t *testing.T) {
		upn := provider.BuildUPN("jsmith@corp.example.com")
		assert.Equal(t, "jsmith@corp.example.com", upn)
	})
}

func TestADProvider_BuildSearchFilter(t *testing.T) {
	config := &ADConfig{
		Domain: "example.com",
		Host:   "dc.example.com",
		BaseDN: "dc=example,dc=com",
	}
	provider, _ := NewADProvider(config)

	t.Run("builds filter for sAMAccountName", func(t *testing.T) {
		filter := provider.BuildSearchFilter("jsmith")
		assert.Contains(t, filter, "sAMAccountName=jsmith")
	})

	t.Run("includes user objectClass", func(t *testing.T) {
		filter := provider.BuildSearchFilter("jsmith")
		assert.Contains(t, filter, "objectClass=user")
	})

	t.Run("supports UPN search", func(t *testing.T) {
		filter := provider.BuildSearchFilter("jsmith@example.com")
		assert.Contains(t, filter, "userPrincipalName=jsmith@example.com")
	})
}

func TestADProvider_AttributeMapping(t *testing.T) {
	config := &ADConfig{
		Domain: "example.com",
		Host:   "dc.example.com",
		BaseDN: "dc=example,dc=com",
	}
	provider, _ := NewADProvider(config)

	t.Run("maps AD-specific attributes", func(t *testing.T) {
		attrs := map[string][]string{
			"sAMAccountName":    {"jsmith"},
			"userPrincipalName": {"jsmith@example.com"},
			"mail":              {"john.smith@example.com"},
			"displayName":       {"John Smith"},
			"givenName":         {"John"},
			"sn":                {"Smith"},
			"memberOf": {
				"CN=Domain Users,CN=Users,DC=example,DC=com",
				"CN=Developers,OU=Groups,DC=example,DC=com",
			},
			"department":      {"Engineering"},
			"title":           {"Senior Developer"},
			"manager":         {"CN=Jane Doe,OU=Users,DC=example,DC=com"},
			"telephoneNumber": {"+1-555-1234"},
		}

		user := provider.MapAttributes(attrs)
		assert.Equal(t, "jsmith", user.SAMAccountName)
		assert.Equal(t, "jsmith@example.com", user.UPN)
		assert.Equal(t, "john.smith@example.com", user.Email)
		assert.Equal(t, "John Smith", user.DisplayName)
		assert.Equal(t, "Engineering", user.Department)
		assert.Equal(t, "Senior Developer", user.Title)
		assert.Len(t, user.Groups, 2)
	})
}

func TestADProvider_NestedGroups(t *testing.T) {
	config := &ADConfig{
		Domain:              "example.com",
		Host:                "dc.example.com",
		BaseDN:              "dc=example,dc=com",
		ResolveNestedGroups: true,
	}
	provider, _ := NewADProvider(config)

	t.Run("builds nested group filter", func(t *testing.T) {
		userDN := "CN=John Smith,OU=Users,DC=example,DC=com"
		filter := provider.BuildNestedGroupFilter(userDN)
		assert.Contains(t, filter, "member:1.2.840.113556.1.4.1941:=")
		assert.Contains(t, filter, userDN)
	})

	t.Run("LDAP_MATCHING_RULE_IN_CHAIN OID is correct", func(t *testing.T) {
		assert.Equal(t, "1.2.840.113556.1.4.1941", ADNestedGroupOID)
	})
}

func TestADProvider_DisabledAccounts(t *testing.T) {
	config := &ADConfig{
		Domain: "example.com",
		Host:   "dc.example.com",
		BaseDN: "dc=example,dc=com",
	}
	provider, _ := NewADProvider(config)

	t.Run("detects disabled account", func(t *testing.T) {
		// userAccountControl with ACCOUNTDISABLE flag (0x0002)
		uac := 514 // Normal account (512) + Disabled (2)
		assert.True(t, provider.IsAccountDisabled(uac))
	})

	t.Run("detects enabled account", func(t *testing.T) {
		uac := 512 // Normal account, enabled
		assert.False(t, provider.IsAccountDisabled(uac))
	})

	t.Run("detects locked account", func(t *testing.T) {
		uac := 528 // Normal account (512) + Locked (16)
		assert.True(t, provider.IsAccountLocked(uac))
	})

	t.Run("detects password expired", func(t *testing.T) {
		uac := 8389120 // PASSWORD_EXPIRED flag
		assert.True(t, provider.IsPasswordExpired(uac))
	})
}

func TestADProvider_PasswordPolicy(t *testing.T) {
	config := &ADConfig{
		Domain: "example.com",
		Host:   "dc.example.com",
		BaseDN: "dc=example,dc=com",
	}
	provider, _ := NewADProvider(config)

	t.Run("parses pwdLastSet timestamp", func(t *testing.T) {
		// Windows FILETIME: 100-nanosecond intervals since 1601
		// This represents roughly 2024-01-15
		filetime := int64(133500000000000000)
		timestamp := provider.ParseADTimestamp(filetime)
		assert.False(t, timestamp.IsZero())
		assert.True(t, timestamp.Year() >= 2024)
	})

	t.Run("handles never-set password", func(t *testing.T) {
		timestamp := provider.ParseADTimestamp(0)
		assert.True(t, timestamp.IsZero())
	})
}

func TestADProvider_GlobalCatalog(t *testing.T) {
	t.Run("uses port 3268 for GC", func(t *testing.T) {
		config := &ADConfig{
			Domain:           "example.com",
			Host:             "gc.example.com",
			BaseDN:           "dc=example,dc=com",
			UseGlobalCatalog: true,
		}
		config.ApplyDefaults()
		assert.Equal(t, 3268, config.Port)
	})

	t.Run("uses port 3269 for GC with TLS", func(t *testing.T) {
		config := &ADConfig{
			Domain:           "example.com",
			Host:             "gc.example.com",
			BaseDN:           "dc=example,dc=com",
			UseGlobalCatalog: true,
			UseTLS:           true,
		}
		config.ApplyDefaults()
		assert.Equal(t, 3269, config.Port)
	})
}

func TestADProvider_DomainController(t *testing.T) {
	t.Run("discovers DC via DNS SRV", func(t *testing.T) {
		config := &ADConfig{
			Domain: "example.com",
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewADProvider(config)

		// Build SRV record name
		srv := provider.GetDCSRVRecord()
		assert.Equal(t, "_ldap._tcp.dc._msdcs.example.com", srv)
	})
}

func TestADProvider_Authenticate(t *testing.T) {
	t.Run("returns error for empty username", func(t *testing.T) {
		config := &ADConfig{
			Domain: "example.com",
			Host:   "dc.example.com",
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewADProvider(config)

		result, err := provider.Authenticate(context.Background(), "", "password")
		assert.Error(t, err)
		assert.Nil(t, result)
	})

	t.Run("returns error for empty password", func(t *testing.T) {
		config := &ADConfig{
			Domain: "example.com",
			Host:   "dc.example.com",
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewADProvider(config)

		result, err := provider.Authenticate(context.Background(), "jsmith", "")
		assert.Error(t, err)
		assert.Nil(t, result)
	})
}

func TestADAuthResult(t *testing.T) {
	t.Run("successful auth includes AD fields", func(t *testing.T) {
		result := &ADAuthResult{
			Success:        true,
			SAMAccountName: "jsmith",
			UPN:            "jsmith@example.com",
			Email:          "john.smith@example.com",
			DisplayName:    "John Smith",
			Department:     "Engineering",
			Groups:         []string{"Domain Users", "Developers"},
		}
		assert.True(t, result.Success)
		assert.Equal(t, "jsmith", result.SAMAccountName)
	})
}

func TestDefaultADConfig(t *testing.T) {
	t.Run("provides AD-specific defaults", func(t *testing.T) {
		config := DefaultADConfig()
		assert.Equal(t, 389, config.Port)
		assert.Equal(t, 30*time.Second, config.Timeout)
		assert.Equal(t, "sAMAccountName", config.UserAttribute)
	})
}

func TestADUserAccountControl(t *testing.T) {
	t.Run("defines correct UAC flags", func(t *testing.T) {
		assert.Equal(t, 0x0002, UACDisabled)
		assert.Equal(t, 0x0010, UACLockout)
		assert.Equal(t, 0x0200, UACNormalAccount)
		assert.Equal(t, 0x10000, UACPasswordNeverExpires)
		assert.Equal(t, 0x800000, UACPasswordExpired)
	})
}

func TestADProvider_ProviderInfo(t *testing.T) {
	t.Run("returns AD provider info", func(t *testing.T) {
		config := &ADConfig{
			Domain: "example.com",
			Host:   "dc.example.com",
			BaseDN: "dc=example,dc=com",
		}
		provider, _ := NewADProvider(config)

		info := provider.Info()
		assert.Equal(t, "activedirectory", info.Type)
		assert.Equal(t, "example.com", info.Domain)
	})
}
