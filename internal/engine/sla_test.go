// internal/engine/sla_test.go
package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSLAMonitor_CheckCompliance(t *testing.T) {
	monitor := NewSLAMonitor()

	// Define SLA
	monitor.DefineSLA("backend1", SLARequirements{
		UptimePercent: 99.9,
		MaxLatencyP99: 1 * time.Second,
		MinThroughput: 10 * 1024 * 1024, // 10MB/s
	})

	// Record some operations
	for i := 0; i < 100; i++ {
		monitor.RecordOperation("backend1", 500*time.Millisecond, true)
	}

	compliance := monitor.GetCompliance("backend1")
	assert.True(t, compliance.MeetsUptime)
	assert.True(t, compliance.MeetsLatency)
}

func TestSLAMonitor_ViolationDetection(t *testing.T) {
	monitor := NewSLAMonitor()

	monitor.DefineSLA("backend1", SLARequirements{
		UptimePercent: 99.9,
		MaxLatencyP99: 100 * time.Millisecond,
	})

	// Record operations with high latency
	for i := 0; i < 10; i++ {
		monitor.RecordOperation("backend1", 200*time.Millisecond, true)
	}

	violations := monitor.GetViolations("backend1")
	assert.Greater(t, len(violations), 0)
	assert.Equal(t, "LATENCY_VIOLATION", violations[0].Type)
}
