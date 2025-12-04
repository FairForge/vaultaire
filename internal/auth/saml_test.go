// internal/auth/saml_test.go
package auth

import (
	"encoding/xml"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSAMLConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:       "https://sp.example.com/saml",
			ACSURL:         "https://sp.example.com/saml/acs",
			IDPMetadataURL: "https://idp.example.com/metadata",
			CertificatePEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
			PrivateKeyPEM:  "-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty EntityID", func(t *testing.T) {
		config := &SAMLConfig{
			ACSURL: "https://sp.example.com/saml/acs",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "entity ID")
	})

	t.Run("rejects empty ACS URL", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID: "https://sp.example.com/saml",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ACS URL")
	})

	t.Run("requires IDP metadata or SSO URL", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID: "https://sp.example.com/saml",
			ACSURL:   "https://sp.example.com/saml/acs",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "IDP")
	})
}

func TestNewSAMLProvider(t *testing.T) {
	t.Run("creates provider with valid config", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:       "https://sp.example.com/saml",
			ACSURL:         "https://sp.example.com/saml/acs",
			IDPSSOURL:      "https://idp.example.com/sso",
			IDPCertificate: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
		}
		provider, err := NewSAMLProvider(config)
		require.NoError(t, err)
		assert.NotNil(t, provider)
	})

	t.Run("rejects nil config", func(t *testing.T) {
		provider, err := NewSAMLProvider(nil)
		assert.Error(t, err)
		assert.Nil(t, provider)
	})
}

func TestSAMLProvider_GenerateAuthnRequest(t *testing.T) {
	config := &SAMLConfig{
		EntityID:  "https://sp.example.com/saml",
		ACSURL:    "https://sp.example.com/saml/acs",
		IDPSSOURL: "https://idp.example.com/sso",
	}
	provider, _ := NewSAMLProvider(config)

	t.Run("generates valid AuthnRequest", func(t *testing.T) {
		request, err := provider.GenerateAuthnRequest()
		require.NoError(t, err)
		assert.NotEmpty(t, request.ID)
		assert.Equal(t, "https://sp.example.com/saml/acs", request.AssertionConsumerServiceURL)
		assert.Equal(t, "https://sp.example.com/saml", request.Issuer)
		assert.Equal(t, "2.0", request.Version)
	})

	t.Run("includes request ID with underscore prefix", func(t *testing.T) {
		request, _ := provider.GenerateAuthnRequest()
		assert.True(t, request.ID[0] == '_', "SAML ID should start with underscore")
	})

	t.Run("sets IssueInstant", func(t *testing.T) {
		request, _ := provider.GenerateAuthnRequest()
		assert.False(t, request.IssueInstant.IsZero())
		assert.True(t, time.Since(request.IssueInstant) < time.Minute)
	})
}

func TestSAMLProvider_BuildRedirectURL(t *testing.T) {
	config := &SAMLConfig{
		EntityID:  "https://sp.example.com/saml",
		ACSURL:    "https://sp.example.com/saml/acs",
		IDPSSOURL: "https://idp.example.com/sso",
	}
	provider, _ := NewSAMLProvider(config)

	t.Run("builds redirect URL with SAMLRequest", func(t *testing.T) {
		url, err := provider.BuildRedirectURL("")
		require.NoError(t, err)
		assert.Contains(t, url, "https://idp.example.com/sso")
		assert.Contains(t, url, "SAMLRequest=")
	})

	t.Run("includes RelayState when provided", func(t *testing.T) {
		url, err := provider.BuildRedirectURL("https://app.example.com/dashboard")
		require.NoError(t, err)
		assert.Contains(t, url, "RelayState=")
	})
}

func TestSAMLProvider_ParseResponse(t *testing.T) {
	config := &SAMLConfig{
		EntityID:       "https://sp.example.com/saml",
		ACSURL:         "https://sp.example.com/saml/acs",
		IDPSSOURL:      "https://idp.example.com/sso",
		IDPCertificate: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
	}
	provider, _ := NewSAMLProvider(config)

	t.Run("rejects empty response", func(t *testing.T) {
		_, err := provider.ParseResponse("")
		assert.Error(t, err)
	})

	t.Run("rejects invalid base64", func(t *testing.T) {
		_, err := provider.ParseResponse("not-valid-base64!!!")
		assert.Error(t, err)
	})
}

