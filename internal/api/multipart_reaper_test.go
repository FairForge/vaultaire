// multipart_reaper_test.go — WP-10-minimal (item 1.10, H-1): the disk-fill
// DoS. Multipart part data streams to /tmp/vaultaire-multipart/{uploadID}/
// on LOCAL disk and is unbilled until CompleteMultipartUpload — quota is only
// reserved at complete (WP-1). An attacker (or a crashed client) can initiate
// uploads, push parts forever, never complete, and fill the prod disk with
// zero quota charge. Nothing ever cleaned these up: no reaper existed, and
// abort/complete leave terminal rows forever.
//
// Two defenses, both tested here:
//  1. MultipartReaper — aborts active uploads with no activity past
//     AbandonAge (removing their temp dirs), and purges terminal
//     (completed/aborted) rows past TerminalRetention (parts rows cascade).
//  2. Per-upload byte cap — UploadPart rejects a part that would push the
//     upload's accumulated in-flight bytes past the cap (EntityTooLarge),
//     checked against the DECLARED size before the body is read and against
//     the MEASURED size before the part is recorded.
package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// reaperFixtureUpload inserts a multipart upload row (+ optional part row and
// temp dir with a file) and registers cleanup.
func reaperFixtureUpload(t *testing.T, f *quotaAccountingFixture, status string, uploadAge, partAge time.Duration, withPart bool) string {
	t.Helper()
	uploadID := "wp10-reap-" + uuid.New().String()

	_, err := f.db.Exec(`
		INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status, created_at)
		VALUES ($1, $2, 'test-bucket', 'reap.bin', $3, NOW() - $4::interval)`,
		uploadID, f.tenantID, status, fmt.Sprintf("%d seconds", int(uploadAge.Seconds())))
	require.NoError(t, err)

	if withPart {
		_, err = f.db.Exec(`
			INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes, created_at)
			VALUES ($1, 1, 'etag-x', 1024, NOW() - $2::interval)`,
			uploadID, fmt.Sprintf("%d seconds", int(partAge.Seconds())))
		require.NoError(t, err)
	}

	require.NoError(t, os.MkdirAll(multipartDir(uploadID), 0o700))
	require.NoError(t, os.WriteFile(partFilePath(uploadID, 1), []byte("part-data"), 0o600))
	t.Cleanup(func() {
		_ = os.RemoveAll(multipartDir(uploadID))
		_, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE upload_id = $1`, uploadID)
	})
	return uploadID
}

func uploadStatus(t *testing.T, f *quotaAccountingFixture, uploadID string) (status string, exists bool) {
	t.Helper()
	err := f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

func TestMultipartReaper_AbortsAbandonedUploads(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	reaper := NewMultipartReaper(f.db, zap.NewNop())
	require.NotNil(t, reaper)
	reaper.AbandonAge = 48 * time.Hour

	// Abandoned: initiated 3 days ago, last part 3 days ago.
	stale := reaperFixtureUpload(t, f, "active", 72*time.Hour, 72*time.Hour, true)
	// Fresh: initiated just now.
	fresh := reaperFixtureUpload(t, f, "active", 0, 0, true)
	// Old upload but RECENT part activity — a slow-but-live client must not
	// be killed mid-upload; abandonment is measured from last activity.
	slowButAlive := reaperFixtureUpload(t, f, "active", 72*time.Hour, time.Hour, true)

	result, err := reaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Aborted, 1)

	st, ok := uploadStatus(t, f, stale)
	require.True(t, ok, "abandoned upload row must remain (terminal, for audit) until retention purge")
	assert.Equal(t, "aborted", st, "abandoned upload must be aborted")
	assert.NoDirExists(t, multipartDir(stale), "abandoned upload's temp dir (the leaked disk) must be removed")

	st, ok = uploadStatus(t, f, fresh)
	require.True(t, ok)
	assert.Equal(t, "active", st, "fresh upload must not be touched")
	assert.DirExists(t, multipartDir(fresh))

	st, ok = uploadStatus(t, f, slowButAlive)
	require.True(t, ok)
	assert.Equal(t, "active", st, "upload with recent part activity must not be reaped")
	assert.DirExists(t, multipartDir(slowButAlive))
}

func TestMultipartReaper_PurgesTerminalRowsPastRetention(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	reaper := NewMultipartReaper(f.db, zap.NewNop())
	require.NotNil(t, reaper)
	reaper.TerminalRetention = 7 * 24 * time.Hour

	oldCompleted := reaperFixtureUpload(t, f, "completed", 8*24*time.Hour, 8*24*time.Hour, true)
	oldAborted := reaperFixtureUpload(t, f, "aborted", 8*24*time.Hour, 8*24*time.Hour, true)
	recentCompleted := reaperFixtureUpload(t, f, "completed", time.Hour, time.Hour, false)

	result, err := reaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Purged, 2)

	_, ok := uploadStatus(t, f, oldCompleted)
	assert.False(t, ok, "old completed upload row must be purged")
	_, ok = uploadStatus(t, f, oldAborted)
	assert.False(t, ok, "old aborted upload row must be purged")
	assert.NoDirExists(t, multipartDir(oldAborted), "any leftover temp dir must go with the row")

	var partRows int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM multipart_parts WHERE upload_id IN ($1, $2)`,
		oldCompleted, oldAborted).Scan(&partRows))
	assert.Zero(t, partRows, "part rows must cascade with the purged upload rows")

	_, ok = uploadStatus(t, f, recentCompleted)
	assert.True(t, ok, "recent terminal rows stay within retention")
}

