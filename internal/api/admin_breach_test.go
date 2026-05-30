package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/compliance"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newAdminBreachTestServer(t *testing.T) (*Server, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	logger := zap.NewNop()

	breachService := compliance.NewBreachService(nil, logger)
	complianceHandler := compliance.NewAPIHandler(
		nil, nil, nil, breachService, nil, nil, logger,
	)

	s := &Server{
		logger: logger,
		router: chi.NewRouter(),
		db:     db,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("X-Request-Id", uuid.New().String())
				ctx := context.WithValue(req.Context(), userIDKey, "admin-user")
				ctx = context.WithValue(ctx, tenantIDKey, "admin-tenant")
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})

		r.Post("/breach", s.requireAdmin(complianceHandler.HandleReportBreach))
		r.Get("/breaches", s.requireAdmin(complianceHandler.HandleListBreaches))
	})

	return s, mock, func() { _ = db.Close() }
}

func TestAdminBreachEndpoint_Create(t *testing.T) {
	srv, mock, cleanup := newAdminBreachTestServer(t)
	defer cleanup()

	// requireAdmin checks role from DB
	mock.ExpectQuery(`SELECT COALESCE\(role, ''\) FROM users WHERE id = \$1`).
		WithArgs("admin-user").
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))

	body, _ := json.Marshal(map[string]interface{}{
		"breach_type":     "data_leakage",
		"description":     "test breach from admin",
		"root_cause":      "test",
		"data_categories": []string{"email"},
	})

	req := httptest.NewRequest("POST", "/api/v1/admin/breach", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var result map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))
	assert.Equal(t, "data_leakage", result["BreachType"])
}

func TestAdminBreachEndpoint_RequiresAdmin(t *testing.T) {
	srv, mock, cleanup := newAdminBreachTestServer(t)
	defer cleanup()

	// Non-admin user
	mock.ExpectQuery(`SELECT COALESCE\(role, ''\) FROM users WHERE id = \$1`).
		WithArgs("admin-user").
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("user"))

	body, _ := json.Marshal(map[string]interface{}{
		"breach_type": "data_leakage",
		"description": "should be rejected",
	})

	req := httptest.NewRequest("POST", "/api/v1/admin/breach", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "admin access required")
}
