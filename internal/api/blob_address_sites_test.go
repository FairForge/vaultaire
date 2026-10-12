package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R2-1 (2B): the same rule as blob_address_test.go at every other entry
// point that reads, copies, moves, checks or deletes whole-object bytes —
// /cdn, the CopyObject source, Smart demotion / flip-back / reclaim, the
// vault parity, the deletion runner, routing truth. Each test leaves a stale
// blob at <key> and the row's bytes at its storage key.

func TestCDN_ServesTheRowsStorageKey(t *testing.T) {
	f := setupCDNFixture(t)
	fresh := []byte("fresh public bytes at the storage key")
	container := f.tenantID + "_" + f.bucket
	_, err := f.server.engine.Put(common.WithTenantID(context.Background(), f.tenantID), container, f.key+"@v1", bytes.NewReader(fresh))
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE object_head_cache SET storage_key = $4, size_bytes = $5, etag = $6
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, f.key, f.key+"@v1", len(fresh), etagOf(fresh))
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, fresh, w.Body.Bytes(), "/cdn reads the row's address, not the blob at <key>")
}

// pointLedgerRowAt is pointRowAt for the demotion fixtures (local drivers,
// path = <dir>/<tenant>_<bucket>/<artifact>).
func (f *ledgerFixture) pointRowAt(bucket, key, storageKey, body string) {
	f.t.Helper()
	_, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = $4, size_bytes = $5, etag = $6
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, bucket, key, storageKey, len(body), etagOf([]byte(body)))
	require.NoError(f.t, err)
}

func TestCopyObject_SourceIsTheRowsStorageKey(t *testing.T) {
	f := setupLedgerFixture(t)
	f.put("b", "src", "stale source bytes at the key")
	f.blob(f.hotDir, "b", "src@v1", "fresh source bytes")
	f.pointRowAt("b", "src", "src@v1", "fresh source bytes")

	r := f.req("PUT", "/b/dest", nil)
	r.Header.Set("x-amz-copy-source", "/b/src")
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, r, &S3Request{Bucket: "b", Object: "dest", TenantID: f.tenantID})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "fresh source bytes", f.read(f.hotPath("b", "dest")), "the copy read the source row's address")
}

func TestSmartDemotion_CopiesUnderTheStorageKeyAndFlipBackKeepsIt(t *testing.T) {
	// Arrange: an idle object whose row names k@v1; a stale blob at k.
	f := setupLedgerFixture(t)
	f.object("b", "k", 100, 30, 20)
	f.blob(f.hotDir, "b", "k@v1", "fresh hot bytes")
	f.pointRowAt("b", "k", "k@v1", "fresh hot bytes")

	// Act: demote.
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert: the cold copy is the row's bytes under the SAME address.
	require.NoError(t, err)
	require.Equal(t, 1, res.Demoted, "%+v", res)
	assert.Equal(t, "geyser", f.backendOf("b", "k"))
	assert.Equal(t, "fresh hot bytes", f.read(f.coldPath("b", "k@v1")), "cold copy under the storage key")
	assert.False(t, f.coldExists("b", "k"), "nothing written at <key> on cold")
	var ledgerKey *string
	require.NoError(t, f.db.QueryRow(`SELECT storage_key FROM smart_demotions WHERE tenant_id=$1 AND bucket='b' AND object_key='k'`, f.tenantID).Scan(&ledgerKey))
	require.NotNil(t, ledgerKey)
	assert.Equal(t, "k@v1", *ledgerKey, "the ledger records the address it copied under")

	// A read inside the grace flips back to the hot copy at the same address.
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, f.req("GET", "/b/k", nil), "b", "k")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "fresh hot bytes", w.Body.String())
	assert.Equal(t, "idrive", f.backendOf("b", "k"))
	assert.False(t, f.coldExists("b", "k@v1"), "flip-back dropped the cold copy at the storage key")
	assert.Equal(t, "fresh hot bytes", f.read(f.hotPath("b", "k@v1")))
}

