package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- test ETags
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// Hydrate-before-serve of large Sia objects (sia_promotion.go): a GET of a
// large object recorded on `sia` answers 503 SlowDown + Retry-After while
// one promotion copies it into the primary, then is served from the copy.

type siaFixture struct {
	t        *testing.T
	db       *sql.DB
	eng      *engine.CoreEngine
	hotDir   string
	siaDir   string
	tenantID string
	p        *SiaPromoter
	adapter  *S3ToEngine
	tn       *tenant.Tenant
}

func setupSiaFixture(t *testing.T, cfg siaPromoteConfig) *siaFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()
	hotDir, siaDir := t.TempDir(), t.TempDir()
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("idrive", drivers.NewLocalDriver(hotDir, logger))
	eng.AddDriver("sia", drivers.NewLocalDriver(siaDir, logger))
	eng.SetPrimary("idrive")

	tenantID := "tenant-sia-" + uuid.New().String()[:8]
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		tenantID, "sia promotion test", tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM sia_promotions WHERE tenant_id = $1`,
			`DELETE FROM retired_blobs WHERE tenant_id = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM object_versions WHERE tenant_id = $1`,
			`DELETE FROM object_locks WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, tenantID)
		}
	})

	p := NewSiaPromoter(db, eng, cfg, logger)
	require.NotNil(t, p)
	p.sync = true
	p.onlyTenant = tenantID
	p.JobName = "sia_promotion_test_" + tenantID
	a := NewS3ToEngine(eng, db, logger)
	a.siaPromoter = p
	return &siaFixture{t: t, db: db, eng: eng, hotDir: hotDir, siaDir: siaDir, tenantID: tenantID, p: p, adapter: a,
		tn: &tenant.Tenant{ID: tenantID, Namespace: "tenant/" + tenantID + "/"}}
}

func siaTestConfig() siaPromoteConfig {
	return siaPromoteConfig{MinBytes: 1024, Concurrency: 2, TTL: 7 * 24 * time.Hour}
}

// object plants a head row on `sia` at a fresh address and its blob.
func (f *siaFixture) object(bucket, key string, body []byte) string {
	f.t.Helper()
	addr, err := newBlobAddress(key)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, storage_key, is_chunked, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,'sia',$6,FALSE,NOW(),NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET size_bytes = EXCLUDED.size_bytes, etag = EXCLUDED.etag,
		  backend_name = EXCLUDED.backend_name, storage_key = EXCLUDED.storage_key`,
		f.tenantID, bucket, key, len(body), md5hex(body), addr)
	require.NoError(f.t, err)
	dir := filepath.Join(f.siaDir, f.tenantID+"_"+bucket)
	require.NoError(f.t, os.MkdirAll(dir, 0o755))
	require.NoError(f.t, os.WriteFile(filepath.Join(dir, addr), body, 0o600))
	return addr
}

func (f *siaFixture) get(bucket, key, rangeHeader string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+bucket+"/"+key, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tn))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, bucket, key)
	return w
}

type siaRow struct {
	state, promotedKey, sourceKey, backend string
}

func (f *siaFixture) rows(bucket, key string) []siaRow {
	f.t.Helper()
	rs, err := f.db.Query(`SELECT state, promoted_key, source_key, promoted_backend FROM sia_promotions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, bucket, key)
	require.NoError(f.t, err)
	defer func() { _ = rs.Close() }()
	var out []siaRow
	for rs.Next() {
		var r siaRow
		require.NoError(f.t, rs.Scan(&r.state, &r.promotedKey, &r.sourceKey, &r.backend))
		out = append(out, r)
	}
	require.NoError(f.t, rs.Err())
	return out
}

func (f *siaFixture) hotCopy(bucket, addr string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(f.hotDir, f.tenantID+"_"+bucket, addr))
	return b, err == nil
}

