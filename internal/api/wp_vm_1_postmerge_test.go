package api

// Stage 1a Prompt 8, Part 0: the WP-VM-1 post-merge probe
// (docs/reviews/WP-P8.md). A Suspended plain DELETE of a chunked object
// under Object Lock is refused before the null row is touched; a chunked
// copy into an Enabled bucket under `versioned_chunking`, then every
// version deleted, keeps the ledger equal to the bill at each step and
// releases each manifest exactly once.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockState is what a refused delete must leave as it was: the head row,
// the key's version rows, the retired queue, the manifest and the bill.
type lockState struct {
	headChunked bool
	headAddr    string
	versions    int
	markers     int
	retired     int
	refs        int
	used        int64
}

func (f *mpChunkFixture) lockState(t *testing.T, key string) lockState {
	t.Helper()
	var s lockState
	s.headChunked, s.headAddr = f.head(t, key)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE is_delete_marker)
		FROM object_versions WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&s.versions, &s.markers))
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1`, f.tenantID).Scan(&s.retired))
	s.refs, _ = f.manifestRows(t, manifestKeyOf(t, f.db, f.tenantID, "test-bucket", key))
	s.used = f.used(t)
	return s
}

func TestVM1PM_SuspendedDeleteOfAChunkedObjectUnderObjectLock_IsRefusedBeforeTheNullRow(t *testing.T) {
	locks := map[string]string{
		"compliance retention": `INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
			VALUES ($1, 'test-bucket', $2, 'COMPLIANCE', NOW() + INTERVAL '30 days')`,
		"governance retention": `INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
			VALUES ($1, 'test-bucket', $2, 'GOVERNANCE', NOW() + INTERVAL '30 days')`,
		"legal hold": `INSERT INTO object_locks (tenant_id, bucket, object_key, legal_hold) VALUES ($1, 'test-bucket', $2, TRUE)`,
	}
	// The two shapes of a Suspended bucket's chunked current object: written
	// before versioning (a head row, no version row: the unanchored null
	// version) and written while Suspended under the flag (a null version row).
	shapes := map[string]func(t *testing.T, f *mpChunkFixture, key string) []byte{
		"pre-versioning": func(t *testing.T, f *mpChunkFixture, key string) []byte {
			b := vbody(f, 4096, 1)
			require.Equal(t, http.StatusOK, mustCode(f.putBody(t, key, b)))
			f.versioning(t, "Suspended")
			return b
		},
		"written suspended": func(t *testing.T, f *mpChunkFixture, key string) []byte {
			f.versioning(t, "Suspended")
			versionedChunkingFor(t, f, true)
			b := vbody(f, 4096, 2)
			require.Equal(t, http.StatusOK, mustCode(f.putBody(t, key, b)))
			return b
		},
	}
	for lockName, lockSQL := range locks {
		for shapeName, write := range shapes {
			t.Run(lockName+"/"+shapeName, func(t *testing.T) {
				f := setupMPChunkFixture(t, false)
				body := write(t, f, "obj.bin")
				chunked, _ := f.head(t, "obj.bin")
				require.True(t, chunked)
				_, err := f.db.Exec(lockSQL, f.tenantID, "obj.bin")
				require.NoError(t, err)
				t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_locks WHERE tenant_id = $1`, f.tenantID) })
				before := f.lockState(t, "obj.bin")

				single := f.req(t, "DELETE", "/test-bucket/obj.bin", nil, nil)
				res := f.batchDelete(t, batchEntry{key: "obj.bin"})

				assert.Equal(t, http.StatusForbidden, single.Code, single.Body.String())
				require.Len(t, res.Errors, 1, "DeleteObjects refuses the key too")
				assert.Equal(t, "AccessDenied", res.Errors[0].Code)
				assert.Empty(t, res.Deleted)
				assert.Equal(t, before, f.lockState(t, "obj.bin"), "nothing touched: head row, null row, retired queue, manifest, bill")
				f.ledgerAgrees(t, "after the refused deletes")
				assert.Equal(t, body, f.getBytes(t, "obj.bin", ""))
			})
		}
	}
}

