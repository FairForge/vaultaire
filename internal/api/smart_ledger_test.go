package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// WP-R13-2 — demotion ledger hygiene (Review R13-06, R13-10).
//
// A demoted object has bytes on two backends until its hot copy is reclaimed
// and on the cold one afterwards. Everything that replaces or removes the
// object must leave no copy behind on a backend the key no longer routes to,
// and nothing may delete a copy the key DOES route to.

// ledgerFixture is the demotion fixture (two local drivers: "idrive" hot and
// primary, "geyser" cold) with the S3 handlers on top.
type ledgerFixture struct {
	*demotionFixture
	server  *Server
	adapter *S3ToEngine
	tn      *tenant.Tenant
	p       *SmartPromoter
}

func setupLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	s := &Server{engine: f.eng, db: f.db, logger: zap.NewNop(), smartPromoter: p}
	a := NewS3ToEngine(f.eng, f.db, zap.NewNop())
	a.smartPromoter = p
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id = $1`, f.tenantID)
	})
	return &ledgerFixture{
		demotionFixture: f, server: s, adapter: a, p: p,
		tn: &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"},
	}
}

// demoted plants an object and demotes it through the job. reclaimed = the
// state after the grace: hot copy gone, ledger row closed as `deleted`.
func (f *ledgerFixture) demoted(bucket, key string, reclaimed bool) {
	f.t.Helper()
	f.object(bucket, key, 100, 30, 20)
	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(f.t, err)
	require.Equal(f.t, 1, res.Demoted, "%+v", res)
	require.Equal(f.t, "geyser", f.backendOf(bucket, key))
	require.True(f.t, f.coldExists(bucket, key))
	if reclaimed {
		require.NoError(f.t, os.Remove(f.hotPath(bucket, key)))
		_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at = NOW(), hot_outcome = 'deleted'
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, bucket, key)
		require.NoError(f.t, err)
	}
}

func (f *ledgerFixture) hotPath(bucket, key string) string {
	return filepath.Join(f.hotDir, f.tenantID+"_"+bucket, key)
}

func (f *ledgerFixture) coldPath(bucket, key string) string {
	return filepath.Join(f.coldDir, f.tenantID+"_"+bucket, key)
}

func (f *ledgerFixture) read(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (f *ledgerFixture) req(method, target string, body []byte) *http.Request {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	return r.WithContext(s3Ctx(r.Context(), f.tn))
}

func (f *ledgerFixture) put(bucket, key, body string, hdr ...string) {
	f.t.Helper()
	r := f.req("PUT", "/"+bucket+"/"+key, []byte(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, r, bucket, key)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
}

func (f *ledgerFixture) del(bucket, key string) {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, f.req("DELETE", "/"+bucket+"/"+key, nil), bucket, key)
	require.Equal(f.t, http.StatusNoContent, w.Code, w.Body.String())
}

// openLedger plants a ledger row demoted `age` ago with its hot copy on record.
func (f *ledgerFixture) openLedger(bucket, key, etag string, age time.Duration) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
		VALUES ($1,$2,$3,$4,100,'idrive','geyser','idle',$5)`, f.tenantID, bucket, key, etag, f.now.Add(-age))
	require.NoError(f.t, err)
}

func (f *ledgerFixture) blob(dir, bucket, key, body string) {
	f.t.Helper()
	p := filepath.Join(dir, f.tenantID+"_"+bucket, key)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(f.t, os.WriteFile(p, []byte(body), 0o600))
}

// --- R13-10: an overwrite must not leave the cold copy behind ---------------

func TestOverwriteOfDemotedObject_PlainPutRemovesTheColdCopy(t *testing.T) {
	// Arrange: demoted, hot copy reclaimed — the only bytes are on tape.
	f := setupLedgerFixture(t)
	f.demoted("b", "report.pdf", true)

	// Act: the customer uploads the key again; a plain PUT lands hot.
	f.put("b", "report.pdf", "new bytes")

	// Assert: the old bytes on the cold backend are gone, the new ones live.
	assert.Equal(t, "idrive", f.backendOf("b", "report.pdf"))
	assert.Equal(t, "new bytes", f.read(f.hotPath("b", "report.pdf")))
	assert.False(t, f.coldExists("b", "report.pdf"), "the displaced cold copy has nothing pointing at it — it must be deleted")
}