func (f *siaFixture) retired(addr string) bool {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2 AND reason = $3`,
		f.tenantID, addr, retiredSiaPromotion).Scan(&n))
	return n == 1
}

func siaBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

func TestSiaPromotion_MissAnswers503ThenServesFromThePrimary(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	data := siaBody(4096)
	f.object("b", "big", data)
	started := promtest.ToFloat64(siaPromotions.WithLabelValues(siaOutcomeStarted))

	w := f.get("b", "big", "")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "SlowDown")
	assert.Equal(t, "60", w.Header().Get("Retry-After"), "no read budget set: 60 s")
	assert.Equal(t, started+1, promtest.ToFloat64(siaPromotions.WithLabelValues(siaOutcomeStarted)))
	rows := f.rows("b", "big")
	require.Len(t, rows, 1)
	assert.Equal(t, "ready", rows[0].state)
	assert.Equal(t, "idrive", rows[0].backend)
	got, ok := f.hotCopy("b", rows[0].promotedKey)
	require.True(t, ok, "the copy is on the primary at its own address")
	assert.Equal(t, data, got)

	// The sia blob is now unreadable: the second read must come from the copy.
	require.NoError(t, os.Remove(filepath.Join(f.siaDir, f.tenantID+"_b", rows[0].sourceKey)))
	w = f.get("b", "big", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, data, w.Body.Bytes(), "byte-exact from the promoted copy")
	assert.Equal(t, "promoted", w.Header().Get("x-vaultaire-served-from"))
	assert.Equal(t, `"`+md5hex(data)+`"`, w.Header().Get("ETag"))
	assert.Equal(t, "STANDARD", w.Header().Get("x-amz-storage-class"))

	// A small range of a promoted object is served from the copy too.
	w = f.get("b", "big", "bytes=10-19")
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.Equal(t, data[10:20], w.Body.Bytes())

	var route string
	require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'big'`, f.tenantID).Scan(&route))
	assert.Equal(t, "sia", route, "the head row is never re-pointed: the copy is a cache")
}

func TestSiaPromotion_RetryAfterFollowsTheReadBudget(t *testing.T) {
	cfg := siaTestConfig()
	cfg.ReadBudget = 10
	f := setupSiaFixture(t, cfg)
	assert.Equal(t, 410, f.p.retryAfter(siaObject{size: 4096}), "4096 B at 10 B/s")
	assert.Equal(t, siaRetryAfterMin, f.p.retryAfter(siaObject{size: 10}), "never under the floor")
	assert.Equal(t, siaRetryAfterMax, f.p.retryAfter(siaObject{size: 1 << 30}), "never over an hour")
}

func TestSiaPromotion_ConcurrentReadsStartOnePromotion(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.p.sync = false
	data := siaBody(8192)
	f.object("b", "hot", data)
	release := make(chan struct{})
	f.p.beforeCommit = func(string, string) { <-release }
	started := promtest.ToFloat64(siaPromotions.WithLabelValues(siaOutcomeStarted))

	var wg sync.WaitGroup
	codes := make([]int, 12)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.get("b", "hot", "").Code
		}(i)
	}
	wg.Wait()
	close(release)
	f.p.Wait()

	for _, c := range codes {
		assert.Equal(t, http.StatusServiceUnavailable, c)
	}
	assert.Equal(t, started+1, promtest.ToFloat64(siaPromotions.WithLabelValues(siaOutcomeStarted)), "one promotion for the object")
	rows := f.rows("b", "hot")
	require.Len(t, rows, 1)
	assert.Equal(t, "ready", rows[0].state)
	w := f.get("b", "hot", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, data, w.Body.Bytes())
}

