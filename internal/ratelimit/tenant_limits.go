// internal/ratelimit/tenant_limits.go
package ratelimit

import (
	"sync"

	"golang.org/x/time/rate"
)

// TenantLimiter manages per-tenant rate limit customization
type TenantLimiter struct {
	mu            sync.RWMutex
	defaultLimits map[string]*OperationConfig            // operation -> default config
	tenantLimits  map[string]map[string]*OperationConfig // tenant -> operation -> config
	tierLimits    map[string]map[string]*OperationConfig // tier -> operation -> config
	tenantTiers   map[string]string                      // tenant -> tier
	limiters      map[string]*rate.Limiter               // tenant:operation -> limiter
}

// NewTenantLimiter creates a new tenant-aware limiter
func NewTenantLimiter() *TenantLimiter {
	return &TenantLimiter{
		defaultLimits: make(map[string]*OperationConfig),
		tenantLimits:  make(map[string]map[string]*OperationConfig),
		tierLimits:    make(map[string]map[string]*OperationConfig),
		tenantTiers:   make(map[string]string),
		limiters:      make(map[string]*rate.Limiter),
	}
}

// SetDefaultLimit sets the default rate limit for an operation
func (tl *TenantLimiter) SetDefaultLimit(operation string, ratePerSecond, burst int) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	tl.defaultLimits[operation] = &OperationConfig{
		RatePerSecond: ratePerSecond,
		Burst:         burst,
	}
}

// SetTenantLimit sets a custom limit for a specific tenant
func (tl *TenantLimiter) SetTenantLimit(tenantID, operation string, ratePerSecond, burst int) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	if tl.tenantLimits[tenantID] == nil {
		tl.tenantLimits[tenantID] = make(map[string]*OperationConfig)
	}

	tl.tenantLimits[tenantID][operation] = &OperationConfig{
		RatePerSecond: ratePerSecond,
		Burst:         burst,
	}
}

// SetTierLimit sets limits for a tier
func (tl *TenantLimiter) SetTierLimit(tier, operation string, ratePerSecond, burst int) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	if tl.tierLimits[tier] == nil {
		tl.tierLimits[tier] = make(map[string]*OperationConfig)
	}

	tl.tierLimits[tier][operation] = &OperationConfig{
		RatePerSecond: ratePerSecond,
		Burst:         burst,
	}
}

// SetTenantTier assigns a tenant to a tier
func (tl *TenantLimiter) SetTenantTier(tenantID, tier string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	tl.tenantTiers[tenantID] = tier
}

// Allow checks if a tenant can perform an operation
func (tl *TenantLimiter) Allow(tenantID, operation string) bool {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	// Find the appropriate config (tenant-specific > tier > default)
	var config *OperationConfig

	// Check tenant-specific limit
	if tenantOps, exists := tl.tenantLimits[tenantID]; exists {
		if opConfig, exists := tenantOps[operation]; exists {
			config = opConfig
		}
	}

	// Check tier limit if no tenant-specific limit
	if config == nil {
		if tier, exists := tl.tenantTiers[tenantID]; exists {
			if tierOps, exists := tl.tierLimits[tier]; exists {
				if opConfig, exists := tierOps[operation]; exists {
					config = opConfig
				}
			}
		}
	}

	// Fall back to default limit
	if config == nil {
		config = tl.defaultLimits[operation]
	}

	// No limit configured
	if config == nil {
		return true
	}

	// Get or create limiter
	key := tenantID + ":" + operation
	limiter, exists := tl.limiters[key]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(config.RatePerSecond), config.Burst)
		tl.limiters[key] = limiter
	}

	return limiter.Allow()
}

// GetTenantTier returns the tier for a tenant
func (tl *TenantLimiter) GetTenantTier(tenantID string) (string, bool) {
	tl.mu.RLock()
	defer tl.mu.RUnlock()

	tier, exists := tl.tenantTiers[tenantID]
	return tier, exists
}