func TestOverwriteOfDemotedObject_InsideTheGraceKeepsTheNewHotBytes(t *testing.T) {
	// Arrange: demoted a moment ago — the hot copy is still there.
	f := setupLedgerFixture(t)
	f.demoted("b", "k", false)
	require.True(t, f.hotExists("b", "k"))

	// Act
	f.put("b", "k", "new bytes")

	// Assert: the PUT replaced the hot copy in place; only the cold copy goes.
	assert.Equal(t, "new bytes", f.read(f.hotPath("b", "k")))
	assert.False(t, f.coldExists("b", "k"))

	// The reclaim, a day later, closes the ledger and touches nothing live.
	f.runner.now = func() time.Time { return f.now.Add(48 * time.Hour) }
	_, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, "new bytes", f.read(f.hotPath("b", "k")))
	_, closed, outcome, ok := f.ledger("b", "k")
	require.True(t, ok)
	assert.True(t, closed)
	assert.Equal(t, "kept_changed", outcome)
}

func TestOverwriteOfDemotedObject_CopyObjectRemovesTheColdCopy(t *testing.T) {
	// Arrange
	f := setupLedgerFixture(t)
	f.demoted("b", "dest", true)
	f.put("b", "src", "source bytes")

	// Act: CopyObject onto the demoted key.
	r := f.req("PUT", "/b/dest", nil)
	r.Header.Set("x-amz-copy-source", "/b/src")
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, r, &S3Request{Bucket: "b", Object: "dest", TenantID: f.tenantID})

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "idrive", f.backendOf("b", "dest"))
	assert.Equal(t, "source bytes", f.read(f.hotPath("b", "dest")))
	assert.False(t, f.coldExists("b", "dest"), "CopyObject onto a demoted key leaked the cold copy")
}

func TestOverwriteOfDemotedObject_MultipartCompleteRemovesTheColdCopy(t *testing.T) {
	// Arrange
	f := setupLedgerFixture(t)
	f.demoted("b", "big.bin", true)

	w := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(w, f.req("POST", "/b/big.bin?uploads", nil), "b", "big.bin")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })

	w = httptest.NewRecorder()
	f.server.handleUploadPart(w, f.req("PUT", "/b/big.bin?partNumber=1&uploadId="+init.UploadID, []byte("multipart bytes")), "b", "big.bin")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	etag := w.Header().Get("ETag")

	// Act
	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag)
	w = httptest.NewRecorder()
	f.server.handleCompleteMultipartUpload(w, f.req("POST", "/b/big.bin?uploadId="+init.UploadID, []byte(body)), "b", "big.bin")

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "idrive", f.backendOf("b", "big.bin"))
	assert.Equal(t, "multipart bytes", f.read(f.hotPath("b", "big.bin")))
	assert.False(t, f.coldExists("b", "big.bin"), "CompleteMultipartUpload onto a demoted key leaked the cold copy")
}

func TestOverwriteOfDemotedObject_ArchiveClassPutKeepsTheColdBytesAndTheReclaimDropsTheStaleHotCopy(t *testing.T) {
	// Arrange: demoted inside the grace; the customer then PUTs the key with
	// an archive class — THEIR bytes land on the cold backend at this key.
	f := setupLedgerFixture(t)
	f.demoted("b", "k", false)

	// Act
	f.put("b", "k", "attic bytes", "x-amz-storage-class", "GLACIER")

	// Assert: nothing of theirs was deleted.
	require.Equal(t, "geyser", f.backendOf("b", "k"))
	assert.Equal(t, "attic bytes", f.read(f.coldPath("b", "k")), "the cold blob is the customer's new object")

	// The reclaim after the grace: the cold copy is theirs (kept), the hot
	// copy is the OLD object and nothing routes to it — it goes.
	f.runner.now = func() time.Time { return f.now.Add(48 * time.Hour) }
	_, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, "attic bytes", f.read(f.coldPath("b", "k")))
	assert.False(t, f.hotExists("b", "k"), "the stale hot copy of the replaced object stayed on paid storage")
	_, closed, outcome, _ := f.ledger("b", "k")
	assert.True(t, closed)
	assert.Equal(t, "kept_changed", outcome)
}

