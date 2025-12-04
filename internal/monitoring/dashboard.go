// internal/monitoring/dashboard.go
package monitoring

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Panel types
const (
	PanelTypeGraph   = "graph"
	PanelTypeGauge   = "gauge"
	PanelTypeStat    = "stat"
	PanelTypeTable   = "table"
	PanelTypeHeatmap = "heatmap"
)

// Data source types
const (
	DataSourceTypeMetrics = "metrics"
	DataSourceTypeLogs    = "logs"
	DataSourceTypeTraces  = "traces"
	DataSourceTypeAlerts  = "alerts"
	DataSourceTypeHealth  = "health"
	DataSourceTypeSystem  = "system"
	DataSourceTypeSLO     = "slo"
	DataSourceTypeOnCall  = "oncall"
)

// DashboardConfig configures a dashboard
type DashboardConfig struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	RefreshRate time.Duration `json:"refresh_rate"`
}

// Validate checks configuration
func (c *DashboardConfig) Validate() error {
	if c.Name == "" {
		return errors.New("dashboard: name is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *DashboardConfig) ApplyDefaults() {
	if c.RefreshRate == 0 {
		c.RefreshRate = 30 * time.Second
	}
}

// GridPos represents panel grid position
type GridPos struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Panel represents a dashboard panel
type Panel struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Type       string  `json:"type"`
	DataSource string  `json:"data_source"`
	Query      string  `json:"query"`
	GridPos    GridPos `json:"grid_pos"`
}

// DataSource provides data to panels
type DataSource struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Provider func() interface{} `json:"-"`
}

// SystemStats contains system statistics
type SystemStats struct {
	CPUPercent    float64       `json:"cpu_percent"`
	MemoryPercent float64       `json:"memory_percent"`
	DiskPercent   float64       `json:"disk_percent"`
	Goroutines    int           `json:"goroutines"`
	Uptime        time.Duration `json:"uptime"`
}

// AlertsSummary contains alert statistics
type AlertsSummary struct {
	Total    int `json:"total"`
	Firing   int `json:"firing"`
	Pending  int `json:"pending"`
	Silenced int `json:"silenced"`
}

// SLOSummary contains SLO statistics
type SLOSummary struct {
	Total     int `json:"total"`
	Meeting   int `json:"meeting"`
	Breaching int `json:"breaching"`
}

// OnCallSummary contains on-call status
type OnCallSummary struct {
	Primary     string    `json:"primary"`
	Secondary   string    `json:"secondary"`
	NextHandoff time.Time `json:"next_handoff"`
}

// DashboardSnapshot represents a point-in-time dashboard state
type DashboardSnapshot struct {
	Name        string                 `json:"name"`
	GeneratedAt time.Time              `json:"generated_at"`
	Panels      map[string]interface{} `json:"panels"`
}

// Dashboard aggregates monitoring data
type Dashboard struct {
	config      *DashboardConfig
	panels      map[string]*Panel
	dataSources map[string]*DataSource
	mu          sync.RWMutex
}

// NewDashboard creates a dashboard
func NewDashboard(config *DashboardConfig) *Dashboard {
	if config == nil {
		config = &DashboardConfig{Name: "default"}
	}
	config.ApplyDefaults()

	return &Dashboard{
		config:      config,
		panels:      make(map[string]*Panel),
		dataSources: make(map[string]*DataSource),
	}
}

// Name returns the dashboard name
func (d *Dashboard) Name() string {
	return d.config.Name
}

// AddPanel adds a panel to the dashboard
func (d *Dashboard) AddPanel(panel *Panel) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.panels[panel.ID]; exists {
		return fmt.Errorf("dashboard: panel %s already exists", panel.ID)
	}

	d.panels[panel.ID] = panel
	return nil
}

// GetPanel returns a panel by ID
func (d *Dashboard) GetPanel(id string) *Panel {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.panels[id]
}

// RemovePanel removes a panel
func (d *Dashboard) RemovePanel(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.panels[id]; !exists {
		return fmt.Errorf("dashboard: panel %s not found", id)
	}

	delete(d.panels, id)
	return nil
}

// ListPanels returns all panels
func (d *Dashboard) ListPanels() []*Panel {
	d.mu.RLock()
	defer d.mu.RUnlock()

	panels := make([]*Panel, 0, len(d.panels))
	for _, p := range d.panels {
		panels = append(panels, p)
	}
	return panels
}

// RegisterDataSource registers a data source
func (d *Dashboard) RegisterDataSource(ds *DataSource) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.dataSources[ds.ID] = ds
	return nil
}

// GetData returns data for a panel
func (d *Dashboard) GetData(panelID string) interface{} {
	d.mu.RLock()
	panel, exists := d.panels[panelID]
	if !exists {
		d.mu.RUnlock()
		return nil
	}

	ds, exists := d.dataSources[panel.DataSource]
	d.mu.RUnlock()

	if !exists || ds.Provider == nil {
		return nil
	}

	return ds.Provider()
}

// Snapshot creates a point-in-time snapshot
func (d *Dashboard) Snapshot() *DashboardSnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()

	panels := make(map[string]interface{})

	for id, panel := range d.panels {
		if ds, exists := d.dataSources[panel.DataSource]; exists && ds.Provider != nil {
			panels[id] = ds.Provider()
		}
	}

	return &DashboardSnapshot{
		Name:        d.config.Name,
		GeneratedAt: time.Now(),
		Panels:      panels,
	}
}

// HTTPHandler returns an HTTP handler for the dashboard
func (d *Dashboard) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot := d.Snapshot()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	})
}

// SystemOverview returns system statistics
func (d *Dashboard) SystemOverview() *SystemStats {
	d.mu.RLock()
	ds, exists := d.dataSources["system"]
	d.mu.RUnlock()

	if !exists || ds.Provider == nil {
		return nil
	}

	if stats, ok := ds.Provider().(*SystemStats); ok {
		return stats
	}
	return nil
}

// AlertsSummary returns alert statistics
func (d *Dashboard) AlertsSummary() *AlertsSummary {
	d.mu.RLock()
	ds, exists := d.dataSources["alerts"]
	d.mu.RUnlock()

	if !exists || ds.Provider == nil {
		return nil
	}

	if summary, ok := ds.Provider().(*AlertsSummary); ok {
		return summary
	}
	return nil
}

// SLOStatus returns SLO statistics
func (d *Dashboard) SLOStatus() *SLOSummary {
	d.mu.RLock()
	ds, exists := d.dataSources["slo"]
	d.mu.RUnlock()

	if !exists || ds.Provider == nil {
		return nil
	}

	if summary, ok := ds.Provider().(*SLOSummary); ok {
		return summary
	}
	return nil
}

// OnCallStatus returns on-call status
func (d *Dashboard) OnCallStatus() *OnCallSummary {
	d.mu.RLock()
	ds, exists := d.dataSources["oncall"]
	d.mu.RUnlock()

	if !exists || ds.Provider == nil {
		return nil
	}

	if summary, ok := ds.Provider().(*OnCallSummary); ok {
		return summary
	}
	return nil
}
