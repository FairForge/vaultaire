package api

// Stage 1a Prompt 7, Part 0: the WP-R10-8 post-merge probe (#718, #719).
// The races its own tests do not stage, each with the ledger compared to
// usage.BilledBytes (the reconcile's computation, never run) afterwards.
// "Before" = the same tests on origin/main 06b9b86, in a separate worktree.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — the S3 part ETag
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// gatedBody is a part body that blocks its first Read until open is closed;
// started is closed when the first Read is reached — the handler reserves
// the part before it reads a byte, so started means "reserved".
type gatedBody struct {
	r       io.Reader
	started chan struct{}
	open    chan struct{}
	once    sync.Once
}

func newGatedBody(b []byte) *gatedBody {
	return &gatedBody{r: bytes.NewReader(b), started: make(chan struct{}), open: make(chan struct{})}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.started)
		<-g.open
	})
	return g.r.Read(p)
}

// waitOrFail waits for ch, or fails the test after 10 s.
func waitOrFail(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// gatePutDriver holds a Put after the inner driver stored the bytes, while
// armed: reached is signalled once per held Put, resume releases them all.
// failNext makes the next Put fail before it stores anything.
type gatePutDriver struct {
	engine.Driver
	armed    atomic.Bool
	failNext atomic.Bool
	reached  chan struct{}
	resume   chan struct{}
}

func (g *gatePutDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	if g.failNext.CompareAndSwap(true, false) {
		_, _ = io.Copy(io.Discard, data)
		return fmt.Errorf("backend refused: %w", errors.New("connection reset by peer"))
	}
	if err := g.Driver.Put(ctx, container, artifact, data, opts...); err != nil {
		return err
	}
	if g.armed.Load() {
		g.reached <- struct{}{}
		<-g.resume
	}
	return nil
}

// gateCompletes replaces the fixture's local driver with a gatePutDriver.
func (f *quotaAccountingFixture) gateCompletes(t *testing.T) *gatePutDriver {
	t.Helper()
	ce := f.server.engine
	g := &gatePutDriver{Driver: drivers.NewLocalDriver(f.tempDir, zap.NewNop()),
		reached: make(chan struct{}, 4), resume: make(chan struct{})}
	ce.AddDriver("local", g)
	return g
}

// partsBody is the CompleteMultipartUpload body listing parts n → etag.
func partsBody(etags map[int]string, nums ...int) string {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, n := range nums {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, etags[n])
	}
	b.WriteString("</CompleteMultipartUpload>")
	return b.String()
}

// twoParts uploads parts 1 and 2 of 64 KiB each.
func (f *quotaAccountingFixture) twoParts(t *testing.T, key string) (string, map[int]string) {
	t.Helper()
	id := f.createUpload(t, key, nil)
	etags := map[int]string{}
	for i := 1; i <= 2; i++ {
		w := f.putPart(t, key, id, i, 64*kib)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		etags[i] = w.Header().Get("ETag")
	}
	return id, etags
}

func (f *quotaAccountingFixture) completeReq(key, id, body string) *httptest.ResponseRecorder {
	return doS3Request(f.server, f.tenant, "POST", "/test-bucket/"+key+"?uploadId="+id, strings.NewReader(body))
}

func (f *quotaAccountingFixture) uploadStatus(t *testing.T, id string) string {
	t.Helper()
	var s string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&s))
	return s
}

// stagedFiles lists every file left in the upload's staging directory.
func stagedFiles(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(multipartDir(id), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, filepath.Base(p))
		}
		return nil
	})
	return out
}