func TestSiaPromotion_OverwriteDuringPromotionRetiresTheCopy(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "k", siaBody(4096))
	newer := siaBody(5000)
	var newAddr string
	f.p.beforeCommit = func(bucket, key string) {
		newAddr = f.object(bucket, key, newer) // a PUT committed while the copy ran
	}

	w := f.get("b", "k", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	assert.Empty(t, f.rows("b", "k"), "the copy of the old object is not recorded")
	var queued int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = $2`, f.tenantID, retiredSiaPromotion).Scan(&queued))
	assert.Equal(t, 1, queued, "the stale copy is queued for the collector")

	// The next read promotes the NEW object and serves its bytes, never the old.
	f.p.beforeCommit = nil
	require.Equal(t, http.StatusServiceUnavailable, f.get("b", "k", "").Code)
	rows := f.rows("b", "k")
	require.Len(t, rows, 1)
	assert.Equal(t, newAddr, rows[0].sourceKey)
	w = f.get("b", "k", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, newer, w.Body.Bytes())
}

func TestSiaPromotion_WriteFlipRetiresAPromotedCopy(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "k", siaBody(4096))
	require.Equal(t, http.StatusServiceUnavailable, f.get("b", "k", "").Code)
	rows := f.rows("b", "k")
	require.Len(t, rows, 1)

	// An overwrite through the one flip (commitBlobWrite): the copy goes with it.
	newAddr, err := newBlobAddress("k")
	require.NoError(t, err)
	_, err = commitBlobWrite(context.Background(), f.db, blobWrite{TenantID: f.tenantID, Bucket: "b", Key: "k",
		Upsert: func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE object_head_cache SET storage_key = $4, etag = 'new-etag', size_bytes = 3
				WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, "b", "k", newAddr)
			return err
		}})
	require.NoError(t, err)

	assert.Empty(t, f.rows("b", "k"))
	assert.True(t, f.retired(rows[0].promotedKey))
}

func TestSiaPromotion_DeleteRetiresAPromotedCopy(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "gone", siaBody(4096))
	require.Equal(t, http.StatusServiceUnavailable, f.get("b", "gone", "").Code)
	rows := f.rows("b", "gone")
	require.Len(t, rows, 1)

	req := httptest.NewRequest(http.MethodDelete, "/b/gone", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tn))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "b", "gone")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	assert.Empty(t, f.rows("b", "gone"))
	assert.True(t, f.retired(rows[0].promotedKey))
	w = f.get("b", "gone", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "a deleted object is never served from its copy")
}

func TestSiaPromotion_JobRetiresStaleAndIdleCopies(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "idle", siaBody(4096))
	f.object("b", "stale", siaBody(4096))
	f.object("b", "fresh", siaBody(4096))
	for _, k := range []string{"idle", "stale", "fresh"} {
		require.Equal(t, http.StatusServiceUnavailable, f.get("b", k, "").Code)
	}
	idle, stale := f.rows("b", "idle")[0], f.rows("b", "stale")[0]
	_, err := f.db.Exec(`UPDATE sia_promotions SET last_accessed = NOW() - INTERVAL '8 days' WHERE tenant_id = $1 AND object_key = 'idle'`, f.tenantID)
	require.NoError(t, err)
	// A head row changed behind the hooks' back (a path that retired nothing).
	_, err = f.db.Exec(`UPDATE object_head_cache SET etag = 'other' WHERE tenant_id = $1 AND object_key = 'stale'`, f.tenantID)
	require.NoError(t, err)

	res, err := f.p.RunOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, res.Reclaimed, "%+v", res)
	assert.Equal(t, 1, res.Retired, "%+v", res)
	assert.Empty(t, f.rows("b", "idle"))
	assert.Empty(t, f.rows("b", "stale"))
	require.Len(t, f.rows("b", "fresh"), 1, "a copy read within the TTL stays")
	assert.True(t, f.retired(idle.promotedKey))
	assert.True(t, f.retired(stale.promotedKey))

	// The collector deletes the copy from the primary after the grace.
	c := NewRetiredBlobCollector(f.db, f.eng, nil, zap.NewNop())
	c.onlyTenant = f.tenantID
	c.now = func() time.Time { return time.Now().Add(retiredBlobGrace + time.Hour) }
	_, ok := f.hotCopy("b", idle.promotedKey)
	require.True(t, ok)
	_, err = c.RunOnce(context.Background())
	require.NoError(t, err)
	_, ok = f.hotCopy("b", idle.promotedKey)
	assert.False(t, ok, "the idle copy is deleted from the primary")
	_, ok = f.hotCopy("b", f.rows("b", "fresh")[0].promotedKey)
	assert.True(t, ok, "a live copy is never collected")
}

func TestSiaPromotion_AbandonedPromotionIsTakenOver(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "k", siaBody(4096))
	o := siaObject{tenantID: f.tenantID, bucket: "b", key: "k"}
	require.NoError(t, f.db.QueryRow(`SELECT etag, storage_key, size_bytes FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'k'`, f.tenantID).
		Scan(&o.etag, &o.storageKey, &o.size))
	addr, claimed, err := f.p.claim(context.Background(), o, "idrive")
	require.NoError(t, err)
	require.True(t, claimed)
	_, claimed, err = f.p.claim(context.Background(), o, "idrive")
	require.NoError(t, err)
	assert.False(t, claimed, "a running promotion is not claimed twice")

	// The process that claimed it stopped: past its timeout the next read takes over.
	_, err = f.db.Exec(`UPDATE sia_promotions SET requested_at = NOW() - INTERVAL '1 day' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, f.get("b", "k", "").Code)
	rows := f.rows("b", "k")
	require.Len(t, rows, 1)
	assert.Equal(t, "ready", rows[0].state)
	assert.NotEqual(t, addr, rows[0].promotedKey)
	assert.True(t, f.retired(addr), "the abandoned address is queued for the collector")
}