func TestSmartDemotion_OverwrittenObjectIsYoungAgain(t *testing.T) {
	// Arrange: a key created a month ago and not read for weeks, overwritten
	// an hour ago. created_at and last_accessed are the old row's — the
	// upsert keeps them — so the object looked "old and idle" the day after
	// the upload and its fresh bytes went straight to tape, every time a
	// backup job rewrote the key.
	f := setupLedgerFixture(t)
	f.object("b", "latest.tar", 100, 30, 20)
	_, err := f.db.Exec(`UPDATE object_head_cache SET updated_at = $2 WHERE tenant_id = $1 AND object_key = 'latest.tar'`,
		f.tenantID, f.now.Add(-time.Hour))
	require.NoError(t, err)

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert: MinAge (upload settling) counts from the last write.
	require.NoError(t, err)
	assert.Equal(t, 0, res.Demoted, "%+v", res)
	assert.Equal(t, "idrive", f.backendOf("b", "latest.tar"))
}

// --- the reclaim: one rule for every copy the ledger knows -------------------

func TestReclaim_KeptChangedDeletesTheColdCopyWhenTheObjectMovedOffCold(t *testing.T) {
	// Arrange: the state every overwrite before this WP left behind — the row
	// routes hot with new bytes, the ledger is open, the cold copy is an orphan.
	f := setupLedgerFixture(t)
	f.object("b", "k", 100, 30, 1) // head row on idrive, recently read
	f.blob(f.coldDir, "b", "k", "old cold bytes")
	f.openLedger("b", "k", "old-etag", 48*time.Hour)

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.False(t, f.coldExists("b", "k"), "kept_changed left the cold copy on tape")
	assert.True(t, f.hotExists("b", "k"), "the hot copy is the live object")
	_, closed, outcome, _ := f.ledger("b", "k")
	assert.True(t, closed)
	assert.Equal(t, "kept_changed", outcome)
}

func TestReclaim_ChunkedRowRoutesNoWholeObjectSoBothCopiesGo(t *testing.T) {
	// Arrange: the demoted key was overwritten by a chunked upload — the
	// object now lives in _global chunks, no whole blob at the key is live.
	f := setupLedgerFixture(t)
	f.object("b", "k", 100, 30, 1, chunked())
	f.blob(f.coldDir, "b", "k", "old cold bytes")
	f.openLedger("b", "k", "old-etag", 48*time.Hour)

	// Act
	_, err := f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.False(t, f.coldExists("b", "k"))
	assert.False(t, f.hotExists("b", "k"))
}

