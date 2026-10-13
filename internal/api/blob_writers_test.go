package api

import (
	"context"
	"crypto/md5" // #nosec G505 -- S3 ETag semantics
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
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

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R2-1 Part C (Stage 1a Prompt 2C, migration 087): every whole-object
// write puts its bytes at a NEW address <key>@<id>, and one transaction flips
// the head row (and the version row) to it; what it displaced is retired,
// never overwritten. Red on 5632e3c (+ 087): every writer wrote in place.

// --- helpers --------------------------------------------------------------------

func (f *ledgerFixture) headAddr(bucket, key string) string {
	f.t.Helper()
	var sk string
	require.NoError(f.t, f.db.QueryRow(`SELECT COALESCE(storage_key, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, bucket, key).Scan(&sk))
	return sk
}

func (f *ledgerFixture) retired() map[string]string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT storage_key, reason FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, r string
		require.NoError(f.t, rows.Scan(&k, &r))
		out[k] = r
	}
	require.NoError(f.t, rows.Err())
	return out
}

func (f *ledgerFixture) get(bucket, key string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, f.req("GET", "/"+bucket+"/"+key, nil), bucket, key)
	return w
}

// blobsOf lists the files of a key on the hot backend: <key> and <key>@*.
func (f *ledgerFixture) blobsOf(bucket, key string) []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.hotPath(bucket, key)))
	require.NoError(f.t, err)
	var out []string
	for _, e := range entries {
		if e.Name() == key || strings.HasPrefix(e.Name(), key+"@") {
			out = append(out, e.Name())
		}
	}
	return out
}

func cleanupRetired(t *testing.T, f *ledgerFixture) {
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM job_runs WHERE job_name LIKE 'test-retired-%'`)
	})
}

// failFlipFor makes the flip of (tenant, key) fail after its upsert — a DB
// error after the backend write — `times` times.
func failFlipFor(t *testing.T, tenantID, key string, times int) {
	t.Helper()
	var mu sync.Mutex
	prev := headFlipHook
	headFlipHook = func(tn, b, k string) error {
		mu.Lock()
		defer mu.Unlock()
		if tn == tenantID && k == key && times > 0 {
			times--
			return errors.New("injected: the head-row transaction fails after the backend write")
		}
		return prev(tn, b, k)
	}
	t.Cleanup(func() { headFlipHook = prev })
}

// --- PUT ---------------------------------------------------------------------------

func TestPut_WritesNewAddress_RetiresTheOld(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)

	f.put("b", "k", "first body")
	a1 := f.headAddr("b", "k")
	f.put("b", "k", "second body")
	a2 := f.headAddr("b", "k")

	require.True(t, isBlobAddressOf("k", a1), "the first PUT is at its own address, not at <key>: %q", a1)
	require.True(t, isBlobAddressOf("k", a2), "%q", a2)
	assert.NotEqual(t, a1, a2, "every write gets a new address")
	assert.Equal(t, "second body", f.get("b", "k").Body.String())
	assert.Equal(t, "second body", f.read(f.hotPath("b", a2)))
	assert.Equal(t, "first body", f.read(f.hotPath("b", a1)), "the displaced blob stays through its grace")
	assert.Equal(t, map[string]string{a1: retiredOverwritten}, f.retired())
	assert.NotContains(t, f.blobsOf("b", "k"), "k", "nothing is written at <key>")
}

func TestPut_FailureAfterWrite_OldObjectIntact(t *testing.T) {
	// R2-05 residue: a DB error after the backend write.
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.put("b", "k", "the customer's object")
	failFlipFor(t, f.tenantID, "k", 1)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, f.req("PUT", "/b/k", []byte("a write that fails")), "b", "k")

	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	g := f.get("b", "k")
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "the customer's object", g.Body.String(), "the previous object survives a failed write")
	ret := f.retired()
	require.Len(t, ret, 1, "the failed write's blob is queued: %v", ret)
	for addr, reason := range ret {
		assert.Equal(t, retiredOrphanWrite, reason)
		assert.True(t, isBlobAddressOf("k", addr))
		assert.Equal(t, "a write that fails", f.read(f.hotPath("b", addr)))
	}
}