func TestSiaPromotion_CorruptCopyIsRetiredNeverServed(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "k", siaBody(4096))
	_, err := f.db.Exec(`UPDATE object_head_cache SET etag = $2 WHERE tenant_id = $1 AND object_key = 'k'`, f.tenantID, md5hex([]byte("not these bytes")))
	require.NoError(t, err)
	failed := promtest.ToFloat64(siaPromotions.WithLabelValues(siaOutcomeFailed))

	require.Equal(t, http.StatusServiceUnavailable, f.get("b", "k", "").Code)

	assert.Equal(t, failed+1, promtest.ToFloat64(siaPromotions.WithLabelValues(siaOutcomeFailed)))
	assert.Empty(t, f.rows("b", "k"))
	var queued int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = $2`, f.tenantID, retiredSiaPromotion).Scan(&queued))
	assert.Equal(t, 1, queued, "the bad copy is retired")
}

func TestSiaPromotion_MissingCopyIsRetiredAndReadWaits(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	f.object("b", "k", siaBody(4096))
	require.Equal(t, http.StatusServiceUnavailable, f.get("b", "k", "").Code)
	row := f.rows("b", "k")[0]
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", row.promotedKey)))

	w := f.get("b", "k", "")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "never a 404 for an object that is on sia")
	assert.True(t, f.retired(row.promotedKey))
}

func TestSiaPromotion_SmallReadsStreamFromSia(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	small := siaBody(100)
	f.object("b", "small", small)
	big := siaBody(4096)
	f.object("b", "big", big)

	w := f.get("b", "small", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, small, w.Body.Bytes())
	assert.Empty(t, f.rows("b", "small"))

	w = f.get("b", "big", "bytes=0-99")
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.Equal(t, big[:100], w.Body.Bytes())
	assert.Empty(t, f.rows("b", "big"), "a small range of an unpromoted object starts nothing")
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))
}

func TestSiaPromotion_OffStreamsFromSia(t *testing.T) {
	cfg := siaTestConfig()
	cfg.MinBytes = 0
	f := setupSiaFixture(t, cfg)
	big := siaBody(4096)
	f.object("b", "big", big)

	w := f.get("b", "big", "")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(big, w.Body.Bytes()))
	assert.Empty(t, f.rows("b", "big"))
}

func TestSiaPromotion_PrimaryUnaskableStreamsFromSia(t *testing.T) {
	f := setupSiaFixture(t, siaTestConfig())
	big := siaBody(4096)
	f.object("b", "big", big)
	f.eng.SetPrimary("sia") // a lab whose primary is sia: nothing to promote into

	w := f.get("b", "big", "")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, big, w.Body.Bytes())
	assert.Empty(t, f.rows("b", "big"))
}

func TestSiaPromoteConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c := siaPromoteConfigFromEnv(env(nil), zap.NewNop())
	assert.Equal(t, siaPromoteConfig{MinBytes: 64 << 20, Concurrency: 2, TTL: 7 * 24 * time.Hour}, c)

	c = siaPromoteConfigFromEnv(env(map[string]string{"SIA_PROMOTE_MIN_BYTES": "256MiB", "SIA_PROMOTE_CONCURRENCY": "4",
		"SIA_PROMOTE_TTL": "48h", "SIA_READ_BUDGET": "25MiB"}), zap.NewNop())
	assert.Equal(t, siaPromoteConfig{MinBytes: 256 << 20, Concurrency: 4, TTL: 48 * time.Hour, ReadBudget: 25 << 20}, c)

	for _, off := range []string{"off", "OFF", "0"} {
		assert.Equal(t, int64(0), siaPromoteConfigFromEnv(env(map[string]string{"SIA_PROMOTE_MIN_BYTES": off}), zap.NewNop()).MinBytes)
	}
	// Rejected values keep the defaults.
	c = siaPromoteConfigFromEnv(env(map[string]string{"SIA_PROMOTE_MIN_BYTES": "12", "SIA_PROMOTE_CONCURRENCY": "0",
		"SIA_PROMOTE_TTL": "5m", "SIA_READ_BUDGET": "lots"}), zap.NewNop())
	assert.Equal(t, siaPromoteConfig{MinBytes: 64 << 20, Concurrency: 2, TTL: 7 * 24 * time.Hour}, c)
}

func TestSiaPromoter_OnlyWithTheSiaBackendAndItsJob(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	noSia := engine.NewEngine(nil, zap.NewNop(), nil)
	noSia.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	assert.Nil(t, NewSiaPromoter(db, noSia, siaTestConfig(), zap.NewNop()), "no sia backend, no promoter")

	f := setupSiaFixture(t, siaTestConfig())
	sp := f.p.spec()
	assert.Equal(t, "every 10m0s", sp.schedule())
	assert.Greater(t, sp.MaxRunTime, time.Duration(0))
	assert.Greater(t, sp.BootDelay, time.Duration(0))
}

func TestSiaPromotion_HashingCounter(t *testing.T) {
	hc := &hashingCounter{r: bytes.NewReader([]byte("abc")), h: md5.New()} // #nosec G401 -- test
	b, err := io.ReadAll(hc)
	require.NoError(t, err)
	assert.Equal(t, "abc", string(b))
	assert.Equal(t, int64(3), hc.n)
	assert.Equal(t, md5hex([]byte("abc")), hex.EncodeToString(hc.h.Sum(nil)))
}
