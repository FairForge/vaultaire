// internal/auth/ldap.go
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LDAPConfig configures the LDAP authentication provider
type LDAPConfig struct {
	// Connection settings
	Host               string        `json:"host"`
	Port               int           `json:"port"`
	UseTLS             bool          `json:"use_tls"`
	UseStartTLS        bool          `json:"use_starttls"`
	InsecureSkipVerify bool          `json:"insecure_skip_verify"`
	Timeout            time.Duration `json:"timeout"`
	MaxConnections     int           `json:"max_connections"`

	// Bind credentials (for searching)
	BindDN       string `json:"bind_dn"`
	BindPassword string `json:"bind_password"`

	// Search settings
	BaseDN         string `json:"base_dn"`
	UserSearchBase string `json:"user_search_base"`
	UserAttribute  string `json:"user_attribute"`
	SearchFilter   string `json:"search_filter"`

	// Attribute mapping
	AttributeMapping AttributeMapping `json:"attribute_mapping"`
}

// AttributeMapping maps LDAP attributes to user fields
type AttributeMapping struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Groups      string `json:"groups"`
	Phone       string `json:"phone"`
}

// DefaultLDAPConfig returns a config with sensible defaults
func DefaultLDAPConfig() *LDAPConfig {
	return &LDAPConfig{
		Port:           389,
		UserAttribute:  "cn",
		Timeout:        30 * time.Second,
		MaxConnections: 10,
		AttributeMapping: AttributeMapping{
			Email:       "mail",
			DisplayName: "displayName",
			FirstName:   "givenName",
			LastName:    "sn",
			Groups:      "memberOf",
		},
	}
}

// Validate checks if the configuration is valid
func (c *LDAPConfig) Validate() error {
	if c.Host == "" {
		return errors.New("ldap: host is required")
	}
	if c.BaseDN == "" {
		return errors.New("ldap: base DN is required")
	}
	return nil
}

// ApplyDefaults fills in default values for unset fields
func (c *LDAPConfig) ApplyDefaults() {
	defaults := DefaultLDAPConfig()

	if c.Port == 0 {
		if c.UseTLS {
			c.Port = 636
		} else {
			c.Port = defaults.Port
		}
	}
	if c.UserAttribute == "" {
		c.UserAttribute = defaults.UserAttribute
	}
	if c.Timeout == 0 {
		c.Timeout = defaults.Timeout
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = defaults.MaxConnections
	}
	if c.AttributeMapping.Email == "" {
		c.AttributeMapping = defaults.AttributeMapping
	}
}

// LDAPUser represents a user retrieved from LDAP
type LDAPUser struct {
	DN          string
	Username    string
	Email       string
	DisplayName string
	FirstName   string
	LastName    string
	Groups      []string
	RawAttrs    map[string][]string
}

// LDAPAuthResult contains the result of an authentication attempt
type LDAPAuthResult struct {
	Success     bool
	UserDN      string
	Username    string
	Email       string
	DisplayName string
	Groups      []string
	Error       string
}

// ProviderInfo contains information about the LDAP provider
type ProviderInfo struct {
	Type string
	Host string
	Port int
}

// LDAPProvider handles LDAP authentication
type LDAPProvider struct {
	config *LDAPConfig
}

// NewLDAPProvider creates a new LDAP authentication provider
func NewLDAPProvider(config *LDAPConfig) (*LDAPProvider, error) {
	if config == nil {
		return nil, errors.New("ldap: config is required")
	}

	config.ApplyDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &LDAPProvider{
		config: config,
	}, nil
}

// BuildUserDN constructs the full DN for a user
func (p *LDAPProvider) BuildUserDN(username string) string {
	attr := p.config.UserAttribute
	if attr == "" {
		attr = "cn"
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("%s=%s", attr, username))

	if p.config.UserSearchBase != "" {
		parts = append(parts, p.config.UserSearchBase)
	}

	parts = append(parts, p.config.BaseDN)

	return strings.Join(parts, ",")
}

// BuildSearchFilter constructs the LDAP search filter for a user
func (p *LDAPProvider) BuildSearchFilter(username string) string {
	escapedUsername := EscapeLDAPFilter(username)

	if p.config.SearchFilter != "" {
		return fmt.Sprintf(p.config.SearchFilter, escapedUsername)
	}

	attr := p.config.UserAttribute
	if attr == "" {
		attr = "cn"
	}

	return fmt.Sprintf("(%s=%s)", attr, escapedUsername)
}