func TestPut_ShortBody_IncompleteBody(t *testing.T) {
	// R7-12: a body that ends cleanly short of its declared length.
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.put("b", "k", "the customer's object")

	r := f.req("PUT", "/b/k", []byte("only half"))
	r.ContentLength = 100
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, r, "b", "k")

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "IncompleteBody")
	assert.Equal(t, "the customer's object", f.get("b", "k").Body.String(), "a short body never replaces the object")
	ret := f.retired()
	require.Len(t, ret, 1)
	for _, reason := range ret {
		assert.Equal(t, retiredOrphanWrite, reason, "the truncated blob is queued, never committed")
	}
}

// pauseAfterPut is a backend whose first Put, once its bytes are stored,
// waits before returning: the window between a writer's backend write and
// its head-row transaction, held open on purpose.
type pauseAfterPut struct {
	engine.Driver
	first   atomic.Bool
	written chan struct{}
	release chan struct{}
}

func (p *pauseAfterPut) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	err := p.Driver.Put(ctx, container, artifact, data, opts...)
	if p.first.CompareAndSwap(false, true) {
		close(p.written)
		<-p.release
	}
	return err
}

func TestPut_ConcurrentSameKey_RowDescribesItsBytes(t *testing.T) {
	// R2-15: PUT A's bytes land, then PUT B writes and commits, then A
	// commits. In place, the key's one blob held B's bytes under A's row.
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	hot, _ := f.eng.GetDriver("idrive")
	p := &pauseAfterPut{Driver: hot, written: make(chan struct{}), release: make(chan struct{})}
	f.eng.AddDriver("idrive", p)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		f.adapter.HandlePut(w, f.req("PUT", "/b/race", []byte("body of PUT A")), "b", "race")
		done <- w
	}()
	<-p.written
	f.put("b", "race", "body of PUT B, the longer one") // writes and commits while A waits
	close(p.release)
	require.Equal(t, http.StatusOK, (<-done).Code)

	g := f.get("b", "race")
	require.Equal(t, http.StatusOK, g.Code)
	sum := md5.Sum(g.Body.Bytes()) // #nosec G401 -- S3 ETag semantics
	assert.Equal(t, `"`+hex.EncodeToString(sum[:])+`"`, g.Header().Get("ETag"), "the head row's ETag is the MD5 of the bytes GET serves")
	assert.Equal(t, "body of PUT A", g.Body.String(), "the last commit wins, with its own bytes")
}

func TestPut_KeyTooLong_RefusedBeforeBody(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	seg := strings.Repeat("s", 200) + "/"
	longKey := func(n int) string { // n bytes, segments the local filesystem takes
		k := strings.Repeat(seg, n/len(seg))
		return k + strings.Repeat("x", n-len(k))
	}

	r := f.req("PUT", "/b/x", nil)
	r.Body = io.NopCloser(&unreadBody{t: t})
	r.ContentLength = 10
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, r, "b", longKey(maxObjectKeyBytes+1))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "KeyTooLongError")

	f.put("b", longKey(maxObjectKeyBytes), "at the limit")
	assert.Equal(t, 857, maxObjectKeyBytes)

	// A Sync-tier bucket: the leaf plus the address must fit the bridge's
	// 248-character name (with the 2-character leaf marker).
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, tier_preference) VALUES ($1, 's', 'private', 'sync')`, f.tenantID)
	require.NoError(t, err)
	r = f.req("PUT", "/s/x", nil)
	r.Body = io.NopCloser(&unreadBody{t: t})
	r.ContentLength = 10
	w = httptest.NewRecorder()
	f.adapter.HandlePut(w, r, "s", "dir/"+strings.Repeat("l", 220))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "KeyTooLongError")
	w = httptest.NewRecorder()
	f.adapter.HandlePut(w, f.req("PUT", "/s/x", []byte("fits")), "s", "dir/"+strings.Repeat("l", 219))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// unreadBody fails the test when the handler reads the body.
type unreadBody struct{ t *testing.T }

func (r *unreadBody) Read([]byte) (int, error) {
	r.t.Error("the body was read before the key was refused")
	return 0, io.EOF
}

// --- CompleteMultipartUpload ---------------------------------------------------------

func (f *ledgerFixture) startUpload(bucket, key, part string) (uploadID, completeBody string) {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(w, f.req("POST", "/"+bucket+"/"+key+"?uploads", nil), bucket, key)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(f.t, xml.Unmarshal(w.Body.Bytes(), &init))
	f.t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	w = httptest.NewRecorder()
	f.server.handleUploadPart(w, f.req("PUT", "/"+bucket+"/"+key+"?partNumber=1&uploadId="+init.UploadID, []byte(part)), bucket, key)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	return init.UploadID, fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`,
		w.Header().Get("ETag"))
}

