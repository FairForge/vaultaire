// internal/auth/serviceaccount.go
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Valid name pattern: lowercase letters, numbers, hyphens
var serviceAccountNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// ServiceAccountConfig configures a service account
type ServiceAccountConfig struct {
	ID          string     `json:"id,omitempty"`
	Name        string     `json:"name"`
	TenantID    string     `json:"tenant_id"`
	Description string     `json:"description,omitempty"`
	Roles       []string   `json:"roles,omitempty"`
	IPAllowlist []string   `json:"ip_allowlist,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// Validate checks if the configuration is valid
func (c *ServiceAccountConfig) Validate() error {
	if c.Name == "" {
		return errors.New("serviceaccount: name is required")
	}
	if len(c.Name) < 3 || len(c.Name) > 63 {
		return errors.New("serviceaccount: name must be 3-63 characters")
	}
	if !serviceAccountNameRegex.MatchString(c.Name) {
		return errors.New("serviceaccount: name must contain only lowercase letters, numbers, and hyphens")
	}
	if c.TenantID == "" {
		return errors.New("serviceaccount: tenant ID is required")
	}
	return nil
}

// ServiceAccount represents a service account
type ServiceAccount struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TenantID    string     `json:"tenant_id"`
	Description string     `json:"description,omitempty"`
	Roles       []string   `json:"roles,omitempty"`
	IPAllowlist []string   `json:"ip_allowlist,omitempty"`
	Enabled     bool       `json:"enabled"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// IsExpired checks if the account has expired
func (sa *ServiceAccount) IsExpired() bool {
	if sa.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*sa.ExpiresAt)
}

// ServiceAccountCredentials contains the key ID and secret
type ServiceAccountCredentials struct {
	KeyID     string    `json:"key_id"`
	KeySecret string    `json:"key_secret"`
	CreatedAt time.Time `json:"created_at"`
	AccountID string    `json:"account_id"`
}

// ServiceAccountAuthResult contains authentication result
type ServiceAccountAuthResult struct {
	Success   bool
	AccountID string
	TenantID  string
	Name      string
	Roles     []string
}

// storedCredentials stores hashed credentials
type storedCredentials struct {
	KeyID      string
	SecretHash []byte
	AccountID  string
	CreatedAt  time.Time
}

// ServiceAccountManager manages service accounts
type ServiceAccountManager struct {
	accounts    map[string]*ServiceAccount    // ID -> Account
	credentials map[string]*storedCredentials // KeyID -> Credentials
	byName      map[string]map[string]string  // TenantID -> Name -> ID
	mu          sync.RWMutex
}

// NewServiceAccountManager creates a new service account manager
func NewServiceAccountManager() *ServiceAccountManager {
	return &ServiceAccountManager{
		accounts:    make(map[string]*ServiceAccount),
		credentials: make(map[string]*storedCredentials),
		byName:      make(map[string]map[string]string),
	}
}

// Create creates a new service account with credentials
func (m *ServiceAccountManager) Create(ctx context.Context, config *ServiceAccountConfig) (*ServiceAccount, *ServiceAccountCredentials, error) {
	if err := config.Validate(); err != nil {
		return nil, nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check for duplicate name in tenant
	if tenantNames, exists := m.byName[config.TenantID]; exists {
		if _, nameExists := tenantNames[config.Name]; nameExists {
			return nil, nil, fmt.Errorf("serviceaccount: name %q already exists in tenant", config.Name)
		}
	}

	// Generate ID
	id := "sa-" + uuid.New().String()[:8]

	// Create account
	now := time.Now().UTC()
	account := &ServiceAccount{
		ID:          id,
		Name:        config.Name,
		TenantID:    config.TenantID,
		Description: config.Description,
		Roles:       config.Roles,
		IPAllowlist: config.IPAllowlist,
		Enabled:     true,
		ExpiresAt:   config.ExpiresAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Generate credentials
	credentials, storedCreds, err := m.generateCredentials(id)
	if err != nil {
		return nil, nil, err
	}

	// Store everything
	m.accounts[id] = account
	m.credentials[credentials.KeyID] = storedCreds

	if m.byName[config.TenantID] == nil {
		m.byName[config.TenantID] = make(map[string]string)
	}
	m.byName[config.TenantID][config.Name] = id

	return account, credentials, nil
}

// generateCredentials creates new credentials for an account
func (m *ServiceAccountManager) generateCredentials(accountID string) (*ServiceAccountCredentials, *storedCredentials, error) {
	// Generate key ID
	keyIDBytes := make([]byte, 16)
	if _, err := rand.Read(keyIDBytes); err != nil {
		return nil, nil, fmt.Errorf("serviceaccount: failed to generate key ID: %w", err)
	}
	keyID := base64.RawURLEncoding.EncodeToString(keyIDBytes)

	// Generate secret
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, nil, fmt.Errorf("serviceaccount: failed to generate secret: %w", err)
	}
	keySecret := base64.RawURLEncoding.EncodeToString(secretBytes)

	// Hash the secret for storage
	secretHash, err := bcrypt.GenerateFromPassword([]byte(keySecret), bcrypt.DefaultCost)
	if err != nil {
		return nil, nil, fmt.Errorf("serviceaccount: failed to hash secret: %w", err)
	}

	now := time.Now().UTC()

	credentials := &ServiceAccountCredentials{
		KeyID:     keyID,
		KeySecret: keySecret,
		CreatedAt: now,
		AccountID: accountID,
	}

	stored := &storedCredentials{
		KeyID:      keyID,
		SecretHash: secretHash,
		AccountID:  accountID,
		CreatedAt:  now,
	}

	return credentials, stored, nil
}

// Get retrieves a service account by ID
func (m *ServiceAccountManager) Get(id string) *ServiceAccount {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accounts[id]
}

// GetByName retrieves a service account by tenant and name
func (m *ServiceAccountManager) GetByName(tenantID, name string) *ServiceAccount {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if tenantNames, exists := m.byName[tenantID]; exists {
		if id, nameExists := tenantNames[name]; nameExists {
			return m.accounts[id]
		}
	}
	return nil
}

// ListByTenant returns all service accounts for a tenant
func (m *ServiceAccountManager) ListByTenant(tenantID string) []*ServiceAccount {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*ServiceAccount
	for _, account := range m.accounts {
		if account.TenantID == tenantID {
			result = append(result, account)
		}
	}
	return result
}

// Delete removes a service account
func (m *ServiceAccountManager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, exists := m.accounts[id]
	if !exists {
		return fmt.Errorf("serviceaccount: ID %s not found", id)
	}

	// Remove credentials
	for keyID, creds := range m.credentials {
		if creds.AccountID == id {
			delete(m.credentials, keyID)
		}
	}

	// Remove from name index
	if tenantNames, exists := m.byName[account.TenantID]; exists {
		delete(tenantNames, account.Name)
	}

	// Remove account
	delete(m.accounts, id)

	return nil
}

// Enable enables a service account
func (m *ServiceAccountManager) Enable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, exists := m.accounts[id]
	if !exists {
		return fmt.Errorf("serviceaccount: ID %s not found", id)
	}

	account.Enabled = true
	account.UpdatedAt = time.Now().UTC()
	return nil
}

