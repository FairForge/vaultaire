// dedup_gc_coherence_test.go — WP-6 (item 1.7, CR-6): dedup GC must stay
// coherent with the GCI in-memory cache and with concurrent chunked PUTs.
//
// The failure this closes: the GC sweep deleted GCI rows (and chunk blobs)
// WITHOUT invalidating the GCI's in-memory cache. A later PUT of the same
// content dedup-hit the stale cache entry and called IncrementRef on a row
// that no longer existed — which reported success while updating zero rows —
// installing a manifest that pointed at deleted data. Two independent layers
// fix it: the sweep invalidates the cache around the row delete, and
// IncrementRef now reports rows-affected so the PUT re-stores a chunk that
// vanished anyway (stale re-cache race). An advisory lock keyed on
// (dedup_scope, plaintext_hash) serializes the sweep's row+blob delete against
// the PUT-side chunk store.
//
// Also folded in per the ultra-review:
//
//	F5  — GDPR ExecuteDeletion must decrement GCI ref counts for the deleted
//	      tenant's chunk refs (set-based, in the deletion tx) so the tenant's
//	      chunks get marked and swept instead of living forever.
//	F10 — an aborted chunked PUT must release the ref increments it took on
//	      shared chunks (compensating decrements on the error paths).
package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type scopedChunk struct {
	scope, hash, storageKey string
}

