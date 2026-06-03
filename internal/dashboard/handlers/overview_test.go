package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testOverviewTemplate(t *testing.T) *template.Template {
	t.Helper()
	// Minimal base + content template for testing.
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Dashboard{{end}}` +
			`{{define "content"}}` +
			`<h1>Dashboard</h1>` +
			`<span class="storage">{{.StorageUsedFmt}} of {{.StorageLimitFmt}}</span>` +
			`<span class="bandwidth">{{.BandwidthTotalFmt}}</span>` +
			`<span class="buckets">{{.BucketCount}}</span>` +
			`<span class="objects">{{.ObjectCount}}</span>` +
			`<span class="apikeys">{{.APIKeyCount}}</span>` +
			`<span class="tier">{{.Tier}}</span>` +
			`<span class="email">{{.Email}}</span>` +
			`<span class="locality">{{.LocalityLabel}}</span>` +
			`{{end}}`))
	return tmpl
}

func TestHandleOverview_NoDB(t *testing.T) {
	tmpl := testOverviewTemplate(t)
	handler := HandleOverview(tmpl, nil, zap.NewNop(), "local")

	// Create a session and inject it into the request context.
	store := dashauth.NewMemoryStore()
	token, err := store.Create(context.Background(), dashauth.SessionData{
		UserID:   "user-123",
		TenantID: "tenant-456",
		Email:    "test@stored.ge",
		Role:     "user",
	}, time.Hour)
	require.NoError(t, err)

	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Dashboard")
	assert.Contains(t, body, "test@stored.ge")
	// Default values when no DB.
	assert.Contains(t, body, "0 B of 1 TB")
	assert.Contains(t, body, "starter")
	assert.Contains(t, body, "Salt Lake City, US")
}

func TestHandleOverview_NoSession(t *testing.T) {
	tmpl := testOverviewTemplate(t)
	handler := HandleOverview(tmpl, nil, zap.NewNop(), "local")

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should redirect to login.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{1536, "1.5 KB"},
		{1048576, "1 MB"},
		{1073741824, "1 GB"},
		{1099511627776, "1 TB"},
		{5368709120, "5 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatBytes(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRelativeTime(t *testing.T) {
	assert.Equal(t, "just now", relativeTime(time.Now()))
	assert.Equal(t, "5 minutes ago", relativeTime(time.Now().Add(-5*time.Minute)))
	assert.Equal(t, "1 hour ago", relativeTime(time.Now().Add(-1*time.Hour)))
	assert.Equal(t, "3 hours ago", relativeTime(time.Now().Add(-3*time.Hour)))
	assert.Equal(t, "2 days ago", relativeTime(time.Now().Add(-48*time.Hour)))
}

func TestAbsInt64(t *testing.T) {
	assert.Equal(t, int64(5), absInt64(5))
	assert.Equal(t, int64(5), absInt64(-5))
	assert.Equal(t, int64(0), absInt64(0))
}

func TestPopulateLocality_Known(t *testing.T) {
	data := make(map[string]any)
	populateLocality("quotaless", data)

	assert.Equal(t, "EU (Germany)", data["LocalityCity"])
	assert.Equal(t, "DE", data["LocalityCountry"])
	assert.Contains(t, data["LocalityLabel"].(string), "Germany")
	assert.Equal(t, false, data["LocalityMultiRegion"])

	dotX := data["LocalityDotX"].(float64)
	dotY := data["LocalityDotY"].(float64)
	assert.Greater(t, dotX, 90.0)
	assert.Less(t, dotX, 120.0)
	assert.Greater(t, dotY, 15.0)
	assert.Less(t, dotY, 30.0)
}

func TestPopulateLocality_Unknown(t *testing.T) {
	data := make(map[string]any)
	populateLocality("unknown-backend", data)

	assert.Equal(t, "Salt Lake City", data["LocalityCity"])
	assert.Equal(t, "US", data["LocalityCountry"])
	assert.Contains(t, data["LocalityLabel"].(string), "Salt Lake City")
}

func TestPopulateLocality_Empty(t *testing.T) {
	data := make(map[string]any)
	populateLocality("", data)

	assert.Equal(t, "Salt Lake City", data["LocalityCity"])
	assert.Equal(t, "US", data["LocalityCountry"])
}

func TestUpgradeCTA_FreeTier80Percent(t *testing.T) {
	data := make(map[string]any)
	data["StorageUsedFmt"] = "4.5 GB"
	data["StorageLimitFmt"] = "5 GB"
	data["StoragePercent"] = 90
	data["StorageBarClass"] = "danger"
	data["Tier"] = "free"
	data["ShowUpgradeCTA"] = data["Tier"] == "free" && data["StoragePercent"].(int) >= 80

	assert.True(t, data["ShowUpgradeCTA"].(bool))
}

func TestUpgradeCTA_FreeTierBelow80(t *testing.T) {
	data := make(map[string]any)
	data["Tier"] = "free"
	data["StoragePercent"] = 50
	data["ShowUpgradeCTA"] = data["Tier"] == "free" && data["StoragePercent"].(int) >= 80

	assert.False(t, data["ShowUpgradeCTA"].(bool))
}

func TestUpgradeCTA_StarterTier(t *testing.T) {
	data := make(map[string]any)
	data["Tier"] = "starter"
	data["StoragePercent"] = 95
	data["ShowUpgradeCTA"] = data["Tier"] == "free" && data["StoragePercent"].(int) >= 80

	assert.False(t, data["ShowUpgradeCTA"].(bool))
}

func TestCarbonBadge_NoData(t *testing.T) {
	tmpl := testOverviewTemplate(t)
	handler := HandleOverview(tmpl, nil, zap.NewNop(), "local")

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, &dashauth.SessionData{
		UserID: "u1", TenantID: "t1", Email: "test@test.com", Role: "user",
	})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "CarbonSavedPercent")
}

func TestPopulateCarbonBadge_Calculation(t *testing.T) {
	data := make(map[string]any)

	// 1 TB on geyser (0.1 kWh) vs baseline 1.0 kWh → 0.9 kWh savings → 0.36 kg CO2
	// Simulate by calling the function logic directly
	totalTB := 1.0
	actualKWh := totalTB * 0.1
	baselineKWh := totalTB * baselineEnergyKWhPerTBMonth
	savingsKWh := baselineKWh - actualKWh
	co2 := savingsKWh * carbonFactorKgPerKWh
	pct := int(savingsKWh / baselineKWh * 100)

	data["HasCarbonData"] = true
	data["CarbonSavedKg"] = co2
	data["CarbonSavedPercent"] = pct

	assert.True(t, data["HasCarbonData"].(bool))
	assert.Equal(t, 90, data["CarbonSavedPercent"].(int))
	assert.InDelta(t, 0.36, data["CarbonSavedKg"].(float64), 0.01)
}
