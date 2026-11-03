package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 ETags are MD5 by definition
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-VAULT-3 PR B: the vault read order (D-V3-6). With the tenant's
// `vault_parity_serve` flag ON, a GET of a vault-floor object whose parity
// row is complete and whose leg's breaker is closed is read from the parity
// FIRST; Geyser is asked only when the parity cannot serve. OFF: today's
// order exactly. HEAD's restore lookup is bounded to 3 s (D-V3-7).

// countingGeyser counts the whole-object Gets the vault object's backend is
// asked (flakyGetDriver counts its ranges already).
type countingGeyser struct {
	*flakyGetDriver
	gets atomic.Int32
}

func (d *countingGeyser) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	d.gets.Add(1)
	return d.flakyGetDriver.Get(ctx, container, artifact)
}

// serveFixture: the parity fixture with Geyser counted, and the two flags.
func serveFixture(t *testing.T, serveFirst bool) (*parityFixture, *countingGeyser) {
	t.Helper()
	f := setupParityFixture(t)
	g := &countingGeyser{flakyGetDriver: f.geyser}
	f.eng.AddDriver("geyser", g)
	f.setServeFirst(serveFirst)
	return f, g
}

func (f *parityFixture) setServeFirst(on bool) {
	f.svc.flags = stubFlags{on: map[string]bool{
		flagVaultParity + "/" + f.tenantID:      true,
		flagVaultParityServe + "/" + f.tenantID: on,
	}}
}

// zero forgets the Gets the parity job's protect made (it reads the object
// from Geyser to encode it): the counts below are the GET's alone.
func (d *countingGeyser) zero() {
	d.gets.Store(0)
	d.rangeGets.Store(0)
}

func (f *parityFixture) servedFirst() float64 {
	return promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("served_first"))
}

func TestVaultGet_FlagOff_GeyserFirstThenParity(t *testing.T) {
	f, g := serveFixture(t, false)
	size := 1<<20 + 5
	body, _ := f.object("films/off.mkv", size)
	f.run()
	g.zero()
	first := f.servedFirst()

	w := f.get(f.adapter(), "films/off.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"), "Geyser served it")
	assert.Equal(t, int32(1), g.gets.Load(), "today's order: the object's own backend first")
	assert.Equal(t, first, f.servedFirst())

	g.failGet.Store(true)
	g.failRange.Store(true)
	w = f.get(f.adapter(), "films/off.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"), "then the parity, on a failure")
	assert.Equal(t, int32(2), g.gets.Load())
}

func TestVaultGet_FlagOn_ParityFirst_GeyserNeverAsked(t *testing.T) {
	f, g := serveFixture(t, true)
	size := 1<<20 + 77
	body, etag := f.object("films/first.mkv", size)
	f.run()
	g.zero()
	first := f.servedFirst()

	w := f.get(f.adapter(), "films/first.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	sum := md5.Sum(w.Body.Bytes()) // #nosec G401
	assert.Equal(t, etag, hex.EncodeToString(sum[:]), "the body is the object")
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Equal(t, `"`+etag+`"`, w.Header().Get("ETag"))
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"))
	assert.Equal(t, int32(0), g.gets.Load(), "Geyser was never asked")
	assert.Equal(t, int32(0), g.rangeGets.Load())
	assert.Equal(t, first+1, f.servedFirst())
}

func TestVaultGet_FlagOn_ArchivedServedFromParity(t *testing.T) {
	// On tape: with the flag on the GET is served from the parity — 200,
	// never 403 InvalidObjectState, and no recall is started by a GET.
	f, _ := serveFixture(t, true)
	body, _ := f.object("films/tape.mkv", 1<<20)
	f.run()
	f.eng.AddDriver("geyser", &archivedDriver{LocalDriver: f.geyser.LocalDriver})

	w := f.get(f.adapter(), "films/tape.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, "an archived object is served from its parity copy")
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.Empty(t, w.Header().Get("x-amz-restore"))
}

func TestVaultGet_FlagOff_ArchivedStays403(t *testing.T) {
	f, _ := serveFixture(t, false)
	f.object("films/tape-off.mkv", 1<<20)
	f.run()
	f.eng.AddDriver("geyser", &archivedDriver{LocalDriver: f.geyser.LocalDriver})

	w := f.get(f.adapter(), "films/tape-off.mkv", "")
	require.Equal(t, http.StatusForbidden, w.Code, "the restore semantics, unchanged with the flag off")
	assert.Contains(t, w.Body.String(), "InvalidObjectState")
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))
}

func TestVaultGet_FlagOn_PartialRowFallsBackToGeyser(t *testing.T) {
	f, g := serveFixture(t, true)
	body, _ := f.object("films/partial.mkv", 1<<20+3)
	f.run()
	g.zero()
	_, err := f.db.Exec(`UPDATE vault_parity SET state = 'partial' WHERE tenant_id = $1 AND object_key = 'films/partial.mkv'`, f.tenantID)
	require.NoError(t, err)
	unavailable := promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("unavailable"))

	w := f.get(f.adapter(), "films/partial.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"), "served from Geyser")
	assert.Equal(t, int32(1), g.gets.Load())
	assert.Equal(t, unavailable, promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("unavailable")),
		"a row that cannot serve is no parity failure: the parity was never tried")
}

