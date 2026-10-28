package api

// WP-R8-2 B.2 (R8-09b, RP8-11): a chunk blob at the one address that no
// index row names — a PUT the deadline cut after the backend stored it, a
// writer that died between its PUT and its row, a row dedup GC deleted
// whose blob delete failed — was found by nothing and paid for ever. The
// chunk_orphans job lists `_global/_chunks/` on every backend that holds
// chunks, from a cursor, and erases what no row names once it is older
// than the grace — re-checked under the chunk's lock.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/engine"
)

// ListPage makes the test double an engine.PageLister: the files under
// t-<tenant>/<container>/<prefix>, in key order, after startAfter; the
// file's modification time is its LastModified.
func (d *fixedBucketDriver) ListPage(ctx context.Context, container, prefix, startAfter string, limit int) ([]engine.ListedObject, bool, error) {
	tn, err := d.tenant(ctx)
	if err != nil {
		return nil, false, err
	}
	base := filepath.Join(d.dir, "t-"+tn, container)
	var all []engine.ListedObject
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, prefix) && rel > startAfter {
			all = append(all, engine.ListedObject{Artifact: rel, Size: info.Size(), LastModified: info.ModTime()})
		}
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].Artifact < all[j].Artifact })
	if limit > 0 && len(all) > limit {
		return all[:limit], true, nil
	}
	return all, false, nil
}

type orphanFixture struct {
	t       *testing.T
	f       *pendingFixture
	backend string
	drv     *fixedBucketDriver
	eng     *engine.CoreEngine
	sweeper *ChunkOrphanSweeper
	scope   string
}

func setupOrphanFixture(t *testing.T) *orphanFixture {
	t.Helper()
	f := setupPendingFixture(t)
	close(f.gated.gate)
	o := &orphanFixture{t: t, f: f, backend: "co-" + uuid.NewString()[:8], drv: &fixedBucketDriver{dir: t.TempDir()},
		scope: "co-" + uuid.NewString()[:8]}
	o.eng = engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: o.backend})
	require.NoError(t, o.eng.AddDriver(o.backend, o.drv))
	o.sweeper = NewChunkOrphanSweeper(f.db, o.eng, zap.NewNop())
	o.sweeper.Grace = time.Hour
	o.sweeper.onlyBackends = []string{o.backend}
	o.sweeper.JobName = "chunk_orphans_test_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE backend_id = $1`, o.backend)
		_, _ = f.db.Exec(`DELETE FROM retired_chunk_blobs WHERE backend_id = $1`, o.backend)
		_, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, o.sweeper.JobName)
	})
	return o
}