func (f *ledgerFixture) complete(bucket, key, uploadID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.server.handleCompleteMultipartUpload(w, f.req("POST", "/"+bucket+"/"+key+"?uploadId="+uploadID, []byte(body)), bucket, key)
	return w
}

func TestComplete_NewAddress_RetryReusesIt(t *testing.T) {
	// R3-15 + the replay: a complete whose head row fails after the
	// assembled bytes landed; the retry writes the SAME address.
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.put("b", "big.bin", "the object before the upload")
	uploadID, body := f.startUpload("b", "big.bin", "multipart bytes")
	failFlipFor(t, f.tenantID, "big.bin", 1)

	w := f.complete("b", "big.bin", uploadID, body)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Equal(t, "the object before the upload", f.get("b", "big.bin").Body.String(), "R3-15: the failed complete destroyed nothing")
	var chosen string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(storage_key, '') FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&chosen))
	require.True(t, isBlobAddressOf("big.bin", chosen), "the upload records its address before the write: %q", chosen)

	// The collector keeps the address while the upload is active.
	c := f.collector()
	c.now = func() time.Time { return time.Now().Add(time.Minute) }
	_, err := c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "multipart bytes", f.read(f.hotPath("b", chosen)), "an active upload's address is never collected")

	w = f.complete("b", "big.bin", uploadID, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, chosen, f.headAddr("b", "big.bin"), "the retry reuses the address")
	assert.Equal(t, "multipart bytes", f.get("b", "big.bin").Body.String())
	addrs := 0
	for _, b := range f.blobsOf("b", "big.bin") {
		if b != chosen && f.retired()[b] == "" {
			t.Errorf("a blob no row and no queue knows: %s", b)
		}
		addrs++
	}
	assert.Equal(t, 2, addrs, "the earlier object (retired) and the one completed address")

	// A third complete of the completed upload answers the same result.
	w = f.complete("b", "big.bin", uploadID, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, chosen, f.headAddr("b", "big.bin"))
}

// --- CopyObject and generated objects -------------------------------------------------

func TestCopy_NewAddress(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.put("b", "src", "source bytes")
	f.put("b", "dest", "old destination")
	oldDest := f.headAddr("b", "dest")

	r := f.req("PUT", "/b/dest", nil)
	r.Header.Set("x-amz-copy-source", "/b/src")
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, r, &S3Request{Bucket: "b", Object: "dest", TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	addr := f.headAddr("b", "dest")
	require.True(t, isBlobAddressOf("dest", addr), "%q", addr)
	assert.NotEqual(t, oldDest, addr)
	assert.Equal(t, "source bytes", f.read(f.hotPath("b", addr)))
	assert.Equal(t, retiredOverwritten, f.retired()[oldDest])

	// A copy onto its own key (a metadata change) reads the old address and
	// writes a new one: never the same file for both.
	srcAddr := f.headAddr("b", "src")
	r = f.req("PUT", "/b/src", nil)
	r.Header.Set("x-amz-copy-source", "/b/src")
	r.Header.Set("x-amz-metadata-directive", "REPLACE")
	r.Header.Set("Content-Type", "text/plain")
	w = httptest.NewRecorder()
	f.server.handleCopyObject(w, r, &S3Request{Bucket: "b", Object: "src", TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEqual(t, srcAddr, f.headAddr("b", "src"))
	assert.Equal(t, "source bytes", f.get("b", "src").Body.String())
}

func TestGeneratedObject_NewAddress(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'b', 'private')`, f.tenantID)
	require.NoError(t, err)
	g := newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop())
	write := func(body string) {
		_, err := g.write(context.Background(), generatedKindInventory, f.tenantID, "b", "inventory.csv", "text/csv",
			func(w io.Writer) error { _, err := io.WriteString(w, body); return err })
		require.NoError(t, err)
	}

	write("day one")
	a1 := f.headAddr("b", "inventory.csv")
	write("day two")
	a2 := f.headAddr("b", "inventory.csv")

	require.True(t, isBlobAddressOf("inventory.csv", a1), "%q", a1)
	require.True(t, isBlobAddressOf("inventory.csv", a2), "%q", a2)
	assert.NotEqual(t, a1, a2)
	assert.Equal(t, "day two", f.get("b", "inventory.csv").Body.String())
	assert.Equal(t, retiredOverwritten, f.retired()[a1])
}

// --- R13-06 -------------------------------------------------------------------------

func TestReclaim_NeverDeletesAReput(t *testing.T) {
	// Demoted, deleted, and re-PUT while the reclaim runs: the reclaim's
	// delete of the stale hot copy lands between the PUT's backend write and
	// its head-row commit. The new bytes are at their own address.
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.blob(f.hotDir, "b", "k", "orphan hot copy")
	f.openLedger("b", "k", "old-etag", 48*time.Hour)
	done := make(chan *httptest.ResponseRecorder, 1)
	f.runner.beforeHotDelete = func(bucket, key string) {
		go func() {
			w := httptest.NewRecorder()
			f.adapter.HandlePut(w, f.req("PUT", "/b/k", []byte("the customer's new upload")), "b", "k")
			done <- w
		}()
		// Wait until the PUT's bytes are on the backend (wherever it puts them).
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, n := range f.blobsOf(bucket, key) {
				if f.read(f.hotPath(bucket, n)) == "the customer's new upload" {
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	_, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	w := <-done
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	g := f.get("b", "k")
	require.Equal(t, http.StatusOK, g.Code, "R13-06: the reclaim deleted the re-PUT's bytes")
	assert.Equal(t, "the customer's new upload", g.Body.String())
}

// --- versions ---------------------------------------------------------------------

func (f *ledgerFixture) versioned(bucket string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status) VALUES ($1, $2, 'private', 'Enabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'Enabled'`, f.tenantID, bucket)
	require.NoError(f.t, err)
}

func (f *ledgerFixture) putV(bucket, key, body string) string {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, f.req("PUT", "/"+bucket+"/"+key, []byte(body)), bucket, key)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	vid := w.Header().Get("x-amz-version-id")
	require.NotEmpty(f.t, vid)
	return vid
}

