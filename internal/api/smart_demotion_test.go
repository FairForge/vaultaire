package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Smart-tier demotion (Phase 5.15.8): per tenant, at most hot budget =
// HotFraction × quota stays on the hot backend. Demote idle ≥ IdleAfter, or
// LRU while over budget, never objects younger than MinAge. Routing flips in
// object_head_cache (the source of truth GET/HEAD/restore route on); the hot
// copy is reclaimed on a later run after a grace period, etag-guarded.

const (
	tb = int64(1) << 40
	gb = int64(1) << 30
)

type stubFlags struct{ on map[string]bool }

func (f stubFlags) Enabled(key, tenantID string) bool {
	if v, ok := f.on[key+"/"+tenantID]; ok {
		return v
	}
	return f.on[key]
}

type demotionFixture struct {
	t        *testing.T
	db       *sql.DB
	eng      *engine.CoreEngine
	hotDir   string
	coldDir  string
	tenantID string
	runner   *SmartDemotionRunner
	now      time.Time
}

func setupDemotionFixture(t *testing.T, quotaBytes int64, tier string) *demotionFixture {
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
	hotDir := t.TempDir()
	coldDir := t.TempDir()
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("idrive", drivers.NewLocalDriver(hotDir, logger))
	eng.AddDriver("geyser", drivers.NewLocalDriver(coldDir, logger))
	eng.SetPrimary("idrive")

	tenantID := "tenant-" + uuid.New().String()[:8]
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		tenantID, "demotion test", tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier) VALUES ($1,$2,0,$3)`,
		tenantID, quotaBytes, tier)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM smart_demotions WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenant_quotas WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	now := time.Now()
	r := NewSmartDemotionRunner(db, eng, stubFlags{on: map[string]bool{flagSmartDemotion: true}}, logger)
	require.NotNil(t, r)
	r.now = func() time.Time { return now }
	return &demotionFixture{t: t, db: db, eng: eng, hotDir: hotDir, coldDir: coldDir, tenantID: tenantID, runner: r, now: now}
}

// object plants a head-cache row on the hot backend plus a real (small) blob
// in the hot driver dir. size is the BILLED size (selection math), the blob
// content is tiny.
func (f *demotionFixture) object(bucket, key string, size int64, ageDays, idleDays float64, opts ...func(*headRow)) headRow {
	f.t.Helper()
	h := headRow{bucket: bucket, key: key, size: size, etag: fmt.Sprintf("etag-%s-%s", bucket, key), backend: "idrive"}
	for _, o := range opts {
		o(&h)
	}
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, is_chunked, created_at, updated_at, last_accessed)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9)`,
		f.tenantID, bucket, key, size, h.etag, h.backend, h.chunked,
		f.now.Add(-time.Duration(ageDays*24)*time.Hour), f.now.Add(-time.Duration(idleDays*24)*time.Hour))
	require.NoError(f.t, err)
	container := f.tenantID + "_" + bucket
	dir := f.hotDir
	if h.backend == "geyser" {
		dir = f.coldDir
	}
	require.NoError(f.t, os.MkdirAll(filepath.Join(dir, container), 0o755))
	require.NoError(f.t, os.WriteFile(filepath.Join(dir, container, key), []byte("blob:"+key), 0o600))
	return h
}

type headRow struct {
	bucket, key, etag, backend string
	size                       int64
	chunked                    bool
}

func chunked() func(*headRow)           { return func(h *headRow) { h.chunked = true } }
func onBackend(b string) func(*headRow) { return func(h *headRow) { h.backend = b } }

func (f *demotionFixture) backendOf(bucket, key string) string {
	var b string
	require.NoError(f.t, f.db.QueryRow(`SELECT COALESCE(backend_name,'') FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key).Scan(&b))
	return b
}

func (f *demotionFixture) hotExists(bucket, key string) bool {
	_, err := os.Stat(filepath.Join(f.hotDir, f.tenantID+"_"+bucket, key))
	return err == nil
}

func (f *demotionFixture) coldExists(bucket, key string) bool {
	_, err := os.Stat(filepath.Join(f.coldDir, f.tenantID+"_"+bucket, key))
	return err == nil
}

func (f *demotionFixture) ledger(bucket, key string) (reason string, hotDeleted bool, outcome string, ok bool) {
	var deletedAt sql.NullTime
	err := f.db.QueryRow(`SELECT reason, hot_deleted_at, hot_outcome FROM smart_demotions WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key).Scan(&reason, &deletedAt, &outcome)
	if err != nil {
		return "", false, "", false
	}
	return reason, deletedAt.Valid, outcome, true
}

func TestSmartDemotion_OverBudgetDemotesLRUUntilUnderBudget(t *testing.T) {
	// Arrange: 10 TB quota → 1.5 TB hot budget; 2 TB hot, nothing idle.
	f := setupDemotionFixture(t, 10*tb, "standard")
	f.object("b", "newest", 512*gb, 10, 1)
	f.object("b", "lru", 512*gb, 10, 9) // least recently used
	f.object("b", "mid1", 512*gb, 10, 5)
	f.object("b", "mid2", 512*gb, 10, 3)

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert: exactly one demotion (2.0 − 0.5 = 1.5 ≤ 1.5), the LRU one.
	require.NoError(t, err)
	assert.Equal(t, 1, res.Demoted, "%+v", res)
	assert.Equal(t, 512*gb, res.BytesDemoted)
	assert.Equal(t, "geyser", f.backendOf("b", "lru"))
	assert.Equal(t, "idrive", f.backendOf("b", "mid1"))
	assert.True(t, f.coldExists("b", "lru"), "blob copied to cold")
	assert.True(t, f.hotExists("b", "lru"), "hot copy kept until the grace period elapses")
	reason, deleted, _, ok := f.ledger("b", "lru")
	require.True(t, ok, "ledger row written")
	assert.Equal(t, "over_budget", reason)
	assert.False(t, deleted)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, int64(1536*gb), res.Tenants[0].HotBudgetBytes)
	assert.Equal(t, int64(2048*gb), res.Tenants[0].HotBytesBefore)
	assert.Equal(t, int64(1536*gb), res.Tenants[0].HotBytesAfter)
}