func TestPostMerge108_SamePartNumberTwiceAtOnce_HoldsTheInstalledPart(t *testing.T) {
	// Two attempts at part 1 (64 KiB and 32 KiB — aws-cli retrying a part
	// it timed out on while the first attempt still streams), both reserved
	// before either is staged.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	a, b := newGatedBody(testBytes(64*kib)), newGatedBody(bytes.Repeat([]byte{7}, 32*kib))
	var wa, wb *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Go(func() { wa = f.uploadPart("big.bin", id, 1, a, 64*kib) })
	wg.Go(func() { wb = f.uploadPart("big.bin", id, 1, b, 32*kib) })
	waitOrFail(t, a.started, "part attempt a reserved")
	waitOrFail(t, b.started, "part attempt b reserved")
	assert.Equal(t, int64(96*kib), f.uploadHolds(t, id), "each attempt reserved its own bytes")
	f.ledgerAgrees(t, "both attempts reserved, neither staged")

	close(a.open)
	close(b.open)
	wg.Wait()

	require.Equal(t, http.StatusOK, wa.Code, wa.Body.String())
	require.Equal(t, http.StatusOK, wb.Code, wb.Body.String())
	var size int64
	var etag string
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes, etag FROM multipart_parts WHERE upload_id = $1 AND part_number = 1`, id).Scan(&size, &etag))
	data, err := os.ReadFile(partFilePath(id, 1))
	require.NoError(t, err)
	sum := md5.Sum(data) // #nosec G401
	assert.Equal(t, strings.Trim(etag, `"`), hex.EncodeToString(sum[:]), "the part row describes the file on disk")
	assert.Equal(t, size, int64(len(data)))
	assert.Equal(t, size, f.uploadHolds(t, id), "the upload holds exactly the installed attempt")
	assert.Equal(t, size, f.used(t))
	f.ledgerAgrees(t, "after both attempts")
}

func TestPostMerge108_UploadPartRacingAbort_TheReservationNeverOutlivesItsUpload(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	require.Equal(t, http.StatusOK, f.put(t, "other", testBytes(1000)))
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	g := newGatedBody(testBytes(64 * kib))
	var w *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() { w = f.uploadPart("big.bin", id, 2, g, 64*kib); close(done) }()
	waitOrFail(t, g.started, "part 2 reserved")
	require.Equal(t, int64(1000+128*kib), f.used(t))

	// The abort lands while part 2 is reserved and not yet staged.
	ab := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/big.bin?uploadId="+id, nil)
	require.Equal(t, http.StatusNoContent, ab.Code, ab.Body.String())
	assert.Equal(t, int64(1000), f.used(t), "the abort released part 1 AND part 2's reservation")
	close(g.open)
	waitOrFail(t, done, "part 2 answered")

	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "NoSuchUpload")
	assert.Equal(t, int64(1000), f.used(t), "nothing released twice, nothing left reserved")
	assert.Zero(t, f.uploadHolds(t, id))
	assert.Equal(t, "aborted", f.uploadStatus(t, id))
	assert.Empty(t, stagedFiles(t, id), "no part bytes left on the staging disk")
	f.ledgerAgrees(t, "after the abort and the late part")
}

func TestPostMerge108_UploadPartRacingComplete_TheInFlightPartIsReleasedOnce(t *testing.T) {
	// Complete lists parts 1–2 while part 3 is reserved and streaming: the
	// commit takes what the upload holds (part 3's reservation too) and
	// releases what it does not list; the late part then finds the upload
	// completed.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id, etags := f.twoParts(t, "big.bin")
	g := newGatedBody(testBytes(64 * kib))
	var w3 *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() { w3 = f.uploadPart("big.bin", id, 3, g, 64*kib); close(done) }()
	waitOrFail(t, g.started, "part 3 reserved")
	require.Equal(t, int64(192*kib), f.used(t))

	wc := f.completeReq("big.bin", id, partsBody(etags, 1, 2))
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	assert.Equal(t, int64(128*kib), f.used(t), "exactly the object")
	close(g.open)
	waitOrFail(t, done, "part 3 answered")

	assert.Equal(t, http.StatusNotFound, w3.Code, w3.Body.String())
	assert.Equal(t, int64(128*kib), f.used(t))
	assert.Empty(t, stagedFiles(t, id))
	f.ledgerAgrees(t, "after the complete and the late part")
}

