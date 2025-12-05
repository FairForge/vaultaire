// internal/devops/dashboard_test.go
package devops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDevOpsDashboardConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &DevOpsDashboardConfig{
			Name:        "ops-dashboard",
			RefreshRate: 30 * time.Second,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &DevOpsDashboardConfig{}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewDevOpsDashboard(t *testing.T) {
	t.Run("creates dashboard", func(t *testing.T) {
		dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})
		assert.NotNil(t, dashboard)
	})
}

func TestDevOpsDashboard_PipelineStatus(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	dashboard.SetPipelineProvider(func() []*PipelineSummary {
		return []*PipelineSummary{
			{Name: "build", Status: "success", Duration: 5 * time.Minute},
			{Name: "deploy", Status: "running", Duration: 2 * time.Minute},
		}
	})

	t.Run("returns pipeline status", func(t *testing.T) {
		pipelines := dashboard.Pipelines()
		assert.Len(t, pipelines, 2)
		assert.Equal(t, "build", pipelines[0].Name)
	})
}

func TestDevOpsDashboard_DeploymentStatus(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	dashboard.SetDeploymentProvider(func() []*DeploymentSummary {
		return []*DeploymentSummary{
			{Name: "web-app", Environment: "production", Version: "v1.2.3", Status: "success"},
		}
	})

	t.Run("returns deployment status", func(t *testing.T) {
		deployments := dashboard.Deployments()
		assert.Len(t, deployments, 1)
		assert.Equal(t, "production", deployments[0].Environment)
	})
}

func TestDevOpsDashboard_EnvironmentStatus(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	dashboard.SetEnvironmentProvider(func() []*EnvironmentSummary {
		return []*EnvironmentSummary{
			{Name: "production", Type: "production", Healthy: true, Locked: false},
			{Name: "staging", Type: "staging", Healthy: true, Locked: true},
		}
	})

	t.Run("returns environment status", func(t *testing.T) {
		envs := dashboard.Environments()
		assert.Len(t, envs, 2)
		assert.True(t, envs[1].Locked)
	})
}

func TestDevOpsDashboard_ReleaseStatus(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	dashboard.SetReleaseProvider(func() []*ReleaseSummary {
		return []*ReleaseSummary{
			{Name: "v1.2.3", Status: "released", ReleasedAt: time.Now()},
			{Name: "v1.3.0", Status: "candidate", ReleasedAt: time.Time{}},
		}
	})

	t.Run("returns release status", func(t *testing.T) {
		releases := dashboard.Releases()
		assert.Len(t, releases, 2)
	})
}

func TestDevOpsDashboard_IncidentStatus(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	dashboard.SetIncidentProvider(func() []*IncidentSummary {
		return []*IncidentSummary{
			{ID: "INC-001", Title: "API Latency", Severity: "high", Status: "open"},
		}
	})

	t.Run("returns incident status", func(t *testing.T) {
		incidents := dashboard.Incidents()
		assert.Len(t, incidents, 1)
		assert.Equal(t, "high", incidents[0].Severity)
	})
}

func TestDevOpsDashboard_Metrics(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	dashboard.SetMetricsProvider(func() *DevOpsMetrics {
		return &DevOpsMetrics{
			DeployFrequency:   2.5,
			LeadTime:          4 * time.Hour,
			MTTR:              30 * time.Minute,
			ChangeFailureRate: 0.05,
		}
	})

	t.Run("returns DORA metrics", func(t *testing.T) {
		metrics := dashboard.Metrics()
		assert.NotNil(t, metrics)
		assert.InDelta(t, 2.5, metrics.DeployFrequency, 0.1)
		assert.InDelta(t, 0.05, metrics.ChangeFailureRate, 0.01)
	})
}

func TestDevOpsDashboard_Snapshot(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "ops"})

	dashboard.SetPipelineProvider(func() []*PipelineSummary {
		return []*PipelineSummary{{Name: "build", Status: "success"}}
	})

	t.Run("creates snapshot", func(t *testing.T) {
		snapshot := dashboard.Snapshot()
		require.NotNil(t, snapshot)
		assert.Equal(t, "ops", snapshot.Name)
		assert.False(t, snapshot.GeneratedAt.IsZero())
	})
}

func TestDevOpsDashboard_HTTPHandler(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "api"})

	dashboard.SetPipelineProvider(func() []*PipelineSummary {
		return []*PipelineSummary{{Name: "test", Status: "success"}}
	})

	t.Run("returns JSON", func(t *testing.T) {
		handler := dashboard.HTTPHandler()
		req := httptest.NewRequest("GET", "/dashboard", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

		var snapshot DevOpsSnapshot
		err := json.Unmarshal(rec.Body.Bytes(), &snapshot)
		assert.NoError(t, err)
	})
}

func TestDevOpsDashboard_Widget(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})

	t.Run("adds widget", func(t *testing.T) {
		err := dashboard.AddWidget(&DashboardWidget{
			ID:       "pipeline-status",
			Type:     WidgetTypePipelines,
			Title:    "Pipeline Status",
			Position: WidgetPosition{X: 0, Y: 0, W: 6, H: 4},
		})
		assert.NoError(t, err)
	})

	t.Run("lists widgets", func(t *testing.T) {
		widgets := dashboard.Widgets()
		assert.Len(t, widgets, 1)
	})
}

func TestWidgetTypes(t *testing.T) {
	t.Run("defines widget types", func(t *testing.T) {
		assert.Equal(t, "pipelines", WidgetTypePipelines)
		assert.Equal(t, "deployments", WidgetTypeDeployments)
		assert.Equal(t, "environments", WidgetTypeEnvironments)
		assert.Equal(t, "releases", WidgetTypeReleases)
		assert.Equal(t, "incidents", WidgetTypeIncidents)
		assert.Equal(t, "metrics", WidgetTypeMetrics)
	})
}

func TestPipelineSummary(t *testing.T) {
	t.Run("creates summary", func(t *testing.T) {
		summary := &PipelineSummary{
			Name:     "build-deploy",
			Status:   "success",
			Duration: 10 * time.Minute,
			LastRun:  time.Now(),
		}
		assert.Equal(t, "build-deploy", summary.Name)
	})
}

func TestDeploymentSummary(t *testing.T) {
	t.Run("creates summary", func(t *testing.T) {
		summary := &DeploymentSummary{
			Name:        "web-app",
			Environment: "production",
			Version:     "v1.0.0",
			Status:      "success",
			DeployedAt:  time.Now(),
		}
		assert.Equal(t, "v1.0.0", summary.Version)
	})
}

func TestDevOpsMetrics(t *testing.T) {
	t.Run("creates metrics", func(t *testing.T) {
		metrics := &DevOpsMetrics{
			DeployFrequency:   3.0,
			LeadTime:          2 * time.Hour,
			MTTR:              15 * time.Minute,
			ChangeFailureRate: 0.02,
		}
		assert.InDelta(t, 3.0, metrics.DeployFrequency, 0.1)
	})
}

func TestDevOpsDashboard_RemoveWidget(t *testing.T) {
	dashboard := NewDevOpsDashboard(&DevOpsDashboardConfig{Name: "test"})
	_ = dashboard.AddWidget(&DashboardWidget{ID: "remove-me", Type: WidgetTypePipelines})

	t.Run("removes widget", func(t *testing.T) {
		err := dashboard.RemoveWidget("remove-me")
		assert.NoError(t, err)
		assert.Len(t, dashboard.Widgets(), 0)
	})
}
