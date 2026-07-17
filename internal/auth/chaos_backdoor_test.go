package auth

import (
	"database/sql"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestValidateRequest_ChaosTestKeyRejected asserts the removed chaos-testing
// backdoor stays removed: with a real (non-nil) DB, the header
// X-API-Key: test-key-chaos-testing must never authenticate, regardless of ENV.
func TestValidateRequest_ChaosTestKeyRejected(t *testing.T) {
	// A lazy handle is enough — the request carries no Authorization header,
	// so a correct ValidateRequest rejects it before any DB query.
	db, err := sql.Open("postgres", "postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	auth := NewAuth(db, zap.NewNop())

	for _, env := range []string{"", "test", "development", "production"} {
		t.Run("ENV="+env, func(t *testing.T) {
			t.Setenv("ENV", env)

			req := httptest.NewRequest("GET", "/test-bucket/test-key", nil)
			req.Header.Set("X-API-Key", "test-key-chaos-testing")
			req.Header.Set("X-Tenant-ID", "chaos-test-tenant")

			tenantID, scope, err := auth.ValidateRequest(req)

			require.Error(t, err)
			require.Empty(t, tenantID)
			require.Nil(t, scope)
		})
	}
}
