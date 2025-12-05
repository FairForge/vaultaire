// internal/devops/dashboard.go
package devops

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Widget types
const (
	WidgetTypePipelines    = "pipelines"
	WidgetTypeDeployments  = "deployments"
	WidgetTypeEnvironments = "environments"
	WidgetTypeReleases     = "releases"
	WidgetTypeIncidents    = "incidents"
	WidgetTypeMetrics      = "metrics"
)

// DevOpsDashboardConfig configures the dashboard
type DevOpsDashboardConfig struct {
	Name        string        `json:"name"`
	RefreshRate time.Duration `json:"refresh_rate"`
}

// Validate checks configuration
func (c *DevOpsDashboardConfig) Validate() error {
	if c.Name == "" {
		return errors.New("dashboard: name is required")
	}
	return nil
}

// PipelineSummary summarizes a pipeline
type PipelineSummary struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Duration time.Duration `json:"duration"`
	LastRun  time.Time     `json:"last_run"`
}

// DeploymentSummary summarizes a deployment
type DeploymentSummary struct {
	Name        string    `json:"name"`
	Environment string    `json:"environment"`
	Version     string    `json:"version"`
	Status      string    `json:"status"`
	DeployedAt  time.Time `json:"deployed_at"`
}

// EnvironmentSummary summarizes an environment
type EnvironmentSummary struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Healthy bool   `json:"healthy"`
	Locked  bool   `json:"locked"`
}

// ReleaseSummary summarizes a release
type ReleaseSummary struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	ReleasedAt time.Time `json:"released_at"`
}

// IncidentSummary summarizes an incident
type IncidentSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
}

// DevOpsMetrics contains DORA metrics
type DevOpsMetrics struct {
	DeployFrequency   float64       `json:"deploy_frequency"`
	LeadTime          time.Duration `json:"lead_time"`
	MTTR              time.Duration `json:"mttr"`
	ChangeFailureRate float64       `json:"change_failure_rate"`
}

// WidgetPosition defines widget position
type WidgetPosition struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// DashboardWidget represents a dashboard widget
type DashboardWidget struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Position WidgetPosition `json:"position"`
}

// DevOpsSnapshot represents a dashboard snapshot
type DevOpsSnapshot struct {
	Name         string                `json:"name"`
	GeneratedAt  time.Time             `json:"generated_at"`
	Pipelines    []*PipelineSummary    `json:"pipelines"`
	Deployments  []*DeploymentSummary  `json:"deployments"`
	Environments []*EnvironmentSummary `json:"environments"`
	Releases     []*ReleaseSummary     `json:"releases"`
	Incidents    []*IncidentSummary    `json:"incidents"`
	Metrics      *DevOpsMetrics        `json:"metrics"`
}

// DevOpsDashboard provides a unified DevOps view
type DevOpsDashboard struct {
	config              *DevOpsDashboardConfig
	widgets             []*DashboardWidget
	pipelineProvider    func() []*PipelineSummary
	deploymentProvider  func() []*DeploymentSummary
	environmentProvider func() []*EnvironmentSummary
	releaseProvider     func() []*ReleaseSummary
	incidentProvider    func() []*IncidentSummary
	metricsProvider     func() *DevOpsMetrics
	mu                  sync.RWMutex
}

// NewDevOpsDashboard creates a dashboard
func NewDevOpsDashboard(config *DevOpsDashboardConfig) *DevOpsDashboard {
	if config == nil {
		config = &DevOpsDashboardConfig{
			Name:        "devops",
			RefreshRate: 30 * time.Second,
		}
	}

	return &DevOpsDashboard{
		config:  config,
		widgets: make([]*DashboardWidget, 0),
	}
}

// SetPipelineProvider sets the pipeline data provider
func (d *DevOpsDashboard) SetPipelineProvider(provider func() []*PipelineSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pipelineProvider = provider
}

// SetDeploymentProvider sets the deployment data provider
func (d *DevOpsDashboard) SetDeploymentProvider(provider func() []*DeploymentSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deploymentProvider = provider
}

// SetEnvironmentProvider sets the environment data provider
func (d *DevOpsDashboard) SetEnvironmentProvider(provider func() []*EnvironmentSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.environmentProvider = provider
}

// SetReleaseProvider sets the release data provider
func (d *DevOpsDashboard) SetReleaseProvider(provider func() []*ReleaseSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.releaseProvider = provider
}

// SetIncidentProvider sets the incident data provider
func (d *DevOpsDashboard) SetIncidentProvider(provider func() []*IncidentSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.incidentProvider = provider
}

// SetMetricsProvider sets the metrics data provider
func (d *DevOpsDashboard) SetMetricsProvider(provider func() *DevOpsMetrics) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.metricsProvider = provider
}

// Pipelines returns pipeline summaries
func (d *DevOpsDashboard) Pipelines() []*PipelineSummary {
	d.mu.RLock()
	provider := d.pipelineProvider
	d.mu.RUnlock()

	if provider != nil {
		return provider()
	}
	return nil
}

// Deployments returns deployment summaries
func (d *DevOpsDashboard) Deployments() []*DeploymentSummary {
	d.mu.RLock()
	provider := d.deploymentProvider
	d.mu.RUnlock()

	if provider != nil {
		return provider()
	}
	return nil
}

// Environments returns environment summaries
func (d *DevOpsDashboard) Environments() []*EnvironmentSummary {
	d.mu.RLock()
	provider := d.environmentProvider
	d.mu.RUnlock()

	if provider != nil {
		return provider()
	}
	return nil
}

// Releases returns release summaries
func (d *DevOpsDashboard) Releases() []*ReleaseSummary {
	d.mu.RLock()
	provider := d.releaseProvider
	d.mu.RUnlock()

	if provider != nil {
		return provider()
	}
	return nil
}

// Incidents returns incident summaries
func (d *DevOpsDashboard) Incidents() []*IncidentSummary {
	d.mu.RLock()
	provider := d.incidentProvider
	d.mu.RUnlock()

	if provider != nil {
		return provider()
	}
	return nil
}

// Metrics returns DevOps metrics
func (d *DevOpsDashboard) Metrics() *DevOpsMetrics {
	d.mu.RLock()
	provider := d.metricsProvider
	d.mu.RUnlock()

	if provider != nil {
		return provider()
	}
	return nil
}

// Snapshot creates a dashboard snapshot
func (d *DevOpsDashboard) Snapshot() *DevOpsSnapshot {
	return &DevOpsSnapshot{
		Name:         d.config.Name,
		GeneratedAt:  time.Now(),
		Pipelines:    d.Pipelines(),
		Deployments:  d.Deployments(),
		Environments: d.Environments(),
		Releases:     d.Releases(),
		Incidents:    d.Incidents(),
		Metrics:      d.Metrics(),
	}
}

// HTTPHandler returns an HTTP handler for the dashboard
func (d *DevOpsDashboard) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot := d.Snapshot()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(snapshot); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// AddWidget adds a widget
func (d *DevOpsDashboard) AddWidget(widget *DashboardWidget) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.widgets = append(d.widgets, widget)
	return nil
}

// RemoveWidget removes a widget
func (d *DevOpsDashboard) RemoveWidget(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for i, w := range d.widgets {
		if w.ID == id {
			d.widgets = append(d.widgets[:i], d.widgets[i+1:]...)
			return nil
		}
	}
	return nil
}

// Widgets returns all widgets
func (d *DevOpsDashboard) Widgets() []*DashboardWidget {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.widgets
}