func TestMultipartReaper_NilGuards(t *testing.T) {
	assert.Nil(t, NewMultipartReaper(nil, zap.NewNop()), "nil db must return nil reaper")
}

// --- Per-upload byte cap ---

func capFixtureInitiate(t *testing.T, f *quotaAccountingFixture, key string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/test-bucket/"+key+"?uploads", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(w, req, "test-bucket", key)
	require.Equal(t, 200, w.Code)
	var res InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	t.Cleanup(func() {
		_ = os.RemoveAll(multipartDir(res.UploadID))
		_, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE upload_id = $1`, res.UploadID)
	})
	return res.UploadID
}

func capFixtureUploadPart(t *testing.T, f *quotaAccountingFixture, key, uploadID string, partNumber int, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/test-bucket/%s?uploadId=%s&partNumber=%d", key, uploadID, partNumber)
	req := httptest.NewRequest("PUT", url, bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleUploadPart(w, req, "test-bucket", key)
	return w
}

func TestUploadPart_PerUploadByteCap(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.server.multipartMaxUploadBytes = 3 << 10 // 3 KiB cap for the test

	uploadID := capFixtureInitiate(t, f, "capped.bin")

	// Two 1 KiB parts fit under the 3 KiB cap.
	require.Equal(t, 200, capFixtureUploadPart(t, f, "capped.bin", uploadID, 1, testBytes(1<<10)).Code)
	require.Equal(t, 200, capFixtureUploadPart(t, f, "capped.bin", uploadID, 2, testBytes(1<<10)).Code)

	// A third 2 KiB part would take the upload to 4 KiB — over the cap.
	w := capFixtureUploadPart(t, f, "capped.bin", uploadID, 3, testBytes(2<<10))
	// 413 — this codebase maps EntityTooLarge to Payload Too Large (see
	// s3_errors.go), matching the SSE oversize path.
	assert.Equal(t, 413, w.Code, "part pushing the upload past the cap must be rejected")
	assert.Contains(t, w.Body.String(), "EntityTooLarge")

	// The rejected part must leave nothing behind: no row, no temp file.
	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1 AND part_number = 3`,
		uploadID).Scan(&n))
	assert.Zero(t, n, "rejected part must not be recorded")
	assert.NoFileExists(t, partFilePath(uploadID, 3), "rejected part's temp file must be removed")

	// Re-uploading an EXISTING part number replaces bytes, not adds — a 1 KiB
	// re-upload of part 1 keeps the total at 2 KiB and must succeed.
	require.Equal(t, 200, capFixtureUploadPart(t, f, "capped.bin", uploadID, 1, testBytes(1<<10)).Code)
}

func TestUploadPart_CapDisabledByDefaultFixture(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	// Fixture server has no cap set (0 = unlimited in struct-literal servers);
	// normal parts must be unaffected.
	uploadID := capFixtureInitiate(t, f, "uncapped.bin")
	assert.Equal(t, 200, capFixtureUploadPart(t, f, "uncapped.bin", uploadID, 1, testBytes(8<<10)).Code)
}