func newHash(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// blob writes a chunk blob at the one address on the fixture's backend,
// key `_chunks/<hash>` (or `_chunks/<scope>/<hash>`), aged by age.
func (o *orphanFixture) blob(key string, age time.Duration) {
	o.t.Helper()
	require.NoError(o.t, o.drv.Put(engine.ChunkContext(context.Background()), chunkContainer, key, bytes.NewReader([]byte("chunk bytes of "+key))))
	p := o.drv.path(engine.ChunkAddressTenant, chunkContainer, key)
	at := time.Now().Add(-age)
	require.NoError(o.t, os.Chtimes(p, at, at))
}

func (o *orphanFixture) present(key string) bool {
	_, err := os.Stat(o.drv.path(engine.ChunkAddressTenant, chunkContainer, key))
	return err == nil
}

func (o *orphanFixture) row(scope, hash, key, state string, pendingAge time.Duration) {
	o.t.Helper()
	var since any
	if state == "pending" {
		since = time.Now().Add(-pendingAge)
	}
	_, err := o.f.db.Exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count, state, pending_since)
		VALUES ($1, $2, $3, $4, 10, $5, $6, $7)`, scope, hash, o.backend, key, map[string]int{"ready": 1, "pending": 0}[state], state, since)
	require.NoError(o.t, err)
}

func (o *orphanFixture) rowExists(scope, hash string) bool {
	var n int
	require.NoError(o.t, o.f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash).Scan(&n))
	return n == 1
}

func TestChunkOrphans_AnOldBlobNoRowNamesIsErased_AYoungOneIsLeft(t *testing.T) {
	// Arrange
	o := setupOrphanFixture(t)
	oldKey, youngKey := "_chunks/"+newHash(t), "_chunks/"+o.scope+"/"+newHash(t)
	o.blob(oldKey, 2*time.Hour)
	o.blob(youngKey, 10*time.Minute)

	// Act
	res, err := o.sweeper.Run(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.False(t, o.present(oldKey), "the orphan past the grace is erased")
	assert.True(t, o.present(youngKey), "a blob younger than the grace is left (its row may be on its way)")
	b := res.Backends[o.backend]
	require.NotNil(t, b)
	assert.Equal(t, 1, b.Erased)
	assert.Equal(t, 1, b.Young)
	assert.Equal(t, int64(len("chunk bytes of "+oldKey)), b.BytesErased)
	assert.Equal(t, 1, res.OrphansErased)
	assert.True(t, b.Complete)
}

func TestChunkOrphans_ABlobARowNamesIsNeverTouched(t *testing.T) {
	// Arrange: a ready row, a young pending row, an abandoned pending row.
	o := setupOrphanFixture(t)
	readyH, youngH, deadH := newHash(t), newHash(t), newHash(t)
	readyK, youngK, deadK := "_chunks/"+o.scope+"/"+readyH, "_chunks/"+o.scope+"/"+youngH, "_chunks/"+o.scope+"/"+deadH
	for _, k := range []string{readyK, youngK, deadK} {
		o.blob(k, 3*time.Hour)
	}
	o.row(o.scope, readyH, readyK, "ready", 0)
	o.row(o.scope, youngH, youngK, "pending", 5*time.Minute)
	o.row(o.scope, deadH, deadK, "pending", 3*time.Hour)

	// Act
	res, err := o.sweeper.Run(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.True(t, o.present(readyK) && o.rowExists(o.scope, readyH), "a ready row's blob")
	assert.True(t, o.present(youngK) && o.rowExists(o.scope, youngH), "a young claim's blob: its writer is still on it")
	assert.False(t, o.present(deadK), "an abandoned claim's blob goes with it")
	assert.False(t, o.rowExists(o.scope, deadH), "an abandoned claim (older than the grace, no reference) is erased")
	assert.Equal(t, 1, res.Backends[o.backend].PendingErased)
}

func TestChunkOrphans_AnAbandonedClaimWithNoBlobIsErased(t *testing.T) {
	o := setupOrphanFixture(t)
	h := newHash(t)
	o.row(o.scope, h, "_chunks/"+o.scope+"/"+h, "pending", 3*time.Hour)

	res, err := o.sweeper.Run(context.Background(), false)

	require.NoError(t, err)
	assert.False(t, o.rowExists(o.scope, h))
	assert.Equal(t, 1, res.Backends[o.backend].PendingErased)
}

func TestChunkOrphans_ARetiredAddressIsTheCollectorsNotTheSweeps(t *testing.T) {
	o := setupOrphanFixture(t)
	h := newHash(t)
	k := "_chunks/" + o.scope + "/" + h + ".k2"
	o.blob(k, 3*time.Hour)
	_, err := o.f.db.Exec(`INSERT INTO retired_chunk_blobs (backend_id, storage_key, dedup_scope, plaintext_hash, retire_after)
		VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 hour')`, o.backend, k, o.scope, h)
	require.NoError(t, err)

	res, err := o.sweeper.Run(context.Background(), false)

	require.NoError(t, err)
	assert.True(t, o.present(k))
	assert.Equal(t, 1, res.Backends[o.backend].Retired)
}

