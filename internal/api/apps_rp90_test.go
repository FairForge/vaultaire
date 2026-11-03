package api

// RP90-8 + RP90-9 (Stage 1a Prompt 56 Part 0): "deleted — its keys stopped
// now" is true on both doors (the API and the dashboard, one
// account.ScheduleApp): the app and its users scheduled, and BOTH key kinds
// of the whole subtree revoked; and a claim needs the apps flag.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// appWithKeys makes an app, one end user with an S3 key, and an S3 key on
// the app itself; it answers the app's id and its management key.
func (f *platformFixture) appWithKeys(label string) (app, key string) {
	f.t.Helper()
	app, key = f.makeApp(label)
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", key, map[string]any{"label": label + " user"})
	require.Equal(f.t, http.StatusCreated, code, out)
	user := out["id"].(string)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+user+"/keys", key, map[string]any{"name": "end user key"})
	require.Equal(f.t, http.StatusCreated, code, out)
	_, err := f.db.Exec(`INSERT INTO api_keys (user_id, tenant_id, name, key_id, secret_hash, secret_key)
		VALUES ($1, $2, 'the app''s own', $3, 'h', 's')`, f.user.ID, app, "VKAPP"+uuid.NewString()[:10])
	require.NoError(f.t, err)
	return app, key
}

func (f *platformFixture) liveKeysUnder(app string) (s3, mgmt int) {
	f.t.Helper()
	sub := `(SELECT id FROM tenants WHERE id = $1 OR parent_tenant_id = $1)`
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL AND tenant_id IN `+sub, app).Scan(&s3))
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM management_keys WHERE revoked_at IS NULL AND tenant_id IN `+sub, app).Scan(&mgmt))
	return s3, mgmt
}

func TestApps_DeleteStopsEveryKey(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withApps(true, true)

	// The API.
	app, key := f.appWithKeys("api door")
	s3, mgmt := f.liveKeysUnder(app)
	require.Positive(t, s3)
	require.Positive(t, mgmt)
	code, out := f.call(http.MethodDelete, "/api/v1/manage/apps/"+app, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	s3, mgmt = f.liveKeysUnder(app)
	assert.Zero(t, s3, "every S3 key of the app and of its users stopped now")
	assert.Zero(t, mgmt)
	code, _ = f.call(http.MethodGet, "/api/v1/platform/tenants", key, nil)
	assert.Equal(t, http.StatusUnauthorized, code)

	// The dashboard: the same function.
	app2, _ := f.appWithKeys("dashboard door")
	r := chi.NewRouter()
	r.Post("/dashboard/apps/{id}/delete", handlers.HandleAppDelete(f.db, zap.NewNop()))
	req := httptest.NewRequest(http.MethodPost, "/dashboard/apps/"+app2+"/delete", nil)
	req = req.WithContext(dashauth.WithSession(context.Background(), &dashauth.SessionData{UserID: f.user.ID, TenantID: f.user.TenantID}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusSeeOther, w.Code)
	s3, mgmt = f.liveKeysUnder(app2)
	assert.Zero(t, s3, "the dashboard's delete stops the same keys")
	assert.Zero(t, mgmt)
	var unscheduled, audited int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE (id = $1 OR parent_tenant_id = $1) AND deletion_scheduled_at IS NULL`, app2).Scan(&unscheduled))
	assert.Zero(t, unscheduled, "the app and its users are scheduled")
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'tenant.deletion_scheduled' AND resource IN
		(SELECT 'tenant:' || id FROM tenants WHERE parent_tenant_id = $1)`, app2).Scan(&audited))
	assert.Equal(t, 1, audited, "each end user's schedule has its audit row")
}