func TestVaultGet_FlagOn_LegOpenFallsBackToGeyser(t *testing.T) {
	f, g := serveFixture(t, true)
	body, _ := f.object("films/legopen.mkv", 1<<20)
	f.run()
	g.zero()
	// Five failures through the engine open the leg's breaker.
	f.leg.failGet.Store(true)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	for i := 0; i < 5; i++ {
		_, _ = f.eng.Get(engine.WithRecordedBackend(ctx, "permafrost"), "c", "k")
	}
	require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["permafrost"])
	f.leg.failGet.Store(false)

	w := f.get(f.adapter(), "films/legopen.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))
	assert.Equal(t, int32(1), g.gets.Load(), "the leg's breaker is open: Geyser serves")
}

func TestVaultGet_FlagOn_ParityFailsBeforeTheStatusLine_GeyserServesOnce(t *testing.T) {
	// The parity is tried first and cannot give its first stripe: the read
	// falls through to Geyser, and the parity is not tried a second time —
	// one `unavailable`, then Geyser's own answer.
	f, g := serveFixture(t, true)
	body, etag := f.object("films/broken.mkv", 1<<20)
	f.run()
	g.zero()
	prefix := shardPrefix(f.bucket, "films/broken.mkv", etag)
	for j := 0; j < 4; j++ {
		require.NoError(t, os.Remove(filepath.Join(f.legDir, parityContainer(f.tenantID), shardArtifact(prefix, j))))
	}
	unavailable := promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("unavailable"))

	w := f.get(f.adapter(), "films/broken.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))
	assert.Equal(t, int32(1), g.gets.Load())
	assert.Equal(t, unavailable+1, promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("unavailable")))

	// Geyser down too: its error is the answer, the parity not retried.
	g.failGet.Store(true)
	g.failRange.Store(true)
	w = f.get(f.adapter(), "films/broken.mkv", "")
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Equal(t, unavailable+2, promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("unavailable")), "counted once per GET")
}

func TestVaultGet_FlagOn_SealedAndPlain(t *testing.T) {
	// Both branches of the GET: the plain one (the adapter) and the sealed
	// one (openSealedGet — SSE-S3 here; SSE-C takes the same branch).
	f, g := serveFixture(t, true)
	plain, _ := f.object("films/plain.mkv", 512<<10+9)
	sealed, a := f.sealedVaultObject(t, "films/sealed.mkv", 300<<10+19)
	f.run()
	g.zero()
	first := f.servedFirst()

	w := f.get(a, "films/plain.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.True(t, bytes.Equal(plain, w.Body.Bytes()))

	w = f.get(a, "films/sealed.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.True(t, bytes.Equal(sealed, w.Body.Bytes()), "the rebuilt ciphertext, opened")

	w = f.get(a, "films/sealed.mkv", "bytes=100000-200000")
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.True(t, bytes.Equal(sealed[100000:200001], w.Body.Bytes()))

	assert.Equal(t, int32(0), g.gets.Load(), "Geyser never asked")
	assert.Equal(t, int32(0), g.rangeGets.Load())
	assert.Equal(t, first+3, f.servedFirst())
}

func TestVaultGet_FlagOn_RangeFromParity(t *testing.T) {
	f, g := serveFixture(t, true)
	size := 1<<20 + 77
	body, _ := f.object("films/range.mkv", size)
	f.run()
	g.zero()

	w := f.get(f.adapter(), "films/range.mkv", "bytes=300000-799999")
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body[300000:800000], w.Body.Bytes()))
	assert.Equal(t, fmt.Sprintf("bytes 300000-799999/%d", size), w.Header().Get("Content-Range"))
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.Equal(t, int32(0), g.gets.Load())
	assert.Equal(t, int32(0), g.rangeGets.Load())

	// Unsatisfiable: the 416 is answered before any read.
	w = f.get(f.adapter(), "films/range.mkv", fmt.Sprintf("bytes=%d-", size+10))
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
}

