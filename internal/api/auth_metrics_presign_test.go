package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Post-merge review of R11 (R11-28): verifyPresignedURL answers Expired /
// RequestTimeTooSkewed BEFORE any credential lookup, and presignFailureReason
// called both "key known" — so an unauthenticated request with a random
// X-Amz-Credential and a stale X-Amz-Date minted a new
// vaultaire_auth_failures_by_key_total{key_hash} series per request: the
// unbounded cardinality R11-10 set out to prevent. A presign failure is now
// attributed to a real key only after the id has been looked up.
func TestR11_PresignFailure_UnknownKeyIsNeverHashed(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()
	s.router.HandleFunc("/*", s.handleS3Request)

	ak := "VKghost" + uuid.NewString()[:8]
	// The id is looked up once to decide key_known; nothing has it.
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM tenants WHERE access_key`).
		WithArgs(ak).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	stale := time.Now().UTC().Add(-2 * time.Hour)
	q := signPresignedURL("GET", "/b/k", "localhost:8000", ak, "whatever", 60, stale)
	req := httptest.NewRequest(http.MethodGet, "/b/k?"+q.Encode(), nil)
	req.Host = "localhost:8000"

	beforeUnknown := promtest.ToFloat64(authFailures.WithLabelValues("presign_expired", "false"))
	beforeKnown := promtest.ToFloat64(authFailures.WithLabelValues("presign_expired", "true"))
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	assert.Equal(t, float64(0), promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(ak))),
		"an id nothing owns must not get a per-key series")
	assert.Equal(t, beforeUnknown+1, promtest.ToFloat64(authFailures.WithLabelValues("presign_expired", "false")))
	assert.Equal(t, beforeKnown, promtest.ToFloat64(authFailures.WithLabelValues("presign_expired", "true")))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestR11_PresignFailure_KnownKeyIsHashed(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()
	s.router.HandleFunc("/*", s.handleS3Request)

	ak := "VKreal" + uuid.NewString()[:8]
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM tenants WHERE access_key`).
		WithArgs(ak).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	stale := time.Now().UTC().Add(-2 * time.Hour)
	q := signPresignedURL("GET", "/b/k", "localhost:8000", ak, "whatever", 60, stale)
	req := httptest.NewRequest(http.MethodGet, "/b/k?"+q.Encode(), nil)
	req.Host = "localhost:8000"

	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, float64(1), promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(ak))),
		"a real key's expired URL is attributed to that key")
	require.NoError(t, mock.ExpectationsWereMet())
}
