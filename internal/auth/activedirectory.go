// internal/auth/activedirectory.go
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Active Directory userAccountControl flags
const (
	UACDisabled             = 0x0002
	UACLockout              = 0x0010
	UACPasswordNotRequired  = 0x0020
	UACNormalAccount        = 0x0200
	UACDontExpirePassword   = 0x10000
	UACPasswordNeverExpires = 0x10000
	UACPasswordExpired      = 0x800000
)

// ADNestedGroupOID is the OID for LDAP_MATCHING_RULE_IN_CHAIN
const ADNestedGroupOID = "1.2.840.113556.1.4.1941"

// ADConfig configures the Active Directory authentication provider
type ADConfig struct {
	// Domain settings
	Domain string `json:"domain"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	BaseDN string `json:"base_dn"`

	// Connection settings
	UseTLS             bool          `json:"use_tls"`
	UseStartTLS        bool          `json:"use_starttls"`
	InsecureSkipVerify bool          `json:"insecure_skip_verify"`
	Timeout            time.Duration `json:"timeout"`
	MaxConnections     int           `json:"max_connections"`

	// Bind credentials
	BindUser     string `json:"bind_user"`
	BindPassword string `json:"bind_password"`

	// Search settings
	UserSearchBase string `json:"user_search_base"`
	UserAttribute  string `json:"user_attribute"`
	SearchFilter   string `json:"search_filter"`

	// AD-specific settings
	UseGlobalCatalog    bool `json:"use_global_catalog"`
	ResolveNestedGroups bool `json:"resolve_nested_groups"`
}

// DefaultADConfig returns a config with AD-specific defaults
func DefaultADConfig() *ADConfig {
	return &ADConfig{
		Port:           389,
		UserAttribute:  "sAMAccountName",
		Timeout:        30 * time.Second,
		MaxConnections: 10,
	}
}

// Validate checks if the configuration is valid
func (c *ADConfig) Validate() error {
	if c.Domain == "" {
		return errors.New("ad: domain is required")
	}
	if c.Host == "" && c.Domain == "" {
		return errors.New("ad: host or domain is required")
	}
	return nil
}

// ApplyDefaults fills in default values for unset fields
func (c *ADConfig) ApplyDefaults() {
	defaults := DefaultADConfig()

	if c.Port == 0 {
		if c.UseGlobalCatalog {
			if c.UseTLS {
				c.Port = 3269
			} else {
				c.Port = 3268
			}
		} else if c.UseTLS {
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

	// Auto-generate BaseDN from domain
	if c.BaseDN == "" && c.Domain != "" {
		c.BaseDN = domainToBaseDN(c.Domain)
	}

	// Use domain as host if not specified
	if c.Host == "" && c.Domain != "" {
		c.Host = c.Domain
	}
}

// domainToBaseDN converts a domain to an LDAP base DN
func domainToBaseDN(domain string) string {
	parts := strings.Split(domain, ".")
	dcParts := make([]string, len(parts))
	for i, part := range parts {
		dcParts[i] = "dc=" + part
	}
	return strings.Join(dcParts, ",")
}

// ADUser represents a user retrieved from Active Directory
type ADUser struct {
	DN              string
	SAMAccountName  string
	UPN             string
	Email           string
	DisplayName     string
	FirstName       string
	LastName        string
	Department      string
	Title           string
	Manager         string
	Phone           string
	Groups          []string
	UserAccountCtrl int
	PasswordLastSet time.Time
	RawAttrs        map[string][]string
}

// ADAuthResult contains the result of an AD authentication attempt
type ADAuthResult struct {
	Success        bool
	SAMAccountName string
	UPN            string
	DN             string
	Email          string
	DisplayName    string
	Department     string
	Title          string
	Groups         []string
	Error          string
}

// ADProviderInfo contains information about the AD provider
type ADProviderInfo struct {
	Type   string
	Domain string
	Host   string
	Port   int
}

// ADProvider handles Active Directory authentication
type ADProvider struct {
	config *ADConfig
}

// NewADProvider creates a new Active Directory authentication provider
func NewADProvider(config *ADConfig) (*ADProvider, error) {
	if config == nil {
		return nil, errors.New("ad: config is required")
	}

	config.ApplyDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &ADProvider{
		config: config,
	}, nil
}

// BuildUPN constructs a User Principal Name
func (p *ADProvider) BuildUPN(username string) string {
	// If already a UPN, return as-is
	if strings.Contains(username, "@") {
		return username
	}
	return fmt.Sprintf("%s@%s", username, p.config.Domain)
}

// BuildSearchFilter constructs the AD search filter for a user
func (p *ADProvider) BuildSearchFilter(username string) string {
	escaped := EscapeLDAPFilter(username)

	// If custom filter is specified, use it
	if p.config.SearchFilter != "" {
		return fmt.Sprintf(p.config.SearchFilter, escaped)
	}

	// Check if it's a UPN
	if strings.Contains(username, "@") {
		return fmt.Sprintf("(&(objectClass=user)(userPrincipalName=%s))", escaped)
	}

	// Default: search by sAMAccountName
	return fmt.Sprintf("(&(objectClass=user)(sAMAccountName=%s))", escaped)
}

// BuildNestedGroupFilter constructs filter for nested group membership
func (p *ADProvider) BuildNestedGroupFilter(userDN string) string {
	escaped := EscapeLDAPFilter(userDN)
	return fmt.Sprintf("(member:%s:=%s)", ADNestedGroupOID, escaped)
}

// MapAttributes converts AD attributes to an ADUser
func (p *ADProvider) MapAttributes(attrs map[string][]string) *ADUser {
	user := &ADUser{
		RawAttrs: attrs,
		Groups:   make([]string, 0),
	}

	if v, ok := attrs["sAMAccountName"]; ok && len(v) > 0 {
		user.SAMAccountName = v[0]
	}
	if v, ok := attrs["userPrincipalName"]; ok && len(v) > 0 {
		user.UPN = v[0]
	}
	if v, ok := attrs["mail"]; ok && len(v) > 0 {
		user.Email = v[0]
	}
	if v, ok := attrs["displayName"]; ok && len(v) > 0 {
		user.DisplayName = v[0]
	}
	if v, ok := attrs["givenName"]; ok && len(v) > 0 {
		user.FirstName = v[0]
	}
	if v, ok := attrs["sn"]; ok && len(v) > 0 {
		user.LastName = v[0]
	}
	if v, ok := attrs["department"]; ok && len(v) > 0 {
		user.Department = v[0]
	}
	if v, ok := attrs["title"]; ok && len(v) > 0 {
		user.Title = v[0]
	}
	if v, ok := attrs["manager"]; ok && len(v) > 0 {
		user.Manager = v[0]
	}
	if v, ok := attrs["telephoneNumber"]; ok && len(v) > 0 {
		user.Phone = v[0]
	}
	if v, ok := attrs["memberOf"]; ok {
		for _, group := range v {
			user.Groups = append(user.Groups, extractCNFromDN(group))
		}
	}

	return user
}

// IsAccountDisabled checks if the account is disabled
func (p *ADProvider) IsAccountDisabled(uac int) bool {
	return uac&UACDisabled != 0
}

// IsAccountLocked checks if the account is locked
func (p *ADProvider) IsAccountLocked(uac int) bool {
	return uac&UACLockout != 0
}

// IsPasswordExpired checks if the password has expired
func (p *ADProvider) IsPasswordExpired(uac int) bool {
	return uac&UACPasswordExpired != 0
}

// ParseADTimestamp converts Windows FILETIME to Go time.Time
// FILETIME is 100-nanosecond intervals since January 1, 1601 UTC
func (p *ADProvider) ParseADTimestamp(filetime int64) time.Time {
	if filetime == 0 || filetime == 0x7FFFFFFFFFFFFFFF {
		return time.Time{}
	}

	// Windows epoch: January 1, 1601
	// Unix epoch: January 1, 1970
	// Difference: 11644473600 seconds
	const epochDiff = 11644473600

	// Convert 100-nanosecond intervals to seconds
	seconds := filetime / 10000000
	unixSeconds := seconds - epochDiff

	return time.Unix(unixSeconds, 0).UTC()
}

// GetDCSRVRecord returns the DNS SRV record for DC discovery
func (p *ADProvider) GetDCSRVRecord() string {
	return fmt.Sprintf("_ldap._tcp.dc._msdcs.%s", p.config.Domain)
}

// GetSearchAttributes returns the list of attributes to request
func (p *ADProvider) GetSearchAttributes() []string {
	return []string{
		"dn",
		"sAMAccountName",
		"userPrincipalName",
		"mail",
		"displayName",
		"givenName",
		"sn",
		"department",
		"title",
		"manager",
		"telephoneNumber",
		"memberOf",
		"userAccountControl",
		"pwdLastSet",
	}
}

// Authenticate attempts to authenticate a user against AD
func (p *ADProvider) Authenticate(ctx context.Context, username, password string) (*ADAuthResult, error) {
	if username == "" {
		return nil, errors.New("ad: username is required")
	}
	if password == "" {
		return nil, errors.New("ad: password is required")
	}

	// Note: Actual AD connection would go here
	// For now, return error indicating no real connection
	return nil, errors.New("ad: connection not implemented (requires go-ldap/ldap)")
}

// Info returns information about the provider
func (p *ADProvider) Info() ADProviderInfo {
	return ADProviderInfo{
		Type:   "activedirectory",
		Domain: p.config.Domain,
		Host:   p.config.Host,
		Port:   p.config.Port,
	}
}

// ConnectionString returns the LDAP connection string
func (p *ADProvider) ConnectionString() string {
	scheme := "ldap"
	if p.config.UseTLS {
		scheme = "ldaps"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, p.config.Host, p.config.Port)
}

// UsesTLS returns whether TLS is enabled
func (p *ADProvider) UsesTLS() bool {
	return p.config.UseTLS
}

// UsesGlobalCatalog returns whether Global Catalog is used
func (p *ADProvider) UsesGlobalCatalog() bool {
	return p.config.UseGlobalCatalog
}

// ResolvesNestedGroups returns whether nested group resolution is enabled
func (p *ADProvider) ResolvesNestedGroups() bool {
	return p.config.ResolveNestedGroups
}
