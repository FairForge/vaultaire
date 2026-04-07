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

func testUsageTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Usage{{end}}` +
			`{{define "content"}}` +
			`<h1>Usage</h1>` +
			`<span class="storage">{{.StorageUsedFmt}} of {{.StorageLimitFmt}}</span>` +
			`<span class="pct">{{.StoragePercent}}</span>` +
			`<span class="tier">{{.Tier}}</span>` +
			`<span class="ingress">{{.IngressFmt}}</span>` +
			`<span class="egress">{{.EgressFmt}}</span>` +
			`<span class="total">{{.BandwidthTotalFmt}}</span>` +
			`<span class="requests">{{.RequestsCount}}</span>` +
			`{{end}}`))
	return tmpl
}

func usageSessionCtx(t *testing.T) context.Context {
	t.Helper()
	store := dashauth.NewMemoryStore()
	token, err := store.Create(context.Background(), dashauth.SessionData{
		UserID:   "user-1",
		TenantID: "tenant-1",
		Email:    "test@stored.ge",
		Role:     "user",
	}, time.Hour)
	require.NoError(t, err)

	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)

	return context.WithValue(context.Background(), dashauth.SessionKey, sd)
}

func TestHandleUsage_NoDB(t *testing.T) {
	tmpl := testUsageTemplate(t)
	handler := HandleUsage(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/usage", nil)
	req = req.WithContext(usageSessionCtx(t))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Usage")
	assert.Contains(t, body, "0 B of 1 TB")
	assert.Contains(t, body, "starter")
}

func TestHandleUsage_NoSession(t *testing.T) {
	tmpl := testUsageTemplate(t)
	handler := HandleUsage(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/usage", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestSetUsageDefaults(t *testing.T) {
	data := map[string]any{}
	setUsageDefaults(data)

	assert.Equal(t, "0 B", data["StorageUsedFmt"])
	assert.Equal(t, "1 TB", data["StorageLimitFmt"])
	assert.Equal(t, 0, data["StoragePercent"])
	assert.Equal(t, "starter", data["Tier"])
	assert.Equal(t, "0 B", data["IngressFmt"])
	assert.Equal(t, "0 B", data["EgressFmt"])
	assert.Equal(t, "0 B", data["BandwidthTotalFmt"])
	assert.Equal(t, int64(0), data["RequestsCount"])
	assert.Nil(t, data["ChartBars"])
	assert.False(t, data["HasChartData"].(bool))
	assert.Nil(t, data["UsageHistory"])
}
