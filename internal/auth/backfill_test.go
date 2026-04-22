package auth

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://viera@localhost:5432/vaultaire?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	return db
}

func cleanupBackfillData(t *testing.T, db *sql.DB) {
	t.Helper()
	_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id LIKE 'test-backfill-%'`)
	_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id LIKE 'test-backfill-%'`)
	_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id LIKE 'test-backfill-%'`)
	_, _ = db.Exec(`DELETE FROM tenants WHERE id LIKE 'test-backfill-%'`)
}

func TestBackfillBuckets_CreatesFromObjectHeadCache(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	// Arrange: insert tenant + objects in object_head_cache.
	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-1', 'Test Co', 'backfill1@test.com', 'VK-bf1', 'SK-bf1') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type) VALUES ('test-backfill-1', 'bucket-a', 'file1.txt', 100, 'abc', 'text/plain') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type) VALUES ('test-backfill-1', 'bucket-b', 'file2.txt', 200, 'def', 'text/plain') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	// Act
	err = BackfillBuckets(ctx, db, zap.NewNop())
	require.NoError(t, err)

	// Assert
	var count int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets WHERE tenant_id = 'test-backfill-1'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestBackfillBuckets_SkipsExisting(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-2', 'Test Co 2', 'backfill2@test.com', 'VK-bf2', 'SK-bf2') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO buckets (tenant_id, name, visibility) VALUES ('test-backfill-2', 'bucket-a', 'public-read') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type) VALUES ('test-backfill-2', 'bucket-a', 'file1.txt', 100, 'abc', 'text/plain') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type) VALUES ('test-backfill-2', 'bucket-b', 'file2.txt', 200, 'def', 'text/plain') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	err = BackfillBuckets(ctx, db, zap.NewNop())
	require.NoError(t, err)

	// bucket-a should still be public-read (not overwritten).
	var vis string
	err = db.QueryRowContext(ctx, `SELECT visibility FROM buckets WHERE tenant_id = 'test-backfill-2' AND name = 'bucket-a'`).Scan(&vis)
	require.NoError(t, err)
	assert.Equal(t, "public-read", vis)

	// bucket-b should have been added.
	err = db.QueryRowContext(ctx, `SELECT visibility FROM buckets WHERE tenant_id = 'test-backfill-2' AND name = 'bucket-b'`).Scan(&vis)
	require.NoError(t, err)
	assert.Equal(t, "private", vis)
}

func TestBackfillBuckets_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-3', 'Test Co 3', 'backfill3@test.com', 'VK-bf3', 'SK-bf3') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type) VALUES ('test-backfill-3', 'bucket-x', 'file.txt', 100, 'abc', 'text/plain') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	// Run twice.
	err = BackfillBuckets(ctx, db, zap.NewNop())
	require.NoError(t, err)
	err = BackfillBuckets(ctx, db, zap.NewNop())
	require.NoError(t, err)

	var count int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets WHERE tenant_id = 'test-backfill-3'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestBackfillSlugs_GeneratesFromCompanyName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-4', 'Acme Corp', 'backfill4@test.com', 'VK-bf4', 'SK-bf4') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	err = BackfillSlugs(ctx, db, zap.NewNop())
	require.NoError(t, err)

	var slug sql.NullString
	err = db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE id = 'test-backfill-4'`).Scan(&slug)
	require.NoError(t, err)
	assert.True(t, slug.Valid)
	assert.Equal(t, "acme-corp", slug.String)
}

func TestBackfillSlugs_SkipsExistingSlugs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key, slug) VALUES ('test-backfill-5', 'Whatever', 'backfill5@test.com', 'VK-bf5', 'SK-bf5', 'custom-slug') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	err = BackfillSlugs(ctx, db, zap.NewNop())
	require.NoError(t, err)

	var slug string
	err = db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE id = 'test-backfill-5'`).Scan(&slug)
	require.NoError(t, err)
	assert.Equal(t, "custom-slug", slug)
}

func TestBackfillSlugs_HandlesCollision(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-6a', 'Acme Corp', 'backfill6a@test.com', 'VK-bf6a', 'SK-bf6a') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-6b', 'Acme Corp', 'backfill6b@test.com', 'VK-bf6b', 'SK-bf6b') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	err = BackfillSlugs(ctx, db, zap.NewNop())
	require.NoError(t, err)

	var slug1, slug2 string
	err = db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE id = 'test-backfill-6a'`).Scan(&slug1)
	require.NoError(t, err)
	err = db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE id = 'test-backfill-6b'`).Scan(&slug2)
	require.NoError(t, err)

	assert.NotEqual(t, slug1, slug2)
	assert.Contains(t, []string{"acme-corp", "acme-corp-1"}, slug1)
	assert.Contains(t, []string{"acme-corp", "acme-corp-1"}, slug2)
}

func TestBackfillSlugs_EmptyCompanyName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	cleanupBackfillData(t, db)
	defer cleanupBackfillData(t, db)

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-backfill-7', '', 'backfill7@test.com', 'VK-bf7', 'SK-bf7') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	err = BackfillSlugs(ctx, db, zap.NewNop())
	require.NoError(t, err)

	var slug string
	err = db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE id = 'test-backfill-7'`).Scan(&slug)
	require.NoError(t, err)
	assert.True(t, len(slug) >= 2)
	assert.True(t, slug == "tenant" || len(slug) > len("tenant"))
}
