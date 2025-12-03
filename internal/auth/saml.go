// internal/auth/saml.go
package auth

import (
	"compress/flate"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NameID formats
const (
	NameIDFormatUnspecified = "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified"
	NameIDFormatEmail       = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
	NameIDFormatPersistent  = "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent"
	NameIDFormatTransient   = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
)

// SAMLConfig configures the SAML authentication provider
type SAMLConfig struct {
	// Service Provider settings
	EntityID       string `json:"entity_id"`
	ACSURL         string `json:"acs_url"`
	SLOURL         string `json:"slo_url"`
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`

	// Identity Provider settings
	IDPMetadataURL string `json:"idp_metadata_url"`
	IDPSSOURL      string `json:"idp_sso_url"`
	IDPSLOURL      string `json:"idp_slo_url"`
	IDPCertificate string `json:"idp_certificate"`

	// SAML options
	NameIDFormat           string        `json:"nameid_format"`
	SignRequests           bool          `json:"sign_requests"`
	RequireSignedResponses bool          `json:"require_signed_responses"`
	RequireSignedAssertion bool          `json:"require_signed_assertion"`
	ClockSkew              time.Duration `json:"clock_skew"`

	// Attribute mapping
	AttributeMapping SAMLAttributeMapping `json:"attribute_mapping"`
}

// SAMLAttributeMapping maps SAML attributes to user fields
type SAMLAttributeMapping struct {
	Email       string `json:"email"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	DisplayName string `json:"display_name"`
	Groups      string `json:"groups"`
	Phone       string `json:"phone"`
}

// DefaultSAMLConfig returns a config with sensible defaults
func DefaultSAMLConfig() *SAMLConfig {
	return &SAMLConfig{
		NameIDFormat:           NameIDFormatUnspecified,
		RequireSignedResponses: true,
		ClockSkew:              5 * time.Minute,
		AttributeMapping: SAMLAttributeMapping{
			Email:       "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
			FirstName:   "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname",
			LastName:    "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname",
			DisplayName: "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
			Groups:      "http://schemas.xmlsoap.org/claims/Group",
		},
	}
}

// Validate checks if the configuration is valid
func (c *SAMLConfig) Validate() error {
	if c.EntityID == "" {
		return errors.New("saml: entity ID is required")
	}
	if c.ACSURL == "" {
		return errors.New("saml: ACS URL is required")
	}
	if c.IDPMetadataURL == "" && c.IDPSSOURL == "" {
		return errors.New("saml: IDP metadata URL or SSO URL is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *SAMLConfig) ApplyDefaults() {
	defaults := DefaultSAMLConfig()
	if c.NameIDFormat == "" {
		c.NameIDFormat = defaults.NameIDFormat
	}
	if c.ClockSkew == 0 {
		c.ClockSkew = defaults.ClockSkew
	}
	if c.AttributeMapping.Email == "" {
		c.AttributeMapping = defaults.AttributeMapping
	}
}

// SAMLAuthnRequest represents a SAML AuthnRequest
type SAMLAuthnRequest struct {
	XMLName                     xml.Name      `xml:"samlp:AuthnRequest"`
	XMLNS                       string        `xml:"xmlns:samlp,attr"`
	XMLNSSAML                   string        `xml:"xmlns:saml,attr"`
	ID                          string        `xml:"ID,attr"`
	Version                     string        `xml:"Version,attr"`
	IssueInstant                time.Time     `xml:"IssueInstant,attr"`
	Destination                 string        `xml:"Destination,attr,omitempty"`
	AssertionConsumerServiceURL string        `xml:"AssertionConsumerServiceURL,attr"`
	ProtocolBinding             string        `xml:"ProtocolBinding,attr,omitempty"`
	Issuer                      string        `xml:"saml:Issuer"`
	NameIDPolicy                *NameIDPolicy `xml:"samlp:NameIDPolicy,omitempty"`
}

// NameIDPolicy specifies the name identifier format
type NameIDPolicy struct {
	XMLName     xml.Name `xml:"samlp:NameIDPolicy"`
	Format      string   `xml:"Format,attr,omitempty"`
	AllowCreate bool     `xml:"AllowCreate,attr,omitempty"`
}

// SAMLResponse represents a SAML Response
type SAMLResponse struct {
	XMLName      xml.Name       `xml:"Response"`
	ID           string         `xml:"ID,attr"`
	InResponseTo string         `xml:"InResponseTo,attr"`
	Version      string         `xml:"Version,attr"`
	IssueInstant time.Time      `xml:"IssueInstant,attr"`
	Destination  string         `xml:"Destination,attr"`
	Status       SAMLStatus     `xml:"Status"`
	Assertion    *SAMLAssertion `xml:"Assertion"`
}

// SAMLStatus represents the status of a SAML response
type SAMLStatus struct {
	StatusCode SAMLStatusCode `xml:"StatusCode"`
}

// SAMLStatusCode represents a SAML status code
type SAMLStatusCode struct {
	Value string `xml:"Value,attr"`
}

// SAMLAssertion represents a SAML Assertion
type SAMLAssertion struct {
	XMLName            xml.Name            `xml:"Assertion"`
	ID                 string              `xml:"ID,attr"`
	Version            string              `xml:"Version,attr"`
	IssueInstant       time.Time           `xml:"IssueInstant,attr"`
	Issuer             string              `xml:"Issuer"`
	Subject            SAMLSubject         `xml:"Subject"`
	Conditions         SAMLConditions      `xml:"Conditions"`
	AttributeStatement *AttributeStatement `xml:"AttributeStatement"`
}

// SAMLSubject represents the subject of an assertion
type SAMLSubject struct {
	NameID              SAMLNameID          `xml:"NameID"`
	SubjectConfirmation SubjectConfirmation `xml:"SubjectConfirmation"`
}

// SAMLNameID represents a name identifier
type SAMLNameID struct {
	Format string `xml:"Format,attr,omitempty"`
	Value  string `xml:",chardata"`
}

// SubjectConfirmation confirms the subject
type SubjectConfirmation struct {
	Method                  string                  `xml:"Method,attr"`
	SubjectConfirmationData SubjectConfirmationData `xml:"SubjectConfirmationData"`
}

// SubjectConfirmationData contains confirmation data
type SubjectConfirmationData struct {
	InResponseTo string    `xml:"InResponseTo,attr,omitempty"`
	NotOnOrAfter time.Time `xml:"NotOnOrAfter,attr"`
	Recipient    string    `xml:"Recipient,attr,omitempty"`
}

// SAMLConditions defines assertion conditions
type SAMLConditions struct {
	NotBefore           time.Time           `xml:"NotBefore,attr"`
	NotOnOrAfter        time.Time           `xml:"NotOnOrAfter,attr"`
	AudienceRestriction AudienceRestriction `xml:"AudienceRestriction"`
}

// AudienceRestriction restricts the audience
type AudienceRestriction struct {
	Audience string `xml:"Audience"`
}

// AttributeStatement contains SAML attributes
type AttributeStatement struct {
	Attributes []SAMLAttribute `xml:"Attribute"`
}

// SAMLAttribute represents a SAML attribute
type SAMLAttribute struct {
	Name         string   `xml:"Name,attr"`
	NameFormat   string   `xml:"NameFormat,attr,omitempty"`
	FriendlyName string   `xml:"FriendlyName,attr,omitempty"`
	Values       []string `xml:"AttributeValue"`
}

// SAMLUser represents a user extracted from SAML
type SAMLUser struct {
	NameID      string
	Email       string
	FirstName   string
	LastName    string
	DisplayName string
	Groups      []string
	Attributes  map[string][]string
}

// SAMLAuthResult contains the result of SAML authentication
type SAMLAuthResult struct {
	Success     bool
	NameID      string
	SessionID   string
	Email       string
	DisplayName string
	FirstName   string
	LastName    string
	Groups      []string
	Attributes  map[string][]string
	Error       string
}

// SAMLProvider handles SAML authentication
type SAMLProvider struct {
	config *SAMLConfig
}

// NewSAMLProvider creates a new SAML provider
func NewSAMLProvider(config *SAMLConfig) (*SAMLProvider, error) {
	if config == nil {
		return nil, errors.New("saml: config is required")
	}

	config.ApplyDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &SAMLProvider{
		config: config,
	}, nil
}

// GenerateAuthnRequest creates a new SAML AuthnRequest
func (p *SAMLProvider) GenerateAuthnRequest() (*SAMLAuthnRequest, error) {
	id := "_" + uuid.New().String()

	request := &SAMLAuthnRequest{
		XMLNS:                       "urn:oasis:names:tc:SAML:2.0:protocol",
		XMLNSSAML:                   "urn:oasis:names:tc:SAML:2.0:assertion",
		ID:                          id,
		Version:                     "2.0",
		IssueInstant:                time.Now().UTC(),
		Destination:                 p.config.IDPSSOURL,
		AssertionConsumerServiceURL: p.config.ACSURL,
		ProtocolBinding:             "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST",
		Issuer:                      p.config.EntityID,
	}

	if p.config.NameIDFormat != "" {
		request.NameIDPolicy = &NameIDPolicy{
			Format:      p.config.NameIDFormat,
			AllowCreate: true,
		}
	}

	return request, nil
}

// BuildRedirectURL builds the SSO redirect URL
func (p *SAMLProvider) BuildRedirectURL(relayState string) (string, error) {
	request, err := p.GenerateAuthnRequest()
	if err != nil {
		return "", fmt.Errorf("saml: failed to generate request: %w", err)
	}

	// Marshal to XML
	xmlData, err := xml.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("saml: failed to marshal request: %w", err)
	}

	// Deflate compress
	var compressed strings.Builder
	writer, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		return "", fmt.Errorf("saml: failed to create compressor: %w", err)
	}
	_, err = writer.Write(xmlData)
	if err != nil {
		return "", fmt.Errorf("saml: failed to compress: %w", err)
	}
	_ = writer.Close()

	// Base64 encode
	encoded := base64.StdEncoding.EncodeToString([]byte(compressed.String()))

	// Build URL
	redirectURL, err := url.Parse(p.config.IDPSSOURL)
	if err != nil {
		return "", fmt.Errorf("saml: invalid IDP SSO URL: %w", err)
	}

	query := redirectURL.Query()
	query.Set("SAMLRequest", encoded)
	if relayState != "" {
		query.Set("RelayState", relayState)
	}
	redirectURL.RawQuery = query.Encode()

	return redirectURL.String(), nil
}

// ParseResponse parses a SAML response
func (p *SAMLProvider) ParseResponse(samlResponse string) (*SAMLResponse, error) {
	if samlResponse == "" {
		return nil, errors.New("saml: empty response")
	}

	// Base64 decode
	decoded, err := base64.StdEncoding.DecodeString(samlResponse)
	if err != nil {
		return nil, fmt.Errorf("saml: invalid base64: %w", err)
	}

	// Parse XML
	var response SAMLResponse
	if err := xml.Unmarshal(decoded, &response); err != nil {
		return nil, fmt.Errorf("saml: invalid XML: %w", err)
	}

	return &response, nil
}

// ValidateAssertion validates a SAML assertion
func (p *SAMLProvider) ValidateAssertion(assertion *SAMLAssertion) error {
	now := time.Now()
	clockSkew := p.config.ClockSkew

	// Check NotBefore
	if !assertion.Conditions.NotBefore.IsZero() {
		if now.Add(clockSkew).Before(assertion.Conditions.NotBefore) {
			return errors.New("saml: assertion not yet valid")
		}
	}

	// Check NotOnOrAfter
	if !assertion.Conditions.NotOnOrAfter.IsZero() {
		if now.Add(-clockSkew).After(assertion.Conditions.NotOnOrAfter) {
			return errors.New("saml: assertion expired")
		}
	}

	// Check audience
	audience := assertion.Conditions.AudienceRestriction.Audience
	if audience != "" && audience != p.config.EntityID {
		return fmt.Errorf("saml: invalid audience: expected %s, got %s", p.config.EntityID, audience)
	}

	return nil
}

// ExtractAttributes extracts user attributes from SAML attributes
func (p *SAMLProvider) ExtractAttributes(attrs []SAMLAttribute) *SAMLUser {
	user := &SAMLUser{
		Groups:     make([]string, 0),
		Attributes: make(map[string][]string),
	}

	mapping := p.config.AttributeMapping

	for _, attr := range attrs {
		// Store all attributes
		user.Attributes[attr.Name] = attr.Values

		// Map known attributes
		if len(attr.Values) > 0 {
			switch attr.Name {
			case mapping.Email:
				user.Email = attr.Values[0]
			case mapping.FirstName:
				user.FirstName = attr.Values[0]
			case mapping.LastName:
				user.LastName = attr.Values[0]
			case mapping.DisplayName:
				user.DisplayName = attr.Values[0]
			case mapping.Groups:
				user.Groups = append(user.Groups, attr.Values...)
			}
		}
	}

	return user
}

// GenerateMetadata generates SP metadata XML
func (p *SAMLProvider) GenerateMetadata() string {
	var sb strings.Builder

	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="`)
	sb.WriteString(p.config.EntityID)
	sb.WriteString(`">`)
	sb.WriteString(`<md:SPSSODescriptor AuthnRequestsSigned="`)
	if p.config.SignRequests {
		sb.WriteString("true")
	} else {
		sb.WriteString("false")
	}
	sb.WriteString(`" WantAssertionsSigned="`)
	if p.config.RequireSignedAssertion {
		sb.WriteString("true")
	} else {
		sb.WriteString("false")
	}
	sb.WriteString(`" protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">`)

	if p.config.CertificatePEM != "" {
		sb.WriteString(`<md:KeyDescriptor use="signing"><ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:X509Data><ds:X509Certificate>`)
		// Strip PEM headers
		cert := strings.ReplaceAll(p.config.CertificatePEM, "-----BEGIN CERTIFICATE-----", "")
		cert = strings.ReplaceAll(cert, "-----END CERTIFICATE-----", "")
		cert = strings.ReplaceAll(cert, "\n", "")
		sb.WriteString(cert)
		sb.WriteString(`</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>`)
	}

	sb.WriteString(`<md:NameIDFormat>`)
	sb.WriteString(p.config.NameIDFormat)
	sb.WriteString(`</md:NameIDFormat>`)

	sb.WriteString(`<md:AssertionConsumerService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="`)
	sb.WriteString(p.config.ACSURL)
	sb.WriteString(`" index="0" isDefault="true"/>`)

	if p.config.SLOURL != "" {
		sb.WriteString(`<md:SingleLogoutService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="`)
		sb.WriteString(p.config.SLOURL)
		sb.WriteString(`"/>`)
	}

	sb.WriteString(`</md:SPSSODescriptor>`)
	sb.WriteString(`</md:EntityDescriptor>`)

	return sb.String()
}

// NameIDFormat returns the configured NameID format
func (p *SAMLProvider) NameIDFormat() string {
	if p.config.NameIDFormat == "" {
		return NameIDFormatUnspecified
	}
	return p.config.NameIDFormat
}

// SignsRequests returns whether requests are signed
func (p *SAMLProvider) SignsRequests() bool {
	return p.config.SignRequests
}

// RequiresSignedResponses returns whether signed responses are required
func (p *SAMLProvider) RequiresSignedResponses() bool {
	return p.config.RequireSignedResponses
}

// Info returns provider information
func (p *SAMLProvider) Info() ProviderInfo {
	return ProviderInfo{
		Type: "saml",
		Host: p.config.IDPSSOURL,
	}
}

// Ensure io import is used
var _ = io.Discard