// Disable disables a service account
func (m *ServiceAccountManager) Disable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, exists := m.accounts[id]
	if !exists {
		return fmt.Errorf("serviceaccount: ID %s not found", id)
	}

	account.Enabled = false
	account.UpdatedAt = time.Now().UTC()
	return nil
}

// Authenticate validates service account credentials
func (m *ServiceAccountManager) Authenticate(ctx context.Context, keyID, keySecret string) (*ServiceAccountAuthResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Find credentials
	creds, exists := m.credentials[keyID]
	if !exists {
		return nil, errors.New("serviceaccount: invalid credentials")
	}

	// Verify secret
	if err := bcrypt.CompareHashAndPassword(creds.SecretHash, []byte(keySecret)); err != nil {
		return nil, errors.New("serviceaccount: invalid credentials")
	}

	// Find account
	account, exists := m.accounts[creds.AccountID]
	if !exists {
		return nil, errors.New("serviceaccount: account not found")
	}

	// Check if enabled
	if !account.Enabled {
		return nil, errors.New("serviceaccount: account is disabled")
	}

	// Check expiration
	if account.IsExpired() {
		return nil, errors.New("serviceaccount: account has expired")
	}

	return &ServiceAccountAuthResult{
		Success:   true,
		AccountID: account.ID,
		TenantID:  account.TenantID,
		Name:      account.Name,
		Roles:     account.Roles,
	}, nil
}

// RotateCredentials generates new credentials for an account
func (m *ServiceAccountManager) RotateCredentials(ctx context.Context, id string) (*ServiceAccountCredentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, exists := m.accounts[id]
	if !exists {
		return nil, fmt.Errorf("serviceaccount: ID %s not found", id)
	}

	// Remove old credentials
	for keyID, creds := range m.credentials {
		if creds.AccountID == id {
			delete(m.credentials, keyID)
		}
	}

	// Generate new credentials
	credentials, storedCreds, err := m.generateCredentials(id)
	if err != nil {
		return nil, err
	}

	m.credentials[credentials.KeyID] = storedCreds
	account.UpdatedAt = time.Now().UTC()

	return credentials, nil
}

// UpdateRoles updates the roles for a service account
func (m *ServiceAccountManager) UpdateRoles(id string, roles []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, exists := m.accounts[id]
	if !exists {
		return fmt.Errorf("serviceaccount: ID %s not found", id)
	}

	account.Roles = roles
	account.UpdatedAt = time.Now().UTC()
	return nil
}

// IsIPAllowed checks if an IP is allowed for the service account
func (m *ServiceAccountManager) IsIPAllowed(id string, ipStr string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	account, exists := m.accounts[id]
	if !exists {
		return false
	}

	// Empty allowlist means all IPs allowed
	if len(account.IPAllowlist) == 0 {
		return true
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	for _, cidrStr := range account.IPAllowlist {
		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			continue
		}
		if cidr.Contains(ip) {
			return true
		}
	}

	return false
}

// UpdateIPAllowlist updates the IP allowlist for a service account
func (m *ServiceAccountManager) UpdateIPAllowlist(id string, allowlist []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, exists := m.accounts[id]
	if !exists {
		return fmt.Errorf("serviceaccount: ID %s not found", id)
	}

	// Validate CIDR notation
	for _, cidr := range allowlist {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("serviceaccount: invalid CIDR %q: %w", cidr, err)
		}
	}

	account.IPAllowlist = allowlist
	account.UpdatedAt = time.Now().UTC()
	return nil
}