func TestReclaim_ObjectGoneBehindADeleteMarkerKeepsTheVersionBytes(t *testing.T) {
	// Arrange: the bucket became versioned after the demotion, the key was
	// overwritten (version v1, hot) and then deleted with a marker: no head
	// row, but v1's bytes are what GET ?versionId=v1 still serves.
	f := setupLedgerFixture(t)
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, versioning_status) VALUES ($1,'b','Enabled')`, f.tenantID)
	require.NoError(t, err)
	f.blob(f.hotDir, "b", "k", "v1 bytes")
	_, err = f.db.Exec(`INSERT INTO object_versions (tenant_id,bucket,object_key,version_id,size_bytes,etag,content_type,is_latest,is_delete_marker,backend_name,created_at)
		VALUES ($1,'b','k','v1',8,'e1','application/octet-stream',FALSE,FALSE,'idrive',NOW() - INTERVAL '1 hour'),
		       ($1,'b','k','m1',0,'','application/octet-stream',TRUE,TRUE,NULL,NOW())`, f.tenantID)
	require.NoError(t, err)
	f.openLedger("b", "k", "old-etag", 48*time.Hour)

	// Act
	_, err = f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, "v1 bytes", f.read(f.hotPath("b", "k")), "the reclaim deleted the bytes of a live version behind a delete marker")
}

func TestReclaim_ReclaimedRowWhoseObjectChangedIsSettledByTheNextRun(t *testing.T) {
	// Arrange: two objects demoted and reclaimed long ago. One was then
	// overwritten hot and one deleted, and in both cases the cold copy was
	// NOT removed at the time — the process restarted between the commit and
	// the delete, or the backend refused it. The ledger rows are closed, so
	// the open-row pass never sees them.
	f := setupLedgerFixture(t)
	f.demoted("b", "rewritten", true)
	f.demoted("b", "deleted", true)
	f.demoted("b", "untouched", true)
	f.blob(f.hotDir, "b", "rewritten", "new bytes")
	_, err := f.db.Exec(`UPDATE object_head_cache SET backend_name = 'idrive', etag = 'new-etag', updated_at = $2
		WHERE tenant_id = $1 AND object_key = 'rewritten'`, f.tenantID, f.now.Add(47*time.Hour))
	require.NoError(t, err)
	_, err = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'deleted'`, f.tenantID)
	require.NoError(t, err)
	f.runner.now = func() time.Time { return f.now.Add(48 * time.Hour) }

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.False(t, f.coldExists("b", "rewritten"), "a cold copy whose delete did not happen at the overwrite is never retried")
	assert.Equal(t, "new bytes", f.read(f.hotPath("b", "rewritten")))
	assert.False(t, f.coldExists("b", "deleted"))
	assert.True(t, f.coldExists("b", "untouched"), "a demoted object that did not change keeps its only copy")
	_, _, outcome, _ := f.ledger("b", "rewritten")
	assert.Equal(t, "kept_changed", outcome)
	_, _, outcome, _ = f.ledger("b", "deleted")
	assert.Equal(t, "object_gone", outcome)
	_, _, outcome, _ = f.ledger("b", "untouched")
	assert.Equal(t, "deleted", outcome)
}

// --- DeleteObject of a demoted key ------------------------------------------

func TestDeleteOfDemotedObject_InsideTheGraceRemovesBothCopiesAtOnce(t *testing.T) {
	// Arrange
	f := setupLedgerFixture(t)
	f.demoted("b", "k", false)
	require.True(t, f.hotExists("b", "k") && f.coldExists("b", "k"))

	// Act
	f.del("b", "k")

	// Assert: nothing is left for a later reclaim to delete at this key — a
	// re-upload of the key cannot meet a job that still owes it a delete.
	assert.False(t, f.coldExists("b", "k"))
	assert.False(t, f.hotExists("b", "k"), "the hot copy of a deleted object stayed until the next reclaim (the R13-06 exposure)")
	_, closed, outcome, ok := f.ledger("b", "k")
	require.True(t, ok)
	assert.True(t, closed, "the ledger row is settled by the delete")
	assert.Equal(t, "object_gone", outcome)
}

func TestDeleteOfDemotedObject_BatchDeleteRemovesBothCopiesAtOnce(t *testing.T) {
	// Arrange
	f := setupLedgerFixture(t)
	f.demoted("b", "k", false)

	// Act
	body := `<Delete><Object><Key>k</Key></Object></Delete>`
	w := httptest.NewRecorder()
	f.server.handleDeleteObjects(w, f.req("POST", "/b?delete", []byte(body)), &S3Request{Bucket: "b", TenantID: f.tenantID})

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "<Key>k</Key>")
	assert.False(t, f.coldExists("b", "k"))
	assert.False(t, f.hotExists("b", "k"))
	_, closed, _, _ := f.ledger("b", "k")
	assert.True(t, closed)
}

