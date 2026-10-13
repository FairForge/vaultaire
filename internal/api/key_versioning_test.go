package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-R8-1: stored-random tenant keys with real versions.

const (
	keyTestMasterA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	keyTestMasterB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// testTenantKeys is the key store a test's encryption runs on.
func testTenantKeys(t *testing.T, db *sql.DB, masterHex string) *crypto.TenantKeys {
	t.Helper()
	k, err := crypto.NewTenantKeys(db, masterHex, "")
	require.NoError(t, err)
	return k
}

// testSSEService is crypto.NewSSEService on a key store of its own.
func testSSEService(db *sql.DB, masterHex string) (*crypto.SSEService, error) {
	k, err := crypto.NewTenantKeys(db, masterHex, "")
	if err != nil {
		return nil, err
	}
	return crypto.NewSSEService(k), nil
}

// setupKeyedChunkingFixture is the encrypted chunk fixture with its key store.
func setupKeyedChunkingFixture(t *testing.T) (*adapterTestFixture, *crypto.TenantKeys) {
	t.Helper()
	f := setupChunkingFixture(t)
	keys := testTenantKeys(t, f.db, keyTestMasterA)
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(keys)
	f.adapter.sseService = crypto.NewSSEService(keys)
	return f, keys
}

type chunkVersions struct {
	hash     string
	row, ref sql.NullInt32
}

// objectChunkVersions: per chunk of the object, the row's and the ref's version.
func objectChunkVersions(t *testing.T, f *adapterTestFixture, key string) []chunkVersions {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT r.plaintext_hash, g.encryption_key_version, r.encryption_key_version
		  FROM tenant_chunk_refs r
		  JOIN global_content_index g ON g.dedup_scope = r.dedup_scope AND g.plaintext_hash = r.plaintext_hash
		 WHERE r.tenant_id = $1 AND r.bucket_name = 'test-bucket' AND r.object_key = $2
		 ORDER BY r.chunk_index`, f.tenantID, key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []chunkVersions
	for rows.Next() {
		var c chunkVersions
		require.NoError(t, rows.Scan(&c.hash, &c.row, &c.ref))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out)
	return out
}

func requireGet(t *testing.T, f *adapterTestFixture, key string, want []byte) {
	t.Helper()
	w := getChunkedAs(t, f, f.tenant, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, "GET %s: %s", key, truncateForLog(w.Body.String()))
	require.True(t, bytes.Equal(want, w.Body.Bytes()), "GET %s: %d bytes, want %d exact", key, w.Body.Len(), len(want))
}

func truncateForLog(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func TestChunkedPut_RecordsKeyVersionOnRowAndRef_DedupHitCopiesIt(t *testing.T) {
	f, keys := setupKeyedChunkingFixture(t)
	ctx := context.Background()
	content := generateTestData(8 << 10)

	putChunkedObject(t, f, "a.bin", content, "application/octet-stream")
	for _, c := range objectChunkVersions(t, f, "a.bin") {
		assert.Equal(t, sql.NullInt32{Int32: 1, Valid: true}, c.row, "the row records the version the blob is sealed under")
		assert.Equal(t, int32(1), c.ref.Int32)
	}

	v, err := keys.Rotate(ctx, f.tenantID)
	require.NoError(t, err)
	require.Equal(t, 2, v)

	// The same bytes again: a dedup hit on the v1 blob. The ref copies the
	// ROW's version, exactly as it copies its ciphertext hash.
	putChunkedObject(t, f, "b.bin", content, "application/octet-stream")
	for _, c := range objectChunkVersions(t, f, "b.bin") {
		assert.Equal(t, int32(1), c.row.Int32, "the second PUT does not touch the v1 row")
		assert.Equal(t, int32(1), c.ref.Int32, "the dedup hit records the blob's version, not the tenant's current one")
	}

	// New bytes: sealed under v2, recorded as v2.
	putChunkedObject(t, f, "c.bin", generateTestData(8<<10), "application/octet-stream")
	for _, c := range objectChunkVersions(t, f, "c.bin") {
		assert.Equal(t, int32(2), c.row.Int32)
		assert.Equal(t, int32(2), c.ref.Int32)
	}
}

func TestChunkedGet_DecryptsWithTheRecordedVersion_AfterRotate(t *testing.T) {
	f, keys := setupKeyedChunkingFixture(t)
	x := generateTestData(20 << 20)
	z := generateTestData(8 << 10)

	putChunkedObject(t, f, "x.bin", x, "application/octet-stream")
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	putChunkedObject(t, f, "y.bin", x, "application/octet-stream") // dedup hits on the v1 rows
	putChunkedObject(t, f, "z.bin", z, "application/octet-stream") // v2

	requireGet(t, f, "x.bin", x)
	requireGet(t, f, "y.bin", x)
	requireGet(t, f, "z.bin", z)
	for _, c := range objectChunkVersions(t, f, "y.bin") {
		assert.Equal(t, int32(1), c.row.Int32, "row v1 untouched by the second PUT")
	}

	// A fresh process (no cached key) reads them the same way.
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(testTenantKeys(t, f.db, keyTestMasterA))
	f.adapter.gci = crypto.NewGlobalContentIndex(f.db)
	requireGet(t, f, "x.bin", x)
	requireGet(t, f, "z.bin", z)
}

func TestChunkedGet_RowKeyVersionWinsOverRef(t *testing.T) {
	f, keys := setupKeyedChunkingFixture(t)
	content := generateTestData(8 << 10)
	putChunkedObject(t, f, "w.bin", content, "application/octet-stream")
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	c := objectChunkVersions(t, f, "w.bin")[0]

	// The ref says something else: the row (the blob's own record) wins.
	_, err = f.db.Exec(`UPDATE tenant_chunk_refs SET encryption_key_version = 2 WHERE tenant_id = $1 AND object_key = 'w.bin'`, f.tenantID)
	require.NoError(t, err)
	requireGet(t, f, "w.bin", content)

	// A row sealed before 084 carries none: the ref stands in.
	_, err = f.db.Exec(`UPDATE tenant_chunk_refs SET encryption_key_version = 1 WHERE tenant_id = $1 AND object_key = 'w.bin'`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE global_content_index SET encryption_key_version = NULL WHERE dedup_scope = $1 AND plaintext_hash = $2`, f.tenantID, c.hash)
	require.NoError(t, err)
	f.adapter.gci.InvalidateCache(f.tenantID, c.hash)
	requireGet(t, f, "w.bin", content)

	// And with the ref wrong too, the read fails closed (500), never garbage.
	_, err = f.db.Exec(`UPDATE tenant_chunk_refs SET encryption_key_version = 2 WHERE tenant_id = $1 AND object_key = 'w.bin'`, f.tenantID)
	require.NoError(t, err)
	w := getChunkedAs(t, f, f.tenant, "test-bucket", "w.bin")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.False(t, bytes.Contains(w.Body.Bytes(), content[:64]))
}

