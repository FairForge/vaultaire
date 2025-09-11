// internal/engine/migration_progress.go
package engine

import (
	"sync"
	"time"
)

// MigrationProgress tracks live migration progress
type MigrationProgress struct {
	mu               sync.RWMutex
	totalObjects     int
	processedObjects int
	failedObjects    int
	bytesTransferred int64
	startTime        time.Time
	currentObject    string
}

// NewMigrationProgress creates a progress tracker
func NewMigrationProgress(total int) *MigrationProgress {
	return &MigrationProgress{
		totalObjects: total,
		startTime:    time.Now(),
	}
}

// Update updates progress
func (p *MigrationProgress) Update(object string, bytes int64, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.currentObject = object
	if failed {
		p.failedObjects++
	} else {
		p.processedObjects++
		p.bytesTransferred += bytes
	}
}

// GetStatus returns current status
func (p *MigrationProgress) GetStatus() MigrationStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	elapsed := time.Since(p.startTime)
	rate := float64(p.bytesTransferred) / elapsed.Seconds()

	return MigrationStatus{
		Progress:    float64(p.processedObjects) / float64(p.totalObjects) * 100,
		Rate:        rate,
		Remaining:   p.totalObjects - p.processedObjects,
		Failed:      p.failedObjects,
		CurrentFile: p.currentObject,
	}
}

// MigrationStatus represents current migration state
type MigrationStatus struct {
	Progress    float64 // Percentage complete
	Rate        float64 // Bytes per second
	Remaining   int     // Objects remaining
	Failed      int     // Failed objects
	CurrentFile string  // Currently processing
}
