package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/tenant"
)

// R2-01: Object Lock must hold on versioning-enabled buckets — the only
// configuration AWS runs Object Lock in. Before this fix the delete-marker
// branch ran before checkObjectLock, dropped the head row, and the next PUT
// (no head row → no lock check) overwrote the retained bytes in place.

func lockKey(t *testing.T, f *versioningFixture, key, mode string, until time.Time) {
	t.Helper()
	_, err := f.db.Exec(`
		INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			retention_mode = $4, retain_until_date = $5, legal_hold = FALSE`,
		f.tenantID, f.bucket, key, mode, until)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec("DELETE FROM object_locks WHERE tenant_id = $1", f.tenantID) })
}

// putObjectRaw is putObject without the 200 assertion.
func (f *versioningFixture) putObjectRaw(t *testing.T, key, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key, bytes.NewReader([]byte(content)))
	req.Header.Set("Content-Type", "text/plain")
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, key)
	return w
}

func onDisk(t *testing.T, f *versioningFixture, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.bucket), key))
	require.NoError(t, err)
	return string(b)
}

func TestObjectLock_VersionedBucket_DeleteMarkerRefused(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")
	key := "retained.bin"
	f.putObject(t, key, "RETAINED")
	lockKey(t, f, key, "COMPLIANCE", time.Now().Add(24*time.Hour))

	w := f.deleteObject(t, key)
	assert.Equal(t, http.StatusForbidden, w.Code, "a delete marker on a locked key would unbill and hide retained bytes")
	assert.Empty(t, w.Header().Get("x-amz-delete-marker"))

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, f.bucket, key).Scan(&n))
	assert.Equal(t, 1, n, "head row must survive")
	assert.Equal(t, "RETAINED", onDisk(t, f, key))
}

func TestObjectLock_VersionedBucket_DeleteVersionRefused(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")
	key := "retained-v.bin"
	w1 := f.putObject(t, key, "RETAINED")
	vid := w1.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid)
	lockKey(t, f, key, "COMPLIANCE", time.Now().Add(24*time.Hour))

	w := f.deleteObjectVersion(t, key, vid)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 1, f.countVersions(t, key))
}

func TestObjectLock_PutRefusedEvenWithoutHeadRow(t *testing.T) {
	// A live lock row with no head row (the pre-fix marker path, or a crash
	// between head delete and lock delete) must still protect the key: the
	// backend bytes are what the lock retains.
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")
	key := "orphan-lock.bin"
	f.putObject(t, key, "RETAINED")
	lockKey(t, f, key, "COMPLIANCE", time.Now().Add(24*time.Hour))
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, f.bucket, key)
	require.NoError(t, err)

	w := f.putObjectRaw(t, key, "OVERWRITE")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "RETAINED", onDisk(t, f, key))
}

func TestObjectLock_ExpiredLockRowRemovedOnHardDelete(t *testing.T) {
	// Unversioned bucket: deleting a key whose retention has expired must
	// also drop the object_locks row, otherwise the PUT-side check (which no
	// longer depends on a head row) would refuse the next upload.
	f := setupVersioningFixture(t)
	key := "expired.bin"
	f.putObject(t, key, "OLD")
	lockKey(t, f, key, "COMPLIANCE", time.Now().Add(-time.Hour))

	w := f.deleteObject(t, key)
	require.Equal(t, http.StatusNoContent, w.Code)

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_locks WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, f.bucket, key).Scan(&n))
	assert.Equal(t, 0, n, "lock row must go with the object")

	w = f.putObjectRaw(t, key, "NEW")
	assert.Equal(t, http.StatusOK, w.Code)
}
