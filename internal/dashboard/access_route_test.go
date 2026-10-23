package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
)

type fixedAccessLog []adminread.Found

func (f fixedAccessLog) Find(context.Context, string) ([]adminread.Found, error) { return f, nil }

// "Access to your account" through the production router (Stage 1a
// Prompt 34b): GET /dashboard/settings/access is the tenant's view behind
// translog_customer_view, linked from Settings only while it is on; the
// checkpoint link is the trust host's.
func TestAccessRoute_ThroughTheDashboardRouter(t *testing.T) {
	size := int64(24)
	entries := fixedAccessLog{{Index: 23, Kind: translog.KindAdminRead, Scope: translog.ScopeTenantPayments,
		At: time.Date(2026, 10, 23, 11, 24, 0, 0, time.UTC), CheckpointSize: &size}}
	for _, c := range []struct {
		name     string
		flagsOn  *bool
		wantCode int
	}{
		{"no flags service", nil, http.StatusNotFound},
		{"the flag off", ptr(false), http.StatusNotFound},
		{"the flag on", ptr(true), http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			logger := zap.NewNop()
			authSvc := auth.NewAuthService(nil, nil)
			sessions := dashauth.NewMemoryStore()
			var fl *flags.Service
			if c.flagsOn != nil {
				fl = flags.New(nil, logger)
				fl.Register(translog.FlagTranslogCustomerView, *c.flagsOn)
				fl.Register(translog.FlagTranslogAdminReads, true)
			}
			r := chi.NewRouter()
			RegisterRoutes(r, Deps{Auth: authSvc, Sessions: sessions, Logger: logger, Email: email.NewSender(logger),
				BaseURL: "http://localhost:8000", Flags: fl, AccessLog: entries,
				Hosts: hosts.Hosts{Canonical: "https://stored.cloud", S3: "https://s3.stored.cloud", CDN: "https://cdn.stored.cloud",
					Trust: "https://trust.stored.cloud", BaseURL: "https://stored.cloud"}})
			_, _ = authSvc.CreateUser(context.Background(), "access@stored.ge", "securepass123")
			user, _ := authSvc.GetUserByEmail(context.Background(), "access@stored.ge")
			tok, err := sessions.Create(context.Background(), dashauth.SessionData{UserID: user.ID, TenantID: "tenant-access", Email: user.Email, Role: "user"}, time.Hour)
			require.NoError(t, err)
			get := func(path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: tok})
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				return w
			}

			// Act
			page := get("/dashboard/settings/access")
			settings := get("/dashboard/settings")

			// Assert
			assert.Equal(t, c.wantCode, page.Code)
			require.Equal(t, http.StatusOK, settings.Code)
			if c.wantCode == http.StatusOK {
				assert.Contains(t, page.Body.String(), "Access to your account")
				assert.Contains(t, page.Body.String(), "Your plan and payments")
				assert.Contains(t, page.Body.String(), `<a href="https://trust.stored.cloud/translog/checkpoint" rel="noopener">checkpoint 24</a>`)
				assert.Contains(t, settings.Body.String(), `href="/dashboard/settings/access"`)
			} else {
				assert.NotContains(t, settings.Body.String(), `href="/dashboard/settings/access"`, "no link to a page that is off")
			}
		})
	}
}

func ptr(b bool) *bool { return &b }