func TestChunkOrphans_ALegacyAddressIsNeverListed(t *testing.T) {
	// A blob under a tenant's own prefix (written before WP-R8-7) is the
	// chunk move's: the sweep lists the one address only.
	o := setupOrphanFixture(t)
	legacyTenant := "co-legacy-" + uuid.NewString()[:6]
	k := "_chunks/" + newHash(t)
	require.NoError(t, o.drv.Put(engine.LegacyChunkContext(context.Background(), legacyTenant), chunkContainer, k, bytes.NewReader([]byte("old"))))
	p := o.drv.path(legacyTenant, chunkContainer, k)
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(p, old, old))

	res, err := o.sweeper.Run(context.Background(), false)

	require.NoError(t, err)
	_, statErr := os.Stat(p)
	assert.NoError(t, statErr, "the legacy copy is untouched")
	assert.Zero(t, res.Backends[o.backend].Listed)
}

func TestChunkOrphans_AStoreThatLandsBeforeTheEraseKeepsItsBlob(t *testing.T) {
	// Arrange: an orphan the sweep has listed — and, before its erase, a
	// writer of that chunk commits a row naming it.
	o := setupOrphanFixture(t)
	h := newHash(t)
	k := "_chunks/" + o.scope + "/" + h
	o.blob(k, 3*time.Hour)
	o.sweeper.beforeErase = func(c chunkOrphan) {
		if c.key == k {
			o.row(o.scope, h, k, "ready", 0)
		}
	}

	// Act
	res, err := o.sweeper.Run(context.Background(), false)

	// Assert: the re-check under the chunk's lock kept it.
	require.NoError(t, err)
	assert.True(t, o.present(k))
	assert.Equal(t, 1, res.Backends[o.backend].Kept)
	assert.Zero(t, res.Backends[o.backend].Erased)
}

func TestChunkOrphans_ABackendThatCannotBeAskedIsSkippedNotFailed(t *testing.T) {
	o := setupOrphanFixture(t)
	missing := "co-gone-" + uuid.NewString()[:6]
	o.sweeper.onlyBackends = []string{o.backend, missing}
	// Open the breaker of the fixture's backend.
	o.drv.failGet.Store(true)
	for i := 0; i < 5; i++ {
		_, _ = o.eng.GetOn(engine.ChunkContext(context.Background()), o.backend, chunkContainer, "x")
	}
	require.Error(t, o.eng.CanAsk(o.backend))
	k := "_chunks/" + newHash(t)
	o.blob(k, 3*time.Hour)

	res, err := o.sweeper.Run(context.Background(), false)

	require.NoError(t, err, "a backend that cannot be asked is a note, never a failed run")
	assert.True(t, o.present(k))
	assert.Contains(t, res.NotSwept, o.backend)
	assert.Contains(t, res.NotSwept, missing)
}

func TestChunkOrphans_TheListingIsBoundedAndResumesFromItsCursor(t *testing.T) {
	// Arrange: five orphans, a budget of two keys a run.
	o := setupOrphanFixture(t)
	o.sweeper.ListBudget, o.sweeper.PageSize = 2, 2
	var keys []string
	for i := 0; i < 5; i++ {
		k := "_chunks/" + newHash(t)
		keys = append(keys, k)
		o.blob(k, 3*time.Hour)
	}
	sort.Strings(keys)
	record := func(res ChunkOrphanResult) {
		raw, err := json.Marshal(res)
		require.NoError(t, err)
		_, err = o.f.db.Exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
			ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, o.sweeper.JobName, raw)
		require.NoError(t, err)
	}

	// Act: three runs, the result recorded as the scheduler records it.
	var erased []int
	for run := 0; run < 3; run++ {
		res, err := o.sweeper.Run(context.Background(), false)
		require.NoError(t, err)
		erased = append(erased, res.Backends[o.backend].Erased)
		record(res)
	}

	// Assert
	assert.Equal(t, []int{2, 2, 1}, erased, "each run takes up its predecessor's cursor")
	for _, k := range keys {
		assert.False(t, o.present(k), k)
	}
	var raw []byte
	require.NoError(t, o.f.db.QueryRow(`SELECT result FROM job_runs WHERE job = $1`, o.sweeper.JobName).Scan(&raw))
	var last ChunkOrphanResult
	require.NoError(t, json.Unmarshal(raw, &last))
	assert.Empty(t, last.Cursors[o.backend], "a listing that reached its end starts over")
}

