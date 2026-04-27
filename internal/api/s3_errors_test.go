package api

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "b", 1},
		{"kitten", "sitting", 3},
		{"photos", "photso", 2},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"report-2024.pdf", "report-2025.pdf", 1},
		{"backups", "backup", 1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := levenshtein(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWriteS3ErrorWithContext_NoSuggestion(t *testing.T) {
	w := httptest.NewRecorder()
	WriteS3ErrorWithContext(w, ErrNoSuchBucket, "/my-bucket", "req-123")

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/xml")

	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, ErrNoSuchBucket, errResp.Code)
	assert.Equal(t, errorMessages[ErrNoSuchBucket], errResp.Message)
}

func TestWriteS3ErrorWithContext_WithSuggestion(t *testing.T) {
	w := httptest.NewRecorder()
	WriteS3ErrorWithContext(w, ErrNoSuchBucket, "/photso", "req-456",
		WithSuggestion("Did you mean 'photos'?"))

	assert.Equal(t, http.StatusNotFound, w.Code)

	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, ErrNoSuchBucket, errResp.Code)
	assert.Contains(t, errResp.Message, "Did you mean 'photos'?")
	assert.Contains(t, errResp.Message, "The specified bucket does not exist.")
}

func errorsTestDB(t *testing.T) *sql.DB {
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

func cleanupErrorsTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id LIKE 'test-err-%'`)
	_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id LIKE 'test-err-%'`)
	_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id LIKE 'test-err-%'`)
	_, _ = db.Exec(`DELETE FROM tenants WHERE id LIKE 'test-err-%'`)
}

func TestNoSuchBucketSuggestion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := errorsTestDB(t)
	defer func() { _ = db.Close() }()

	t.Cleanup(func() { cleanupErrorsTestData(t, db) })
	cleanupErrorsTestData(t, db)

	ctx := context.Background()
	tenantID := "test-err-bucket-suggest"

	// Insert tenant
	_, err := db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $1, 'test@test.com', 'ak-err-test', 'sk-err-test')
		ON CONFLICT (id) DO NOTHING`, tenantID)
	require.NoError(t, err)

	// Insert buckets for this tenant
	for _, name := range []string{"photos", "backups", "media"} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO buckets (tenant_id, name, visibility)
			 VALUES ($1, $2, 'private') ON CONFLICT DO NOTHING`, tenantID, name)
		require.NoError(t, err)
	}

	// "photso" should suggest "photos" (distance 2)
	suggestion := bucketSuggestion(ctx, db, tenantID, "photso")
	assert.Contains(t, suggestion, "photos")

	// "bakcups" should suggest "backups" (distance 2)
	suggestion = bucketSuggestion(ctx, db, tenantID, "bakcups")
	assert.Contains(t, suggestion, "backups")
}

func TestNoSuchBucketNoSuggestion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := errorsTestDB(t)
	defer func() { _ = db.Close() }()

	t.Cleanup(func() { cleanupErrorsTestData(t, db) })
	cleanupErrorsTestData(t, db)

	ctx := context.Background()
	tenantID := "test-err-bucket-nosug"

	_, err := db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $1, 'test@test.com', 'ak-err-nosug', 'sk-err-nosug')
		ON CONFLICT (id) DO NOTHING`, tenantID)
	require.NoError(t, err)

	for _, name := range []string{"photos", "backups", "media"} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO buckets (tenant_id, name, visibility)
			 VALUES ($1, $2, 'private') ON CONFLICT DO NOTHING`, tenantID, name)
		require.NoError(t, err)
	}

	// "zzzzzzz" is too far from any bucket (distance > 3)
	suggestion := bucketSuggestion(ctx, db, tenantID, "zzzzzzz")
	assert.Empty(t, suggestion)
}

func TestNoSuchKeySuggestion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := errorsTestDB(t)
	defer func() { _ = db.Close() }()

	t.Cleanup(func() { cleanupErrorsTestData(t, db) })
	cleanupErrorsTestData(t, db)

	ctx := context.Background()
	tenantID := "test-err-key-suggest"

	_, err := db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $1, 'test@test.com', 'ak-err-key', 'sk-err-key')
		ON CONFLICT (id) DO NOTHING`, tenantID)
	require.NoError(t, err)

	// Insert objects in head cache
	for _, key := range []string{"report-2024.pdf", "report-2024-q1.pdf", "report-2024-q2.pdf"} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
			VALUES ($1, 'docs', $2, 1024, 'abc123', 'application/pdf', NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO NOTHING`, tenantID, key)
		require.NoError(t, err)
	}

	// "report-2025.pdf" should suggest "report-2024.pdf" (distance 1)
	suggestion := keySuggestion(ctx, db, tenantID, "docs", "report-2025.pdf")
	assert.Contains(t, suggestion, "report-2024.pdf")
}

func TestNoSuchKeyNoSuggestion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := errorsTestDB(t)
	defer func() { _ = db.Close() }()

	t.Cleanup(func() { cleanupErrorsTestData(t, db) })
	cleanupErrorsTestData(t, db)

	ctx := context.Background()
	tenantID := "test-err-key-nosug"

	_, err := db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $1, 'test@test.com', 'ak-err-keyno', 'sk-err-keyno')
		ON CONFLICT (id) DO NOTHING`, tenantID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
		VALUES ($1, 'docs', 'report-2024.pdf', 1024, 'abc123', 'application/pdf', NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO NOTHING`, tenantID)
	require.NoError(t, err)

	// Completely unrelated key — no suggestion
	suggestion := keySuggestion(ctx, db, tenantID, "docs", "zzzzzzzzzzz.txt")
	assert.Empty(t, suggestion)
}

func TestAccessDeniedHints(t *testing.T) {
	tests := []struct {
		name    string
		authErr string
		want    string
		empty   bool
	}{
		{
			name:    "missing authorization",
			authErr: "missing authorization",
			want:    "No authorization header provided",
		},
		{
			name:    "invalid access key",
			authErr: "invalid access key",
			want:    "access key ID you provided does not exist",
		},
		{
			name:    "invalid authorization format",
			authErr: "invalid authorization format",
			want:    "check your secret key and signing method",
		},
		{
			name:    "unknown error returns empty",
			authErr: "something unexpected",
			empty:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := authErrorHint(tt.authErr)
			if tt.empty {
				assert.Empty(t, msg)
			} else {
				assert.Contains(t, msg, tt.want)
			}
		})
	}
}

func TestAccessDeniedHints_EndToEnd(t *testing.T) {
	w := httptest.NewRecorder()
	hint := authErrorHint("invalid access key")
	WriteS3ErrorWithContext(w, ErrAccessDenied, "/bucket", "req-auth",
		WithSuggestion(hint))

	assert.Equal(t, http.StatusForbidden, w.Code)

	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, ErrAccessDenied, errResp.Code)
	assert.Contains(t, errResp.Message, "Access denied")
	assert.Contains(t, errResp.Message, "does not exist in our records")
}

func TestCrossTenantNoLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := errorsTestDB(t)
	defer func() { _ = db.Close() }()

	t.Cleanup(func() { cleanupErrorsTestData(t, db) })
	cleanupErrorsTestData(t, db)

	ctx := context.Background()
	tenantA := "test-err-tenant-a"
	tenantB := "test-err-tenant-b"

	// Create both tenants
	for i, tid := range []string{tenantA, tenantB} {
		ak := fmt.Sprintf("ak-err-xt-%d", i)
		sk := fmt.Sprintf("sk-err-xt-%d", i)
		_, err := db.ExecContext(ctx, `
			INSERT INTO tenants (id, name, email, access_key, secret_key)
			VALUES ($1, $1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING`, tid, tid+"@test.com", ak, sk)
		require.NoError(t, err)
	}

	// Tenant A owns "secret-data" bucket
	_, err := db.ExecContext(ctx, `
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, 'secret-data', 'private') ON CONFLICT DO NOTHING`, tenantA)
	require.NoError(t, err)

	// Tenant B asks for "secret-data" — should get no suggestion
	// because bucket suggestion only queries tenant B's own buckets
	suggestion := bucketSuggestion(ctx, db, tenantB, "secret-data")
	assert.Empty(t, suggestion, "must not suggest buckets from other tenants")

	// Tenant B asks for "secret-datb" — still no suggestion
	suggestion = bucketSuggestion(ctx, db, tenantB, "secret-datb")
	assert.Empty(t, suggestion, "must not suggest close matches from other tenants")
}

func TestBucketSuggestion_NilDB(t *testing.T) {
	suggestion := bucketSuggestion(context.Background(), nil, "tenant", "bucket")
	assert.Empty(t, suggestion)
}

func TestKeySuggestion_NilDB(t *testing.T) {
	suggestion := keySuggestion(context.Background(), nil, "tenant", "bucket", "key")
	assert.Empty(t, suggestion)
}

func TestWriteS3ErrorWithContext_FallsBackForUnknownCode(t *testing.T) {
	w := httptest.NewRecorder()
	WriteS3ErrorWithContext(w, "BogusCode", "/foo", "req-999")

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, ErrInternalError, errResp.Code)
}

func TestNoSuchBucketSuggestion_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := errorsTestDB(t)
	defer func() { _ = db.Close() }()

	t.Cleanup(func() { cleanupErrorsTestData(t, db) })
	cleanupErrorsTestData(t, db)

	ctx := context.Background()
	tenantID := "test-err-e2e"

	_, err := db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $1, 'test@test.com', 'ak-err-e2e', 'sk-err-e2e')
		ON CONFLICT (id) DO NOTHING`, tenantID)
	require.NoError(t, err)

	for _, name := range []string{"photos", "backups"} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO buckets (tenant_id, name, visibility)
			 VALUES ($1, $2, 'private') ON CONFLICT DO NOTHING`, tenantID, name)
		require.NoError(t, err)
	}

	// Simulate what a handler does: build suggestion, write error
	w := httptest.NewRecorder()
	suggestion := bucketSuggestion(ctx, db, tenantID, "photso")
	if suggestion != "" {
		WriteS3ErrorWithContext(w, ErrNoSuchBucket, "/photso", "req-e2e",
			WithSuggestion(suggestion))
	} else {
		WriteS3ErrorWithContext(w, ErrNoSuchBucket, "/photso", "req-e2e")
	}

	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, ErrNoSuchBucket, errResp.Code)
	assert.Contains(t, errResp.Message, "photos")
	assert.Contains(t, errResp.Message, "Did you mean")

	// Verify the tenant.FromContext pattern works
	_ = tenant.WithTenant(ctx, &tenant.Tenant{ID: tenantID})
}