func putSSE(t *testing.T, f *adapterTestFixture, key string, body []byte) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func headKeyVersion(t *testing.T, f *adapterTestFixture, key string) sql.NullInt32 {
	t.Helper()
	var v sql.NullInt32
	require.NoError(t, f.db.QueryRow(`SELECT encryption_key_version FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&v))
	return v
}

func TestSSES3_GetAfterRotate_UsesRecordedVersion(t *testing.T) {
	f := setupAdapterFixture(t)
	keys := testTenantKeys(t, f.db, keyTestMasterA)
	f.adapter.sseService = crypto.NewSSEService(keys)
	one, two := []byte("whole object sealed under v1"), []byte("whole object sealed under v2")

	putSSE(t, f, "one.txt", one)
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	putSSE(t, f, "two.txt", two)

	assert.Equal(t, sql.NullInt32{Int32: 1, Valid: true}, headKeyVersion(t, f, "one.txt"))
	assert.Equal(t, sql.NullInt32{Int32: 2, Valid: true}, headKeyVersion(t, f, "two.txt"))
	for key, want := range map[string][]byte{"one.txt": one, "two.txt": two} {
		w := getChunkedAs(t, f, f.tenant, "test-bucket", key)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, want, w.Body.Bytes(), "%s opens under the version its blob records", key)
	}

	// The backlog gauge counts the v1 object.
	var chunk, whole int64
	require.NoError(t, f.db.QueryRow(keyRotationPendingSQL, crypto.SSEAlgorithm, "").Scan(&chunk, &whole))
	assert.GreaterOrEqual(t, whole, int64(1))
	assert.Equal(t, 2, testutil.CollectAndCount(newKeyRotationPendingCollector(f.db, zap.NewNop())))
}

// newKeyReencryptForTest is the runner on the fixture, scoped to its tenant.
func newKeyReencryptForTest(t *testing.T, f *adapterTestFixture) *KeyReencryptRunner {
	t.Helper()
	k := NewKeyReencryptRunner(f.db, f.eng, f.adapter.gci, f.adapter.chunkEncSvc, zap.NewNop())
	require.NotNil(t, k)
	k.JobName = "test_key_reencrypt_" + f.tenantID[:8]
	k.onlyScope = f.tenantID
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, k.JobName) })
	return k
}

func retiredFor(t *testing.T, f *adapterTestFixture) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT storage_key FROM retired_chunk_blobs WHERE dedup_scope = $1 ORDER BY storage_key`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		out = append(out, k)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestKeyReencrypt_MovesChunksToCurrent_OldBlobRetiredThenDeleted(t *testing.T) {
	f, keys := setupKeyedChunkingFixture(t)
	content := generateTestData(20 << 20)
	putChunkedObject(t, f, "big.bin", content, "application/octet-stream")
	before := objectChunkVersions(t, f, "big.bin")
	require.Greater(t, len(before), 2)
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)

	k := newKeyReencryptForTest(t, f)
	clock := time.Now()
	k.now = func() time.Time { return clock }

	// The budget is respected: one byte = one chunk, then the run stops.
	k.MaxBytesPerRun = 1
	res, err := k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Reencrypted)
	assert.True(t, res.BudgetReached)
	requireGet(t, f, "big.bin", content)

	// The rest, through the scheduler (a job_runs row), with readers running.
	k.MaxBytesPerRun = 50 << 30
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, bad int
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				w := getChunkedAs(t, f, f.tenant, "test-bucket", "big.bin")
				mu.Lock()
				reads++
				if w.Code != http.StatusOK || !bytes.Equal(content, w.Body.Bytes()) {
					bad++
				}
				mu.Unlock()
			}
		}()
	}
	sj := newJobScheduler(f.db, zap.NewNop()).Register(k.spec())
	rep, err := sj.RunNow(context.Background())
	close(stop)
	wg.Wait()
	require.NoError(t, err)
	t.Logf("swap run: %+v; %d concurrent reads, %d bad", rep.Result, reads, bad)
	assert.Zero(t, bad, "a read during the swap returns the object, exact")
	assert.Greater(t, reads, 0)

	var outcome string
	var rowsAffected int64
	require.NoError(t, f.db.QueryRow(`SELECT last_outcome, rows_affected FROM job_runs WHERE job = $1`, k.JobName).Scan(&outcome, &rowsAffected))
	assert.Equal(t, "ok", outcome)
	assert.Equal(t, int64(len(before)-1), rowsAffected)

	after := objectChunkVersions(t, f, "big.bin")
	for _, c := range after {
		assert.Equal(t, int32(2), c.row.Int32, "every chunk is on the current version")
	}
	retired := retiredFor(t, f)
	assert.Len(t, retired, len(before))
	for _, key := range retired {
		_, statErr := os.Stat(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, key))
		assert.NoError(t, statErr, "the old blob stays for its grace: %s", key)
	}
	var newKeys int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND storage_key LIKE '%.k2'`, f.tenantID).Scan(&newKeys))
	assert.Equal(t, len(before), newKeys, "new blobs at _chunks/<tenant>/<hash>.k2")
	requireGet(t, f, "big.bin", content)

	// Inside the grace a run deletes nothing.
	res, err = k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.RetiredDeleted)
	assert.Zero(t, res.Candidates)

	// Past it, the next run deletes the old blobs and their rows.
	clock = clock.Add(keyReencryptRetireAfter + time.Minute)
	res, err = k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, len(before), res.RetiredDeleted)
	assert.Empty(t, retiredFor(t, f))
	for _, key := range retired {
		_, statErr := os.Stat(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, key))
		assert.True(t, os.IsNotExist(statErr), "old blob deleted: %s", key)
	}
	requireGet(t, f, "big.bin", content)

	// A fresh process (no cache) reads it from the new blobs.
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(testTenantKeys(t, f.db, keyTestMasterA))
	f.adapter.gci = crypto.NewGlobalContentIndex(f.db)
	requireGet(t, f, "big.bin", content)
}

// A retired address an index row names again (GC swept the row, a PUT
// re-stored the chunk at its plain key) is never deleted.
func TestKeyReencrypt_RetiredAddressLiveAgain_IsKept(t *testing.T) {
	f, keys := setupKeyedChunkingFixture(t)
	content := generateTestData(8 << 10)
	putChunkedObject(t, f, "r.bin", content, "application/octet-stream")
	c := objectChunkVersions(t, f, "r.bin")[0]
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	k := newKeyReencryptForTest(t, f)
	clock := time.Now()
	k.now = func() time.Time { return clock }
	_, err = k.RunOnce(context.Background())
	require.NoError(t, err)
	old := retiredFor(t, f)
	require.Len(t, old, 1)

	// The row points back at the old address (what a re-store would do).
	_, err = f.db.Exec(`UPDATE global_content_index SET storage_key = $3 WHERE dedup_scope = $1 AND plaintext_hash = $2`, f.tenantID, c.hash, old[0])
	require.NoError(t, err)
	clock = clock.Add(keyReencryptRetireAfter + time.Minute)
	res, err := k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.RetiredKept)
	assert.Zero(t, res.RetiredDeleted)
	_, statErr := os.Stat(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, old[0]))
	assert.NoError(t, statErr, "a live address is never deleted")
}

// The admin endpoint, behind the same middleware the router mounts.
func TestKeyRotateEndpoint_AuditAndIdempotent(t *testing.T) {
	f := setupChunkingFixture(t)
	db := f.db
	adminID, userID := uuid.New().String(), uuid.New().String()
	for id, role := range map[string]string{adminID: "admin", userID: "user"} {
		_, err := db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', $3)`,
			id, fmt.Sprintf("keyrot-%s-%d@test.local", role, time.Now().UnixNano()), role)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM idempotency_cache WHERE tenant_id = $1`, "admin-tenant-"+adminID[:8])
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, adminID, userID)
	})

	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	mount := func() {
		s.router = chi.NewRouter()
		s.router.Route("/api/v1/admin", func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler { // stand-in for requireJWT
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					uid := req.Header.Get("X-Test-User")
					ctx := context.WithValue(req.Context(), userIDKey, uid)
					ctx = context.WithValue(ctx, emailKey, testAdminEmail)
					ctx = context.WithValue(ctx, tenantIDKey, "admin-tenant-"+uid[:8])
					next.ServeHTTP(w, req.WithContext(ctx))
				})
			})
			_, im := s.jsonAPIMiddleware()
			r.With(im.Middleware).Post("/tenants/{tenant_id}/encryption-key/rotate", s.requireAdmin(s.handleRotateTenantKey))
		})
	}
	rotate := func(user, tenantID, idemKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID+"/encryption-key/rotate", nil)
		req.Header.Set("X-Test-User", user)
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		return w
	}

	// No master key: 503 encryption_disabled.
	mount()
	w := rotate(adminID, f.tenantID, "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "encryption_disabled")

	s.tenantKeys = testTenantKeys(t, db, keyTestMasterA)
	mount()
	assert.Equal(t, http.StatusForbidden, rotate(userID, f.tenantID, "").Code, "admin only")
	assert.Equal(t, http.StatusNotFound, rotate(adminID, uuid.NewString(), "").Code, "unknown tenant")

	idem := "rotate-" + uuid.NewString()
	first := rotate(adminID, f.tenantID, idem)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var body struct {
		TenantID   string `json:"tenant_id"`
		KeyVersion int    `json:"key_version"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &body))
	assert.Equal(t, f.tenantID, body.TenantID)
	assert.Equal(t, 1, body.KeyVersion, "a tenant with no key gets its first")

	retry := rotate(adminID, f.tenantID, idem)
	require.Equal(t, http.StatusOK, retry.Code)
	assert.JSONEq(t, first.Body.String(), retry.Body.String(), "a retried request replays the answer and rotates nothing")
	cur, err := s.tenantKeys.CurrentVersion(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, 1, cur)

	next := rotate(adminID, f.tenantID, "rotate-"+uuid.NewString())
	require.Equal(t, http.StatusOK, next.Code)
	require.NoError(t, json.Unmarshal(next.Body.Bytes(), &body))
	assert.Equal(t, 2, body.KeyVersion)

	var n int
	var meta string
	require.NoError(t, db.QueryRow(`SELECT COUNT(*), MAX(metadata::text) FROM audit_logs WHERE tenant_id = $1 AND action = 'admin.key_rotate'`, f.tenantID).Scan(&n, &meta))
	assert.Equal(t, 2, n, "one audit row per rotation, none for the replay")
	assert.Contains(t, meta, `"key_version"`)
}