func (f *ledgerFixture) getV(bucket, key, vid string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, f.req("GET", "/"+bucket+"/"+key+"?versionId="+vid, nil), bucket, key)
	return w
}

func TestVersioned_OverwriteKeepsV1Bytes(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.versioned("b")
	v1 := f.putV("b", "k", "version one")
	f.putV("b", "k", "version two, longer")

	w := f.getV("b", "k", v1)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "version one", w.Body.String(), "R2-03: a non-current version's bytes are retained")
	assert.Equal(t, "version two, longer", f.get("b", "k").Body.String())
	assert.Empty(t, f.retired(), "the displaced blob is the previous version's: kept")
}

// A version written in place before Part C (bytes at <key>, no address):
// the first Part C overwrite no longer destroys it, so its row is given
// <key> as its address and stays readable.
func TestVersioned_LegacyVersionKeepsItsBytesAtTheKey(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.versioned("b")
	v1 := f.putV("b", "k", "written in place")
	a1 := f.headAddr("b", "k")
	// The pre-C shape: the blob at <key>, no address on either row.
	require.NoError(t, os.Rename(f.hotPath("b", a1), f.hotPath("b", "k")))
	_, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = NULL WHERE tenant_id = $1 AND object_key = 'k'`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE object_versions SET storage_key = NULL WHERE tenant_id = $1 AND object_key = 'k'`, f.tenantID)
	require.NoError(t, err)

	f.putV("b", "k", "the first Part C write")

	w := f.getV("b", "k", v1)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "written in place", w.Body.String())
	assert.Empty(t, f.retired(), "the blob at <key> is the old version's now: kept")
}

func TestDeleteCurrentVersion_RepointsHead(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.versioned("b")
	v1 := f.putV("b", "k", "version one")
	v2 := f.putV("b", "k", "version two, longer")
	a2 := f.headAddr("b", "k")

	del := func(vid string) {
		w := httptest.NewRecorder()
		f.adapter.HandleDelete(w, f.req("DELETE", "/b/k?versionId="+vid, nil), "b", "k")
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	}
	del(v2)

	g := f.get("b", "k")
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, "version one", g.Body.String(), "the head row follows the newly-latest version")
	assert.Equal(t, `"`+etagOf([]byte("version one"))+`"`, g.Header().Get("ETag"))
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'k'`, f.tenantID).Scan(&size))
	assert.Equal(t, int64(len("version one")), size)
	assert.Equal(t, retiredVersionDeleted, f.retired()[a2])

	del(v1)
	assert.Equal(t, http.StatusNotFound, f.get("b", "k").Code, "no version is left: no head row")
}

// --- the collector --------------------------------------------------------------------

func (f *ledgerFixture) collector() *RetiredBlobCollector {
	c := NewRetiredBlobCollector(f.db, f.eng, zap.NewNop())
	c.onlyTenant = f.tenantID
	c.JobName = "test-retired-" + f.tenantID
	return c
}

func (f *ledgerFixture) retire(backend, bucket, addr string, after time.Duration) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ($1, $2, $3, $4, 'overwritten', NOW() + make_interval(secs => $5))`,
		backend, f.tenantID+"_"+bucket, addr, f.tenantID, after.Seconds())
	require.NoError(f.t, err)
}

