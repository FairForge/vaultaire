// internal/storage/dedup.go
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// Deduplicator tracks blocks for deduplication
type Deduplicator struct {
	blockSize int
	seen      map[string]bool
	mu        sync.RWMutex
}

// NewDeduplicator creates a new deduplicator
func NewDeduplicator(blockSize int) *Deduplicator {
	return &Deduplicator{
		blockSize: blockSize,
		seen:      make(map[string]bool),
	}
}

// CheckBlock checks if block is duplicate
func (d *Deduplicator) CheckBlock(data []byte) (string, bool) {
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.seen[hashStr] {
		return hashStr, false // duplicate
	}

	d.seen[hashStr] = true
	return hashStr, true // new block
}

// DedupStore manages deduplicated storage
type DedupStore struct {
	name       string
	blocks     map[string][]byte // hash -> data
	references map[string]string // filename -> hash
	mu         sync.RWMutex
}

// NewDedupStore creates a deduplicated store
func NewDedupStore(name string) *DedupStore {
	return &DedupStore{
		name:       name,
		blocks:     make(map[string][]byte),
		references: make(map[string]string),
	}
}

// Store saves data with deduplication
func (ds *DedupStore) Store(filename string, data []byte) (*BlockRef, error) {
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	ds.mu.Lock()
	defer ds.mu.Unlock()

	// Check if block already exists
	if _, exists := ds.blocks[hashStr]; !exists {
		// Store new block
		ds.blocks[hashStr] = data
	}

	// Create reference
	ds.references[filename] = hashStr

	return &BlockRef{
		Filename:  filename,
		BlockHash: hashStr,
		Size:      len(data),
	}, nil
}

// UniqueBlocks returns count of unique blocks
func (ds *DedupStore) UniqueBlocks() int {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return len(ds.blocks)
}

// Get retrieves data by filename
func (ds *DedupStore) Get(filename string) ([]byte, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	hash, exists := ds.references[filename]
	if !exists {
		return nil, fmt.Errorf("file not found: %s", filename)
	}

	data, exists := ds.blocks[hash]
	if !exists {
		return nil, fmt.Errorf("block not found for hash: %s", hash)
	}

	return data, nil
}

// BlockRef represents a reference to a deduplicated block
type BlockRef struct {
	Filename  string
	BlockHash string
	Size      int
}

// Stats returns deduplication statistics
func (ds *DedupStore) Stats() DedupStats {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	totalSize := 0
	for _, data := range ds.blocks {
		totalSize += len(data)
	}

	return DedupStats{
		UniqueBlocks:    len(ds.blocks),
		TotalReferences: len(ds.references),
		StoredSize:      totalSize,
		DedupRatio:      float64(len(ds.references)) / float64(len(ds.blocks)),
	}
}

// DedupStats contains deduplication statistics
type DedupStats struct {
	UniqueBlocks    int
	TotalReferences int
	StoredSize      int
	DedupRatio      float64
}