func TestSmartDemotion_IdleObjectDemotedEvenUnderBudget(t *testing.T) {
	f := setupDemotionFixture(t, 10*tb, "standard")
	f.object("b", "cold-idle", 100, 30, 20) // idle 20d ≥ 14d
	f.object("b", "warm", 100, 30, 2)

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 1, res.Demoted)
	assert.Equal(t, "geyser", f.backendOf("b", "cold-idle"))
	assert.Equal(t, "idrive", f.backendOf("b", "warm"))
	reason, _, _, _ := f.ledger("b", "cold-idle")
	assert.Equal(t, "idle", reason)
}

func TestSmartDemotion_NeverTouchesYoungObjects(t *testing.T) {
	// Way over budget, but everything is < 3 days old (upload settling).
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "fresh1", 512*gb, 1, 0.5)
	f.object("b", "fresh2", 512*gb, 2, 1)

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 0, res.Demoted)
	assert.Equal(t, "idrive", f.backendOf("b", "fresh1"))
}

func TestSmartDemotion_SkipsChunkedVersionedAndPinnedBuckets(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "chunky", 512*gb, 30, 20, chunked())
	f.object("vers", "v1", 512*gb, 30, 20)
	f.object("pinned", "p1", 512*gb, 30, 20)
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, versioning_status) VALUES ($1,'vers','Enabled')`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, tier_preference) VALUES ($1,'pinned','performance')`, f.tenantID)
	require.NoError(t, err)

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 0, res.Demoted, "%+v", res)
	assert.Equal(t, "idrive", f.backendOf("b", "chunky"))
	assert.Equal(t, "idrive", f.backendOf("vers", "v1"))
	assert.Equal(t, "idrive", f.backendOf("pinned", "p1"))
}

func TestSmartDemotion_FlagOffIsNoop_TierFilter(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "idle", 100, 30, 20)

	// tenant-level off
	f.runner.flags = stubFlags{on: map[string]bool{flagSmartDemotion: true, flagSmartDemotion + "/" + f.tenantID: false}}
	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Demoted)
	assert.Equal(t, "idrive", f.backendOf("b", "idle"))

	// flag on but tenant is on a tier the job does not manage
	f.runner.flags = stubFlags{on: map[string]bool{flagSmartDemotion: true}}
	_, err = f.db.Exec(`UPDATE tenant_quotas SET tier='vault' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	res, err = f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.TenantsScanned)
	assert.Equal(t, "idrive", f.backendOf("b", "idle"))
}

func TestSmartDemotion_DryRunReportsWithoutMoving(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "idle", 100, 30, 20)

	res, err := f.runner.RunOnce(context.Background(), true)

	require.NoError(t, err)
	assert.True(t, res.DryRun)
	assert.Equal(t, 1, res.Candidates)
	assert.Equal(t, 0, res.Demoted)
	assert.Equal(t, "idrive", f.backendOf("b", "idle"))
	assert.False(t, f.coldExists("b", "idle"))
	_, _, _, ok := f.ledger("b", "idle")
	assert.False(t, ok, "dry run writes no ledger rows")
}

func TestSmartDemotion_ObjectChangedDuringCopyIsSkipped(t *testing.T) {
	// The etag-guarded routing flip is the TOCTOU defence: an overwrite that
	// lands between candidate selection and the flip must leave the object
	// on the hot backend and remove the cold copy we wrote.
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "racy", 100, 30, 20)
	f.runner.beforeFlip = func(bucket, key string) {
		_, err := f.db.Exec(`UPDATE object_head_cache SET etag='etag-new', updated_at=NOW() WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key)
		require.NoError(t, err)
	}

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 0, res.Demoted)
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, "idrive", f.backendOf("b", "racy"))
	assert.False(t, f.coldExists("b", "racy"), "cold copy of stale bytes removed")
	assert.True(t, f.hotExists("b", "racy"))
	_, _, _, ok := f.ledger("b", "racy")
	assert.False(t, ok)
}

