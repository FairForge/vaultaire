package usage

import (
	"context"
	"fmt"
	"sync"
)

// Quota represents storage limits for a user
type Quota struct {
	UserID            string
	MaxStorageBytes   int64
	MaxObjects        int64
	MaxBandwidthMonth int64
}

// QuotaEnforcer enforces storage quotas
type QuotaEnforcer struct {
	quotas map[string]*Quota
	mu     sync.RWMutex
}

// NewQuotaEnforcer creates a new quota enforcer
func NewQuotaEnforcer() *QuotaEnforcer {
	return &QuotaEnforcer{
		quotas: make(map[string]*Quota),
	}
}

// SetQuota sets quota for a user
func (q *QuotaEnforcer) SetQuota(userID string, quota *Quota) {
	q.mu.Lock()
	defer q.mu.Unlock()
	
	quota.UserID = userID
	q.quotas[userID] = quota
}

// GetQuota returns quota for a user
func (q *QuotaEnforcer) GetQuota(userID string) *Quota {
	q.mu.RLock()
	defer q.mu.RUnlock()
	
	quota, exists := q.quotas[userID]
	if !exists {
		return q.GetDefaultQuota()
	}
	
	return quota
}

// GetDefaultQuota returns the default free tier quota
func (q *QuotaEnforcer) GetDefaultQuota() *Quota {
	return &Quota{
		MaxStorageBytes:   5 * 1024 * 1024 * 1024, // 5GB
		MaxObjects:        10000,                   // 10k objects
		MaxBandwidthMonth: 50 * 1024 * 1024 * 1024, // 50GB/month
	}
}

// CheckQuota checks if usage is within quota
func (q *QuotaEnforcer) CheckQuota(ctx context.Context, userID string, usage *Usage) (bool, error) {
	quota := q.GetQuota(userID)
	
	// Check storage limit
	if usage.StorageBytes > quota.MaxStorageBytes {
		return false, fmt.Errorf("storage quota exceeded: %d > %d", usage.StorageBytes, quota.MaxStorageBytes)
	}
	
	// Check object count limit
	if usage.ObjectCount > quota.MaxObjects {
		return false, fmt.Errorf("object quota exceeded: %d > %d", usage.ObjectCount, quota.MaxObjects)
	}
	
	// Check bandwidth limit
	totalBandwidth := usage.BandwidthUpload + usage.BandwidthDownload
	if totalBandwidth > quota.MaxBandwidthMonth {
		return false, fmt.Errorf("bandwidth quota exceeded: %d > %d", totalBandwidth, quota.MaxBandwidthMonth)
	}
	
	return true, nil
}

// CanUpload checks if a user can upload a file of given size
func (q *QuotaEnforcer) CanUpload(ctx context.Context, userID string, usage *Usage, fileSize int64) (bool, error) {
	quota := q.GetQuota(userID)
	
	// Check if adding this file would exceed storage
	if usage.StorageBytes+fileSize > quota.MaxStorageBytes {
		return false, fmt.Errorf("upload would exceed storage quota")
	}
	
	// Check if adding this object would exceed count
	if usage.ObjectCount+1 > quota.MaxObjects {
		return false, fmt.Errorf("upload would exceed object count quota")
	}
	
	return true, nil
}

// GetUsagePercentage returns usage as percentage of quota
func (q *QuotaEnforcer) GetUsagePercentage(userID string, usage *Usage) map[string]float64 {
	quota := q.GetQuota(userID)
	
	return map[string]float64{
		"storage":   float64(usage.StorageBytes) / float64(quota.MaxStorageBytes) * 100,
		"objects":   float64(usage.ObjectCount) / float64(quota.MaxObjects) * 100,
		"bandwidth": float64(usage.BandwidthUpload+usage.BandwidthDownload) / float64(quota.MaxBandwidthMonth) * 100,
	}
}