// MapAttributes converts LDAP attributes to an LDAPUser
func (p *LDAPProvider) MapAttributes(attrs map[string][]string) *LDAPUser {
	user := &LDAPUser{
		RawAttrs: attrs,
	}

	mapping := p.config.AttributeMapping

	if values, ok := attrs[mapping.Email]; ok && len(values) > 0 {
		user.Email = values[0]
	}
	if values, ok := attrs[mapping.DisplayName]; ok && len(values) > 0 {
		user.DisplayName = values[0]
	}
	if values, ok := attrs[mapping.FirstName]; ok && len(values) > 0 {
		user.FirstName = values[0]
	}
	if values, ok := attrs[mapping.LastName]; ok && len(values) > 0 {
		user.LastName = values[0]
	}
	if values, ok := attrs[mapping.Groups]; ok {
		user.Groups = p.ExtractGroupNames(values)
	}

	return user
}

// ExtractGroupNames extracts group names from full DNs
func (p *LDAPProvider) ExtractGroupNames(groups []string) []string {
	result := make([]string, 0, len(groups))

	for _, group := range groups {
		name := extractCNFromDN(group)
		if name != "" {
			result = append(result, name)
		}
	}

	return result
}

// extractCNFromDN extracts the CN value from a full DN
func extractCNFromDN(dn string) string {
	// If it's not a DN, return as-is
	if !strings.Contains(dn, "=") {
		return dn
	}

	parts := strings.Split(dn, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "cn=") {
			return strings.TrimPrefix(part, "cn=")
		}
	}

	return dn
}

// MaxConnections returns the maximum number of connections
func (p *LDAPProvider) MaxConnections() int {
	if p.config.MaxConnections == 0 {
		return 10
	}
	return p.config.MaxConnections
}

// Timeout returns the connection timeout
func (p *LDAPProvider) Timeout() time.Duration {
	if p.config.Timeout == 0 {
		return 30 * time.Second
	}
	return p.config.Timeout
}

// UsesTLS returns whether TLS is enabled
func (p *LDAPProvider) UsesTLS() bool {
	return p.config.UseTLS
}

// UsesStartTLS returns whether StartTLS is enabled
func (p *LDAPProvider) UsesStartTLS() bool {
	return p.config.UseStartTLS
}

// SkipsVerify returns whether certificate verification is skipped
func (p *LDAPProvider) SkipsVerify() bool {
	return p.config.InsecureSkipVerify
}

// Info returns information about the provider
func (p *LDAPProvider) Info() ProviderInfo {
	return ProviderInfo{
		Type: "ldap",
		Host: p.config.Host,
		Port: p.config.Port,
	}
}

// Authenticate attempts to authenticate a user against LDAP
func (p *LDAPProvider) Authenticate(ctx context.Context, username, password string) (*LDAPAuthResult, error) {
	if username == "" {
		return nil, errors.New("ldap: username is required")
	}
	if password == "" {
		return nil, errors.New("ldap: password is required")
	}

	// Note: Actual LDAP connection would go here
	// For now, return error indicating no real connection
	// In production, use github.com/go-ldap/ldap/v3
	return nil, errors.New("ldap: connection not implemented (requires go-ldap/ldap)")
}

// EscapeLDAPFilter escapes special characters in LDAP filter values
// Per RFC 4515, these characters must be escaped:
// * ( ) \ NUL
func EscapeLDAPFilter(s string) string {
	var buf strings.Builder
	buf.Grow(len(s) * 2)

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			buf.WriteString("\\5c")
		case '*':
			buf.WriteString("\\2a")
		case '(':
			buf.WriteString("\\28")
		case ')':
			buf.WriteString("\\29")
		case '\x00':
			buf.WriteString("\\00")
		default:
			buf.WriteByte(c)
		}
	}

	return buf.String()
}

// GetSearchAttributes returns the list of attributes to request from LDAP
func (p *LDAPProvider) GetSearchAttributes() []string {
	attrs := []string{"dn"}
	mapping := p.config.AttributeMapping

	if mapping.Email != "" {
		attrs = append(attrs, mapping.Email)
	}
	if mapping.DisplayName != "" {
		attrs = append(attrs, mapping.DisplayName)
	}
	if mapping.FirstName != "" {
		attrs = append(attrs, mapping.FirstName)
	}
	if mapping.LastName != "" {
		attrs = append(attrs, mapping.LastName)
	}
	if mapping.Groups != "" {
		attrs = append(attrs, mapping.Groups)
	}
	if mapping.Phone != "" {
		attrs = append(attrs, mapping.Phone)
	}

	return attrs
}

// ConnectionString returns the LDAP connection string
func (p *LDAPProvider) ConnectionString() string {
	scheme := "ldap"
	if p.config.UseTLS {
		scheme = "ldaps"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, p.config.Host, p.config.Port)
}
