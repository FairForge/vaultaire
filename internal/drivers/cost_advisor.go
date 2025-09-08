package drivers

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Recommendation represents a cost optimization suggestion
type Recommendation struct {
	Title            string
	Description      string
	Priority         int
	EstimatedSavings float64
	Implementation   string
}

// FileMetadata tracks file usage patterns
type FileMetadata struct {
	Name        string
	Size        int64
	ContentType string
	UploadTime  time.Time
	LastAccess  time.Time
	AccessCount int
	TenantID    string
}

// CostAdvisor analyzes usage and recommends optimizations
type CostAdvisor struct {
	mu       sync.RWMutex
	files    map[string]map[string]*FileMetadata // tenant -> filename -> metadata
	patterns map[string]*UsagePattern            // tenant -> patterns
}

// UsagePattern tracks tenant behavior
type UsagePattern struct {
	TotalSize        int64
	TextFileSize     int64
	CompressibleSize int64
	InfrequentFiles  int
	DuplicateHashes  map[string]int
}

// NewCostAdvisor creates a new cost advisor
func NewCostAdvisor() *CostAdvisor {
	return &CostAdvisor{
		files:    make(map[string]map[string]*FileMetadata),
		patterns: make(map[string]*UsagePattern),
	}
}

// RecordUpload tracks a new file upload
func (ca *CostAdvisor) RecordUpload(tenantID, filename string, size int64, contentType string) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if ca.files[tenantID] == nil {
		ca.files[tenantID] = make(map[string]*FileMetadata)
	}

	metadata := &FileMetadata{
		Name:        filename,
		Size:        size,
		ContentType: contentType,
		UploadTime:  time.Now(),
		LastAccess:  time.Now(),
		AccessCount: 0,
		TenantID:    tenantID,
	}

	ca.files[tenantID][filename] = metadata
	ca.updatePatterns(tenantID)
}

// RecordAccess tracks file access
func (ca *CostAdvisor) RecordAccess(tenantID, filename string, accessTime time.Time) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if files, exists := ca.files[tenantID]; exists {
		if metadata, exists := files[filename]; exists {
			metadata.LastAccess = accessTime
			metadata.AccessCount++
		}
	}
}

// GetRecommendations returns cost optimization suggestions
func (ca *CostAdvisor) GetRecommendations(tenantID string) []Recommendation {
	ca.mu.RLock()
	defer ca.mu.RUnlock()

	var recommendations []Recommendation

	files := ca.files[tenantID]
	if len(files) == 0 {
		return recommendations
	}

	// Check for compression opportunities
	compressible := ca.checkCompression(files)
	if compressible > 0 {
		rec := Recommendation{
			Title:            "Enable compression for text files",
			Description:      "Compress text, JSON, and log files to reduce storage costs",
			Priority:         1,
			EstimatedSavings: float64(compressible) * 0.6 * 0.009 / (1024 * 1024 * 1024), // 60% reduction
			Implementation:   "Enable gzip compression for text/* and application/json content types",
		}
		recommendations = append(recommendations, rec)
	}

	// Check for archival candidates
	archivable := ca.checkArchival(files)
	if archivable > 0 {
		rec := Recommendation{
			Title:            "Move infrequent files to archive tier",
			Description:      "Files not accessed in 30+ days can use cheaper storage",
			Priority:         2,
			EstimatedSavings: float64(archivable) * 0.005 / (1024 * 1024 * 1024), // Archive is cheaper
			Implementation:   "Implement lifecycle policy to move old files to archive storage",
		}
		recommendations = append(recommendations, rec)
	}

	// Check for duplicate detection
	duplicates := ca.checkDuplicates(files)
	if duplicates > 0 {
		rec := Recommendation{
			Title:            "Implement deduplication",
			Description:      "Multiple copies of the same file detected",
			Priority:         3,
			EstimatedSavings: float64(duplicates) * 0.009 / (1024 * 1024 * 1024),
			Implementation:   "Use content-addressable storage to eliminate duplicates",
		}
		recommendations = append(recommendations, rec)
	}

	// Sort by priority
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].Priority < recommendations[j].Priority
	})

	return recommendations
}

// Internal helper methods

func (ca *CostAdvisor) updatePatterns(tenantID string) {
	if ca.patterns[tenantID] == nil {
		ca.patterns[tenantID] = &UsagePattern{
			DuplicateHashes: make(map[string]int),
		}
	}

	pattern := ca.patterns[tenantID]
	pattern.TotalSize = 0
	pattern.TextFileSize = 0
	pattern.CompressibleSize = 0

	for _, file := range ca.files[tenantID] {
		pattern.TotalSize += file.Size

		if isCompressible(file.ContentType) {
			pattern.CompressibleSize += file.Size
			if strings.HasPrefix(file.ContentType, "text/") {
				pattern.TextFileSize += file.Size
			}
		}
	}
}

func (ca *CostAdvisor) checkCompression(files map[string]*FileMetadata) int64 {
	var compressibleSize int64

	for _, file := range files {
		if isCompressible(file.ContentType) {
			compressibleSize += file.Size
		}
	}

	return compressibleSize
}

func (ca *CostAdvisor) checkArchival(files map[string]*FileMetadata) int64 {
	var archivableSize int64
	threshold := time.Now().AddDate(0, 0, -30) // 30 days ago

	for _, file := range files {
		if file.LastAccess.Before(threshold) {
			archivableSize += file.Size
		}
	}

	return archivableSize
}

func (ca *CostAdvisor) checkDuplicates(files map[string]*FileMetadata) int64 {
	// Simple duplicate detection by size (in production, use content hash)
	sizeMap := make(map[int64]int)
	var duplicateSize int64

	for _, file := range files {
		sizeMap[file.Size]++
	}

	for size, count := range sizeMap {
		if count > 1 {
			duplicateSize += size * int64(count-1)
		}
	}

	return duplicateSize
}

func isCompressible(contentType string) bool {
	compressibleTypes := []string{
		"text/",
		"application/json",
		"application/xml",
		"application/javascript",
	}

	for _, prefix := range compressibleTypes {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}

	return false
}
