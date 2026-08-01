// shutdown_flush_test.go — H-3 (WP-11 slice, land before Stage 4): a deploy
// restarts the process, and anything sitting in the trackers' 5-second write
// buffers is dropped unless Shutdown flushes them. Once Stripe meters are on,
// dropped bandwidth events = dropped billing data. The flusher goroutines are
// started with context.Background(), so their "final flush on ctx.Done" branch
// never fires — Shutdown must flush synchronously after the HTTP server drains.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestShutdown_FlushesBufferedTrackers(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed shutdown flush test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, db.Ping())

	s := NewServer(
		&config.Config{Server: config.ServerConfig{Port: 0}},
		zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), nil),
		nil,
		db,
	)

	// Real tenants row — bandwidth_usage_daily.tenant_id has an FK to
	// tenants(id), and the flushers swallow insert errors.
	tenantID := fmt.Sprintf("tenant-shutdown-flush-%d", time.Now().UnixNano())
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, 'shutdown-flush-test', $2, $3, $4)`,
		tenantID, tenantID+"@test.local", "AK"+tenantID, "SK"+tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cdn_access_log WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM s3_access_log WHERE tenant_id = $1`, tenantID)
		// bandwidth_usage_daily cascades on tenant delete.
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	ctx := context.Background()

	// One event in each tracker's buffer — far below the 100-event
	// auto-flush threshold, and Shutdown runs long before the 5s ticker.
	s.bandwidthTracker.Record(ctx, tenantID, 1024, 2048)
	s.cdnAnalytics.Record(ctx, tenantID, "flush-bucket", "obj.bin", 512, "US", "")
	s.accessLogTracker.Record(ctx, s3AccessEvent{
		tenantID:   tenantID,
		bucket:     "flush-bucket",
		objectKey:  "obj.bin",
		operation:  "GetObject",
		statusCode: 200,
		bytesSent:  512,
	})

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(shutdownCtx))

	var egress int64
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily WHERE tenant_id = $1`,
		tenantID).Scan(&egress))
	require.Equal(t, int64(2048), egress,
		"bandwidth buffer must be flushed on shutdown — buffered egress is billing data")

	var cdnRows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM cdn_access_log WHERE tenant_id = $1`, tenantID).Scan(&cdnRows))
	require.Equal(t, 1, cdnRows, "CDN analytics buffer must be flushed on shutdown")

	var logRows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1`, tenantID).Scan(&logRows))
	require.Equal(t, 1, logRows, "S3 access log buffer must be flushed on shutdown")
}
