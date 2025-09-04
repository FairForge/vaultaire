// internal/storage/garbage_collector.go
package storage

import (
	"sync"
	"time"
)

// GarbageCollector manages cleanup of unused data
type GarbageCollector struct {
	blocks     map[string]*Block
	references map[string][]string // file -> blocks
	ttl        time.Duration
	mu         sync.RWMutex
}

// Block represents a data block
type Block struct {
	ID        string
	Size      int64
	CreatedAt time.Time
}

// NewGarbageCollector creates a new garbage collector
func NewGarbageCollector() *GarbageCollector {
	return &GarbageCollector{
		blocks:     make(map[string]*Block),
		references: make(map[string][]string),
		ttl:        7 * 24 * time.Hour, // Default 7 days
	}
}

// SetTTL sets the time-to-live for blocks
func (gc *GarbageCollector) SetTTL(ttl time.Duration) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.ttl = ttl
}

// AddBlock registers a block
func (gc *GarbageCollector) AddBlock(id string, size int64) {
	gc.AddBlockWithTime(id, size, time.Now())
}

// AddBlockWithTime registers a block with specific time
func (gc *GarbageCollector) AddBlockWithTime(id string, size int64, createdAt time.Time) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	gc.blocks[id] = &Block{
		ID:        id,
		Size:      size,
		CreatedAt: createdAt,
	}
}

// AddReference adds a reference from file to block
func (gc *GarbageCollector) AddReference(fileID, blockID string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	gc.references[fileID] = append(gc.references[fileID], blockID)
}

// FindOrphaned finds blocks with no references
func (gc *GarbageCollector) FindOrphaned() []string {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	// Build set of referenced blocks
	referenced := make(map[string]bool)
	for _, blocks := range gc.references {
		for _, blockID := range blocks {
			referenced[blockID] = true
		}
	}

	// Find orphaned blocks
	var orphaned []string
	for blockID := range gc.blocks {
		if !referenced[blockID] {
			orphaned = append(orphaned, blockID)
		}
	}

	return orphaned
}

// FindExpired finds blocks older than TTL
func (gc *GarbageCollector) FindExpired() []string {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	now := time.Now()
	var expired []string

	for blockID, block := range gc.blocks {
		if now.Sub(block.CreatedAt) > gc.ttl {
			expired = append(expired, blockID)
		}
	}

	return expired
}

// Cleanup removes expired blocks and returns reclaimed space
func (gc *GarbageCollector) Cleanup() int64 {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	expired := gc.findExpiredUnlocked()
	var reclaimed int64

	for _, blockID := range expired {
		if block, exists := gc.blocks[blockID]; exists {
			reclaimed += block.Size
			delete(gc.blocks, blockID)

			// Remove references to this block
			for fileID, blocks := range gc.references {
				cleaned := removeBlockID(blocks, blockID)
				if len(cleaned) == 0 {
					delete(gc.references, fileID)
				} else {
					gc.references[fileID] = cleaned
				}
			}
		}
	}

	return reclaimed
}

// findExpiredUnlocked finds expired blocks (must hold lock)
func (gc *GarbageCollector) findExpiredUnlocked() []string {
	now := time.Now()
	var expired []string

	for blockID, block := range gc.blocks {
		if now.Sub(block.CreatedAt) > gc.ttl {
			expired = append(expired, blockID)
		}
	}

	return expired
}

// removeBlockID removes a block ID from a slice
func removeBlockID(blocks []string, blockID string) []string {
	var result []string
	for _, id := range blocks {
		if id != blockID {
			result = append(result, id)
		}
	}
	return result
}

// Stats returns garbage collection statistics
func (gc *GarbageCollector) Stats() GCStats {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	orphaned := gc.FindOrphaned()
	expired := gc.findExpiredUnlocked()

	var totalSize int64
	var orphanedSize int64
	var expiredSize int64

	for blockID, block := range gc.blocks {
		totalSize += block.Size

		for _, id := range orphaned {
			if id == blockID {
				orphanedSize += block.Size
				break
			}
		}

		for _, id := range expired {
			if id == blockID {
				expiredSize += block.Size
				break
			}
		}
	}

	return GCStats{
		TotalBlocks:    len(gc.blocks),
		OrphanedBlocks: len(orphaned),
		ExpiredBlocks:  len(expired),
		TotalSize:      totalSize,
		OrphanedSize:   orphanedSize,
		ExpiredSize:    expiredSize,
	}
}

// GCStats contains garbage collection statistics
type GCStats struct {
	TotalBlocks    int
	OrphanedBlocks int
	ExpiredBlocks  int
	TotalSize      int64
	OrphanedSize   int64
	ExpiredSize    int64
}