// completeHeldAfterWrite starts a Complete of parts 1–2 and returns once its
// backend write is done and its commit not yet begun.
func (f *quotaAccountingFixture) completeHeldAfterWrite(t *testing.T, g *gatePutDriver, key, id string, etags map[int]string) (wait func() *httptest.ResponseRecorder) {
	t.Helper()
	g.armed.Store(true)
	var w *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() { w = f.completeReq(key, id, partsBody(etags, 1, 2)); close(done) }()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the complete never reached its backend write")
	}
	return func() *httptest.ResponseRecorder {
		g.armed.Store(false)
		close(g.resume)
		waitOrFail(t, done, "the complete answered")
		return w
	}
}

func TestPostMerge108_CompleteRacingAbort_AfterTheBackendWrite(t *testing.T) {
	// The abort commits between the Complete's backend write and its commit.
	// markCompleted is unconditional on purpose (a fully written object
	// never loses its row): the Complete commits, the object is billed at
	// its size, the abort's release of the parts stands.
	f := setupQuotaAccountingFixture(t, 1<<30)
	g := f.gateCompletes(t)
	id, etags := f.twoParts(t, "big.bin")
	finish := f.completeHeldAfterWrite(t, g, "big.bin", id, etags)

	ab := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/big.bin?uploadId="+id, nil)
	require.Equal(t, http.StatusNoContent, ab.Code)
	assert.Zero(t, f.used(t), "the abort released both parts")
	wc := finish()

	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	assert.Equal(t, int64(128*kib), f.headSize(t, "big.bin"))
	assert.Equal(t, int64(128*kib), f.used(t), "the object, charged in full at its commit")
	assert.Equal(t, "completed", f.uploadStatus(t, id))
	f.ledgerAgrees(t, "complete after abort")
}

func TestPostMerge108_CompleteRacingTheReaper_AfterTheBackendWrite(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	g := f.gateCompletes(t)
	id, etags := f.twoParts(t, "big.bin")
	finish := f.completeHeldAfterWrite(t, g, "big.bin", id, etags)

	ended, err := endUploadReleasing(context.Background(), f.db, id, "", "", "") // the reaper's abort
	require.NoError(t, err)
	require.True(t, ended)
	wc := finish()

	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	assert.Equal(t, int64(128*kib), f.used(t))
	f.ledgerAgrees(t, "complete after the reap")
}

func TestPostMerge108_CompleteRacingAbort_BeforeTheLastPartIsRead(t *testing.T) {
	// The abort lands while the Complete still reads the staged parts: the
	// staging directory goes, the Complete cannot read part 2, nothing is
	// committed and nothing is billed.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id, etags := f.twoParts(t, "big.bin")
	old := stagedPartReadHook
	t.Cleanup(func() { stagedPartReadHook = old })
	stagedPartReadHook = func(n int) {
		if n == 2 {
			ab := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/big.bin?uploadId="+id, nil)
			assert.Equal(t, http.StatusNoContent, ab.Code)
		}
	}

	wc := f.completeReq("big.bin", id, partsBody(etags, 1, 2))

	assert.NotEqual(t, http.StatusOK, wc.Code, wc.Body.String())
	_, _, exists := f.headRow(t, "big.bin")
	assert.False(t, exists)
	assert.Zero(t, f.used(t))
	f.ledgerAgrees(t, "abort during the complete's read")
}

func TestPostMerge108_CompleteRetriedAfterItsBackendWriteFailed_HeldTakenOnce(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	g := f.gateCompletes(t)
	id, etags := f.twoParts(t, "big.bin")
	g.failNext.Store(true)

	first := f.completeReq("big.bin", id, partsBody(etags, 1, 2))
	require.NotEqual(t, http.StatusOK, first.Code)
	assert.Equal(t, int64(128*kib), f.used(t), "the failed complete keeps the parts reserved for the retry")
	assert.Equal(t, int64(128*kib), f.uploadHolds(t, id))
	f.ledgerAgrees(t, "after the failed complete")

	second := f.completeReq("big.bin", id, partsBody(etags, 1, 2))
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, int64(128*kib), f.used(t), "the parts became the object once")
	assert.Zero(t, f.uploadHolds(t, id))
	f.ledgerAgrees(t, "after the retry")

	third := f.completeReq("big.bin", id, partsBody(etags, 1, 2))
	require.Equal(t, http.StatusOK, third.Code, "a replay of the completed upload")
	assert.Equal(t, int64(128*kib), f.used(t))
	f.ledgerAgrees(t, "after the replay")
}

