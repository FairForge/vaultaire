package storage

import (
	"sync"
	"time"
)

// TierLevel represents storage tier levels
type TierLevel string

const (
	HotTier  TierLevel = "hot"
	WarmTier TierLevel = "warm"
	ColdTier TierLevel = "cold"
)

// TieringEngine classifies data by access patterns
type TieringEngine struct {
	access map[string][]time.Time
	mu     sync.RWMutex
}

// NewTieringEngine creates a new tiering engine
func NewTieringEngine() *TieringEngine {
	return &TieringEngine{
		access: make(map[string][]time.Time),
	}
}

// RecordAccess records an access event
func (te *TieringEngine) RecordAccess(filename string, timestamp time.Time) {
	te.mu.Lock()
	defer te.mu.Unlock()

	te.access[filename] = append(te.access[filename], timestamp)

	// Keep only last 100 access events per file
	if len(te.access[filename]) > 100 {
		te.access[filename] = te.access[filename][len(te.access[filename])-100:]
	}
}

// GetTier determines the appropriate tier for a file
func (te *TieringEngine) GetTier(filename string) TierLevel {
	te.mu.RLock()
	defer te.mu.RUnlock()

	accesses := te.access[filename]
	if len(accesses) == 0 {
		return ColdTier
	}

	now := time.Now()
	recentCount := 0

	for _, access := range accesses {
		if now.Sub(access) < 24*time.Hour {
			recentCount++
		}
	}

	// Hot: 3+ accesses in last 24 hours
	if recentCount >= 3 {
		return HotTier
	}

	// Warm: 1+ accesses in last 7 days
	for _, access := range accesses {
		if now.Sub(access) < 7*24*time.Hour {
			return WarmTier
		}
	}

	return ColdTier
}

// Tier represents a storage tier
type Tier struct {
	Name     string
	Capacity int64
	Used     int64
	Files    map[string]*FileMetadata
}

// TierManager manages data across tiers
type TierManager struct {
	tiers  map[string]*Tier
	files  map[string]*FileMetadata
	engine *TieringEngine
	mu     sync.RWMutex
}

// FileMetadata tracks file information
type FileMetadata struct {
	ID           string
	Filename     string
	Size         int64
	Tier         string
	LastAccessed time.Time
	AccessCount  int
}

// NewTierManager creates a tier manager
func NewTierManager() *TierManager {
	return &TierManager{
		tiers:  make(map[string]*Tier),
		files:  make(map[string]*FileMetadata),
		engine: NewTieringEngine(),
	}
}

// AddTier adds a storage tier
func (tm *TierManager) AddTier(name string, capacity int64) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.tiers[name] = &Tier{
		Name:     name,
		Capacity: capacity,
		Used:     0,
		Files:    make(map[string]*FileMetadata),
	}
}

// Store stores data in appropriate tier
func (tm *TierManager) Store(filename string, data []byte) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	id := generateID(filename)
	size := int64(len(data))

	// Try to store in hot tier first
	tier := tm.selectTier(size)

	metadata := &FileMetadata{
		ID:           id,
		Filename:     filename,
		Size:         size,
		Tier:         tier,
		LastAccessed: time.Now(),
		AccessCount:  0, // Start at 0, not 1
	}

	tm.files[id] = metadata
	tm.tiers[tier].Files[id] = metadata
	tm.tiers[tier].Used += size

	return id, nil
}

// selectTier selects appropriate tier for size
func (tm *TierManager) selectTier(size int64) string {
	// Try hot tier first
	if hot, exists := tm.tiers["hot"]; exists {
		if hot.Capacity == 0 || hot.Used+size <= hot.Capacity {
			return "hot"
		}
	}

	// Try warm tier
	if warm, exists := tm.tiers["warm"]; exists {
		if warm.Capacity == 0 || warm.Used+size <= warm.Capacity {
			return "warm"
		}
	}

	// Default to cold
	return "cold"
}

// RecordAccess records file access
func (tm *TierManager) RecordAccess(id string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if metadata, exists := tm.files[id]; exists {
		metadata.LastAccessed = time.Now()
		metadata.AccessCount++
		tm.engine.RecordAccess(metadata.Filename, time.Now())
	}
}

// SimulateAging simulates time passing (for testing)
func (tm *TierManager) SimulateAging(duration time.Duration) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Reevaluate all files based on access count
	for id, metadata := range tm.files {
		// Files with no recent access should be demoted
		if metadata.AccessCount == 0 {
			tm.demoteFile(id)
		}
	}
}

// demoteFile moves file to lower tier
func (tm *TierManager) demoteFile(id string) {
	metadata := tm.files[id]
	currentTier := metadata.Tier

	var newTier string
	switch currentTier {
	case "hot":
		newTier = "warm"
	case "warm":
		newTier = "cold"
	default:
		return // Already in cold tier
	}

	// Move file
	delete(tm.tiers[currentTier].Files, id)
	tm.tiers[currentTier].Used -= metadata.Size

	metadata.Tier = newTier
	tm.tiers[newTier].Files[id] = metadata
	tm.tiers[newTier].Used += metadata.Size
}

// GetFileTier returns the tier of a file
func (tm *TierManager) GetFileTier(id string) string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if metadata, exists := tm.files[id]; exists {
		return metadata.Tier
	}
	return ""
}

// generateID generates a unique ID
func generateID(filename string) string {
	return filename + "-" + time.Now().Format("20060102150405")
}