func TestSmartDemotion_ReclaimsHotCopyAfterGrace(t *testing.T) {
	f := setupDemotionFixture(t, 10*tb, "standard")
	// Already demoted 2 days ago: routing says geyser, hot blob still there.
	h := f.object("b", "done", 100, 30, 20, onBackend("geyser"))
	require.NoError(t, os.MkdirAll(filepath.Join(f.hotDir, f.tenantID+"_b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.hotDir, f.tenantID+"_b", "done"), []byte("stale hot"), 0o600))
	_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
		VALUES ($1,'b','done',$2,100,'idrive','geyser','idle',$3)`, f.tenantID, h.etag, f.now.Add(-48*time.Hour))
	require.NoError(t, err)
	// Demoted 2 days ago but overwritten since (etag differs): must NOT delete.
	h2 := f.object("b", "rewritten", 100, 30, 1, onBackend("idrive")) // recently read: not a demotion candidate
	_, err = f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
		VALUES ($1,'b','rewritten','old-etag',100,'idrive','geyser','idle',$2)`, f.tenantID, f.now.Add(-48*time.Hour))
	require.NoError(t, err)
	_ = h2
	// Demoted 1 hour ago: inside grace, untouched.
	h3 := f.object("b", "recent", 100, 30, 20, onBackend("geyser"))
	require.NoError(t, os.WriteFile(filepath.Join(f.hotDir, f.tenantID+"_b", "recent"), []byte("hot"), 0o600))
	_, err = f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
		VALUES ($1,'b','recent',$2,100,'idrive','geyser','idle',$3)`, f.tenantID, h3.etag, f.now.Add(-1*time.Hour))
	require.NoError(t, err)

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 1, res.HotReclaimed, "%+v", res)
	assert.False(t, f.hotExists("b", "done"), "hot copy reclaimed")
	_, deleted, outcome, _ := f.ledger("b", "done")
	assert.True(t, deleted)
	assert.Equal(t, "deleted", outcome)

	assert.True(t, f.hotExists("b", "rewritten"), "overwritten object's hot copy is live data — never deleted")
	_, deleted, outcome, _ = f.ledger("b", "rewritten")
	assert.True(t, deleted, "ledger closed out")
	assert.Equal(t, "kept_changed", outcome)

	assert.True(t, f.hotExists("b", "recent"), "inside grace window")
	_, deleted, _, _ = f.ledger("b", "recent")
	assert.False(t, deleted)
}

func TestSmartDemotion_ReclaimWhenObjectDeleted(t *testing.T) {
	// The object was DELETEd after demotion (head row gone): the cold copy
	// went with the engine delete, the orphaned hot copy is ours to remove.
	f := setupDemotionFixture(t, 10*tb, "standard")
	require.NoError(t, os.MkdirAll(filepath.Join(f.hotDir, f.tenantID+"_b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.hotDir, f.tenantID+"_b", "gone"), []byte("orphan"), 0o600))
	_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
		VALUES ($1,'b','gone','e',100,'idrive','geyser','idle',$2)`, f.tenantID, f.now.Add(-48*time.Hour))
	require.NoError(t, err)

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 1, res.HotReclaimed)
	assert.False(t, f.hotExists("b", "gone"))
	_, deleted, outcome, _ := f.ledger("b", "gone")
	assert.True(t, deleted)
	assert.Equal(t, "object_gone", outcome)
}

func TestSmartDemotion_RateLimitPerRun(t *testing.T) {
	f := setupDemotionFixture(t, 10*tb, "standard")
	for i := 0; i < 5; i++ {
		f.object("b", fmt.Sprintf("idle%d", i), 100*gb, 30, 20)
	}
	f.runner.MaxBytesPerRun = 250 * gb

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 3, res.Demoted, "stops once the per-run byte budget is exceeded")
}

func TestSmartDemotion_AdminTriggerEndpoint(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "idle", 100, 30, 20)
	s := &Server{logger: zap.NewNop(), smartDemotion: f.runner}

	rr := doJSON(t, s.handleSmartDemotionTrigger, "POST", "/api/v1/admin/smart-demotion?dry_run=true")
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"dry_run":true`)
	assert.Contains(t, rr.Body.String(), `"candidates":1`)
	assert.Equal(t, "idrive", f.backendOf("b", "idle"))

	rr = doJSON(t, s.handleSmartDemotionTrigger, "POST", "/api/v1/admin/smart-demotion")
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"demoted":1`)
	assert.Equal(t, "geyser", f.backendOf("b", "idle"))

	s.smartDemotion = nil
	rr = doJSON(t, s.handleSmartDemotionTrigger, "POST", "/api/v1/admin/smart-demotion")
	assert.Equal(t, 503, rr.Code)
}

func doJSON(t *testing.T, h http.HandlerFunc, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(method, target, nil))
	return rr
}