func TestChunkOrphans_ADryRunReportsAndDeletesNothing(t *testing.T) {
	o := setupOrphanFixture(t)
	h := newHash(t)
	k := "_chunks/" + h
	o.blob(k, 3*time.Hour)
	dead := newHash(t)
	o.row(o.scope, dead, "_chunks/"+o.scope+"/"+dead, "pending", 3*time.Hour)

	res, err := o.sweeper.Run(context.Background(), true)

	require.NoError(t, err)
	assert.True(t, o.present(k))
	assert.True(t, o.rowExists(o.scope, dead))
	b := res.Backends[o.backend]
	assert.Equal(t, 1, b.Found)
	assert.Equal(t, 1, b.PendingFound)
	require.Len(t, b.Candidates, 1)
	assert.Equal(t, k, b.Candidates[0].Key)
	assert.Zero(t, b.Erased)
}

func TestChunkOrphanGrace_FromEnv(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	assert.Equal(t, 24*time.Hour, chunkOrphanGraceFromEnv(env(""), zap.NewNop()))
	assert.Equal(t, 6*time.Hour, chunkOrphanGraceFromEnv(env("6h"), zap.NewNop()))
	for _, bad := range []string{"30m", "8d", "200h", "soon", "-1h"} {
		assert.Equal(t, 24*time.Hour, chunkOrphanGraceFromEnv(env(bad), zap.NewNop()), "%q is a Warn and the default", bad)
	}
}