func TestSAMLProvider_ValidateAssertion(t *testing.T) {
	config := &SAMLConfig{
		EntityID:  "https://sp.example.com/saml",
		ACSURL:    "https://sp.example.com/saml/acs",
		IDPSSOURL: "https://idp.example.com/sso",
	}
	provider, _ := NewSAMLProvider(config)

	t.Run("rejects expired assertion", func(t *testing.T) {
		assertion := &SAMLAssertion{
			Conditions: SAMLConditions{
				NotBefore:    time.Now().Add(-2 * time.Hour),
				NotOnOrAfter: time.Now().Add(-1 * time.Hour),
			},
		}
		err := provider.ValidateAssertion(assertion)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})

	t.Run("rejects future assertion", func(t *testing.T) {
		assertion := &SAMLAssertion{
			Conditions: SAMLConditions{
				NotBefore:    time.Now().Add(1 * time.Hour),
				NotOnOrAfter: time.Now().Add(2 * time.Hour),
			},
		}
		err := provider.ValidateAssertion(assertion)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not yet valid")
	})

	t.Run("accepts valid time window", func(t *testing.T) {
		assertion := &SAMLAssertion{
			Conditions: SAMLConditions{
				NotBefore:    time.Now().Add(-5 * time.Minute),
				NotOnOrAfter: time.Now().Add(5 * time.Minute),
				AudienceRestriction: AudienceRestriction{
					Audience: "https://sp.example.com/saml",
				},
			},
		}
		err := provider.ValidateAssertion(assertion)
		assert.NoError(t, err)
	})

	t.Run("validates audience restriction", func(t *testing.T) {
		assertion := &SAMLAssertion{
			Conditions: SAMLConditions{
				NotBefore:    time.Now().Add(-5 * time.Minute),
				NotOnOrAfter: time.Now().Add(5 * time.Minute),
				AudienceRestriction: AudienceRestriction{
					Audience: "https://wrong-sp.example.com/saml",
				},
			},
		}
		err := provider.ValidateAssertion(assertion)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "audience")
	})
}

func TestSAMLProvider_ExtractAttributes(t *testing.T) {
	config := &SAMLConfig{
		EntityID:  "https://sp.example.com/saml",
		ACSURL:    "https://sp.example.com/saml/acs",
		IDPSSOURL: "https://idp.example.com/sso",
		AttributeMapping: SAMLAttributeMapping{
			Email:       "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
			FirstName:   "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname",
			LastName:    "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname",
			DisplayName: "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
			Groups:      "http://schemas.xmlsoap.org/claims/Group",
		},
	}
	provider, _ := NewSAMLProvider(config)

	t.Run("extracts mapped attributes", func(t *testing.T) {
		attrs := []SAMLAttribute{
			{Name: "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress", Values: []string{"jsmith@example.com"}},
			{Name: "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname", Values: []string{"John"}},
			{Name: "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname", Values: []string{"Smith"}},
			{Name: "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name", Values: []string{"John Smith"}},
			{Name: "http://schemas.xmlsoap.org/claims/Group", Values: []string{"Developers", "Users"}},
		}

		user := provider.ExtractAttributes(attrs)
		assert.Equal(t, "jsmith@example.com", user.Email)
		assert.Equal(t, "John", user.FirstName)
		assert.Equal(t, "Smith", user.LastName)
		assert.Equal(t, "John Smith", user.DisplayName)
		assert.Contains(t, user.Groups, "Developers")
		assert.Contains(t, user.Groups, "Users")
	})
}

func TestSAMLProvider_GenerateMetadata(t *testing.T) {
	config := &SAMLConfig{
		EntityID:       "https://sp.example.com/saml",
		ACSURL:         "https://sp.example.com/saml/acs",
		IDPSSOURL:      "https://idp.example.com/sso",
		SLOURL:         "https://sp.example.com/saml/slo",
		CertificatePEM: "-----BEGIN CERTIFICATE-----\nMIItest\n-----END CERTIFICATE-----",
	}
	provider, _ := NewSAMLProvider(config)

	t.Run("generates valid SP metadata", func(t *testing.T) {
		metadata := provider.GenerateMetadata()
		assert.Contains(t, metadata, "EntityDescriptor")
		assert.Contains(t, metadata, "https://sp.example.com/saml")
		assert.Contains(t, metadata, "AssertionConsumerService")
	})
}

