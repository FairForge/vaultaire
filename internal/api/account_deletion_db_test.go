// account_deletion_db_test.go — WP-8 (CR-9): GDPR ExecuteDeletion must remove
// every row belonging to the user/tenant without tripping a foreign key, even
// when webhook deliveries, chunked-object manifests, STS tokens, MFA settings,
// OAuth links, and encryption keys exist. Red before WP-8: DELETE FROM events
// violates webhook_deliveries.event_id, and half these tables were never
// touched at all.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func TestExecuteDeletion_RemovesAllTenantData(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	require.NoError(t, db.PingContext(ctx))

	// UUID-string tenant ID so rows can exist in the UUID-typed chunk tables
	// (tenant_chunk_refs, object_metadata) as they do for real chunked objects.
	userID := uuid.New().String()
	tenantID := uuid.New().String()
	suffix := userID[:8]
	email := fmt.Sprintf("wp8-deletion-%s@example.com", suffix)
	bucket := "wp8-del-bucket-" + suffix
	chunkHash := strings.Repeat("0", 64-len(suffix)) + suffix // 64-char hex-ish key for GCI

	mustExec := func(query string, args ...interface{}) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, "fixture insert: %s", query)
	}

	// The shared-dedup GCI row is global (not tenant-owned) — ExecuteDeletion
	// must NOT remove it; clean it up ourselves along with any debris from a
	// failed run.
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM webhook_deliveries WHERE webhook_id LIKE 'wh-wp8-%'`,
			`DELETE FROM webhook_endpoints WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM tenant_chunk_refs WHERE tenant_id::text = $1`,
			`DELETE FROM object_metadata WHERE tenant_id::text = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM object_versions WHERE tenant_id = $1`,
			`DELETE FROM object_locks WHERE tenant_id = $1`,
			`DELETE FROM object_locations WHERE tenant_id = $1`,
			`DELETE FROM sts_tokens WHERE tenant_id = $1`,
			`DELETE FROM tenant_encryption_keys WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM quota_usage_events WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM artifacts WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			if _, err := db.ExecContext(ctx, q, tenantID); err != nil {
				t.Logf("cleanup (residue may break other DB tests): %s: %v", q, err)
			}
		}
		for _, q := range []string{
			`DELETE FROM user_mfa WHERE user_id = $1`,
			`DELETE FROM oauth_accounts WHERE user_id::text = $1`,
			`DELETE FROM user_activities WHERE user_id = $1`,
			`DELETE FROM api_keys WHERE user_id::text = $1`,
			`DELETE FROM users WHERE id::text = $1`,
		} {
			if _, err := db.ExecContext(ctx, q, userID); err != nil {
				t.Logf("cleanup (residue may break other DB tests): %s: %v", q, err)
			}
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM global_content_index WHERE plaintext_hash = $1`, chunkHash); err != nil {
			t.Logf("cleanup: GCI row %s: %v", chunkHash, err)
		}
	})

	// --- Arrange: a tenant with data in every table deletion must cover ---
	// company must be non-NULL: auth.LoadFromDB scans it without COALESCE and
	// package auth's tests can run concurrently against this shared database.
	mustExec(`INSERT INTO users (id, email, password_hash, company, created_at, updated_at)
	          VALUES ($1, $2, 'x', 'WP8 Deletion Co', NOW(), NOW())`, userID, email)
	mustExec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
	          VALUES ($1, 'WP8 Deletion Co', $2, $3, $4)`,
		tenantID, email, "VKWP8"+suffix, "SKWP8"+suffix)
	mustExec(`INSERT INTO api_keys (id, user_id, name, key_id, secret_hash)
	          VALUES ($1, $2, 'primary', $3, 'hash')`,
		uuid.New().String(), userID, "VLT_WP8"+suffix)
	mustExec(`INSERT INTO tenant_quotas (tenant_id) VALUES ($1)`, tenantID)
	mustExec(`INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key)
	          VALUES ($1, 'RESERVE', 1024, 'wp8/key')`, tenantID)

	// An upload: head-cache row + bucket + version + lock + location.
	mustExec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, tenantID, bucket)
	mustExec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag)
	          VALUES ($1, $2, 'wp8/key', 1024, 'etag-wp8')`, tenantID, bucket)
	mustExec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag)
	          VALUES ($1, $2, 'wp8/key', 'v1', 1024, 'etag-wp8')`, tenantID, bucket)
	mustExec(`INSERT INTO object_locks (tenant_id, bucket, object_key) VALUES ($1, $2, 'wp8/key')`,
		tenantID, bucket)
	mustExec(`INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name)
	          VALUES ($1, $2, 'wp8/key', 'local')`, tenantID, bucket)

	// A chunked object: GCI row (global, survives) + tenant manifest rows.
	mustExec(`INSERT INTO global_content_index (plaintext_hash, backend_id, storage_key, size_bytes)
	          VALUES ($1, 'local', $2, 1024) ON CONFLICT DO NOTHING`,
		chunkHash, "_chunks/"+chunkHash)
	mustExec(`INSERT INTO tenant_chunk_refs (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash)
	          VALUES ($1, $2, 'wp8/chunked', 0, 0, $3)`, tenantID, bucket, chunkHash)
	mustExec(`INSERT INTO object_metadata (tenant_id, bucket_name, object_key, total_size, chunk_count, logical_size)
	          VALUES ($1, $2, 'wp8/chunked', 1024, 1, 1024)`, tenantID, bucket)

	// A webhook delivery: endpoint + event + delivery row referencing both.
	mustExec(`INSERT INTO events (id, type, tenant_id) VALUES ($1, 'object.created', $2)`,
		"ev-wp8-"+suffix, tenantID)
	mustExec(`INSERT INTO webhook_endpoints (id, tenant_id, url, secret)
	          VALUES ($1, $2, 'https://example.com/hook', 'whsec_wp8')`,
		"wh-wp8-"+suffix, tenantID)
	mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event_id, status)
	          VALUES ($1, $2, $3, 'success')`,
		"whd-wp8-"+suffix, "wh-wp8-"+suffix, "ev-wp8-"+suffix)

	// Credentials + identity residue: STS token, MFA, encryption key, OAuth link, activity.
	mustExec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at)
	          VALUES ($1, 'sk', $2, 'parent', $3)`,
		"ASIAWP8"+suffix, tenantID, time.Now().Add(time.Hour))
	mustExec(`INSERT INTO user_mfa (user_id, secret) VALUES ($1, 'totp-secret')`, userID)
	mustExec(`INSERT INTO tenant_encryption_keys (tenant_id, seed, public_key)
	          VALUES ($1, $2, $3)`, tenantID, []byte("seed"), []byte("pub"))
	mustExec(`INSERT INTO oauth_accounts (user_id, provider, provider_id, email)
	          VALUES ($1, 'google', $2, $3)`, userID, "wp8-oauth-"+suffix, email)
	mustExec(`INSERT INTO user_activities (id, user_id, action)
	          VALUES ($1, $2, 'login')`, uuid.New().String(), userID)
	mustExec(`INSERT INTO artifacts (tenant_id, container, name, size)
	          VALUES ($1, $2, 'wp8/key', 1024)`, tenantID, bucket)
	// review-D G2: an admin note AUTHORED BY this user — its FK to users has
	// no ON DELETE action, so before the fix this row made the entire GDPR
	// deletion roll back.
	mustExec(`INSERT INTO admin_notes (tenant_id, admin_user_id, note)
	          VALUES ($1, $2, 'wp8 note')`, tenantID, userID)

	// --- Act ---
	svc := NewAccountDeletionService(db, zap.NewNop())
	err = svc.ExecuteDeletion(ctx, userID, tenantID)
	require.NoError(t, err, "ExecuteDeletion must not trip a foreign key")

	// --- Assert: no rows left anywhere ---
	tenantScoped := map[string]string{
		"tenants":                "id = $1",
		"tenant_quotas":          "tenant_id = $1",
		"quota_usage_events":     "tenant_id = $1",
		"buckets":                "tenant_id = $1",
		"object_head_cache":      "tenant_id = $1",
		"object_versions":        "tenant_id = $1",
		"object_locks":           "tenant_id = $1",
		"object_locations":       "tenant_id = $1",
		"tenant_chunk_refs":      "tenant_id::text = $1",
		"object_metadata":        "tenant_id::text = $1",
		"events":                 "tenant_id = $1",
		"webhook_endpoints":      "tenant_id = $1",
		"sts_tokens":             "tenant_id = $1",
		"tenant_encryption_keys": "tenant_id = $1",
		"artifacts":              "tenant_id = $1",
	}
	for table, where := range tenantScoped {
		var n int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where, tenantID).Scan(&n)
		require.NoError(t, err, "count %s", table)
		assert.Zero(t, n, "%s must be empty for the deleted tenant", table)
	}
	userScoped := map[string]string{
		"users":           "id::text = $1",
		"api_keys":        "user_id::text = $1",
		"user_mfa":        "user_id = $1",
		"oauth_accounts":  "user_id::text = $1",
		"user_activities": "user_id = $1",
		"admin_notes":     "admin_user_id::text = $1",
	}
	for table, where := range userScoped {
		var n int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where, userID).Scan(&n)
		require.NoError(t, err, "count %s", table)
		assert.Zero(t, n, "%s must be empty for the deleted user", table)
	}
	var deliveries int
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = $1", "wh-wp8-"+suffix).Scan(&deliveries)
	require.NoError(t, err)
	assert.Zero(t, deliveries, "webhook_deliveries must be gone (cascade or explicit)")

	// The global dedup index row must survive — other tenants may share it.
	var gci int
	err = db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM global_content_index WHERE plaintext_hash = $1", chunkHash).Scan(&gci)
	require.NoError(t, err)
	assert.Equal(t, 1, gci, "shared GCI row must NOT be deleted with the tenant")
}