func TestDeleteOfDemotedObject_HotDeleteFailureIsRetriedByTheReclaim(t *testing.T) {
	// Arrange: the hot backend refuses the delete (a flaky backend).
	f := setupLedgerFixture(t)
	hot := &flakyDriver{LocalDriver: drivers.NewLocalDriver(f.hotDir, zap.NewNop())}
	f.eng.AddDriver("idrive", hot)
	f.demoted("b", "k", false)
	hot.fail.Store(true)

	// Act: the customer's DELETE still succeeds — the object is gone for them.
	f.del("b", "k")

	// Assert: the ledger stays open, so the copy is still owed.
	assert.True(t, f.hotExists("b", "k"))
	_, closed, _, ok := f.ledger("b", "k")
	require.True(t, ok)
	assert.False(t, closed, "a failed delete must leave the ledger open for the reclaim")

	// The backend is back; the reclaim after the grace removes it.
	hot.fail.Store(false)
	f.runner.now = func() time.Time { return f.now.Add(48 * time.Hour) }
	_, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.False(t, f.hotExists("b", "k"))
	_, closed, outcome, _ := f.ledger("b", "k")
	assert.True(t, closed)
	assert.Equal(t, "object_gone", outcome)
}

// --- R13-06: detection of a write lost to a stale-copy delete -----------------

func observedLedgerFixture(t *testing.T) (*ledgerFixture, *observer.ObservedLogs) {
	f := setupLedgerFixture(t)
	core, logs := observer.New(zapcore.WarnLevel)
	f.runner.logger = zap.New(core)
	return f, logs
}

func TestReclaim_ObjectGoneDetectsAReuploadItDeleted(t *testing.T) {
	// Arrange: the object was deleted after demotion and its hot copy is
	// still owed a delete. A PUT of the same key is in flight: its bytes land
	// on the hot backend BEFORE the reclaim's delete, its head row commits
	// after. No lock can order the two (the backend write precedes the
	// transaction — WP-R2-1), so the reclaim deletes the new bytes.
	f, logs := observedLedgerFixture(t)
	f.blob(f.hotDir, "b", "k", "orphan hot copy")
	f.openLedger("b", "k", "old-etag", 48*time.Hour)
	f.runner.beforeHotDelete = func(bucket, key string) {
		f.blob(f.hotDir, bucket, key, "the customer's new upload")
	}
	f.runner.afterStaleDelete = func(bucket, key string) {
		_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name)
			VALUES ($1,$2,$3,25,'new-etag','idrive')`, f.tenantID, bucket, key)
		require.NoError(t, err)
	}
	before := testutil.ToFloat64(staleCopyLostWrites.WithLabelValues("reclaim"))

	// Act
	_, err := f.runner.RunOnce(context.Background(), false)

	// Assert: it cannot be prevented here; it must not be silent.
	require.NoError(t, err)
	require.False(t, f.hotExists("b", "k"), "the scenario: the reclaim deleted the new bytes")
	assert.Equal(t, before+1, testutil.ToFloat64(staleCopyLostWrites.WithLabelValues("reclaim")))
	entries := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, entries, 1, "%v", logs.All())
	fields := entries[0].ContextMap()
	assert.Equal(t, f.tenantID, fields["tenant"])
	assert.Equal(t, "b", fields["bucket"])
	assert.Equal(t, "k", fields["key"])
	assert.Equal(t, "idrive", fields["backend"])
}

func TestReclaim_ObjectGoneIsSilentWhenTheReuploadLandedAfterTheDelete(t *testing.T) {
	// Arrange: same key, but the PUT's bytes land after the reclaim's delete:
	// a hot-routed row has appeared and its bytes are there. Nothing was lost.
	f, logs := observedLedgerFixture(t)
	f.blob(f.hotDir, "b", "k", "orphan hot copy")
	f.openLedger("b", "k", "old-etag", 48*time.Hour)
	f.runner.afterStaleDelete = func(bucket, key string) {
		f.blob(f.hotDir, bucket, key, "the customer's new upload")
		_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name)
			VALUES ($1,$2,$3,25,'new-etag','idrive')`, f.tenantID, bucket, key)
		require.NoError(t, err)
	}
	before := testutil.ToFloat64(staleCopyLostWrites.WithLabelValues("reclaim"))

	// Act
	_, err := f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, "the customer's new upload", f.read(f.hotPath("b", "k")))
	assert.Equal(t, before, testutil.ToFloat64(staleCopyLostWrites.WithLabelValues("reclaim")))
	assert.Empty(t, logs.FilterLevelExact(zapcore.ErrorLevel).All())
}

