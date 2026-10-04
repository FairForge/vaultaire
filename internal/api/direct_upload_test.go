package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// stubR2 is a local driver that also offers direct uploads: the presigned URL
// is a marker, and Stat reads what was "uploaded" into the local store.
type stubR2 struct {
	engine.Driver
	presigned int
}

func (s *stubR2) Name() string { return "r2" }

func (s *stubR2) PresignPut(ctx context.Context, container, artifact, contentType string, ttl time.Duration) (string, error) {
	s.presigned++
	return "https://r2.example/" + container + "/" + artifact + "?X-Amz-Expires=" + ttl.String(), nil
}

func (s *stubR2) Stat(ctx context.Context, container, artifact string) (int64, string, string, error) {
	rc, err := s.Driver.Get(ctx, container, artifact)
	if err != nil {
		return 0, "", "", err
	}
	defer func() { _ = rc.Close() }()
	var n int64
	buf := make([]byte, 4096)
	for {
		k, rerr := rc.Read(buf)
		n += int64(k)
		if rerr != nil {
			break
		}
	}
	return n, `"stub-etag"`, "image/png", nil
}

func TestMgmtDirectUpload_PublicBucketOnly_ThenRegistersTheObject(t *testing.T) {
	f := setupVersioningFixture(t)
	stub := &stubR2{Driver: drivers.NewLocalDriver(t.TempDir(), f.server.logger)}
	f.eng.AddDriver("r2", stub)

	// A private bucket is refused before any presign.
	w := f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads", `{"key":"img/a.png","content_type":"image/png"}`, f.server.handleMgmtCreateDirectUpload)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "bucket_not_public")
	assert.Equal(t, 0, stub.presigned)

	_, err := f.db.Exec(`UPDATE buckets SET visibility = 'public-read' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)

	// Public: a presigned PUT comes back with the key under the tenant container.
	w = f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads", `{"key":"img/a.png","content_type":"image/png"}`, f.server.handleMgmtCreateDirectUpload)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created mgmtDirectUpload
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(t, "PUT", created.Method)
	assert.Contains(t, created.URL, f.tenantID+"_"+f.bucket+"/img/a.png")
	assert.Equal(t, 1, stub.presigned)

	// Complete before the bytes exist: refused, nothing registered.
	w = f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads/complete", `{"key":"img/a.png"}`, f.server.handleMgmtCompleteDirectUpload)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'img/a.png'`, f.tenantID, f.bucket).Scan(&n))
	assert.Equal(t, 0, n)

	// The client PUTs to R2 (here: into the stub's store), then completes.
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	require.NoError(t, stub.Driver.Put(ctx, f.tenantID+"_"+f.bucket, "img/a.png", strings.NewReader(strings.Repeat("x", 5000))))
	w = f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads/complete", `{"key":"img/a.png"}`, f.server.handleMgmtCompleteDirectUpload)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var done mgmtDirectUploadComplete
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &done))
	assert.Equal(t, int64(5000), done.Size)
	assert.Equal(t, "r2", done.Backend)

	var backend, ctype string
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT backend_name, content_type, size_bytes FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'img/a.png'`, f.tenantID, f.bucket).Scan(&backend, &ctype, &size))
	assert.Equal(t, "r2", backend, "routing truth: the head row names the backend the bytes are on")
	assert.Equal(t, "image/png", ctype)
	assert.Equal(t, int64(5000), size)
}

func TestMgmtDirectUpload_RejectsBadKeysAndMissingDriver(t *testing.T) {
	f := setupVersioningFixture(t)
	w := f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads", `{"key":"/abs.png"}`, f.server.handleMgmtCreateDirectUpload)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_key")
	// no r2 driver registered in this fixture
	w = f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads", `{"key":"ok.png"}`, f.server.handleMgmtCreateDirectUpload)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "direct_upload_unavailable")
}