func TestSAMLProvider_NameIDFormats(t *testing.T) {
	t.Run("supports email format", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:     "https://sp.example.com/saml",
			ACSURL:       "https://sp.example.com/saml/acs",
			IDPSSOURL:    "https://idp.example.com/sso",
			NameIDFormat: NameIDFormatEmail,
		}
		provider, _ := NewSAMLProvider(config)
		assert.Equal(t, NameIDFormatEmail, provider.NameIDFormat())
	})

	t.Run("supports persistent format", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:     "https://sp.example.com/saml",
			ACSURL:       "https://sp.example.com/saml/acs",
			IDPSSOURL:    "https://idp.example.com/sso",
			NameIDFormat: NameIDFormatPersistent,
		}
		provider, _ := NewSAMLProvider(config)
		assert.Equal(t, NameIDFormatPersistent, provider.NameIDFormat())
	})

	t.Run("defaults to unspecified", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:  "https://sp.example.com/saml",
			ACSURL:    "https://sp.example.com/saml/acs",
			IDPSSOURL: "https://idp.example.com/sso",
		}
		provider, _ := NewSAMLProvider(config)
		assert.Equal(t, NameIDFormatUnspecified, provider.NameIDFormat())
	})
}

func TestSAMLProvider_SigningOptions(t *testing.T) {
	t.Run("can sign requests", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:      "https://sp.example.com/saml",
			ACSURL:        "https://sp.example.com/saml/acs",
			IDPSSOURL:     "https://idp.example.com/sso",
			SignRequests:  true,
			PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----",
		}
		provider, _ := NewSAMLProvider(config)
		assert.True(t, provider.SignsRequests())
	})

	t.Run("can require signed responses", func(t *testing.T) {
		config := &SAMLConfig{
			EntityID:               "https://sp.example.com/saml",
			ACSURL:                 "https://sp.example.com/saml/acs",
			IDPSSOURL:              "https://idp.example.com/sso",
			RequireSignedResponses: true,
		}
		provider, _ := NewSAMLProvider(config)
		assert.True(t, provider.RequiresSignedResponses())
	})
}

func TestSAMLAuthResult(t *testing.T) {
	t.Run("successful SAML auth", func(t *testing.T) {
		result := &SAMLAuthResult{
			Success:     true,
			NameID:      "jsmith@example.com",
			SessionID:   "_abc123",
			Email:       "jsmith@example.com",
			DisplayName: "John Smith",
			Groups:      []string{"Developers"},
			Attributes:  map[string][]string{"role": {"admin"}},
		}
		assert.True(t, result.Success)
		assert.NotEmpty(t, result.NameID)
	})
}

func TestSAMLXMLStructures(t *testing.T) {
	t.Run("AuthnRequest marshals correctly", func(t *testing.T) {
		request := &SAMLAuthnRequest{
			XMLName:                     xml.Name{Local: "AuthnRequest"},
			XMLNS:                       "urn:oasis:names:tc:SAML:2.0:protocol",
			XMLNSSAML:                   "urn:oasis:names:tc:SAML:2.0:assertion",
			ID:                          "_test123",
			Version:                     "2.0",
			IssueInstant:                time.Now().UTC(),
			Destination:                 "https://idp.example.com/sso",
			AssertionConsumerServiceURL: "https://sp.example.com/saml/acs",
			Issuer:                      "https://sp.example.com/saml",
		}

		data, err := xml.Marshal(request)
		require.NoError(t, err)
		assert.Contains(t, string(data), "AuthnRequest")
		assert.Contains(t, string(data), "_test123")
	})
}

func TestDefaultSAMLConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultSAMLConfig()
		assert.Equal(t, NameIDFormatUnspecified, config.NameIDFormat)
		assert.True(t, config.RequireSignedResponses)
		assert.Equal(t, 5*time.Minute, config.ClockSkew)
	})
}