func TestChunkOrphans_TheAdminEndpointOnlyReports(t *testing.T) {
	o := setupOrphanFixture(t)
	k := "_chunks/" + newHash(t)
	o.blob(k, 3*time.Hour)
	srv := &Server{logger: zap.NewNop(), db: o.f.db, chunkOrphans: o.sweeper}

	w := httptest.NewRecorder()
	srv.handleChunkOrphans(w, httptest.NewRequest("POST", "/api/v1/admin/chunk-orphans?dry_run=false", nil))
	assert.Equal(t, 400, w.Code)
	assert.True(t, o.present(k))

	w = httptest.NewRecorder()
	srv.handleChunkOrphans(w, httptest.NewRequest("POST", "/api/v1/admin/chunk-orphans", nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var res ChunkOrphanResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.True(t, res.DryRun)
	require.NotNil(t, res.Backends[o.backend])
	assert.Equal(t, k, res.Backends[o.backend].Candidates[0].Key)
	assert.True(t, o.present(k), "a dry run deletes nothing")
}

func TestChunkOrphans_TheBytesGaugeIsReadFromTheTable(t *testing.T) {
	o := setupOrphanFixture(t)
	_, err := o.f.db.Exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)`, o.sweeper.JobName,
		`{"orphans_found":3,"backends":{"lyve":{"orphan_bytes":4194304,"found":3}}}`)
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	reg.MustRegister(newChunkOrphanCollector(o.sweeper))

	body := scrape(t, reg)

	assert.Contains(t, body, `vaultaire_chunk_orphan_bytes{backend="lyve"} 4.194304e+06`)
}

func TestChunkOrphans_TooManyOrphansOnOneBackendErasesNothing(t *testing.T) {
	// Arrange: a backend where every listed key looks like an orphan — what
	// a key-mapping bug would look like (every live chunk unnamed). The
	// guard is lowered so a small fixture trips it.
	o := setupOrphanFixture(t)
	o.sweeper.guardFloor = 3
	var keys []string
	for i := 0; i < 5; i++ {
		k := "_chunks/" + newHash(t)
		keys = append(keys, k)
		o.blob(k, 3*time.Hour)
	}

	// Act
	res, err := o.sweeper.Run(context.Background(), false)

	// Assert: nothing erased, the backend named with why.
	require.NoError(t, err)
	for _, k := range keys {
		assert.True(t, o.present(k), "the guard erased %s", k)
	}
	b := res.Backends[o.backend]
	assert.Equal(t, 5, b.Found)
	assert.Zero(t, b.Erased)
	assert.True(t, b.Guarded)
	assert.Contains(t, res.NotSwept[o.backend], "guard")
}

// zeroTimeLister is a page lister that returns no modification time (a zero
// LastModified) for every key.
type zeroTimeLister struct{ *fixedBucketDriver }

func (d *zeroTimeLister) ListPage(ctx context.Context, container, prefix, startAfter string, limit int) ([]engine.ListedObject, bool, error) {
	objs, more, err := d.fixedBucketDriver.ListPage(ctx, container, prefix, startAfter, limit)
	for i := range objs {
		objs[i].LastModified = time.Time{}
	}
	return objs, more, err
}

func TestChunkOrphans_AKeyWithNoModificationTimeIsNeverErased(t *testing.T) {
	// Arrange (RP13-2): a lister whose keys carry a zero LastModified — read
	// as infinitely old, every unnamed key was erased at its first sighting.
	o := setupOrphanFixture(t)
	core, logs := observer.New(zapcore.DebugLevel)
	o.sweeper.logger = zap.New(core)
	z := &zeroTimeLister{fixedBucketDriver: o.drv}
	require.NoError(t, o.eng.AddDriver(o.backend, z))
	k1, k2 := "_chunks/"+newHash(t), "_chunks/"+o.scope+"/"+newHash(t)
	o.blob(k1, 3*time.Hour)
	o.blob(k2, 3*time.Hour)

	// Act: a dry run, then a real one.
	dry, err := o.sweeper.Run(context.Background(), true)
	require.NoError(t, err)
	res, err := o.sweeper.Run(context.Background(), false)
	require.NoError(t, err)

	// Assert: nothing erased, both counted and reported with their zero time.
	assert.True(t, o.present(k1))
	assert.True(t, o.present(k2))
	b := res.Backends[o.backend]
	require.NotNil(t, b)
	assert.Equal(t, 2, b.UnknownAge)
	assert.Zero(t, b.Found)
	assert.Zero(t, b.Erased)
	db := dry.Backends[o.backend]
	require.NotNil(t, db)
	assert.Equal(t, 2, db.UnknownAge)
	require.Len(t, db.Candidates, 2)
	for _, c := range db.Candidates {
		assert.True(t, c.LastModified.IsZero())
	}
	warns := logs.FilterMessage("chunk orphans: listed keys carry no modification time — never erased").All()
	require.Len(t, warns, 2, "one Warn per run")
	assert.Equal(t, o.backend, warns[0].ContextMap()["backend"])
}

// RP76-3: every registered mirror leg is walked as the primary is — a
// codec chunk's copy no row names (an erased tenant's, a host's that never
// committed: RB6-2) is erased past the grace; a copy a blob_copies row
// names on that leg is never touched, though no index row names the leg.
func TestChunkOrphans_WalksEveryMirrorLeg(t *testing.T) {
	o := setupOrphanFixture(t)
	tenant := "co-" + uuid.NewString()[:8]
	named, unnamed := "_chunks/"+tenant+"/"+newHash(t)+".k1", "_chunks/"+tenant+"/"+newHash(t)+".k1"
	o.blob(named, 3*time.Hour)
	o.blob(unnamed, 3*time.Hour)
	_, err := o.f.db.Exec(`INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, state, size)
		VALUES ($1, '', '', $2, 'idrive', $3, 'mirror', 'ready', 10)`, tenant, named, o.backend)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = o.f.db.Exec(`DELETE FROM blob_copies WHERE tenant_id = $1`, tenant) })

	// The leg holds no index row: it is walked because it is a mirror leg.
	o.sweeper.MirrorLegs = []string{o.backend, "not-registered-leg"}
	o.sweeper.onlyBackends = nil
	bs, err := o.sweeper.backends(context.Background(), ChunkOrphanResult{})
	require.NoError(t, err)
	assert.Contains(t, bs, o.backend)
	assert.NotContains(t, bs, "not-registered-leg", "a leg with no driver is no backend to walk")

	o.sweeper.onlyBackends = []string{o.backend}
	res, err := o.sweeper.Run(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, o.present(named), "a copy its blob_copies row names is never erased")
	assert.False(t, o.present(unnamed), "a copy no row names is erased past the grace")
	assert.Equal(t, 1, res.Backends[o.backend].Erased)
}
