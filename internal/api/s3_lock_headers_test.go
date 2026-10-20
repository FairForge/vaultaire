package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP17-1 (Stage 1a Prompt 17 Part 0): HEAD, GET and a 206 of an object
// under Object Lock send AWS's three headers — x-amz-object-lock-mode,
// x-amz-object-lock-retain-until-date (RFC 3339, UTC) and
// x-amz-object-lock-legal-hold (ON/OFF) — from the object's object_locks
// row, through the one header writer (object_headers.go). None of the three
// without a row (AWS sends none, never empty values). aws-cli head-object
// showed None for an object under a GOVERNANCE default retention.

func (f *lockFixture) headObject(t *testing.T, key, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("HEAD", "/"+f.bucket+"/"+key+query, nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleHeadObject(w, req, &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID})
	return w
}

func (f *lockFixture) getObject(t *testing.T, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+f.bucket+"/"+key, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, f.bucket, key)
	return w
}

func assertLockHeaders(t *testing.T, h http.Header, mode, hold string, until time.Time) {
	t.Helper()
	assert.Equal(t, mode, h.Get("x-amz-object-lock-mode"))
	assert.Equal(t, hold, h.Get("x-amz-object-lock-legal-hold"))
	got := h.Get("x-amz-object-lock-retain-until-date")
	if until.IsZero() {
		assert.Empty(t, got)
		return
	}
	parsed, err := time.Parse(time.RFC3339, got)
	require.NoError(t, err, "RFC 3339: %q", got)
	assert.WithinDuration(t, until, parsed, 2*time.Second)
	assert.True(t, bytes.HasSuffix([]byte(got), []byte("Z")), "UTC: %q", got)
}

func TestObjectLockHeaders_AnObjectUnderTheBucketDefaultRetentionNamesItOnHeadGetAnd206(t *testing.T) {
	// Arrange: a GOVERNANCE default retention of 30 days; the PUT writes the
	// object_locks row.
	f := setupLockFixture(t)
	_, err := f.db.Exec(`UPDATE buckets SET object_lock_enabled = TRUE, default_retention_mode = 'GOVERNANCE', default_retention_days = 30
		WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)
	f.putObject(t, "retained.txt", "retained bytes")
	_, until := f.retentionRow(t, "retained.txt")

	// Act / Assert: HEAD.
	w := f.headObject(t, "retained.txt", "")
	require.Equal(t, http.StatusOK, w.Code)
	assertLockHeaders(t, w.Header(), "GOVERNANCE", "OFF", until)

	// GET 200.
	w = f.getObject(t, "retained.txt", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assertLockHeaders(t, w.Header(), "GOVERNANCE", "OFF", until)

	// GET 206.
	w = f.getObject(t, "retained.txt", map[string]string{"Range": "bytes=0-3"})
	require.Equal(t, http.StatusPartialContent, w.Code)
	assertLockHeaders(t, w.Header(), "GOVERNANCE", "OFF", until)
}

func TestObjectLockHeaders_ALegalHoldIsON(t *testing.T) {
	f := setupLockFixture(t)
	f.putObject(t, "held.txt", "held bytes")
	body := `<LegalHold xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>ON</Status></LegalHold>`
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/held.txt?legal-hold", bytes.NewReader([]byte(body)))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	rec := httptest.NewRecorder()
	f.server.handlePutObjectLegalHold(rec, req, &S3Request{Bucket: f.bucket, Object: "held.txt", TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, rec.Code)

	w := f.headObject(t, "held.txt", "")
	require.Equal(t, http.StatusOK, w.Code)
	// A hold alone: no retention mode or date, the hold ON.
	assertLockHeaders(t, w.Header(), "", "ON", time.Time{})

	w = f.getObject(t, "held.txt", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ON", w.Header().Get("x-amz-object-lock-legal-hold"))
}

func TestObjectLockHeaders_NoneWithoutARow(t *testing.T) {
	f := setupLockFixture(t)
	f.putObject(t, "plain.txt", "plain bytes")

	for _, w := range []*httptest.ResponseRecorder{f.headObject(t, "plain.txt", ""), f.getObject(t, "plain.txt", nil)} {
		require.Equal(t, http.StatusOK, w.Code)
		for _, h := range []string{"x-amz-object-lock-mode", "x-amz-object-lock-retain-until-date", "x-amz-object-lock-legal-hold"} {
			_, present := w.Header()[http.CanonicalHeaderKey(h)]
			assert.False(t, present, "%s sent for an object with no lock row", h)
		}
	}
}

func TestObjectLockHeaders_AVersionCarriesTheKeysLock(t *testing.T) {
	// The lock row is the key's (object_locks has no version column; it is
	// what checkObjectLock enforces on every version), so HEAD ?versionId
	// names it too.
	f := setupLockFixture(t)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled', object_lock_enabled = TRUE, default_retention_mode = 'COMPLIANCE', default_retention_days = 1
		WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID) })
	w := f.putObject(t, "v.txt", "version one")
	vid := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid)
	_, until := f.retentionRow(t, "v.txt")

	w = f.headObject(t, "v.txt", "?versionId="+vid)
	require.Equal(t, http.StatusOK, w.Code)
	assertLockHeaders(t, w.Header(), "COMPLIANCE", "OFF", until)
}