func TestVM1PM_ChunkedCopyIntoAnEnabledBucket_ThenEveryVersionDeleted_ReleasesEachManifestOnce(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Enabled")
	versionedChunkingFor(t, f, true)
	src := vbody(f, 12<<10, 7)
	// keep.bin shares every chunk of src and lives on: a manifest released
	// twice takes a reference keep.bin owns.
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "keep.bin", src)))
	keepRefs := f.chunkRefCounts(t, "keep.bin")
	type step struct{ key, vid string }
	var versions []step
	put := func(key string, b []byte) {
		w := f.req(t, "PUT", "/test-bucket/"+key, b, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		versions = append(versions, step{key, w.Header().Get("x-amz-version-id")})
		f.ledgerAgrees(t, "PUT "+key)
	}
	cp := func(from, to string) {
		w := f.req(t, "PUT", "/test-bucket/"+to, nil, map[string]string{"x-amz-copy-source": "/test-bucket/" + from})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		chunked, _ := f.head(t, to)
		require.True(t, chunked, "a manifest copy")
		versions = append(versions, step{to, w.Header().Get("x-amz-version-id")})
		f.ledgerAgrees(t, "COPY "+from+" → "+to)
	}
	put("src.bin", src)
	cp("src.bin", "dst.bin")
	cp("src.bin", "dst.bin")
	put("src.bin", vbody(f, 12<<10, 8))
	cp("src.bin", "dst.bin")
	for _, v := range versions {
		require.NotEmpty(t, v.vid)
	}
	require.Equal(t, int64(6*(12<<10)), f.used(t), "keep + 5 versions, each at its logical size")
	var manifests int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(DISTINCT storage_key) FROM object_versions
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key IN ('src.bin', 'dst.bin') AND backend_name = $2`,
		f.tenantID, chunkedBackendSentinel).Scan(&manifests))
	require.Equal(t, 5, manifests, "every version has its own manifest")

	for _, v := range versions {
		w := f.req(t, "DELETE", "/test-bucket/"+v.key+"?versionId="+v.vid, nil, nil)
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		f.ledgerAgrees(t, "DELETE "+v.key+"?versionId="+v.vid)
	}
	assert.Equal(t, int64(12<<10), f.used(t), "only keep.bin is billed")
	assert.Equal(t, 5, f.retiredManifests(t), "five manifests retired, none released in a request")

	first := f.collectAfterGrace(t)
	second := f.collectAfterGrace(t)

	assert.Equal(t, 5, first.Deleted, "each retired manifest released by the first run")
	assert.Zero(t, second.Deleted, "and never again")
	assert.Zero(t, f.retiredManifests(t))
	for _, at := range []string{"src.bin", "dst.bin"} {
		var refs int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1
			AND bucket_name = 'test-bucket' AND left(object_key, length($2) + 1) = $2 || '@'`, f.tenantID, at).Scan(&refs))
		assert.Zero(t, refs, "%s: no manifest left", at)
	}
	assert.Equal(t, keepRefs, f.chunkRefCounts(t, "keep.bin"), "keep.bin's chunks keep exactly their references: nothing released twice")
	assert.Equal(t, src, f.getBytes(t, "keep.bin", ""))
	f.ledgerAgrees(t, "after the collector")
}

// chunkRefCounts is the content-index ref_count of every chunk key's
// current manifest references.
func (f *mpChunkFixture) chunkRefCounts(t *testing.T, key string) map[string]int64 {
	t.Helper()
	rows, err := f.db.Query(`SELECT g.plaintext_hash, g.ref_count FROM global_content_index g
		JOIN tenant_chunk_refs r ON r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
		WHERE r.tenant_id::text = $1 AND r.bucket_name = 'test-bucket' AND r.object_key = $2`,
		f.tenantID, manifestKeyOf(t, f.db, f.tenantID, "test-bucket", key))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var h string
		var n int64
		require.NoError(t, rows.Scan(&h, &n))
		out[h] = n
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out)
	return out
}