func TestPostMerge108_CompleteRetriedAfterItsCommitFailed_OneBlobHeldTakenOnce(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	id, etags := f.twoParts(t, "big.bin")
	old := headFlipHook
	t.Cleanup(func() { headFlipHook = old })
	var failed atomic.Bool
	headFlipHook = func(tenantID, bucket, key string) error {
		if tenantID == f.tenantID && failed.CompareAndSwap(false, true) {
			return errors.New("injected: the head-row transaction fails after the backend write")
		}
		return nil
	}

	first := f.completeReq("big.bin", id, partsBody(etags, 1, 2))
	require.Equal(t, http.StatusInternalServerError, first.Code)
	assert.Equal(t, int64(128*kib), f.used(t))
	var orphans int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = 'orphan_write'`, f.tenantID).Scan(&orphans))
	assert.Equal(t, 1, orphans, "the first attempt's blob is queued")

	second := f.completeReq("big.bin", id, partsBody(etags, 1, 2))
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, int64(128*kib), f.used(t))
	f.ledgerAgrees(t, "after the retry")
	// The collector, past the grace: the retry committed the same address,
	// so the queued blob is named and kept.
	c := NewRetiredBlobCollector(f.db, f.server.engine, f.server.gci, zap.NewNop())
	c.onlyTenant = f.tenantID
	c.JobName = "test_retired_" + f.tenantID[:8]
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Kept)
	assert.Zero(t, res.Deleted)
	get := doS3Request(f.server, f.tenant, "GET", "/test-bucket/big.bin", nil)
	require.Equal(t, http.StatusOK, get.Code)
	assert.Equal(t, 128*kib, get.Body.Len())
}

func TestPostMerge108_UploadPartWhileSetHouseAddsAFloor(t *testing.T) {
	// The house is bought while part 2 is reserved (on the total: no floor
	// row yet) and streaming. SetHouse seeds the new floor row with what
	// the upload holds — part 2's reservation included — and the part's
	// settle and the Complete then move both ledgers together.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	w1 := f.putPart(t, "big.bin", id, 1, 64*kib)
	require.Equal(t, http.StatusOK, w1.Code)
	g := newGatedBody(testBytes(48 * kib))
	var w *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() { w = f.uploadPart("big.bin", id, 2, g, 48*kib); close(done) }()
	waitOrFail(t, g.started, "part 2 reserved")

	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	std, _ := f.floors(t)
	assert.Equal(t, int64(112*kib), std, "the new floor row is seeded with both parts' reservations")
	close(g.open)
	waitOrFail(t, done, "part 2 answered")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	f.ledgerAgrees(t, "after the part settled under the new floor")
	wc := f.completeReq("big.bin", id, partsBody(map[int]string{1: w1.Header().Get("ETag"), 2: w.Header().Get("ETag")}, 1, 2))
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	std, _ = f.floors(t)
	assert.Equal(t, int64(112*kib), std)
	assert.Equal(t, int64(112*kib), f.used(t))
	f.ledgerAgrees(t, "after the complete")
}

func TestPostMerge108_UploadPartOfUnknownLengthWhileSetHouseAddsAFloor(t *testing.T) {
	// R108-8's shape: a part that reserved nothing up front is charged at its
	// install — on the floor row SetHouse created meanwhile.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	g := newGatedBody(testBytes(64 * kib))
	var w *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() { w = f.uploadPart("big.bin", id, 1, g, -1); close(done) }()
	waitOrFail(t, g.started, "part 1 started")
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	close(g.open)
	waitOrFail(t, done, "part 1 answered")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	std, _ := f.floors(t)
	assert.Equal(t, int64(64*kib), std)
	assert.Equal(t, int64(64*kib), f.used(t))
	f.ledgerAgrees(t, "unknown-length part under a new floor")
}

func (f *quotaAccountingFixture) versionRowsNaming(t *testing.T, key string) (rows int, distinctBlobs int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT storage_key) FROM object_versions
		WHERE tenant_id = $1 AND object_key = $2 AND storage_key IS NOT NULL AND NOT is_delete_marker`, f.tenantID, key).Scan(&rows, &distinctBlobs))
	return rows, distinctBlobs
}