func TestMasterKeyRewrap_PreviousKeyAtBoot(t *testing.T) {
	f, _ := setupKeyedChunkingFixture(t)
	content := generateTestData(8 << 10)
	putChunkedObject(t, f, "m.bin", content, "application/octet-stream")
	putSSE(t, f, "m.txt", []byte("whole object under master A"))

	// Restart with the new master and the old one as PREVIOUS.
	core, logs := observer.New(zap.InfoLevel)
	keys, err := crypto.NewTenantKeys(f.db, keyTestMasterB, keyTestMasterA)
	require.NoError(t, err)
	rewrappedBefore := testutil.ToFloat64(tenantKeysRewrapped)
	checkTenantKeys(context.Background(), keys, zap.New(core), f.tenantID) // scoped: the table is shared
	assert.GreaterOrEqual(t, testutil.ToFloat64(tenantKeysRewrapped)-rewrappedBefore, float64(1))
	require.Len(t, logs.FilterMessageSnippet("re-wrapped").All(), 1, "one Info line with the count")

	// PREVIOUS removed: reads work under the new master alone.
	only := testTenantKeys(t, f.db, keyTestMasterB)
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(only)
	f.adapter.sseService = crypto.NewSSEService(only)
	requireGet(t, f, "m.bin", content)
	w := getChunkedAs(t, f, f.tenant, "test-bucket", "m.txt")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "whole object under master A", w.Body.String())
}

