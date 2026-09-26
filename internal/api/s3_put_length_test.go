package api

import (
	"bytes"
	"crypto/md5" // #nosec G501 — Content-MD5 is the S3 wire digest
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/tenant"
)

// R2-05 / R2-06 / R2-12: everything that can reject a PUT for a client-side
// reason must do so BEFORE the backend write (the write is in place, so a late
// rejection destroys the previous object), and the stored size must be the
// measured size, never a client-declared one.

func adapterPut(t *testing.T, f *adapterTestFixture, key string, body []byte, hdr func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	if hdr != nil {
		hdr(req)
	}
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	return w
}

func adapterDisk(t *testing.T, f *adapterTestFixture, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.tempDir, f.tenant.NamespaceContainer("test-bucket"), key))
	require.NoError(t, err)
	return string(b)
}

func putHeadRow(t *testing.T, f *adapterTestFixture, key string) (int64, string, bool) {
	t.Helper()
	var size int64
	var etag string
	err := f.db.QueryRow(`SELECT size_bytes, etag FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, "test-bucket", key).Scan(&size, &etag)
	if err != nil {
		return 0, "", false
	}
	return size, etag, true
}

func s3Code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e), "body: %s", w.Body.String())
	return e.Code
}

func TestHandlePut_DeclaredLengthMismatch_Rejected(t *testing.T) {
	f := setupAdapterFixture(t)
	body := bytes.Repeat([]byte("x"), 5000)

	// A plain (non aws-chunked) body claiming a 1-byte logical size: the
	// header is transport framing for aws-chunked only and is ignored here,
	// so the row records the MEASURED size (before: size_bytes=1 for 5000
	// stored bytes, quota +1).
	w := adapterPut(t, f, "spoof.bin", body, func(r *http.Request) {
		r.Header.Set("x-amz-decoded-content-length", "1")
	})
	require.Equal(t, http.StatusOK, w.Code)
	size, _, ok := putHeadRow(t, f, "spoof.bin")
	require.True(t, ok)
	assert.Equal(t, int64(5000), size)

	// A body that ends before its declared Content-Length (the local driver
	// happily stores what arrived) must be rejected, not recorded at the
	// declared size.
	req := httptest.NewRequest("PUT", "/test-bucket/short.bin", bytes.NewReader(body[:10]))
	req.ContentLength = 5000
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w = httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "short.bin")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, ErrIncompleteBody, s3Code(t, w))
	_, _, ok = putHeadRow(t, f, "short.bin")
	assert.False(t, ok, "no head row for a rejected PUT")
}

func TestHandlePut_ContentMD5_Verified(t *testing.T) {
	f := setupAdapterFixture(t)
	body := []byte("digest me")
	sum := md5.Sum(body) // #nosec G401
	good := base64.StdEncoding.EncodeToString(sum[:])
	other := md5.Sum([]byte("something else")) // #nosec G401
	bad := base64.StdEncoding.EncodeToString(other[:])

	w := adapterPut(t, f, "md5.bin", body, func(r *http.Request) { r.Header.Set("Content-MD5", good) })
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, fmt.Sprintf(`"%x"`, sum), w.Header().Get("ETag"))

	w = adapterPut(t, f, "md5.bin", []byte("replacement"), func(r *http.Request) { r.Header.Set("Content-MD5", bad) })
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, ErrBadDigest, s3Code(t, w))
	size, etag, ok := putHeadRow(t, f, "md5.bin")
	require.True(t, ok)
	assert.Equal(t, int64(len(body)), size)
	assert.Equal(t, fmt.Sprintf("%x", sum), etag)
	// The plain path streams to the backend before the digest is known, so
	// the stored bytes ARE replaced (in-place write, WP-R2-1); what this
	// guarantees is that the head row — HEAD/GET/billing truth — never
	// describes a body the client did not vouch for.

	w = adapterPut(t, f, "md5-malformed.bin", body, func(r *http.Request) { r.Header.Set("Content-MD5", "not base64!") })
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, ErrInvalidDigest, s3Code(t, w))
}

func TestHandlePut_MetadataRejectedBeforeWrite(t *testing.T) {
	f := setupAdapterFixture(t)
	w := adapterPut(t, f, "meta.bin", []byte("ORIGINAL"), nil)
	require.Equal(t, http.StatusOK, w.Code)

	w = adapterPut(t, f, "meta.bin", []byte("REPLACEMENT-LONGER"), func(r *http.Request) {
		for i := 0; i <= metadataMaxKeys; i++ {
			r.Header.Set(fmt.Sprintf("x-amz-meta-k%d", i), "v")
		}
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, ErrInvalidRequest, s3Code(t, w))
	assert.Equal(t, "ORIGINAL", adapterDisk(t, f, "meta.bin"), "rejected PUT must not touch the stored object")
	size, _, ok := putHeadRow(t, f, "meta.bin")
	require.True(t, ok)
	assert.Equal(t, int64(len("ORIGINAL")), size)
}
