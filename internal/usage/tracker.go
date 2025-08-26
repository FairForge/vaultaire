package usage

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// BandwidthType represents upload or download
type BandwidthType string

const (
	BandwidthTypeUpload   BandwidthType = "upload"
	BandwidthTypeDownload BandwidthType = "download"
)

// Usage represents a user's current usage
type Usage struct {
	UserID            string
	StorageBytes      int64
	ObjectCount       int64
	BandwidthUpload   int64
	BandwidthDownload int64
	LastUpdated       time.Time
}

// UsageTracker tracks storage and bandwidth usage
type UsageTracker struct {
	db    Database
	cache map[string]*Usage
	mu    sync.RWMutex
}

// Database interface for usage operations
type Database interface {
	// Will be implemented with PostgreSQL
	UpdateUsage(ctx context.Context, usage *Usage) error
	GetUsage(ctx context.Context, userID string) (*Usage, error)
}

// NewUsageTracker creates a new usage tracker
func NewUsageTracker(db Database) *UsageTracker {
	return &UsageTracker{
		db:    db,
		cache: make(map[string]*Usage),
	}
}

// RecordUpload records a file upload
func (u *UsageTracker) RecordUpload(ctx context.Context, userID, bucket, key string, size int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	usage, exists := u.cache[userID]
	if !exists {
		usage = &Usage{
			UserID: userID,
		}
		u.cache[userID] = usage
	}

	usage.StorageBytes += size
	usage.ObjectCount++
	usage.LastUpdated = time.Now()

	// TODO: Persist to database
	// if u.db != nil {
	//     return u.db.UpdateUsage(ctx, usage)
	// }

	return nil
}

// RecordDelete records a file deletion
func (u *UsageTracker) RecordDelete(ctx context.Context, userID, bucket, key string, size int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	usage, exists := u.cache[userID]
	if !exists {
		usage = &Usage{
			UserID: userID,
		}
		u.cache[userID] = usage
	}

	usage.StorageBytes -= size
	if usage.StorageBytes < 0 {
		usage.StorageBytes = 0
	}

	usage.ObjectCount--
	if usage.ObjectCount < 0 {
		usage.ObjectCount = 0
	}

	usage.LastUpdated = time.Now()

	// TODO: Persist to database

	return nil
}

// RecordBandwidth records bandwidth usage
func (u *UsageTracker) RecordBandwidth(ctx context.Context, userID string, bwType BandwidthType, bytes int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	usage, exists := u.cache[userID]
	if !exists {
		usage = &Usage{
			UserID: userID,
		}
		u.cache[userID] = usage
	}

	switch bwType {
	case BandwidthTypeUpload:
		usage.BandwidthUpload += bytes
	case BandwidthTypeDownload:
		usage.BandwidthDownload += bytes
	default:
		return fmt.Errorf("invalid bandwidth type: %s", bwType)
	}

	usage.LastUpdated = time.Now()

	// TODO: Persist to database

	return nil
}

// GetUsage returns current usage for a user
func (u *UsageTracker) GetUsage(ctx context.Context, userID string) (*Usage, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()

	usage, exists := u.cache[userID]
	if !exists {
		// Try database
		// if u.db != nil {
		//     return u.db.GetUsage(ctx, userID)
		// }

		return &Usage{
			UserID:      userID,
			LastUpdated: time.Now(),
		}, nil
	}

	return usage, nil
}

// ResetMonthlyBandwidth resets bandwidth counters (for monthly billing)
func (u *UsageTracker) ResetMonthlyBandwidth(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	for _, usage := range u.cache {
		usage.BandwidthUpload = 0
		usage.BandwidthDownload = 0
		usage.LastUpdated = time.Now()
	}

	// TODO: Persist to database

	return nil
}