// WP-R8-1 follow-up A3: while PREVIOUS is set, every key_reencrypt run
// re-wraps the rows another slot created under the old master after this
// slot's boot check, and its result says when PREVIOUS can go.
func TestKeyReencrypt_WithPrevious_RewrapsRowsTheOldSlotCreated(t *testing.T) {
	f, _ := setupKeyedChunkingFixture(t) // the old slot: master A
	putSSE(t, f, "late.txt", []byte("first encrypted write, on the old slot"))

	// The new slot (B + previous A) booted before that row existed.
	newKeys, err := crypto.NewTenantKeys(f.db, keyTestMasterB, keyTestMasterA)
	require.NoError(t, err)
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(newKeys)
	k := newKeyReencryptForTest(t, f)
	rewrappedBefore := testutil.ToFloat64(tenantKeysRewrapped)
	newKeys.OnRewrap(tenantKeysRewrapped.Inc)

	res, err := k.RunOnce(context.Background())
	require.NoError(t, err)
	t.Logf("run with PREVIOUS set: %+v", res)
	assert.True(t, res.MasterRekey)
	assert.Equal(t, 1, res.Rewrapped, "the old slot's row is re-wrapped by the job")
	assert.Zero(t, res.UnwrapFailures)
	assert.Equal(t, rewrappedBefore+1, testutil.ToFloat64(tenantKeysRewrapped))

	res, err = k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Rewrapped, "rewrapped=0 unwrap_failures=0: PREVIOUS may go once the old slot is stopped")
	assert.Zero(t, res.UnwrapFailures)

	// PREVIOUS removed: B alone serves the object.
	f.adapter.sseService = crypto.NewSSEService(testTenantKeys(t, f.db, keyTestMasterB))
	w := getChunkedAs(t, f, f.tenant, "test-bucket", "late.txt")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "first encrypted write, on the old slot", w.Body.String())

	// Without PREVIOUS the pass does not run.
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(testTenantKeys(t, f.db, keyTestMasterB))
	k2 := newKeyReencryptForTest(t, f)
	res, err = k2.RunOnce(context.Background())
	require.NoError(t, err)
	assert.False(t, res.MasterRekey)
}

