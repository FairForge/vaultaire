// internal/monitoring/dashboard_test.go
package monitoring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboardConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &DashboardConfig{
			Name:        "main-dashboard",
			RefreshRate: 30 * time.Second,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &DashboardConfig{}
		err := config.Validate()
		assert.Error(t, err)
	})

	t.Run("applies defaults", func(t *testing.T) {
		config := &DashboardConfig{Name: "test"}
		config.ApplyDefaults()
		assert.Equal(t, 30*time.Second, config.RefreshRate)
	})
}

func TestNewDashboard(t *testing.T) {
	t.Run("creates dashboard", func(t *testing.T) {
		dashboard := NewDashboard(&DashboardConfig{Name: "test"})
		assert.NotNil(t, dashboard)
	})
}

func TestDashboard_AddPanel(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})

	t.Run("adds panel", func(t *testing.T) {
		panel := &Panel{
			ID:    "cpu-usage",
			Title: "CPU Usage",
			Type:  PanelTypeGauge,
		}

		err := dashboard.AddPanel(panel)
		assert.NoError(t, err)
	})

	t.Run("rejects duplicate ID", func(t *testing.T) {
		panel := &Panel{ID: "dup", Title: "Test", Type: PanelTypeGraph}
		_ = dashboard.AddPanel(panel)
		err := dashboard.AddPanel(panel)
		assert.Error(t, err)
	})
}

func TestDashboard_GetPanel(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})
	_ = dashboard.AddPanel(&Panel{ID: "panel-1", Title: "Panel 1", Type: PanelTypeGraph})

	t.Run("gets panel by ID", func(t *testing.T) {
		panel := dashboard.GetPanel("panel-1")
		assert.NotNil(t, panel)
		assert.Equal(t, "Panel 1", panel.Title)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		panel := dashboard.GetPanel("unknown")
		assert.Nil(t, panel)
	})
}

func TestDashboard_RegisterDataSource(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})

	t.Run("registers data source", func(t *testing.T) {
		ds := &DataSource{
			ID:   "metrics",
			Type: DataSourceTypeMetrics,
			Provider: func() interface{} {
				return map[string]float64{"cpu": 45.5}
			},
		}

		err := dashboard.RegisterDataSource(ds)
		assert.NoError(t, err)
	})
}

func TestDashboard_GetData(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "metrics",
		Type: DataSourceTypeMetrics,
		Provider: func() interface{} {
			return map[string]float64{"cpu": 75.0, "memory": 60.0}
		},
	})

	_ = dashboard.AddPanel(&Panel{
		ID:         "cpu-panel",
		Title:      "CPU",
		Type:       PanelTypeGauge,
		DataSource: "metrics",
		Query:      "cpu",
	})

	t.Run("returns panel data", func(t *testing.T) {
		data := dashboard.GetData("cpu-panel")
		assert.NotNil(t, data)
	})
}

func TestDashboard_Snapshot(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "stats",
		Type: DataSourceTypeMetrics,
		Provider: func() interface{} {
			return map[string]float64{"requests": 1000}
		},
	})

	_ = dashboard.AddPanel(&Panel{
		ID:         "requests",
		Title:      "Requests",
		Type:       PanelTypeStat,
		DataSource: "stats",
	})

	t.Run("creates snapshot", func(t *testing.T) {
		snapshot := dashboard.Snapshot()
		require.NotNil(t, snapshot)
		assert.Equal(t, "test", snapshot.Name)
		assert.NotEmpty(t, snapshot.Panels)
		assert.False(t, snapshot.GeneratedAt.IsZero())
	})
}

func TestDashboard_HTTPHandler(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "health",
		Type: DataSourceTypeHealth,
		Provider: func() interface{} {
			return map[string]string{"status": "healthy"}
		},
	})

	t.Run("returns JSON snapshot", func(t *testing.T) {
		handler := dashboard.HTTPHandler()
		req := httptest.NewRequest("GET", "/dashboard", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

		var snapshot DashboardSnapshot
		err := json.Unmarshal(rec.Body.Bytes(), &snapshot)
		assert.NoError(t, err)
		assert.Equal(t, "test", snapshot.Name)
	})
}

func TestDashboard_SystemOverview(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "system"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "system",
		Type: DataSourceTypeSystem,
		Provider: func() interface{} {
			return &SystemStats{
				CPUPercent:    45.5,
				MemoryPercent: 60.0,
				DiskPercent:   75.0,
				Goroutines:    100,
				Uptime:        time.Hour,
			}
		},
	})

	t.Run("returns system overview", func(t *testing.T) {
		overview := dashboard.SystemOverview()
		assert.NotNil(t, overview)
		assert.InDelta(t, 45.5, overview.CPUPercent, 0.1)
	})
}