func TestReclaim_DeletesTheHotCopyAtTheStorageKeyNeverTheObjectKey(t *testing.T) {
	// Arrange: demoted two days ago under k@v1; a stale unrelated blob at k.
	f := setupLedgerFixture(t)
	f.object("b", "k", 100, 30, 20, onBackend("geyser"))
	f.blob(f.hotDir, "b", "k", "stale bytes at the key on hot")
	f.blob(f.hotDir, "b", "k@v1", "hot copy")
	f.blob(f.coldDir, "b", "k@v1", "hot copy")
	f.pointRowAt("b", "k", "k@v1", "hot copy")
	f.openLedger("b", "k", etagOf([]byte("hot copy")), 48*time.Hour)
	_, err := f.db.Exec(`UPDATE smart_demotions SET storage_key = 'k@v1' WHERE tenant_id = $1 AND bucket = 'b' AND object_key = 'k'`, f.tenantID)
	require.NoError(t, err)

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	_, closed, outcome, _ := f.ledger("b", "k")
	assert.True(t, closed)
	assert.Equal(t, "deleted", outcome)
	assert.False(t, f.hotExists("b", "k@v1"), "the hot copy at the storage key is reclaimed")
	assert.Equal(t, "stale bytes at the key on hot", f.read(f.hotPath("b", "k")), "the blob at <key> is not the ledger's to delete")
	assert.Equal(t, "hot copy", f.read(f.coldPath("b", "k@v1")), "the live cold copy stays")
}

func TestVaultParity_ProtectsAndRebuildsFromTheStorageKey(t *testing.T) {
	// Arrange: the row's bytes at films/x.mkv@v1, garbage of the same size at
	// the key.
	f := setupParityFixture(t)
	size := 1<<20 + 77
	key := "films/x.mkv"
	body, _ := f.object(key, size)
	container := f.tn.NamespaceContainer(f.bucket)
	require.NoError(t, os.WriteFile(filepath.Join(f.geyserDir, container, key+"@v1"), body, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(f.geyserDir, container, key), bytes.Repeat([]byte{0xEE}, size), 0o600))
	_, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = $3 WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, key, key+"@v1")
	require.NoError(t, err)

	// Act: protect, then lose one parity shard and Geyser's whole GET — the
	// rebuild needs the data ranges.
	f.run()
	state, _, lastErr, _ := f.row(key)
	require.Equal(t, "complete", state, lastErr)
	prefix := shardPrefix(f.bucket, key, etagOf(body))
	require.NoError(t, os.Remove(filepath.Join(f.legDir, parityContainer(f.tenantID), shardArtifact(prefix, 1))))
	f.geyser.failGet.Store(true)
	w := f.get(f.adapter(), key, "")

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.True(t, bytes.Equal(body, w.Body.Bytes()), "shards computed from, and data ranges read at, the storage key")
}

func TestDeletionRunner_DeletesTheStorageKeyAndReleasesQuota(t *testing.T) {
	f := setupDeletionFixture(t)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Runner Co', $2, $3, $4)`,
		f.tenantID, f.email, "VKRN"+f.suffix, "SKRN"+f.suffix)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier) VALUES ($1, 1000000, 100, 'standard')`, f.tenantID)
	f.writeBlob(f.primary, "k", "stale blob at the key")
	f.writeBlob(f.primary, "k@v1", "the object")
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, storage_key)
		VALUES ($1, $2, 'k', 100, 'e', 'local', 'k@v1')`, f.tenantID, f.bucket)

	te := &TenantErasure{TenantID: f.tenantID}
	err := f.runner.deleteObject(common.WithTenantID(context.Background(), f.tenantID), &tenant.Tenant{ID: f.tenantID},
		f.bucket, "k", "k@v1", "local", false, te)

	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(f.primary, f.container(), "k@v1"))
	assert.True(t, os.IsNotExist(statErr), "the blob the row names is deleted")
	_, statErr = os.Stat(filepath.Join(f.primary, f.container(), "k"))
	assert.NoError(t, statErr, "the runner's delete is the row's address; <key> leftovers are the sweep's")
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 0, f.count(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID), "quota released")
}

func TestRoutingTruth_AsksTheRecordedBackendForTheStorageKey(t *testing.T) {
	f := setupRoutingTruthFixture(t)
	dir := filepath.Join(f.hotDir, f.tenantID+"_b")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	// at-sk-1, at-sk-2: bytes at the storage key only → present.
	for _, k := range []string{"at-sk-1", "at-sk-2"} {
		f.row("b", k, f.hot, false)
		require.NoError(t, os.WriteFile(filepath.Join(dir, k+"@v1"), []byte("0123456789"), 0o600))
	}
	// at-key: bytes at <key> only, the row names a storage key → missing.
	f.row("b", "at-key", f.hot, true)
	_, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = object_key || '@v1' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	res := f.run()

	bt := res.Backends[f.hot]
	require.NotNil(t, bt, "%+v", res)
	assert.Equal(t, 2, bt.Present, "the storage key's bytes are there")
	assert.Equal(t, 1, bt.Missing, "bytes at <key> are not the row's")
}