func TestMasterKeyRewrap_UnwrapsUnderNeither_ReadsFailClosed(t *testing.T) {
	f, _ := setupKeyedChunkingFixture(t)
	content := generateTestData(8 << 10)
	putChunkedObject(t, f, "n.bin", content, "application/octet-stream")
	putSSE(t, f, "n.txt", []byte("whole object"))

	// A wrong master key (B, with an unrelated PREVIOUS): nothing opens.
	third := strings.Repeat("11", 32)
	core, logs := observer.New(zap.InfoLevel)
	keys, err := crypto.NewTenantKeys(f.db, keyTestMasterB, third)
	require.NoError(t, err)
	checkTenantKeys(context.Background(), keys, zap.New(core), f.tenantID)
	assert.GreaterOrEqual(t, testutil.ToFloat64(tenantKeysUnwrapFailures), float64(1))
	errLines := logs.FilterLevelExact(zap.ErrorLevel).All()
	require.Len(t, errLines, 1)
	assert.Contains(t, fmt.Sprint(errLines[0].ContextMap()["first"]), f.tenantID)

	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(keys)
	f.adapter.sseService = crypto.NewSSEService(keys)
	for _, key := range []string{"n.bin", "n.txt"} {
		w := getChunkedAs(t, f, f.tenant, "test-bucket", key)
		assert.Equal(t, http.StatusInternalServerError, w.Code, "%s: 500, never ciphertext", key)
		assert.True(t, strings.Contains(w.Body.String(), "InternalError"))
	}
	tenantKeysUnwrapFailures.Set(0)
}