// sealedVaultObject plants an SSE-S3 sealed vault object on Geyser (the
// model of TestVaultParity_SealedObject_RebuiltThenOpened) and returns its
// plaintext and an adapter wired with the SSE service.
func (f *parityFixture) sealedVaultObject(t *testing.T, key string, size int) ([]byte, *S3ToEngine) {
	t.Helper()
	sse, err := testSSEService(f.db, keyTestMasterA)
	require.NoError(t, err)
	sse.SetSegmentSize(4 << 10)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, f.tenantID) })
	f.svc.SSE = sse
	body := make([]byte, size)
	_, _ = rand.Read(body)
	sum := md5.Sum(body) // #nosec G401
	etag := hex.EncodeToString(sum[:])
	blob, v := sealedBlob(t, sse, crypto.StreamIdentity{Tenant: f.tenantID, Bucket: f.bucket, Key: key}, body)
	container := f.tn.NamespaceContainer(f.bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(f.geyserDir, container, filepath.Dir(key)), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(f.geyserDir, container, key), blob, 0o600))
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type,
			encryption_algorithm, encryption_key_version)
		VALUES ($1,$2,$3,$4,$5,'geyser','vault',false,'application/octet-stream',$6,$7)`,
		f.tenantID, f.bucket, key, len(body), etag, crypto.SSEAlgorithm, v)
	require.NoError(t, err)
	a := f.adapter()
	a.sseService = sse
	return body, a
}

// --- HEAD (D-V3-7) ---------------------------------------------------------------

// blockingRestorer is an archive backend whose RestoreStatus never answers
// (and ignores its context — the bound must not rely on the driver).
type blockingRestorer struct {
	*stubArchiveDriver
	release chan struct{}
}

func (d *blockingRestorer) RestoreStatus(context.Context, string, string) (*engine.RestoreStatus, error) {
	<-d.release
	return nil, errors.New("released")
}

func TestVaultHead_RestoreLookupBounded(t *testing.T) {
	f, stub, s := restoreFixture(t)
	putArchiveObject(t, f, "cold.bin", generateTestData(512))
	stub.archived = true
	blocking := &blockingRestorer{stubArchiveDriver: stub, release: make(chan struct{})}
	t.Cleanup(func() { close(blocking.release) })
	f.eng.AddDriver("geyser", blocking)
	timeouts := promtest.ToFloat64(vaultHeadRestoreLookups.WithLabelValues("timeout"))

	req := httptest.NewRequest("HEAD", "/test-bucket/cold.bin", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold.bin", TenantID: f.tenantID})
	took := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Less(t, took, vaultHeadRestoreTimeout+500*time.Millisecond, "HEAD answers within the bound")
	assert.GreaterOrEqual(t, took, vaultHeadRestoreTimeout-100*time.Millisecond)
	assert.Empty(t, w.Header().Get("x-amz-restore"), "past the bound the header is omitted, as on any failure")
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"))
	assert.Equal(t, timeouts+1, promtest.ToFloat64(vaultHeadRestoreLookups.WithLabelValues("timeout")))
}

func TestVaultHead_RestoreLookupUnchangedByTheFlag(t *testing.T) {
	// The flag gates GET only: HEAD's lookup is made, and answered, the
	// same with it on.
	f, stub, s := restoreFixture(t)
	putArchiveObject(t, f, "cold2.bin", generateTestData(512))
	stub.archived = true
	stub.restoreState = `ongoing-request="true"`
	oks := promtest.ToFloat64(vaultHeadRestoreLookups.WithLabelValues("ok"))
	for _, on := range []bool{false, true} {
		fl := flags.New(nil, zap.NewNop())
		fl.Register(flagVaultParityServe, on)
		f.adapter.flags, s.flags = fl, fl
		req := httptest.NewRequest("HEAD", "/test-bucket/cold2.bin", nil)
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		before := stub.statusCalls
		s.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold2.bin", TenantID: f.tenantID})
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, `ongoing-request="true"`, w.Header().Get("x-amz-restore"), "flag %v", on)
		assert.Equal(t, before+1, stub.statusCalls, "one live lookup, flag %v", on)
	}
	assert.Equal(t, oks+2, promtest.ToFloat64(vaultHeadRestoreLookups.WithLabelValues("ok")))
}

// RP91-3: while Geyser's breaker is open, HEAD makes no restore lookup —
// during an outage every archive-class HEAD used to cost a goroutine and a
// connection for up to the driver's bound. The header is omitted, as on any
// failure, and the skip is counted (outcome breaker_open).
func TestVaultHead_RestoreLookupSkippedWhileTheBreakerIsOpen(t *testing.T) {
	f, stub, s := restoreFixture(t)
	putArchiveObject(t, f, "cold3.bin", generateTestData(512))
	stub.archived = true
	stub.restoreState = `ongoing-request="true"`
	for i := 0; i < 10 && f.eng.GetFailoverStatus()["geyser"] != engine.StateOpen.String(); i++ {
		f.eng.RecordReadOutcome("geyser", errors.New("connection reset by peer"))
	}
	require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["geyser"])
	skipped := promtest.ToFloat64(vaultHeadRestoreLookups.WithLabelValues("breaker_open"))

	req := httptest.NewRequest("HEAD", "/test-bucket/cold3.bin", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	before := stub.statusCalls
	s.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold3.bin", TenantID: f.tenantID})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"))
	assert.Empty(t, w.Header().Get("x-amz-restore"), "no lookup: the header is omitted")
	assert.Equal(t, before, stub.statusCalls, "Geyser was not asked")
	assert.Equal(t, skipped+1, promtest.ToFloat64(vaultHeadRestoreLookups.WithLabelValues("breaker_open")))
}