func TestRetiredBlobs_Collector_DueOnly(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.put("b", "k", "one")
	a1 := f.headAddr("b", "k")
	f.put("b", "k", "two")
	f.blob(f.hotDir, "b", "later@00000000000000000000000000", "not yet")
	f.retire("idrive", "b", "later@00000000000000000000000000", time.Hour)
	_, err := f.db.Exec(`UPDATE retired_blobs SET retire_after = NOW() - interval '1 second' WHERE storage_key = $1`, a1)
	require.NoError(t, err)

	res, err := f.collector().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Deleted, "%+v", res)
	assert.False(t, f.hotExists("b", a1), "the due blob is deleted")
	assert.Equal(t, "not yet", f.read(f.hotPath("b", "later@00000000000000000000000000")), "a blob inside its grace is not")
	assert.Equal(t, map[string]string{"later@00000000000000000000000000": "overwritten"}, f.retired())
	assert.Equal(t, "two", f.get("b", "k").Body.String())
}

// An old slot (Part B) writes in place at <key> while a retired row for
// <key> is due: the head row names <key> again — the collector keeps it.
func TestRetiredBlobs_Collector_StillNamedKept(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.put("b", "k", "addressed")
	addr := f.headAddr("b", "k")
	f.blob(f.hotDir, "b", "k", "an old slot's in-place write")
	_, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = NULL WHERE tenant_id = $1 AND object_key = 'k'`, f.tenantID)
	require.NoError(t, err)
	f.retire("idrive", "b", "k", -time.Second)
	f.retire("idrive", "b", addr, -time.Second)

	res, err := f.collector().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Kept, "%+v", res)
	assert.Equal(t, 1, res.Deleted, "%+v", res)
	assert.Equal(t, "an old slot's in-place write", f.read(f.hotPath("b", "k")), "a blob the head row names is never collected")
	assert.False(t, f.hotExists("b", addr))
	assert.Empty(t, f.retired())
}

func TestRetiredBlobs_Collector_OpenBreakerSkipped(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	down := &hungDriver{LocalDriver: drivers.NewLocalDriver(t.TempDir(), zap.NewNop())}
	f.eng.AddDriver("lyve", down)
	container := f.tenantID + "_b"
	for i := 0; i < 10 && f.eng.GetFailoverStatus()["lyve"] == "closed"; i++ {
		f.eng.HintBackend(container, "probe", "lyve")
		_, _ = f.eng.Get(common.WithTenantID(context.Background(), f.tenantID), container, "probe")
	}
	require.Equal(t, "open", f.eng.GetFailoverStatus()["lyve"], "fixture: the breaker must be open")
	f.retire("lyve", "b", "x@00000000000000000000000000", -time.Second)
	f.retire("gone", "b", "y@00000000000000000000000000", -time.Second)

	c := f.collector()
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err, "an open breaker or an unregistered backend is never a failed run")
	assert.Equal(t, 2, res.Skipped, "%+v", res)
	assert.ElementsMatch(t, []string{"lyve", "gone"}, res.SkippedBackends)
	assert.Equal(t, int32(0), down.deletes.Load(), "no call to a backend whose breaker is open")
	assert.Len(t, f.retired(), 2, "the rows wait")

	// The job records it as a note on an ok run.
	sp := c.spec()
	rep, err := sp.Run(context.Background())
	require.NoError(t, err)
	assert.Contains(t, rep.Note, "skipped")
	b, _ := json.Marshal(rep.Result)
	assert.Contains(t, string(b), `"skipped":2`)
}

// --- direct upload ------------------------------------------------------------------

func TestDirectUpload_PresignsAndCommitsTheAddress(t *testing.T) {
	f := setupVersioningFixture(t)
	stub := &stubR2{Driver: drivers.NewLocalDriver(t.TempDir(), f.server.logger)}
	f.eng.AddDriver("r2", stub)
	_, err := f.db.Exec(`UPDATE buckets SET visibility = 'public-read' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID) })
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	container := f.tenantID + "_" + f.bucket

	upload := func(body string) string {
		w := f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads", `{"key":"img/a.png"}`, f.server.handleMgmtCreateDirectUpload)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var created mgmtDirectUpload
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
		require.True(t, isBlobAddressOf("img/a.png", created.StorageKey), "%q", created.StorageKey)
		assert.Contains(t, created.URL, container+"/"+created.StorageKey, "the presign targets the address")
		require.NoError(t, stub.Driver.Put(ctx, container, created.StorageKey, strings.NewReader(body)))
		w = f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads/complete",
			`{"key":"img/a.png","storage_key":"`+created.StorageKey+`"}`, f.server.handleMgmtCompleteDirectUpload)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return created.StorageKey
	}
	a1 := upload("first")
	a2 := upload("second")

	var sk, backend string
	require.NoError(t, f.db.QueryRow(`SELECT storage_key, backend_name FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'img/a.png'`,
		f.tenantID, f.bucket).Scan(&sk, &backend))
	assert.Equal(t, a2, sk)
	assert.Equal(t, "r2", backend)
	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT reason FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, a1).Scan(&reason))
	assert.Equal(t, retiredOverwritten, reason, "the displaced upload is retired, not left for the sweep")
}

