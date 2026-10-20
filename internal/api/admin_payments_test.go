package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// --- the admin money routes ---

type adminPayFixture struct {
	server  *Server
	db      *sql.DB
	adminID string
	userID  string
	tenant  string
}

func setupAdminPayFixture(t *testing.T) *adminPayFixture {
	t.Helper()
	jf := setupJobsFixture(t)
	db := jf.db
	adminID, userID := uuid.New().String(), uuid.New().String()
	for id, role := range map[string]string{adminID: "admin", userID: "user"} {
		_, err := db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', $3)`,
			id, fmt.Sprintf("pay-%s-%d@test.local", role, time.Now().UnixNano()), role)
		require.NoError(t, err)
	}
	tenant := "adminpay-" + uuid.New().String()[:12]
	_, err := db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'P', $2)`, tenant, tenant+"@test.local")
	require.NoError(t, err)
	require.NoError(t, usage.NewQuotaManager(db).CreateTenant(context.Background(), tenant, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, adminID, userID)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant)
	})
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	s.payments = payments.NewRegistry(db, nil, 0, nil)
	s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		s.registerAdminPaymentRoutes(r)
	})
	return &adminPayFixture{server: s, db: db, adminID: adminID, userID: userID, tenant: tenant}
}

func (f *adminPayFixture) call(t *testing.T, as, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Test-User", as)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *adminPayFixture) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2`, f.tenant, action).Scan(&n))
	return n
}

func TestAdminPaymentsRoutes_AdminOnly(t *testing.T) {
	f := setupAdminPayFixture(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/tenants/" + f.tenant + "/payments"},
		{"POST", "/api/v1/admin/tenants/" + f.tenant + "/payments"},
		{"POST", "/api/v1/admin/tenants/" + f.tenant + "/entitlement"},
		{"POST", "/api/v1/admin/tenants/" + f.tenant + "/refunds"},
	} {
		w, _ := f.call(t, f.userID, c.method, c.path, map[string]any{"reason": "x"})
		assert.Equal(t, http.StatusForbidden, w.Code, "%s %s as a tenant", c.method, c.path)
	}
}

