// internal/container/monitoring.go
package container

import (
	"context"
	"errors"
	"time"
)

// HealthStatus represents container health status
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusStarting  HealthStatus = "starting"
	HealthStatusNone      HealthStatus = "none"
)

// ContainerStateStatus represents container state
type ContainerStateStatus string

const (
	StateRunning    ContainerStateStatus = "running"
	StateExited     ContainerStateStatus = "exited"
	StatePaused     ContainerStateStatus = "paused"
	StateRestarting ContainerStateStatus = "restarting"
	StateCreated    ContainerStateStatus = "created"
)

// AlertSeverity represents alert severity level
type AlertSeverity int

const (
	AlertSeverityInfo AlertSeverity = iota
	AlertSeverityWarning
	AlertSeverityCritical
)

// ContainerMetrics represents container resource metrics
type ContainerMetrics struct {
	ContainerID   string    `json:"container_id"`
	Name          string    `json:"name"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryUsage   int64     `json:"memory_usage"`
	MemoryLimit   int64     `json:"memory_limit"`
	MemoryPercent float64   `json:"memory_percent"`
	NetworkRx     int64     `json:"network_rx"`
	NetworkTx     int64     `json:"network_tx"`
	BlockRead     int64     `json:"block_read"`
	BlockWrite    int64     `json:"block_write"`
	PIDs          int       `json:"pids"`
	Timestamp     time.Time `json:"timestamp"`
}

// CalculateMemoryPercent calculates memory usage percentage
func (m *ContainerMetrics) CalculateMemoryPercent() float64 {
	if m.MemoryLimit == 0 {
		return 0.0
	}
	return float64(m.MemoryUsage) / float64(m.MemoryLimit) * 100
}

// HealthCheckLog represents a health check log entry
type HealthCheckLog struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	ExitCode int       `json:"exit_code"`
	Output   string    `json:"output"`
}

// ContainerHealth represents container health status
type ContainerHealth struct {
	Status        HealthStatus     `json:"status"`
	FailingStreak int              `json:"failing_streak"`
	Log           []HealthCheckLog `json:"log"`
}

// ContainerState represents container state
type ContainerState struct {
	Status     ContainerStateStatus `json:"status"`
	Running    bool                 `json:"running"`
	Paused     bool                 `json:"paused"`
	Restarting bool                 `json:"restarting"`
	OOMKilled  bool                 `json:"oom_killed"`
	Dead       bool                 `json:"dead"`
	Pid        int                  `json:"pid"`
	ExitCode   int                  `json:"exit_code"`
	Error      string               `json:"error"`
	StartedAt  time.Time            `json:"started_at"`
	FinishedAt time.Time            `json:"finished_at"`
}

// ResourceAlert represents a resource usage alert
type ResourceAlert struct {
	ContainerID string        `json:"container_id"`
	Resource    string        `json:"resource"`
	Threshold   float64       `json:"threshold"`
	Current     float64       `json:"current"`
	Message     string        `json:"message"`
	Severity    AlertSeverity `json:"severity"`
	Timestamp   time.Time     `json:"timestamp"`
}

// ResourceThreshold defines alert thresholds for a resource
type ResourceThreshold struct {
	Resource string  `json:"resource"`
	Warning  float64 `json:"warning"`
	Critical float64 `json:"critical"`
}

// Check checks a value against thresholds and returns severity
func (t *ResourceThreshold) Check(value float64) AlertSeverity {
	if value >= t.Critical {
		return AlertSeverityCritical
	}
	if value >= t.Warning {
		return AlertSeverityWarning
	}
	return AlertSeverityInfo
}

// MonitoringConfig configures container monitoring
type MonitoringConfig struct {
	Interval     time.Duration       `json:"interval"`
	Thresholds   []ResourceThreshold `json:"thresholds"`
	EnableAlerts bool                `json:"enable_alerts"`
}

// MetricsCollectorConfig configures the metrics collector
type MetricsCollectorConfig struct {
	Provider string        `json:"provider"`
	Interval time.Duration `json:"interval"`
}

// MetricsCollector collects container metrics
type MetricsCollector struct {
	config *MetricsCollectorConfig
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector(config *MetricsCollectorConfig) *MetricsCollector {
	if config.Interval == 0 {
		config.Interval = 10 * time.Second
	}
	return &MetricsCollector{config: config}
}

// Collect collects metrics for a single container
func (c *MetricsCollector) Collect(ctx context.Context, containerID string) (*ContainerMetrics, error) {
	if c.config.Provider == "mock" {
		return &ContainerMetrics{
			ContainerID:   containerID,
			Name:          "mock-container",
			CPUPercent:    25.5,
			MemoryUsage:   256 * 1024 * 1024,
			MemoryLimit:   512 * 1024 * 1024,
			MemoryPercent: 50.0,
			NetworkRx:     1024,
			NetworkTx:     512,
			Timestamp:     time.Now(),
		}, nil
	}
	return nil, errors.New("monitoring: not implemented")
}

// CollectAll collects metrics for all containers
func (c *MetricsCollector) CollectAll(ctx context.Context) ([]*ContainerMetrics, error) {
	if c.config.Provider == "mock" {
		return []*ContainerMetrics{
			{ContainerID: "container-1", CPUPercent: 20},
			{ContainerID: "container-2", CPUPercent: 30},
		}, nil
	}
	return nil, errors.New("monitoring: not implemented")
}

// Stream streams metrics for a container
func (c *MetricsCollector) Stream(ctx context.Context, containerID string) (<-chan *ContainerMetrics, error) {
	ch := make(chan *ContainerMetrics)

	if c.config.Provider == "mock" {
		go func() {
			defer close(ch)
			ticker := time.NewTicker(c.config.Interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					metrics, _ := c.Collect(ctx, containerID)
					select {
					case ch <- metrics:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
		return ch, nil
	}

	close(ch)
	return ch, errors.New("monitoring: not implemented")
}

// ContainerStats holds statistical data for a container
type ContainerStats struct {
	Samples []ContainerMetrics `json:"samples"`
}

// AvgCPU returns average CPU usage
func (s *ContainerStats) AvgCPU() float64 {
	if len(s.Samples) == 0 {
		return 0
	}
	var total float64
	for _, sample := range s.Samples {
		total += sample.CPUPercent
	}
	return total / float64(len(s.Samples))
}

// AvgMemory returns average memory usage
func (s *ContainerStats) AvgMemory() float64 {
	if len(s.Samples) == 0 {
		return 0
	}
	var total float64
	for _, sample := range s.Samples {
		total += sample.MemoryPercent
	}
	return total / float64(len(s.Samples))
}

// MaxCPU returns maximum CPU usage
func (s *ContainerStats) MaxCPU() float64 {
	var max float64
	for _, sample := range s.Samples {
		if sample.CPUPercent > max {
			max = sample.CPUPercent
		}
	}
	return max
}

// DefaultThresholds returns default resource thresholds
func DefaultThresholds() []ResourceThreshold {
	return []ResourceThreshold{
		{Resource: "cpu", Warning: 80.0, Critical: 95.0},
		{Resource: "memory", Warning: 80.0, Critical: 95.0},
		{Resource: "disk", Warning: 85.0, Critical: 95.0},
	}
}