func tenantChunks(t *testing.T, db *sql.DB, tenantID, key string) []scopedChunk {
	t.Helper()
	rows, err := db.Query(`
		SELECT r.dedup_scope, r.plaintext_hash, g.storage_key
		FROM tenant_chunk_refs r
		JOIN global_content_index g
		  ON g.dedup_scope = r.dedup_scope AND g.plaintext_hash = r.plaintext_hash
		WHERE r.tenant_id::text = $1 AND r.object_key = $2`, tenantID, key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []scopedChunk
	for rows.Next() {
		var c scopedChunk
		require.NoError(t, rows.Scan(&c.scope, &c.hash, &c.storageKey))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

func deleteChunkedObject(t *testing.T, f *adapterTestFixture, key string) {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/test-bucket/"+key, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", key)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func getChunkedObject(t *testing.T, f *adapterTestFixture, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

// The WP-6 spec scenario: PUT (chunked) → DELETE → GC sweep → PUT the same
// content through the SAME adapter/GCI instance → GET must round-trip. A
// concurrent reader re-populates the GCI cache between the delete and the
// sweep (LookupChunk caches the still-present marked row); without sweep-side
// invalidation, the re-PUT dedup-hits chunks the sweep deleted.
func TestDedupGC_SweepInvalidatesGCICache(t *testing.T) {
	runner, f := setupGCFixture(t)
	ctx := context.Background()

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "coherence-a.bin", content, "application/octet-stream")

	chunks := tenantChunks(t, f.db, f.tenantID, "coherence-a.bin")
	require.NotEmpty(t, chunks)

	deleteChunkedObject(t, f, "coherence-a.bin")

	// Simulate the concurrent reader: LookupChunk on the marked-but-present
	// rows re-populates the in-memory cache after the delete's invalidation.
	for _, c := range chunks {
		res, err := f.adapter.gci.LookupChunk(ctx, c.scope, c.hash)
		require.NoError(t, err)
		require.True(t, res.Exists, "row must still exist before the sweep (marked, ref 0)")
	}

	// Push the marked rows past grace and sweep them.
	_, err := f.db.Exec(`
		UPDATE global_content_index
		SET marked_at = NOW() - INTERVAL '1 day',
		    last_accessed_at = NOW() - INTERVAL '1 day'
		WHERE marked_for_deletion = TRUE AND ref_count = 0`)
	require.NoError(t, err)
	result, err := runner.RunOnce(ctx)
	require.NoError(t, err)
	require.Greater(t, result.Deleted, 0, "sweep must delete the orphaned chunks")

	// Re-PUT the same content through the same adapter/GCI. With a stale
	// cache this dedup-hits deleted rows and the PUT (or the later GET)
	// breaks; with WP-6 the chunks are stored fresh.
	putChunkedObject(t, f, "coherence-b.bin", content, "application/octet-stream")

	gw := getChunkedObject(t, f, "coherence-b.bin")
	require.Equal(t, http.StatusOK, gw.Code, "GET after sweep + re-PUT must succeed")
	assert.Equal(t, content, gw.Body.Bytes(), "round-trip must be byte-identical")

	// And the index must actually hold the re-stored chunks.
	for _, c := range chunks {
		var refCount int
		require.NoError(t, f.db.QueryRow(`
			SELECT ref_count FROM global_content_index
			WHERE dedup_scope = $1 AND plaintext_hash = $2`, c.scope, c.hash).Scan(&refCount))
		assert.GreaterOrEqual(t, refCount, 1, "re-stored chunk %s must be referenced", c.hash[:16])
	}
}

// Even when the cache goes stale despite sweep invalidation (a LookupChunk can
// re-cache a row in the window before the sweep's delete commits), the PUT
// must detect the vanished chunk: IncrementRef reports zero rows updated and
// the chunk is re-stored as new.
func TestChunkedPut_ReStoresVanishedChunk(t *testing.T) {
	f := setupChunkingFixture(t)
	ctx := context.Background()

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "vanish-a.bin", content, "application/octet-stream")
	chunks := tenantChunks(t, f.db, f.tenantID, "vanish-a.bin")
	require.NotEmpty(t, chunks)

	deleteChunkedObject(t, f, "vanish-a.bin")

	// Re-populate the cache from the still-present rows…
	for _, c := range chunks {
		res, err := f.adapter.gci.LookupChunk(ctx, c.scope, c.hash)
		require.NoError(t, err)
		require.True(t, res.Exists)
	}

	// …then delete rows AND blobs directly, bypassing the (now cache-aware)
	// sweep — this is the stale-re-cache race distilled.
	for _, c := range chunks {
		_, err := f.db.Exec(`
			DELETE FROM global_content_index
			WHERE dedup_scope = $1 AND plaintext_hash = $2`, c.scope, c.hash)
		require.NoError(t, err)
		_ = f.eng.Delete(ctx, chunkContainer, c.storageKey)
	}

	// The re-PUT dedup-hits the stale cache; it must recover by re-storing.
	putChunkedObject(t, f, "vanish-b.bin", content, "application/octet-stream")

	gw := getChunkedObject(t, f, "vanish-b.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes(), "vanished chunks must be re-stored, not referenced")
}

// F10: a chunked PUT that aborts after taking ref increments on shared chunks
// must release them. The abort is injected at the manifest install (the same
// per-tenant trigger WP-3 uses), i.e. after every chunk was processed.
func TestChunkedPut_AbortedInstallReleasesRefs(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "abort-base.bin", content, "application/octet-stream")

	refCounts := func() map[string]int {
		out := map[string]int{}
		rows, err := f.db.Query(`
			SELECT g.plaintext_hash, g.ref_count
			FROM global_content_index g
			WHERE EXISTS (
				SELECT 1 FROM tenant_chunk_refs r
				WHERE r.tenant_id::text = $1
				  AND r.dedup_scope = g.dedup_scope
				  AND r.plaintext_hash = g.plaintext_hash)`, f.tenantID)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var h string
			var n int
			require.NoError(t, rows.Scan(&h, &n))
			out[h] = n
		}
		require.NoError(t, rows.Err())
		return out
	}

	before := refCounts()
	require.NotEmpty(t, before)
	for h, n := range before {
		require.Equal(t, 1, n, "precondition: chunk %s referenced exactly once", h[:16])
	}

	installHeadCacheFailure(t, f.db, f.tenantID)

	req := httptest.NewRequest("PUT", "/test-bucket/abort-doomed.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "abort-doomed.bin")
	require.GreaterOrEqual(t, w.Code, 500, "install failure must abort the chunked PUT")

	dropHeadCacheFailure(t, f.db, f.tenantID)

	after := refCounts()
	assert.Equal(t, before, after,
		"aborted chunked PUT must release the ref increments it took (F10)")

	// The base object is untouched.
	gw := getChunkedObject(t, f, "abort-base.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())
}

// F5: GDPR ExecuteDeletion must decrement GCI ref counts for every chunk ref
// the tenant held — set-based, inside the deletion transaction. Shared chunks
// survive with reduced counts; chunks only the deleted tenant referenced
// (notably its tenant-scoped encrypted chunks) drop to zero and are marked so
// the GC sweep reclaims them.
func TestExecuteDeletion_DecrementsGCIRefs(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	// t.Cleanup (not defer) so the connection outlives the row-cleanup
	// callbacks registered below — cleanups run LIFO.
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, db.PingContext(ctx))

	suffix := uuid.New().String()[:8]
	userID := uuid.New().String()
	doomedTenant := "tenant-wp6-" + suffix
	otherTenant := "tenant-wp6-other-" + suffix
	sharedHash := "aa" + strings.Repeat("0", 54) + suffix // referenced by both tenants
	scopedHash := "bb" + strings.Repeat("0", 54) + suffix // doomed tenant's encrypted chunk

	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		_, execErr := db.ExecContext(ctx, q, args...)
		require.NoError(t, execErr, "fixture: %s", q)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM tenant_chunk_refs WHERE tenant_id::text IN ($1, $2)`,
		} {
			if _, err := db.ExecContext(ctx, q, doomedTenant, otherTenant); err != nil {
				t.Logf("cleanup (residue may break other DB tests): %s: %v", q, err)
			}
		}
		for _, h := range []string{sharedHash, scopedHash} {
			if _, err := db.ExecContext(ctx, `DELETE FROM global_content_index WHERE plaintext_hash = $1`, h); err != nil {
				t.Logf("cleanup GCI %s: %v", h, err)
			}
		}
		for _, q := range []string{
			`DELETE FROM tenant_quotas WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM tenants WHERE id IN ($1, $2)`,
		} {
			if _, err := db.ExecContext(ctx, q, doomedTenant, otherTenant); err != nil {
				t.Logf("cleanup: %s: %v", q, err)
			}
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id::text = $1`, userID); err != nil {
			t.Logf("cleanup users: %v", err)
		}
	})

	// company must be non-NULL: auth.LoadFromDB scans it without COALESCE and
	// package auth's tests can run concurrently against this shared database.
	mustExec(`INSERT INTO users (id, email, password_hash, company, created_at, updated_at)
	          VALUES ($1, $2, 'x', 'WP6 GCI Co', NOW(), NOW())`,
		userID, fmt.Sprintf("wp6-gci-%s@example.com", suffix))
	mustExec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
	          VALUES ($1, 'WP6 Doomed', $2, $3, $4)`,
		doomedTenant, fmt.Sprintf("wp6-doomed-%s@example.com", suffix), "VKWP6D"+suffix, "SK")
	mustExec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
	          VALUES ($1, 'WP6 Other', $2, $3, $4)`,
		otherTenant, fmt.Sprintf("wp6-other-%s@example.com", suffix), "VKWP6O"+suffix, "SK")

	// Shared unencrypted chunk: one ref from each tenant, ref_count 2.
	mustExec(`INSERT INTO global_content_index
	          (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count)
	          VALUES ('_global', $1, 'local', $2, 1024, 2)`,
		sharedHash, "_chunks/"+sharedHash)
	// Tenant-scoped (encrypted) chunk: only the doomed tenant references it.
	mustExec(`INSERT INTO global_content_index
	          (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count, encrypted)
	          VALUES ($1, $2, 'local', $3, 1024, 1, TRUE)`,
		doomedTenant, scopedHash, "_chunks/"+doomedTenant+"/"+scopedHash)

	mustExec(`INSERT INTO tenant_chunk_refs
	          (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash, dedup_scope)
	          VALUES ($1, 'wp6-bucket', 'shared.bin', 0, 0, $2, '_global')`, doomedTenant, sharedHash)
	mustExec(`INSERT INTO tenant_chunk_refs
	          (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash, dedup_scope)
	          VALUES ($1, 'wp6-bucket', 'shared.bin', 0, 0, $2, '_global')`, otherTenant, sharedHash)
	mustExec(`INSERT INTO tenant_chunk_refs
	          (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash, dedup_scope)
	          VALUES ($1, 'wp6-bucket', 'secret.bin', 0, 0, $2, $3)`, doomedTenant, scopedHash, doomedTenant)

	svc := NewAccountDeletionService(db, zap.NewNop())
	require.NoError(t, svc.ExecuteDeletion(ctx, userID, doomedTenant))

	// Shared chunk: one reference released, still alive for the other tenant.
	var sharedCount int
	var sharedMarked bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT ref_count, marked_for_deletion FROM global_content_index
		WHERE dedup_scope = '_global' AND plaintext_hash = $1`, sharedHash).
		Scan(&sharedCount, &sharedMarked))
	assert.Equal(t, 1, sharedCount, "shared chunk must lose exactly the doomed tenant's reference")
	assert.False(t, sharedMarked, "shared chunk still referenced — must not be marked")

	// Tenant-scoped chunk: zero refs, marked so GC reclaims it after grace.
	var scopedCount int
	var scopedMarked bool
	var markedAt sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT ref_count, marked_for_deletion, marked_at FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2`, doomedTenant, scopedHash).
		Scan(&scopedCount, &scopedMarked, &markedAt))
	assert.Zero(t, scopedCount, "deleted tenant's scoped chunk must drop to zero refs (F5)")
	assert.True(t, scopedMarked, "zero-ref chunk must be marked for GC (F5)")
	assert.True(t, markedAt.Valid, "marked_at must be set so grace-period sweep can fire")

	// The doomed tenant's refs are gone; the other tenant's ref survives.
	var doomedRefs, otherRefs int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1`, doomedTenant).Scan(&doomedRefs))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1`, otherTenant).Scan(&otherRefs))
	assert.Zero(t, doomedRefs)
	assert.Equal(t, 1, otherRefs)
}