func TestAdminManualPayment_GrantsAndAudits(t *testing.T) {
	f := setupAdminPayFixture(t)
	path := "/api/v1/admin/tenants/" + f.tenant + "/payments"

	// No reason → 400; no house on a tenant without a row → 400.
	w, _ := f.call(t, f.adminID, "POST", path, map[string]any{"amount_cents": 5388, "period": "annual"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = f.call(t, f.adminID, "POST", path, map[string]any{"amount_cents": 5388, "period": "annual", "reason": "wire"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "std_tb")

	w, out := f.call(t, f.adminID, "POST", path, map[string]any{"amount_cents": 5388, "period": "annual", "purpose": "manual",
		"note": "wire ref 123", "reason": "paid by wire", "order": map[string]any{"std_tb": 1}})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.True(t, out["granted"].(bool))
	assert.Regexp(t, `^ST-\d{4}-\d{6}$`, out["invoice_number"])
	assert.Equal(t, 1, f.audits(t, "billing.payment_recorded_manual"))
	assert.Equal(t, 1, f.audits(t, "billing.house_granted"))
	var actor string
	require.NoError(t, f.db.QueryRow(`SELECT performed_by::text FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.payment_recorded_manual'`, f.tenant).Scan(&actor))
	assert.Equal(t, f.adminID, actor)

	w, out = f.call(t, f.adminID, "GET", path, nil)
	require.Equal(t, http.StatusOK, w.Code)
	ent := out["entitlement"].(map[string]any)
	assert.Equal(t, "manual", ent["provider"])
	assert.Equal(t, "prepaid", ent["renewal"])
	assert.Len(t, out["payments"], 1)
	assert.True(t, out["status"].(map[string]any)["granted"].(bool))
}

func TestAdminManualPayment_ExtendsExistingRow(t *testing.T) {
	f := setupAdminPayFixture(t)
	path := "/api/v1/admin/tenants/" + f.tenant + "/payments"
	w, out := f.call(t, f.adminID, "POST", path, map[string]any{"amount_cents": 499, "period": "monthly", "reason": "r", "order": map[string]any{"std_tb": 1}})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	first, err := time.Parse(time.RFC3339, out["paid_through"].(string))
	require.NoError(t, err)

	w, out = f.call(t, f.adminID, "POST", path, map[string]any{"amount_cents": 499, "period": "monthly", "reason": "r"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	assert.False(t, out["granted"].(bool))
	second, err := time.Parse(time.RFC3339, out["paid_through"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, first.UTC().AddDate(0, 1, 0), second, time.Minute)
	assert.Equal(t, 2, f.audits(t, "billing.house_granted"))
}

func TestAdminPaidThroughAdjust_RequiresReason(t *testing.T) {
	f := setupAdminPayFixture(t)
	w, _ := f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/entitlement", map[string]any{"paid_through": time.Now().Add(time.Hour).Format(time.RFC3339)})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "reason")
	assert.Zero(t, f.audits(t, "billing.paid_through_adjusted"))
}

func TestAdminPaidThroughAdjust_MayShorten(t *testing.T) {
	f := setupAdminPayFixture(t)
	w, _ := f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/payments", map[string]any{"amount_cents": 5388, "period": "annual", "reason": "r", "order": map[string]any{"std_tb": 1}})
	require.Equal(t, http.StatusCreated, w.Code)
	sooner := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	w, out := f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/entitlement",
		map[string]any{"paid_through": sooner.Format(time.RFC3339), "order": map[string]any{"std_tb": 2, "vault_tb": 1}, "reason": "agreed"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	ent := out["entitlement"].(map[string]any)
	got, err := time.Parse(time.RFC3339, ent["paid_through"].(string))
	require.NoError(t, err)
	assert.True(t, sooner.Equal(got))
	assert.Equal(t, 2.0, ent["std_tb"])
	assert.Equal(t, 1, f.audits(t, "billing.paid_through_adjusted"))
	// No row → 404.
	other := "adminpay-none-" + uuid.New().String()[:8]
	w, _ = f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+other+"/entitlement", map[string]any{"paid_through": sooner.Format(time.RFC3339), "reason": "x"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminRefund_ShortensWhenAsked(t *testing.T) {
	f := setupAdminPayFixture(t)
	w, out := f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/payments", map[string]any{"amount_cents": 5388, "period": "annual", "reason": "r", "order": map[string]any{"std_tb": 1}})
	require.Equal(t, http.StatusCreated, w.Code)
	pid := out["payment_id"].(string)
	before, _ := time.Parse(time.RFC3339, out["paid_through"].(string))

	w, out = f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/refunds",
		map[string]any{"payment_id": pid, "amount_cents": 5388, "provider_ref": "wire-back-1", "shorten_paid_through": true, "note": "n", "reason": "cancelled"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	after, err := time.Parse(time.RFC3339, out["paid_through"].(string))
	require.NoError(t, err)
	assert.True(t, after.Before(before.Add(-300*24*time.Hour)), "a full refund takes the whole year back: %s → %s", before, after)
	assert.True(t, out["fully_refunded"].(bool))
	assert.Equal(t, 1, f.audits(t, "billing.refund_recorded"))
	// Over the amount → 409; a missing reason → 400; another tenant's payment → 404.
	w, _ = f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/refunds", map[string]any{"payment_id": pid, "amount_cents": 1, "reason": "x"})
	assert.Equal(t, http.StatusConflict, w.Code)
	w, _ = f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/"+f.tenant+"/refunds", map[string]any{"payment_id": pid, "amount_cents": 1})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = f.call(t, f.adminID, "POST", "/api/v1/admin/tenants/other-tenant/refunds", map[string]any{"payment_id": pid, "amount_cents": 1, "reason": "x"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}