func TestDirectUpload_RejectsAForeignStorageKey(t *testing.T) {
	f := setupVersioningFixture(t)
	stub := &stubR2{Driver: drivers.NewLocalDriver(t.TempDir(), f.server.logger)}
	f.eng.AddDriver("r2", stub)
	_, err := f.db.Exec(`UPDATE buckets SET visibility = 'public-read' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	container := f.tenantID + "_" + f.bucket
	require.NoError(t, stub.Driver.Put(ctx, container, "other.png@0000000000000000000000000a", strings.NewReader("not yours")))
	require.NoError(t, stub.Driver.Put(ctx, container, "img/a.png", strings.NewReader("the key itself")))

	for _, sk := range []string{"", "img/a.png", "other.png@0000000000000000000000000a", "img/a.png@short",
		"img/a.png@0000000000000000000000000I", "img/a.png@0000000000000000000000000a/../x"} {
		w := f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads/complete",
			`{"key":"img/a.png","storage_key":"`+sk+`"}`, f.server.handleMgmtCompleteDirectUpload)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%q: %s", sk, w.Body.String())
		assert.Contains(t, w.Body.String(), "invalid_storage_key", sk)
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, f.bucket).Scan(&n))
	assert.Equal(t, 0, n)
}

// --- the address ------------------------------------------------------------------

func TestNewBlobAddress_Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		a, err := newBlobAddress("dir/k.txt")
		require.NoError(t, err)
		require.True(t, isBlobAddressOf("dir/k.txt", a), a)
		require.Len(t, a, len("dir/k.txt")+27)
		require.False(t, seen[a])
		seen[a] = true
	}
	assert.False(t, isBlobAddressOf("k", "k"))
	assert.False(t, isBlobAddressOf("k", "j@0000000000000000000000000a"))
	assert.Equal(t, "a@b", retiredBlobKey("a@b"))
	assert.Equal(t, "x@y", retiredBlobKey("x@y@0000000000000000000000000a"))
}

// blobPath is where a local driver rooted at dir holds the bytes the head
// row of (tenant, bucket, key) names: <dir>/<tenant>_<bucket>/<address>.
// Without a head row (a test removed it), the one file of the key there:
// <key> or <key>@<id>.
func blobPath(t *testing.T, db *sql.DB, dir, tenantID, bucket, key string) string {
	t.Helper()
	base := filepath.Join(dir, tenantID+"_"+bucket)
	var addr string
	err := db.QueryRow(`SELECT COALESCE(storage_key, object_key) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, tenantID, bucket, key).Scan(&addr)
	if err == nil {
		return filepath.Join(base, addr)
	}
	require.ErrorIs(t, err, sql.ErrNoRows)
	matches, _ := filepath.Glob(filepath.Join(base, key+"@*"))
	if len(matches) == 1 {
		return matches[0]
	}
	return filepath.Join(base, key)
}

// noBlobOf is os.ErrNotExist when a local driver rooted at dir holds no
// blob of the key — neither <key> nor any <key>@<id>.
func noBlobOf(dir, tenantID, bucket, key string) error {
	base := filepath.Join(dir, tenantID+"_"+bucket)
	if m, _ := filepath.Glob(filepath.Join(base, key+"@*")); len(m) > 0 {
		return nil
	}
	_, err := os.Stat(filepath.Join(base, key))
	return err
}
