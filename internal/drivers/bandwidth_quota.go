package drivers

import (
	"sync"
	"time"
)

// BandwidthQuota manages egress bandwidth quotas per tenant
type BandwidthQuota struct {
	mu           sync.RWMutex
	defaultQuota int64            // Default monthly quota in bytes
	tenantQuotas map[string]int64 // Custom quotas per tenant
	usage        map[string]int64 // Current month usage
	resetTime    time.Time        // When to reset quotas
}

// NewBandwidthQuota creates a new bandwidth quota manager
func NewBandwidthQuota(defaultQuotaBytes int64) *BandwidthQuota {
	bq := &BandwidthQuota{
		defaultQuota: defaultQuotaBytes,
		tenantQuotas: make(map[string]int64),
		usage:        make(map[string]int64),
		resetTime:    time.Now().AddDate(0, 1, 0), // Next month
	}

	// Start monthly reset timer
	go bq.startResetTimer()

	return bq
}

// AllowEgress checks if tenant can use bandwidth and updates usage
func (bq *BandwidthQuota) AllowEgress(tenantID string, bytes int64) bool {
	bq.mu.Lock()
	defer bq.mu.Unlock()

	// Check if we need to reset
	if time.Now().After(bq.resetTime) {
		bq.resetUsage()
	}

	quota := bq.getQuotaForTenant(tenantID)
	currentUsage := bq.usage[tenantID]

	if currentUsage+bytes > quota {
		return false // Would exceed quota
	}

	bq.usage[tenantID] = currentUsage + bytes
	return true
}

// GetRemaining returns remaining bandwidth for tenant
func (bq *BandwidthQuota) GetRemaining(tenantID string) int64 {
	bq.mu.RLock()
	defer bq.mu.RUnlock()

	quota := bq.getQuotaForTenant(tenantID)
	used := bq.usage[tenantID]

	if remaining := quota - used; remaining > 0 {
		return remaining
	}
	return 0
}

// SetTenantQuota sets a custom quota for a specific tenant
func (bq *BandwidthQuota) SetTenantQuota(tenantID string, bytes int64) {
	bq.mu.Lock()
	defer bq.mu.Unlock()
	bq.tenantQuotas[tenantID] = bytes
}

// GetUsage returns current usage for tenant
func (bq *BandwidthQuota) GetUsage(tenantID string) int64 {
	bq.mu.RLock()
	defer bq.mu.RUnlock()
	return bq.usage[tenantID]
}

// Reset manually resets all usage (for testing)
func (bq *BandwidthQuota) Reset() {
	bq.mu.Lock()
	defer bq.mu.Unlock()
	bq.resetUsage()
}

// Internal methods

func (bq *BandwidthQuota) getQuotaForTenant(tenantID string) int64 {
	if quota, exists := bq.tenantQuotas[tenantID]; exists {
		return quota
	}
	return bq.defaultQuota
}

func (bq *BandwidthQuota) resetUsage() {
	bq.usage = make(map[string]int64)
	bq.resetTime = time.Now().AddDate(0, 1, 0)
}

func (bq *BandwidthQuota) startResetTimer() {
	for {
		now := time.Now()
		nextReset := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
		time.Sleep(time.Until(nextReset))

		bq.mu.Lock()
		bq.resetUsage()
		bq.mu.Unlock()
	}
}
