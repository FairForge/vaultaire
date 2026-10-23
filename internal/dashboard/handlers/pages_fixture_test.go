package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestPages_WriteScreenshotFixtures renders the buckets and settings pages
// through the REAL templates for screenshots and Lighthouse
// (DASH_OUT=dir go test -run TestPages_WriteScreenshotFixtures). The
// overview and billing pages have their own fixture tests.
func TestPages_WriteScreenshotFixtures(t *testing.T) {
	out := os.Getenv("DASH_OUT")
	if out == "" {
		t.Skip("DASH_OUT not set")
	}
	require.NoError(t, os.MkdirAll(out, 0o755))
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)

	page := func(name string) *template.Template {
		base := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/base.html"))
		tmpl := template.Must(base.Clone())
		template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/"+name))
		return tmpl
	}
	write := func(name string, w *httptest.ResponseRecorder) {
		require.Equal(t, 200, w.Code, name)
		html := strings.ReplaceAll(w.Body.String(), `"/static/`, `"static/`)
		require.NoError(t, os.WriteFile(filepath.Join(out, name+".html"), []byte(html), 0o644))
	}

	// Buckets: two rooms, one in the attic.
	for _, b := range []struct{ name, tier, vis string }{{"photos", "standard", "private"}, {"cold-archive", "archive", "private"}, {"site-assets", "auto", "public-read"}} {
		_, err := db.Exec(`INSERT INTO buckets (tenant_id, name, tier_preference, visibility) VALUES ($1, $2, $3, $4)`, id, b.name, b.tier, b.vis)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, id) })
	w := httptest.NewRecorder()
	HandleBuckets(page("buckets.html"), db, t.TempDir(), nil, zap.NewNop()).ServeHTTP(w,
		injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/buckets", nil), id))
	write("buckets", w)

	// Settings.
	w = httptest.NewRecorder()
	HandleSettings(page("settings.html"), auth.NewAuthService(nil, db), db, nil, nil, func(string) bool { return true }, zap.NewNop()).ServeHTTP(w,
		injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/settings", nil), id))
	write("settings", w)

	// Access to your account (Stage 1a Prompt 34b): reads and switch
	// changes, some in a signed checkpoint, the newest not yet.
	cp := int64(27)
	on, off := true, false
	at := func(m int) time.Time { return time.Date(2026, 10, 23, 15, m, 51, 0, time.UTC) }
	access := &countingFinder{found: []adminread.Found{
		{Index: 23, Kind: translog.KindAdminRead, Scope: translog.ScopeTenantPayments, At: time.Date(2026, 10, 23, 11, 24, 0, 0, time.UTC), CheckpointSize: &cp},
		{Index: 25, Kind: translog.KindLogPolicy, Flag: translog.FlagTranslogAdminReads, Action: translog.PolicyActionSet, Enabled: &off, At: at(13), CheckpointSize: &cp},
		{Index: 26, Kind: translog.KindLogPolicy, Flag: translog.FlagTranslogAdminReads, Action: translog.PolicyActionSet, Enabled: &on, At: at(13), CheckpointSize: &cp},
		{Index: 27, Kind: translog.KindAdminRead, Scope: translog.ScopeTenantAudit, At: at(14)},
	}}
	// Unpublished (translog_public OFF — prod today, RP46-3), then published.
	for name, published := range map[string]bool{"access": false, "access-published": true} {
		w = httptest.NewRecorder()
		HandleAccess(page("access.html"), func(string) bool { return true }, func(string) bool { return true }, access,
			"https://trust.stored.cloud/translog/checkpoint", func() bool { return published }, zap.NewNop()).ServeHTTP(w,
			injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/settings/access", nil), id))
		write(name, w)
	}

	// Bucket settings, with the customer-key requirement on (WP-SSEC-1 B.5).
	_, err := db.Exec(`UPDATE buckets SET ssec_required = TRUE WHERE tenant_id = $1 AND name = 'photos'`, id)
	require.NoError(t, err)
	w = httptest.NewRecorder()
	HandleBucketSettings(page("bucket_settings.html"), db, zap.NewNop()).ServeHTTP(w,
		injectBucketRoute(injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/buckets/photos/settings", nil), id), "photos"))
	write("bucket-settings", w)

	// Two-factor enrolment (WP-R12-8): the setup page with the QR code the
	// server renders, and the page that shows the backup codes once.
	authSvc := auth.NewAuthService(nil, nil)
	u, err := authSvc.CreateUser(context.Background(), "test@stored.ge", "securepass123")
	require.NoError(t, err)
	mfaSvc, enrol := auth.NewMFAService("stored.ge"), NewMFAEnrolmentStore()
	mfaReq := func(method, body string) *http.Request {
		req := httptest.NewRequest(method, "/dashboard/settings/mfa", strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: "fixture-session"})
		sd := &dashauth.SessionData{UserID: u.ID, TenantID: id, Email: u.Email, Role: "user"}
		return req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, sd))
	}
	w = httptest.NewRecorder()
	HandleMFASetup(page("mfa_setup.html"), authSvc, mfaSvc, enrol, zap.NewNop()).ServeHTTP(w, mfaReq("GET", ""))
	require.Equal(t, 200, w.Code)
	qr := httptest.NewRecorder()
	HandleMFAQR(enrol, zap.NewNop()).ServeHTTP(qr, mfaReq("GET", ""))
	require.Equal(t, 200, qr.Code)
	require.NoError(t, os.WriteFile(filepath.Join(out, "mfa-qr.png"), qr.Body.Bytes(), 0o644))
	setupHTML := strings.ReplaceAll(strings.ReplaceAll(w.Body.String(), `"/static/`, `"static/`), `"`+mfaQRPath+`"`, `"mfa-qr.png"`)
	require.NoError(t, os.WriteFile(filepath.Join(out, "mfa-setup.html"), []byte(setupHTML), 0o644))

	pending, ok := enrol.Peek("fixture-session", u.ID)
	require.True(t, ok)
	code, err := totp.GenerateCode(pending.Secret, time.Now())
	require.NoError(t, err)
	w = httptest.NewRecorder()
	HandleMFAEnable(page("mfa_setup.html"), authSvc, mfaSvc, enrol, nil, zap.NewNop()).ServeHTTP(w, mfaReq("POST", "password=securepass123&totp_code="+code))
	write("mfa-enrolled", w)
}
