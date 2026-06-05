package main

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G401
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type testFixture struct {
	db       *sql.DB
	eng      *engine.CoreEngine
	gci      *crypto.GlobalContentIndex
	d        *deps
	tenantID string
	tenant   *tenant.Tenant
	tempDir  string
}

func setupFixture(t *testing.T) *testFixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()

	tempDir, err := os.MkdirTemp("", "dedup-migrate-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, &engine.Config{
		EnableCaching:  false,
		EnableML:       false,
		DefaultBackend: "local",
	})
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	gci := crypto.NewGlobalContentIndex(db)

	tenantUUID := uuid.New()
	tenantIDStr := tenantUUID.String()

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantIDStr, "Migrate Test", "migrate@test.local", "AK-"+tenantIDStr[:8], "SK-"+tenantIDStr[:8])
	require.NoError(t, err)

	tn := &tenant.Tenant{ID: tenantIDStr}
	container := tn.NamespaceContainer("test-bucket")
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))
	// _global container for chunks
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "_global", "_chunks"), 0755))

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tenantUUID)
		_, _ = db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tenantUUID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantIDStr)
		_, _ = db.Exec("DELETE FROM global_content_index WHERE plaintext_hash LIKE 'test-%' OR storage_key LIKE '_chunks/%'")
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantIDStr)
	})

	d := &deps{eng: eng, gci: gci, db: db, log: logger}

	return &testFixture{
		db:       db,
		eng:      eng,
		gci:      gci,
		d:        d,
		tenantID: tenantIDStr,
		tenant:   tn,
		tempDir:  tempDir,
	}
}

func seedObject(t *testing.T, f *testFixture, key string, content []byte) string {
	t.Helper()
	ctx := context.Background()

	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(ctx, container, key, bytes.NewReader(content), engine.WithContentLength(int64(len(content))))
	require.NoError(t, err)

	h := md5.New() // #nosec G401
	_, _ = h.Write(content)
	etag := fmt.Sprintf("%x", h.Sum(nil))

	_, err = f.db.ExecContext(ctx, `
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, is_chunked, encryption_algorithm)
		VALUES ($1, $2, $3, $4, $5, 'application/octet-stream', 'local', FALSE, '')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = EXCLUDED.size_bytes,
			etag = EXCLUDED.etag,
			is_chunked = FALSE,
			encryption_algorithm = ''
	`, f.tenantID, "test-bucket", key, len(content), etag)
	require.NoError(t, err)

	return etag
}

func TestMigrate_DryRunNoWrites(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	content := make([]byte, 128*1024)
	_, _ = rand.Read(content)
	seedObject(t, f, "dry-run.bin", content)

	c := candidate{
		tenantID: f.tenantID,
		bucket:   "test-bucket",
		key:      "dry-run.bin",
		size:     int64(len(content)),
		etag:     computeEtag(content),
	}

	res, err := migrateObject(ctx, f.d, c, true, false)
	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.False(t, res.Failed)
	assert.Equal(t, int64(len(content)), res.SizeBytes)
	assert.Greater(t, res.ChunkCount, 0)

	// Verify: object still not chunked
	var isChunked bool
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "dry-run.bin").Scan(&isChunked))
	assert.False(t, isChunked, "dry-run must not flip is_chunked")

	// No chunks written to _global
	files, _ := filepath.Glob(filepath.Join(f.tempDir, "_global", "_chunks", "*"))
	assert.Empty(t, files, "dry-run must not write chunks")
}

func TestMigrate_ConvertsObject(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	content := make([]byte, 128*1024)
	_, _ = rand.Read(content)
	etag := seedObject(t, f, "convert.bin", content)

	c := candidate{
		tenantID: f.tenantID,
		bucket:   "test-bucket",
		key:      "convert.bin",
		size:     int64(len(content)),
		etag:     etag,
	}

	res, err := migrateObject(ctx, f.d, c, false, false)
	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.False(t, res.Failed)
	assert.Equal(t, int64(len(content)), res.SizeBytes)
	assert.Greater(t, res.ChunkCount, 0)

	// is_chunked should be TRUE
	var isChunked bool
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "convert.bin").Scan(&isChunked))
	assert.True(t, isChunked)

	// Chunks exist in _global
	files, _ := filepath.Glob(filepath.Join(f.tempDir, "_global", "_chunks", "*"))
	assert.NotEmpty(t, files, "chunks must be stored in _global")

	// Original deleted
	container := f.tenant.NamespaceContainer("test-bucket")
	_, getErr := f.eng.Get(ctx, container, "convert.bin")
	assert.Error(t, getErr, "original monolithic object should be deleted")

	// Manifest exists with correct chunk count
	var chunkCount int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "convert.bin").Scan(&chunkCount))
	assert.Equal(t, res.ChunkCount, chunkCount)

	// Reassemble via chunks and verify ETag
	reassembled := reassembleFromChunks(t, f, "convert.bin")
	assert.Equal(t, content, reassembled, "reassembled content must be byte-identical")
}