func TestErasure_ShredsKeys_LeftoverBlobUndecryptable(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	keys := testTenantKeys(t, f.db, keyTestMasterA)
	enc := crypto.NewChunkEncryptionService(keys)

	// A blob sealed under the tenant's key, copied aside (a backend's own
	// backup, a copy the sweep did not reach).
	data := []byte("a chunk that outlives its account somewhere")
	hash := strings.Repeat("ab", 32)
	ct, ctHash, v, err := enc.EncryptChunkData(context.Background(), f.tenantID, hash, data)
	require.NoError(t, err)
	got, err := enc.DecryptChunkData(context.Background(), f.tenantID, v, hash, ct, ctHash)
	require.NoError(t, err)
	require.Equal(t, data, got)

	f.runner.Keys = keys
	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err, "run: %+v", res)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	require.Equal(t, outcomeErased, te.Outcome, te.Error)
	assert.Equal(t, int64(1), te.KeysShredded)

	var rows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_key_versions WHERE tenant_id = $1`, f.tenantID).Scan(&rows))
	assert.Zero(t, rows, "the key rows are gone")

	_, err = enc.DecryptChunkData(context.Background(), f.tenantID, v, hash, ct, ctHash)
	assert.ErrorIs(t, err, crypto.ErrNoTenantKey, "the leftover blob is unreadable in this process")
	_, err = crypto.NewChunkEncryptionService(testTenantKeys(t, f.db, keyTestMasterA)).
		DecryptChunkData(context.Background(), f.tenantID, v, hash, ct, ctHash)
	assert.ErrorIs(t, err, crypto.ErrNoTenantKey, "and in any other, master key or not")

	var meta string
	require.NoError(t, f.db.QueryRow(`SELECT metadata::text FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID).Scan(&meta))
	assert.Contains(t, meta, `"keys_shredded": 1`)
}
