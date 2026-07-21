// s3_chunked_integrity_test.go — regression tests for the ultra-review
// findings on the chunked path (review PR A):
//
//	F1: chunked GET must never fall through to the plain path — a stale
//	    whole-object blob can live at the same key from before chunking.
//	F2: manifest swap + head-cache upsert must commit atomically — a failed
//	    upsert must leave the PREVIOUS version fully intact and retrievable.
//	F3: versioned buckets must skip chunking (manifests aren't version-aware).
//	F11: physical_size must stay truthful (0 = fully deduplicated), not be
//	     forced to logical size.
package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChunkedGet_NeverServesStaleWholeObjectBlob — F1. An object was stored
// whole (pre-chunking), then overwritten via the chunked path. If the chunked
// manifest is damaged, GET must 500 — the old fallthrough served the previous
// object's bytes under the new ETag/size.
func TestChunkedGet_NeverServesStaleWholeObjectBlob(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)
	container := f.tenant.NamespaceContainer("test-bucket")

	// v1: whole-object blob, as the plain path would have stored it.
	oldBytes := []byte("OLD VERSION BYTES — must never be served again")
	_, err := f.eng.Put(context.Background(), container, "swap.bin", bytes.NewReader(oldBytes))
	require.NoError(t, err)

	// v2: chunked overwrite.
	newBytes := generateTestData(8 * 1024)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "swap.bin", newBytes).Code)

	// The chunked PUT must have removed the stale whole-object blob.
	stalePath := filepath.Join(f.tempDir, container, "swap.bin")
	_, statErr := os.Stat(stalePath)
	assert.True(t, os.IsNotExist(statErr),
		"chunked PUT must delete the stale whole-object blob at the same key")

	// Healthy chunked GET returns the new bytes.
	gw := stringTenantGet(t, f, "swap.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, newBytes, gw.Body.Bytes())

	// Damage the manifest, and re-plant a stale blob to tempt the fallthrough.
	_, err = f.db.Exec(`DELETE FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'swap.bin'`,
		f.tenantID)
	require.NoError(t, err)
	_, err = f.eng.Put(context.Background(), container, "swap.bin", bytes.NewReader(oldBytes))
	require.NoError(t, err)

	gw = stringTenantGet(t, f, "swap.bin")
	assert.GreaterOrEqual(t, gw.Code, 500,
		"damaged chunked object must fail loudly, not fall through")
	assert.NotEqual(t, oldBytes, gw.Body.Bytes(),
		"the stale whole-object blob must never be served")
}

// TestChunkedPut_FailedInstallLeavesPreviousVersionIntact — F2. When the
// combined manifest-swap + head-upsert transaction fails, the previous
// version must remain fully retrievable (old manifest, old head row) — not
// split-brained with a destroyed manifest and a stale head row.
func TestChunkedPut_FailedInstallLeavesPreviousVersionIntact(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)

	v1 := generateTestData(8 * 1024)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "atomic.bin", v1).Code)

	var v1Refs int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'atomic.bin'`,
		f.tenantID).Scan(&v1Refs))
	require.Positive(t, v1Refs)

	// Head-cache writes for this tenant now fail — the whole install must
	// roll back as one unit.
	installHeadCacheFailure(t, f.db, f.tenantID)

	v2 := generateTestData(8 * 1024)
	w := stringTenantPut(t, f, "atomic.bin", v2)
	require.GreaterOrEqual(t, w.Code, 500, "failed install must fail the PUT")

	// The old manifest must be untouched (same refs), and GET must still
	// return v1's bytes.
	var refsAfter int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'atomic.bin'`,
		f.tenantID).Scan(&refsAfter))
	assert.Equal(t, v1Refs, refsAfter,
		"failed install must not destroy the previous version's manifest")

	dropHeadCacheFailure(t, f.db, f.tenantID)
	gw := stringTenantGet(t, f, "atomic.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, v1, gw.Body.Bytes(),
		"previous version must remain retrievable after a failed overwrite")
}

// TestChunkedPut_VersionedBucketSkipsChunking — F3. tenant_chunk_refs has no
// version_id, so chunked overwrites on versioned buckets silently destroy
// prior versions. Versioned buckets must keep the plain (whole-object) path.
func TestChunkedPut_VersionedBucketSkipsChunking(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)

	_, err := f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, versioning_status)
		VALUES ($1, 'test-bucket', 'Enabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'Enabled'`,
		f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
	})

	content := generateTestData(8 * 1024) // above the 1 KiB test threshold
	w := stringTenantPut(t, f, "versioned.bin", content)
	require.Equal(t, http.StatusOK, w.Code)

	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'versioned.bin'`,
		f.tenantID).Scan(&isChunked))
	assert.False(t, isChunked,
		"versioned buckets must take the plain path until manifests are version-aware")

	gw := stringTenantGet(t, f, "versioned.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())
}

// TestChunkedPut_FullyDedupedReportsZeroPhysical — F11. A re-upload of
// identical content adds zero new physical bytes; physical_size must say so
// instead of being forced to the logical size (which reported perfect dedup
// as "no dedup" and would poison COGS math).
func TestChunkedPut_FullyDedupedReportsZeroPhysical(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)

	content := generateTestData(8 * 1024)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "phys-a.bin", content).Code)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "phys-b.bin", content).Code)

	var physical int64
	require.NoError(t, f.db.QueryRow(`
		SELECT physical_size FROM object_metadata
		WHERE tenant_id::text = $1 AND object_key = 'phys-b.bin'`,
		f.tenantID).Scan(&physical))
	assert.Zero(t, physical,
		"a fully-deduplicated upload adds no new physical bytes")
}
