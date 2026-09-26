package api

import (
	"bytes"
	"crypto/md5" // #nosec G501 — SSE-C key MD5 is part of the S3 wire protocol
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
)

// R2-02: a Range GET on an encrypted object must slice the PLAINTEXT. The
// backend-native GetRange fast path reads the stored ciphertext, so it can
// never be used for SSE-S3 / SSE-C objects.

func ssePlaintext() []byte {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func rangeGet(t *testing.T, f *adapterTestFixture, key, rng string, hdr func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	req.Header.Set("Range", rng)
	if hdr != nil {
		hdr(req)
	}
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

func TestHandleGet_Range_SSES3_ServesPlaintextSlice(t *testing.T) {
	f := setupAdapterFixture(t)
	svc, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = svc
	t.Cleanup(func() { _, _ = f.db.Exec("DELETE FROM tenant_encryption_keys WHERE tenant_id = $1", f.tenantID) })

	body := ssePlaintext()
	req := httptest.NewRequest("PUT", "/test-bucket/enc.bin", bytes.NewReader(body))
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "enc.bin")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))

	for _, rng := range []string{"bytes=0-99", "bytes=1000-1999", "bytes=-100", "bytes=4000-"} {
		g := rangeGet(t, f, "enc.bin", rng, nil)
		require.Equal(t, http.StatusPartialContent, g.Code, rng)
		r, perr := parseRangeHeader(rng, int64(len(body)))
		require.NoError(t, perr)
		assert.Equal(t, body[r.start:r.end+1], g.Body.Bytes(), "range %s must be the plaintext slice, not ciphertext", rng)
		assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", r.start, r.end, len(body)), g.Header().Get("Content-Range"))
	}
}

func TestHandleGet_Range_SSEC_ServesPlaintextSlice(t *testing.T) {
	f := setupAdapterFixture(t)
	key := bytes.Repeat([]byte{7}, 32)
	keyB64 := base64.StdEncoding.EncodeToString(key)
	sum := md5.Sum(key) // #nosec G401
	keyMD5 := base64.StdEncoding.EncodeToString(sum[:])
	ssec := func(r *http.Request) {
		r.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		r.Header.Set("x-amz-server-side-encryption-customer-key", keyB64)
		r.Header.Set("x-amz-server-side-encryption-customer-key-MD5", keyMD5)
	}

	body := ssePlaintext()
	req := httptest.NewRequest("PUT", "/test-bucket/ssec.bin", bytes.NewReader(body))
	ssec(req)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "ssec.bin")
	require.Equal(t, http.StatusOK, w.Code)

	g := rangeGet(t, f, "ssec.bin", "bytes=100-299", ssec)
	require.Equal(t, http.StatusPartialContent, g.Code)
	assert.Equal(t, body[100:300], g.Body.Bytes())
	assert.Equal(t, "bytes 100-299/4096", g.Header().Get("Content-Range"))
}