func TestDashboard_AlertsSummary(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "alerts"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "alerts",
		Type: DataSourceTypeAlerts,
		Provider: func() interface{} {
			return &AlertsSummary{
				Total:    10,
				Firing:   3,
				Pending:  2,
				Silenced: 1,
			}
		},
	})

	t.Run("returns alerts summary", func(t *testing.T) {
		summary := dashboard.AlertsSummary()
		assert.NotNil(t, summary)
		assert.Equal(t, 10, summary.Total)
		assert.Equal(t, 3, summary.Firing)
	})
}

func TestDashboard_SLOStatus(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "slo"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "slo",
		Type: DataSourceTypeSLO,
		Provider: func() interface{} {
			return &SLOSummary{
				Total:     5,
				Meeting:   4,
				Breaching: 1,
			}
		},
	})

	t.Run("returns SLO status", func(t *testing.T) {
		status := dashboard.SLOStatus()
		assert.NotNil(t, status)
		assert.Equal(t, 5, status.Total)
		assert.Equal(t, 4, status.Meeting)
	})
}

func TestDashboard_OnCallStatus(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "oncall"})

	_ = dashboard.RegisterDataSource(&DataSource{
		ID:   "oncall",
		Type: DataSourceTypeOnCall,
		Provider: func() interface{} {
			return &OnCallSummary{
				Primary:     "alice",
				Secondary:   "bob",
				NextHandoff: time.Now().Add(8 * time.Hour),
			}
		},
	})

	t.Run("returns on-call status", func(t *testing.T) {
		status := dashboard.OnCallStatus()
		assert.NotNil(t, status)
		assert.Equal(t, "alice", status.Primary)
	})
}

func TestPanelTypes(t *testing.T) {
	t.Run("defines panel types", func(t *testing.T) {
		assert.Equal(t, "graph", PanelTypeGraph)
		assert.Equal(t, "gauge", PanelTypeGauge)
		assert.Equal(t, "stat", PanelTypeStat)
		assert.Equal(t, "table", PanelTypeTable)
		assert.Equal(t, "heatmap", PanelTypeHeatmap)
	})
}

func TestDataSourceTypes(t *testing.T) {
	t.Run("defines data source types", func(t *testing.T) {
		assert.Equal(t, "metrics", DataSourceTypeMetrics)
		assert.Equal(t, "logs", DataSourceTypeLogs)
		assert.Equal(t, "traces", DataSourceTypeTraces)
		assert.Equal(t, "alerts", DataSourceTypeAlerts)
	})
}

func TestPanel(t *testing.T) {
	t.Run("creates panel", func(t *testing.T) {
		panel := &Panel{
			ID:         "test-panel",
			Title:      "Test Panel",
			Type:       PanelTypeGraph,
			DataSource: "prometheus",
			Query:      "rate(requests[5m])",
			GridPos:    GridPos{X: 0, Y: 0, W: 12, H: 8},
		}
		assert.Equal(t, "test-panel", panel.ID)
		assert.Equal(t, 12, panel.GridPos.W)
	})
}

func TestGridPos(t *testing.T) {
	t.Run("creates grid position", func(t *testing.T) {
		pos := GridPos{X: 0, Y: 0, W: 24, H: 6}
		assert.Equal(t, 24, pos.W)
	})
}

func TestDashboard_RemovePanel(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})
	_ = dashboard.AddPanel(&Panel{ID: "to-remove", Title: "Remove Me", Type: PanelTypeGraph})

	t.Run("removes panel", func(t *testing.T) {
		err := dashboard.RemovePanel("to-remove")
		assert.NoError(t, err)
		assert.Nil(t, dashboard.GetPanel("to-remove"))
	})
}

func TestDashboard_ListPanels(t *testing.T) {
	dashboard := NewDashboard(&DashboardConfig{Name: "test"})
	_ = dashboard.AddPanel(&Panel{ID: "p1", Title: "Panel 1", Type: PanelTypeGraph})
	_ = dashboard.AddPanel(&Panel{ID: "p2", Title: "Panel 2", Type: PanelTypeGauge})

	t.Run("lists panels", func(t *testing.T) {
		panels := dashboard.ListPanels()
		assert.Len(t, panels, 2)
	})
}

func TestDashboardSnapshot(t *testing.T) {
	t.Run("serializes to JSON", func(t *testing.T) {
		snapshot := &DashboardSnapshot{
			Name:        "test",
			GeneratedAt: time.Now(),
			Panels: map[string]interface{}{
				"cpu": 75.5,
			},
		}

		data, err := json.Marshal(snapshot)
		assert.NoError(t, err)
		assert.Contains(t, string(data), "test")
	})
}
