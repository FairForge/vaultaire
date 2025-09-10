// internal/engine/performance_monitor.go
package engine

import (
	"context"
	"time"
)

// PerformanceMonitor continuously monitors backend performance
type PerformanceMonitor struct {
	analytics *Analytics
	alerts    chan PerformanceAlert
}

// PerformanceAlert represents a performance issue
type PerformanceAlert struct {
	Backend   string
	Type      string
	Message   string
	Timestamp time.Time
}

// NewPerformanceMonitor creates a monitor
func NewPerformanceMonitor(analytics *Analytics) *PerformanceMonitor {
	return &PerformanceMonitor{
		analytics: analytics,
		alerts:    make(chan PerformanceAlert, 100),
	}
}

// Start begins monitoring
func (pm *PerformanceMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.checkPerformance()
		case <-ctx.Done():
			return
		}
	}
}

func (pm *PerformanceMonitor) checkPerformance() {
	// Check each backend - only need the key, not the value
	for backend := range pm.analytics.metrics {
		stats := pm.analytics.GetStats(backend)

		// Alert on high error rate
		if stats.ErrorRate > 0.05 { // 5% error threshold
			pm.alerts <- PerformanceAlert{
				Backend:   backend,
				Type:      "HIGH_ERROR_RATE",
				Message:   "Error rate exceeds 5%",
				Timestamp: time.Now(),
			}
		}

		// Alert on high latency
		if stats.P95Latency > 5*time.Second {
			pm.alerts <- PerformanceAlert{
				Backend:   backend,
				Type:      "HIGH_LATENCY",
				Message:   "P95 latency exceeds 5 seconds",
				Timestamp: time.Now(),
			}
		}
	}
}