func TestStaleCopyLostWrites_EverySourceStartsAtZero(t *testing.T) {
	// A counter that does not exist until its first increment cannot be
	// alerted on with increase(): the series must be there at 0.
	s := &Server{}
	s.initMetrics()
	body := scrapeMetrics(t, s)
	for _, src := range []string{"reclaim", "delete", "overwrite"} {
		assert.Contains(t, body, `vaultaire_stale_copy_lost_writes_total{source="`+src+`"}`)
	}
}

// --- a cancelled run -----------------------------------------------------------

func TestSmartDemotion_CancelledRunReturnsTheContextErrorNotAListOfFailures(t *testing.T) {
	// Arrange: three hot copies to reclaim; the process is told to stop while
	// the first is being reclaimed (a deploy).
	f := setupLedgerFixture(t)
	for _, k := range []string{"a", "b", "c"} {
		h := f.object("b", k, 100, 30, 20, onBackend("geyser"))
		f.blob(f.hotDir, "b", k, "hot")
		f.openLedger("b", k, h.etag, 48*time.Hour)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	f.runner.beforeHotDelete = func(_, _ string) {
		if calls.Add(1) == 1 {
			cancel()
		}
	}

	// Act
	res, err := f.runner.RunOnce(ctx, false)

	// Assert: the rest is the next run's work, not this run's failures.
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, res.Errors, "items a cancelled run did not reach are not failures")
	var open int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM smart_demotions WHERE tenant_id = $1 AND hot_deleted_at IS NULL`, f.tenantID).Scan(&open))
	assert.GreaterOrEqual(t, open, 2, "the rows not reached stay open")
}

// --- adversarial pass ------------------------------------------------------------

func TestPromotePending_StaleLedgerNeverCopiesOldBytesOverANewObject(t *testing.T) {
	// Arrange: demoted and reclaimed; a read asked for the copy-back (it did
	// not run — the object was on tape). The customer then overwrote the key:
	// new bytes hot, and the old cold copy is still there (its delete failed,
	// or the overwrite predates this WP).
	f := setupLedgerFixture(t)
	f.demoted("b", "k", true)
	_, err := f.db.Exec(`UPDATE smart_demotions SET promote_requested_at = NOW() WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	f.blob(f.hotDir, "b", "k", "the customer's new bytes")
	_, err = f.db.Exec(`UPDATE object_head_cache SET backend_name = 'idrive', etag = 'new-etag', size_bytes = 24
		WHERE tenant_id = $1 AND bucket = 'b' AND object_key = 'k'`, f.tenantID)
	require.NoError(t, err)

	// Act: the daily job retries pending promotions.
	_, errs := f.p.PromotePending(context.Background())

	// Assert: the copy-back must not write the OLD cold bytes over the new object.
	assert.Empty(t, errs)
	assert.Equal(t, "the customer's new bytes", f.read(f.hotPath("b", "k")),
		"a stale ledger row copied the demoted bytes over the object that replaced them")
	assert.False(t, f.coldExists("b", "k"), "the stale cold copy is removed with the ledger row")
}

func TestSmartDemotion_ObjectDeletedDuringTheCopyLeavesNoColdCopy(t *testing.T) {
	// Arrange: the customer deletes the object between the cold write and
	// the routing flip.
	f := setupLedgerFixture(t)
	f.object("b", "racy", 100, 30, 20)
	f.runner.beforeFlip = func(bucket, key string) {
		_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key)
		require.NoError(t, err)
	}

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
	assert.False(t, f.coldExists("b", "racy"), "the cold copy of a deleted object was left on tape")
}

