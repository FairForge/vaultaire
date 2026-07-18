// s3_headcache_failure_test.go — WP-3 (CR-3): a PUT whose head-cache write
// fails must return 5xx, not 200. HEAD serves exclusively from
// object_head_cache, so a 200 with no cache row is a lie: the client believes
// the object is stored, then every HEAD/GET 404s and the bytes are never
// billed. The blob itself is durable, so the client's retry is safe and
// idempotent (upsert).
//
// Failure is injected with a per-tenant trigger on object_head_cache — the
// rest of the request path (tenants, quotas, buckets config) keeps working,
// which is exactly the partial-failure mode seen in production DB incidents.
package api

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installHeadCacheFailure makes every INSERT/UPDATE on object_head_cache for
// the given tenant raise, simulating a head-cache write outage scoped to the
// test tenant (other packages sharing the DB are unaffected).
func installHeadCacheFailure(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	fn := "wp3_fail_head_cache_" + strings.ReplaceAll(tenantID[:8], "-", "_")
	_, err := db.Exec(fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'injected head-cache failure (WP-3 test)';
		END;
		$$ LANGUAGE plpgsql`, fn))
	require.NoError(t, err)
	_, err = db.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON object_head_cache
		FOR EACH ROW WHEN (NEW.tenant_id = '%s')
		EXECUTE FUNCTION %s()`, fn, tenantID, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON object_head_cache`, fn))
		_, _ = db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

func dropHeadCacheFailure(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	fn := "wp3_fail_head_cache_" + strings.ReplaceAll(tenantID[:8], "-", "_")
	_, err := db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON object_head_cache`, fn))
	require.NoError(t, err)
}

func TestPut_HeadCacheWriteFailure_Returns5xx(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	installHeadCacheFailure(t, f.db, f.tenantID)

	status := f.put(t, "doomed.bin", testBytes(1<<20))

	assert.GreaterOrEqual(t, status, 500,
		"PUT must fail loudly when the head-cache write fails — a 200 here means every subsequent HEAD/GET 404s")

	// The reservation must be released: unrecorded bytes are unbilled bytes.
	assert.Equal(t, int64(0), f.used(t),
		"quota reservation must be released when the PUT fails")

	// And no phantom cache row.
	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'doomed.bin'`,
		f.tenantID).Scan(&n))
	assert.Zero(t, n)
}

func TestPut_HeadCacheRecovers_RetrySucceeds(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	installHeadCacheFailure(t, f.db, f.tenantID)

	require.GreaterOrEqual(t, f.put(t, "retry.bin", testBytes(1<<20)), 500)

	// "Outage" ends; the client's retry must succeed and bill exactly once.
	dropHeadCacheFailure(t, f.db, f.tenantID)

	assert.Equal(t, 200, f.put(t, "retry.bin", testBytes(1<<20)))
	assert.Equal(t, int64(1<<20), f.used(t), "retry after recovery must bill exactly once")
}

func TestChunkedPut_HeadCacheWriteFailure_Returns5xx(t *testing.T) {
	f := setupChunkingFixture(t)
	installHeadCacheFailure(t, f.db, f.tenantID)

	content := testBytes(64 << 10) // well above the fixture's 1 KiB threshold
	req := httptest.NewRequest("PUT", "/test-bucket/doomed-chunked.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "doomed-chunked.bin")

	assert.GreaterOrEqual(t, w.Code, 500,
		"chunked PUT must fail loudly when the head-cache write fails")

	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'doomed-chunked.bin'`,
		f.tenantID).Scan(&n))
	assert.Zero(t, n)
}