func TestMigrate_VerifiesETagBeforeDelete(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	content := make([]byte, 128*1024)
	_, _ = rand.Read(content)
	seedObject(t, f, "bad-etag.bin", content)

	// Deliberately corrupt the stored etag
	_, err := f.db.ExecContext(ctx, `
		UPDATE object_head_cache SET etag = 'deadbeef00000000000000000000dead'
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "bad-etag.bin")
	require.NoError(t, err)

	c := candidate{
		tenantID: f.tenantID,
		bucket:   "test-bucket",
		key:      "bad-etag.bin",
		size:     int64(len(content)),
		etag:     "deadbeef00000000000000000000dead",
	}

	res, err := migrateObject(ctx, f.d, c, false, false)
	require.NoError(t, err)
	assert.True(t, res.Failed, "should fail on etag mismatch")
	assert.Contains(t, res.FailReason, "etag mismatch")

	// Original must still be present
	var isChunked bool
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "bad-etag.bin").Scan(&isChunked))
	assert.False(t, isChunked, "is_chunked must remain FALSE")

	container := f.tenant.NamespaceContainer("test-bucket")
	reader, getErr := f.eng.Get(ctx, container, "bad-etag.bin")
	require.NoError(t, getErr, "original must not be deleted on etag mismatch")
	_ = reader.Close()
}

func TestMigrate_KeepOriginal(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	content := make([]byte, 128*1024)
	_, _ = rand.Read(content)
	etag := seedObject(t, f, "keep.bin", content)

	c := candidate{
		tenantID: f.tenantID,
		bucket:   "test-bucket",
		key:      "keep.bin",
		size:     int64(len(content)),
		etag:     etag,
	}

	res, err := migrateObject(ctx, f.d, c, false, true)
	require.NoError(t, err)
	assert.False(t, res.Failed)

	// is_chunked should be TRUE
	var isChunked bool
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "keep.bin").Scan(&isChunked))
	assert.True(t, isChunked)

	// Original still present
	container := f.tenant.NamespaceContainer("test-bucket")
	reader, getErr := f.eng.Get(ctx, container, "keep.bin")
	require.NoError(t, getErr, "original should be kept with --keep-original")
	_ = reader.Close()
}

func TestMigrate_SkipsEncryptedAndBelowMinAndNonUUID(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	// Non-UUID tenant → skipped
	nonUUID := candidate{
		tenantID: "not-a-uuid",
		bucket:   "test-bucket",
		key:      "nope.bin",
		size:     128 * 1024,
		etag:     "abc",
	}
	res, err := migrateObject(ctx, f.d, nonUUID, false, false)
	require.NoError(t, err)
	assert.True(t, res.Skipped, "non-UUID tenant should be skipped")

	// Encrypted objects are excluded by the SQL query, not by migrateObject.
	// Below-min-size is also excluded by SQL. Verify SQL filtering:
	content := make([]byte, 1024)
	_, _ = rand.Read(content)
	seedObject(t, f, "small.bin", content)

	// Mark it encrypted
	_, err = f.db.ExecContext(ctx, `
		UPDATE object_head_cache SET encryption_algorithm = 'AES256'
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, "test-bucket", "small.bin")
	require.NoError(t, err)

	candidates, err := selectCandidates(ctx, f.db, 64<<20, f.tenantID, "", 0)
	require.NoError(t, err)
	for _, c := range candidates {
		assert.NotEqual(t, "small.bin", c.key, "encrypted/small objects must not be selected")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	f := setupFixture(t)
	ctx := context.Background()

	content := make([]byte, 128*1024)
	_, _ = rand.Read(content)
	etag := seedObject(t, f, "idem.bin", content)

	c := candidate{
		tenantID: f.tenantID,
		bucket:   "test-bucket",
		key:      "idem.bin",
		size:     int64(len(content)),
		etag:     etag,
	}

	// First migration
	res1, err := migrateObject(ctx, f.d, c, false, true)
	require.NoError(t, err)
	require.False(t, res1.Failed)

	// Record ref counts after first run
	refCounts1 := getRefCounts(t, f, "idem.bin")

	// Second run: is_chunked is TRUE, so selectCandidates won't return it.
	// Verify the SQL excludes it.
	candidates, err := selectCandidates(ctx, f.db, 0, f.tenantID, "test-bucket", 0)
	require.NoError(t, err)
	for _, cc := range candidates {
		assert.NotEqual(t, "idem.bin", cc.key, "already-chunked objects must not be selected again")
	}

	// Even if forced through migrateObject a second time, ref counts
	// should not double-bump (ReplaceObjectManifest releases old + installs new).
	res2, err := migrateObject(ctx, f.d, c, false, true)
	require.NoError(t, err)

	// The second run re-reads from the original file (--keep-original), so
	// it should still succeed with matching content.
	require.False(t, res2.Failed)

	refCounts2 := getRefCounts(t, f, "idem.bin")
	assert.Equal(t, refCounts1, refCounts2, "ref counts must not double-bump on re-run")
}

func computeEtag(data []byte) string {
	h := md5.New() // #nosec G401
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func reassembleFromChunks(t *testing.T, f *testFixture, key string) []byte {
	t.Helper()
	ctx := context.Background()

	rows, err := f.db.QueryContext(ctx, `
		SELECT plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
		ORDER BY chunk_index ASC
	`, f.tenantID, "test-bucket", key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var buf bytes.Buffer
	for rows.Next() {
		var hash string
		require.NoError(t, rows.Scan(&hash))

		storageKey := "_chunks/" + hash
		reader, getErr := f.eng.Get(ctx, "_global", storageKey)
		require.NoError(t, getErr)
		_, copyErr := io.Copy(&buf, reader)
		require.NoError(t, copyErr)
		_ = reader.Close()
	}
	require.NoError(t, rows.Err())
	return buf.Bytes()
}

func getRefCounts(t *testing.T, f *testFixture, key string) map[string]int {
	t.Helper()
	ctx := context.Background()

	rows, err := f.db.QueryContext(ctx, `
		SELECT r.plaintext_hash, g.ref_count
		FROM tenant_chunk_refs r
		JOIN global_content_index g ON g.plaintext_hash = r.plaintext_hash
		WHERE r.tenant_id = $1 AND r.bucket_name = $2 AND r.object_key = $3
	`, f.tenantID, "test-bucket", key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)
	for rows.Next() {
		var hash string
		var count int
		require.NoError(t, rows.Scan(&hash, &count))
		counts[hash] = count
	}
	require.NoError(t, rows.Err())
	return counts
}