func TestDisplacedBlob_NotDeletedWhenTheKeyRoutesBackThere(t *testing.T) {
	// Arrange: a writer displaced a cold-routed row with a hot write, and by
	// the time it cleans up, another writer has put the key back on the cold
	// backend (a PUT with an archive class). The blob there is live.
	f := setupLedgerFixture(t)
	f.object("b", "k", 100, 30, 1, onBackend("geyser")) // the row as the second writer left it
	container := f.tenantID + "_b"

	// Act
	dropDisplacedBlob(context.Background(), f.db, f.eng, zap.NewNop(), lostWriteOverwrite,
		f.tenantID, "b", container, "k", displacedRow{Size: 100, Backend: "geyser"}, "idrive")

	// Assert
	assert.True(t, f.coldExists("b", "k"), "a blob the head row routes to must never be deleted as displaced")
}

func TestDisplacedBlob_UnknownOrSameBackendIsLeftAlone(t *testing.T) {
	// Arrange: the key routes hot; a blob sits on each backend.
	f := setupLedgerFixture(t)
	f.object("b", "k", 100, 30, 1)
	f.blob(f.coldDir, "b", "k", "cold blob")
	container := f.tenantID + "_b"

	for _, d := range []displacedRow{
		{Size: 100, Backend: "idrive"},                // same backend: overwritten in place
		{Size: 100, Backend: ""},                      // legacy row with no backend on record
		{Size: 100, Backend: "no-such-driver"},        // a backend this process does not serve
		{Size: 100, Backend: "geyser", Chunked: true}, // a chunked row never had a whole blob there
	} {
		// Act
		dropDisplacedBlob(context.Background(), f.db, f.eng, zap.NewNop(), lostWriteOverwrite,
			f.tenantID, "b", container, "k", d, "idrive")

		// Assert
		assert.True(t, f.hotExists("b", "k"), "%+v", d)
		assert.True(t, f.coldExists("b", "k"), "%+v", d)
	}
}

func scrapeMetrics(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	return strings.TrimSpace(w.Body.String())
}

// hungDriver is a backend in an outage: reads fail, and a delete waits on
// the dead endpoint until its context gives up.
type hungDriver struct {
	*drivers.LocalDriver
	deletes atomic.Int32
}

func (d *hungDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("dial tcp: i/o timeout")
}

func (d *hungDriver) Delete(ctx context.Context, _, _ string) error {
	d.deletes.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// Post-merge review: the displaced-blob delete runs inside the overwrite
// request, straight on the old backend's driver. When that backend is the
// one in an outage — the primary is down, writes fail over, and every
// overwrite displaces a row that routed to the primary — each PUT waited for
// the dead backend (up to staleCopyTimeout, 30 s) before it answered. The
// engine already knows: the backend's circuit breaker is open. The blob is
// then left (an orphan costs money; the wait cost the customer's write path
// during the outage the failover exists for).
func TestDisplacedBlob_NotAttemptedWhileTheOldBackendsBreakerIsOpen(t *testing.T) {
	// Arrange: the old backend is down and its breaker has opened.
	f := setupLedgerFixture(t)
	down := &hungDriver{LocalDriver: drivers.NewLocalDriver(t.TempDir(), zap.NewNop())}
	f.eng.AddDriver("lyve", down)
	f.object("b", "k", 100, 30, 1) // the row as the overwrite left it: routes to idrive
	container := f.tenantID + "_b"
	for i := 0; i < 10 && f.eng.GetFailoverStatus()["lyve"] == "closed"; i++ {
		f.eng.HintBackend(container, "probe", "lyve")
		_, _ = f.eng.Get(context.Background(), container, "probe")
	}
	require.NotEqual(t, "closed", f.eng.GetFailoverStatus()["lyve"], "fixture: the breaker must be open")

	// Act
	start := time.Now()
	dropDisplacedBlob(context.Background(), f.db, f.eng, zap.NewNop(), lostWriteOverwrite,
		f.tenantID, "b", container, "k", displacedRow{Size: 100, Backend: "lyve"}, "idrive")

	// Assert: no call to the dead backend, no wait.
	assert.Equal(t, int32(0), down.deletes.Load(), "a delete was sent to a backend whose breaker is open")
	assert.Less(t, time.Since(start), 5*time.Second)
}