func TestPostMerge108_TwoCompletesOfOneUpload_VersionedBucket_OneVersionOneBill(t *testing.T) {
	// A client that timed out on a long Complete retries while the first
	// still runs: both passed "active", both wrote the same address (087),
	// both commit. Under Enabled versioning each commit wrote a version row
	// naming the one blob; once the key is overwritten both are retained
	// versions of one blob — billed twice by the reconcile, once by the
	// ledger.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	g := f.gateCompletes(t)
	id, etags := f.twoParts(t, "big.bin")
	g.armed.Store(true)
	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() { results[i] = f.completeReq("big.bin", id, partsBody(etags, 1, 2)) })
	}
	for range results {
		select {
		case <-g.reached:
		case <-time.After(10 * time.Second):
			t.Fatal("both completes should reach their backend write")
		}
	}
	g.armed.Store(false)
	close(g.resume)
	wg.Wait()
	for _, w := range results {
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	f.ledgerAgrees(t, "after both completes")

	require.Equal(t, http.StatusOK, f.put(t, "big.bin", testBytes(1000)))

	rows, blobs := f.versionRowsNaming(t, "big.bin")
	assert.Equal(t, 1, blobs-1, "one retained blob besides the new current one")
	assert.Equal(t, 2, rows, "one version row per blob: the completed upload's, and the overwrite's")
	assert.Equal(t, int64(128*kib+1000), f.used(t))
	f.ledgerAgrees(t, "after the overwrite")
}

func TestPostMerge108_TwoCompletesOfOneUpload_Unversioned_BilledOnce(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	g := f.gateCompletes(t)
	id, etags := f.twoParts(t, "big.bin")
	g.armed.Store(true)
	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() { results[i] = f.completeReq("big.bin", id, partsBody(etags, 1, 2)) })
	}
	for range results {
		select {
		case <-g.reached:
		case <-time.After(10 * time.Second):
			t.Fatal("both completes should reach their backend write")
		}
	}
	g.armed.Store(false)
	close(g.resume)
	wg.Wait()
	for _, w := range results {
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	assert.Equal(t, int64(128*kib), f.used(t))
	f.ledgerAgrees(t, "after both completes")
	var retired int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1`, f.tenantID).Scan(&retired))
	assert.Zero(t, retired, "the one blob is the head row's: nothing retired")
}

// sqlExec is the slice of *sql.DB the erasure-race helper needs.
type sqlExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func TestPostMerge108_StagingDirOfAnUploadWithNoRow_IsSweptByTheReaper(t *testing.T) {
	// The erasure's race: a CreateMultipartUpload + UploadPart that lands
	// after the runner's abort pass and before EraseRows. EraseRows removes
	// the row; the reaper only removed directories of rows it read, so the
	// staged part — customer bytes, in plaintext — stayed on the box's
	// staging disk until a reboot. Staged here as the erasure leaves it: a
	// staging directory whose row is gone.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	var ex sqlExec = f.db
	_, err := ex.ExecContext(context.Background(), `DELETE FROM multipart_uploads WHERE upload_id = $1`, id) // EraseRows
	require.NoError(t, err)
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(multipartDir(id), old, old))
	// A directory with a row is never touched, whatever its age.
	live := f.createUpload(t, "live.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "live.bin", live, 1, kib).Code)
	require.NoError(t, os.Chtimes(multipartDir(live), old, old))

	m := NewMultipartReaper(f.db, zap.NewNop())
	_, err = m.RunOnce(context.Background())
	require.NoError(t, err)

	_, statErr := os.Stat(multipartDir(id))
	assert.True(t, os.IsNotExist(statErr), "the orphan staging directory is removed")
	_, statErr = os.Stat(partFilePath(live, 1))
	assert.NoError(t, statErr, "a live upload's part stays")
}
