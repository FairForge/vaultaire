package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRequestLimits_OversizedManagementBody(t *testing.T) {
	s := &Server{
		logger: zap.NewNop(),
	}

	body := make([]byte, 11<<20) // 11 MB — over the 10 MB management limit
	mw := s.requestLimitsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 12<<20)
		_, err := r.Body.Read(buf)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/api/v1/manage/buckets", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestRequestLimits_S3PutNotLimited(t *testing.T) {
	s := &Server{
		logger: zap.NewNop(),
	}

	body := make([]byte, 20<<20) // 20 MB — would exceed any limit, but S3 PUT is exempt
	var bodyRead bool
	mw := s.requestLimitsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 21<<20)
		n, _ := r.Body.Read(buf)
		bodyRead = n == len(body)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("PUT", "/my-bucket/my-object", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bodyRead, "S3 PUT body should not be limited")
}

func TestSecurityHeaders_NotOnS3(t *testing.T) {
	s := &Server{
		logger: zap.NewNop(),
	}

	mw := s.requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/my-bucket/my-object", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)

	assert.Empty(t, w.Header().Get("Content-Security-Policy"),
		"S3 responses must not have CSP headers")
	assert.Empty(t, w.Header().Get("X-Frame-Options"),
		"S3 responses must not have X-Frame-Options")
}

func TestBucketCountLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()

	const tenantID = "test-s3-bucket-limit"
	defer func() {
		_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	}()

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Limit Co', 'limit@test.com', 'VK-limit', 'SK-limit') ON CONFLICT DO NOTHING`, tenantID)
	require.NoError(t, err)

	for i := 0; i < 1000; i++ {
		_, err := db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			tenantID, "bucket-"+padInt(i))
		require.NoError(t, err)
	}

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/" + tenantID) }()

	req := httptest.NewRequest("PUT", "/bucket-1001", nil)
	tn := &tenant.Tenant{ID: tenantID}
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "1000")
}

func padInt(i int) string {
	s := "0000" + itoa(i)
	return s[len(s)-4:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [10]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
