package storage

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentChunker performs content-aware chunking
type ContentChunker struct {
	minSize int
	maxSize int
	mask    uint64
}

// NewContentChunker creates a content-aware chunker
func NewContentChunker(minSize, maxSize int) *ContentChunker {
	return &ContentChunker{
		minSize: minSize,
		maxSize: maxSize,
		mask:    0x0000001FF, // 9-bit mask
	}
}

// Split divides data into content-aware chunks
func (c *ContentChunker) Split(data []byte) []Chunk {
	var chunks []Chunk
	start := 0

	for start < len(data) {
		end := c.findBoundary(data, start)

		// Ensure we never exceed maxSize
		if end-start > c.maxSize {
			end = start + c.maxSize
		}

		chunkData := data[start:end]
		hash := sha256.Sum256(chunkData)

		chunks = append(chunks, Chunk{
			Data:   chunkData,
			Hash:   hex.EncodeToString(hash[:]),
			Offset: start,
			Size:   len(chunkData),
		})

		start = end
	}

	return chunks
}

// findBoundary finds the next chunk boundary using rolling hash
func (c *ContentChunker) findBoundary(data []byte, start int) int {
	if start >= len(data) {
		return len(data)
	}

	// Calculate the actual end boundary
	maxEnd := start + c.maxSize
	if maxEnd > len(data) {
		maxEnd = len(data)
	}

	// If remaining data is less than minSize, take it all
	remaining := len(data) - start
	if remaining <= c.minSize {
		return len(data)
	}

	// Start looking for boundary after minSize
	end := start + c.minSize
	if end > len(data) {
		return len(data)
	}

	// Rolling hash using simple algorithm
	var hash uint64 = 0

	// Look for boundary between minSize and maxSize
	for end < maxEnd {
		// Simple rolling hash
		hash = hash*31 + uint64(data[end])

		// Check if we hit a boundary
		if (hash & c.mask) == 0 {
			return end + 1
		}
		end++
	}

	// Return maxEnd if no boundary found
	return maxEnd
}

// Chunk represents a data chunk
type Chunk struct {
	Data   []byte
	Hash   string
	Offset int
	Size   int
}
