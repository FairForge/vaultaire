package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
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

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R13-1 (Review R13-04): the class a customer sees comes from the floor
// the object is billed on, not from the backend that holds its bytes today.
// A Smart-demoted Standard object routes to the cold backend and used to
// list and HEAD as GLACIER, so `aws s3 sync` / `cp` skipped it.

// classFixture is the demotion fixture plus a Server and the tenant the
// handlers need. The cold backend is the archive stub, so eviction to tape
// and backend round trips can be observed.
type classFixture struct {
	*demotionFixture
	stub   *stubArchiveDriver
	server *Server
	tn     *tenant.Tenant
	p      *SmartPromoter
}

func setupClassFixture(t *testing.T) *classFixture {
	t.Helper()
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	stub := newStubArchiveDriver()
	f.eng.AddDriver("geyser", stub)
	s := &Server{engine: f.eng, db: f.db, logger: zap.NewNop(), smartPromoter: p}
	return &classFixture{
		demotionFixture: f, stub: stub, server: s, p: p,
		tn: &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"},
	}
}

// demoted plants an object on the hot backend and demotes it; with reclaimed
// the hot copy is gone and the ledger row is closed, as after the grace.
func (f *classFixture) demoted(bucket, key string, reclaimed bool) {
	f.t.Helper()
	f.object(bucket, key, 100, 30, 20)
	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(f.t, err)
	require.Equal(f.t, 1, res.Demoted, "%+v", res)
	require.Equal(f.t, "geyser", f.backendOf(bucket, key))
	if reclaimed {
		require.NoError(f.t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_"+bucket, key)))
		_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at = NOW(), hot_outcome = 'deleted'
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, bucket, key)
		require.NoError(f.t, err)
	}
}

// vaultObject plants an attic object: written with an archive class, so it
// is billed on the vault floor and lives on the cold backend with no ledger.
func (f *classFixture) vaultObject(bucket, key string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor)
		VALUES ($1, $2, $3, 9, $4, 'geyser', 'vault')`, f.tenantID, bucket, key, "etag-"+key)
	require.NoError(f.t, err)
	require.NoError(f.t, f.stub.Put(context.Background(), f.tenantID+"_"+bucket, key, strings.NewReader("attic:"+key)))
}

func (f *classFixture) req(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(tenant.WithTenant(r.Context(), f.tn))
}

func (f *classFixture) floorOf(bucket, key string) string {
	var fl string
	require.NoError(f.t, f.db.QueryRow(`SELECT floor FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, bucket, key).Scan(&fl))
	return fl
}

func TestCustomerStorageClass_AgreesWithTheFloorIDs(t *testing.T) {
	// The engine cannot import internal/usage; this pins its floor id to the
	// one the write path records.
	assert.Equal(t, "GLACIER", engine.CustomerStorageClass(usage.FloorVault, "geyser"))
	assert.Equal(t, "STANDARD", engine.CustomerStorageClass(usage.FloorStandard, "geyser"))
	assert.Equal(t, usage.FloorVault, usage.FloorOf("GLACIER"))
}

func TestDemotedStandardObject_ListsAsStandard(t *testing.T) {
	// Arrange
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.object("b", "new.jpg", 100, 1, 0)
	f.vaultObject("b", "tax-2019.zip")
	require.Equal(t, usage.FloorStandard, f.floorOf("b", "old.jpg"), "demotion never changes the floor")

	// Act
	adapter := NewS3ToEngine(f.eng, f.db, zap.NewNop())
	w := httptest.NewRecorder()
	adapter.HandleListV2(w, f.req("GET", "/b?list-type=2"), "b")

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res ListBucketV2Result
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	classes := map[string]string{}
	for _, e := range res.Contents {
		classes[e.Key] = e.StorageClass
	}
	assert.Equal(t, "STANDARD", classes["old.jpg"], "a demoted downstairs object is STANDARD whatever backend holds it")
	assert.Equal(t, "STANDARD", classes["new.jpg"])
	assert.Equal(t, "GLACIER", classes["tax-2019.zip"], "an attic object keeps the archive class")
}

func TestDemotedStandardObject_ListVersionsIsStandard(t *testing.T) {
	// Arrange: demoted while the bucket was unversioned, versioning enabled
	// later — the object surfaces as the "null" version from its head row.
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.vaultObject("b", "attic.zip")
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, versioning_status) VALUES ($1, 'b', 'Enabled')`, f.tenantID)
	require.NoError(t, err)

	// Act
	w := httptest.NewRecorder()
	f.server.handleListObjectVersions(w, f.req("GET", "/b?versions"), &S3Request{Bucket: "b", TenantID: f.tenantID, Query: map[string]string{}})

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res ListVersionsResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	classes := map[string]string{}
	for _, v := range res.Versions {
		classes[v.Key] = v.StorageClass
	}
	assert.Equal(t, "STANDARD", classes["old.jpg"])
	assert.Equal(t, "GLACIER", classes["attic.zip"])
}

func TestListObjectVersions_LatestVersionRowFollowsTheHeadRowFloor(t *testing.T) {
	// Arrange: a version row whose recorded backend is the cold one, twice —
	// once for an attic object, once for a downstairs object (demoted, then
	// versioning was enabled and a row written for the same bytes).
	f := setupClassFixture(t)
	f.demoted("b", "down.bin", true)
	f.vaultObject("b", "up.bin")
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, versioning_status) VALUES ($1, 'b', 'Enabled')`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID) })
	for _, k := range []string{"down.bin", "up.bin"} {
		_, err := f.db.Exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, is_latest, backend_name)
			VALUES ($1, 'b', $2, $3, 9, 'e', TRUE, 'geyser')`, f.tenantID, k, "v-"+k)
		require.NoError(t, err)
	}

	// Act
	w := httptest.NewRecorder()
	f.server.handleListObjectVersions(w, f.req("GET", "/b?versions"), &S3Request{Bucket: "b", TenantID: f.tenantID, Query: map[string]string{}})

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res ListVersionsResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	classes := map[string]string{}
	for _, v := range res.Versions {
		classes[v.Key] = v.StorageClass
	}
	assert.Equal(t, "STANDARD", classes["down.bin"])
	assert.Equal(t, "GLACIER", classes["up.bin"])
}

func TestDemotedStandardObject_HeadMakesNoBackendCall(t *testing.T) {
	// Arrange
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.vaultObject("b", "attic.zip")
	f.stub.restoreState = `ongoing-request="true"`

	// Act: HEAD the demoted downstairs object.
	w := httptest.NewRecorder()
	f.server.handleHeadObject(w, f.req("HEAD", "/b/old.jpg"), &S3Request{Bucket: "b", Object: "old.jpg", TenantID: f.tenantID})

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "STANDARD", w.Header().Get("x-amz-storage-class"))
	assert.Empty(t, w.Header().Get("x-amz-restore"), "a STANDARD object has no restore state")
	assert.Equal(t, 0, f.stub.statusCalls, "HEAD of a downstairs object never touches the backend")

	// The attic object keeps the one deliberate round trip.
	w = httptest.NewRecorder()
	f.server.handleHeadObject(w, f.req("HEAD", "/b/attic.zip"), &S3Request{Bucket: "b", Object: "attic.zip", TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"))
	assert.Equal(t, `ongoing-request="true"`, w.Header().Get("x-amz-restore"))
	assert.Equal(t, 1, f.stub.statusCalls)
}

func TestDemotedStandardObject_GetServedFromColdSaysStandard(t *testing.T) {
	// Arrange: hot copy reclaimed, bytes still on the archive's staging disk.
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.p.sync = false // the copy-back must not flip routing before the response is written
	// It runs on its own goroutine and writes into the fixture's hot dir:
	// the test ends only when it has (TempDir's cleanup raced it — 1 run in 40).
	t.Cleanup(func() {
		assert.Eventually(t, func() bool {
			busy := false
			f.p.inflight.Range(func(_, _ any) bool { busy = true; return false })
			return !busy
		}, 10*time.Second, 5*time.Millisecond, "the async copy-back never finished")
	})

	// Act
	w := httptest.NewRecorder()
	f.server.handleGetObject(w, f.req("GET", "/b/old.jpg"), &S3Request{Bucket: "b", Object: "old.jpg", TenantID: f.tenantID})

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "blob:old.jpg", w.Body.String())
	assert.Equal(t, "STANDARD", w.Header().Get("x-amz-storage-class"))
}

// serveCounted runs a handler behind the logging middleware and reports how
// many responses the 5xx series counted.
func (f *classFixture) serveCounted(h http.HandlerFunc, r *http.Request) (*httptest.ResponseRecorder, int64) {
	before := atomic.LoadInt64(&f.server.errorCount)
	w := httptest.NewRecorder()
	f.server.loggingMiddleware(h).ServeHTTP(w, r)
	return w, atomic.LoadInt64(&f.server.errorCount) - before
}

func TestDemotedStandardObject_EvictedGetIs503AndNotAServerError(t *testing.T) {
	// Arrange: demoted, reclaimed, and the cold backend has moved it to tape.
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.stub.archived = true
	get := func(w http.ResponseWriter, r *http.Request) {
		f.server.handleGetObject(w, r, &S3Request{Bucket: "b", Object: "old.jpg", TenantID: f.tenantID})
	}

	// Act
	w, counted := f.serveCounted(get, f.req("GET", "/b/old.jpg"))

	// Assert: retryable, says what is happening, and is one object's state —
	// not a failure of the service (the 5xx page must not read it).
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "120", w.Header().Get("Retry-After"))
	assert.Contains(t, w.Body.String(), "<Code>ServiceUnavailable</Code>")
	assert.Contains(t, w.Body.String(), "being brought back from cold storage")
	assert.NotContains(t, w.Body.String(), "storage backends are temporarily unavailable", "the backend is fine; the message must not say otherwise")
	assert.NotEqual(t, "GLACIER", w.Header().Get("x-amz-storage-class"))
	assert.GreaterOrEqual(t, f.stub.restoreCalls, 1, "the restore is submitted on the reader's behalf")
	assert.Equal(t, int64(0), counted, "vaultaire_errors_total must not move")

	// A restore the backend refuses is a server-side failure: still a
	// retryable 503, and it counts.
	f.stub.mu.Lock()
	f.stub.restoreErr = fmt.Errorf("geyser restore: 500 internal")
	f.stub.mu.Unlock()
	_, err := f.db.Exec(`UPDATE smart_demotions SET restore_requested_at = NULL WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	w, counted = f.serveCounted(get, f.req("GET", "/b/old.jpg"))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Equal(t, int64(1), counted, "a failed restore request is ours and must be visible")

	// Both answers are on /metrics (no tenant label): the refusals that
	// vaultaire_errors_total no longer shows are counted here.
	mw := httptest.NewRecorder()
	f.server.handleMetrics(mw, httptest.NewRequest("GET", "/metrics", nil))
	assert.Contains(t, mw.Body.String(), `vaultaire_smart_restore_waits_total{result="restoring"}`)
	assert.Contains(t, mw.Body.String(), `vaultaire_smart_restore_waits_total{result="failed"}`)
	assert.NotContains(t, mw.Body.String(), f.tenantID)
}

func TestStandardFloorObjectOnTapeWithoutLedger_GetStillAutoRestores(t *testing.T) {
	// Arrange: a downstairs object on the cold backend with no ledger row
	// (lost row, or a deployment whose primary is the archive). It lists as
	// STANDARD and RestoreObject refuses it, so the GET must not answer
	// "request a restore" — that would be a dead end.
	f := setupClassFixture(t)
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor)
		VALUES ($1, 'b', 'stray.bin', 9, 'e-stray', 'geyser', 'standard')`, f.tenantID)
	require.NoError(t, err)
	require.NoError(t, f.stub.Put(context.Background(), f.tenantID+"_b", "stray.bin", strings.NewReader("123456789")))
	f.stub.archived = true

	// Act
	w, counted := f.serveCounted(func(w http.ResponseWriter, r *http.Request) {
		f.server.handleGetObject(w, r, &S3Request{Bucket: "b", Object: "stray.bin", TenantID: f.tenantID})
	}, f.req("GET", "/b/stray.bin"))

	// Assert
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "120", w.Header().Get("Retry-After"))
	assert.Equal(t, 1, f.stub.restoreCalls)
	assert.Equal(t, int64(0), counted)
}

func TestVaultObject_EvictedGetKeepsGlacierSemantics(t *testing.T) {
	f := setupClassFixture(t)
	f.vaultObject("b", "attic.zip")
	f.stub.archived = true

	w := httptest.NewRecorder()
	f.server.handleGetObject(w, f.req("GET", "/b/attic.zip"), &S3Request{Bucket: "b", Object: "attic.zip", TenantID: f.tenantID})

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>InvalidObjectState</Code>")
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"))
	assert.Equal(t, 0, f.stub.restoreCalls, "an attic restore is the customer's call")
}

func TestRestoreObject_DemotedStandardObjectAnswersLikeAStandardObject(t *testing.T) {
	// Arrange
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.stub.archived = true

	// Act
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/b/old.jpg?restore", strings.NewReader(`<RestoreRequest><Days>2</Days></RestoreRequest>`))
	r = r.WithContext(tenant.WithTenant(r.Context(), f.tn))
	f.server.handleRestoreObject(w, r, &S3Request{Bucket: "b", Object: "old.jpg", TenantID: f.tenantID})

	// Assert: AWS's answer for RestoreObject on a STANDARD object, and no
	// backend call — reading the object is what brings it back.
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>InvalidObjectState</Code>")
	assert.Contains(t, w.Body.String(), "STANDARD")
	assert.Equal(t, 0, f.stub.restoreCalls)
}

func TestCopyObject_DemotedEvictedSourceIs503AndAutoRestores(t *testing.T) {
	// Arrange
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.vaultObject("b", "attic.zip")
	f.stub.archived = true
	cp := func(src string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("x-amz-copy-source", "/b/"+src)
			f.server.handleCopyObject(w, r, &S3Request{Bucket: "b", Object: "copy-of-" + src, TenantID: f.tenantID})
		}
	}

	// Act
	w, counted := f.serveCounted(cp("old.jpg"), f.req("PUT", "/b/copy-of-old.jpg"))

	// Assert: it used to be a 500.
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "120", w.Header().Get("Retry-After"))
	assert.Contains(t, w.Body.String(), "being brought back from cold storage")
	assert.Equal(t, 1, f.stub.restoreCalls)
	assert.Equal(t, int64(0), counted)

	// An attic source answers what AWS answers for a GLACIER copy source.
	w, counted = f.serveCounted(cp("attic.zip"), f.req("PUT", "/b/copy-of-attic.zip"))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>InvalidObjectState</Code>")
	assert.Equal(t, 1, f.stub.restoreCalls)
	assert.Equal(t, int64(0), counted)
}

// cdnRouter serves a public bucket of the fixture tenant through the CDN
// handler behind the logging middleware.
func (f *classFixture) cdnRouter(bucket string) (http.Handler, string) {
	f.t.Helper()
	slug := "slug-" + f.tenantID
	_, err := f.db.Exec(`UPDATE tenants SET slug = $2 WHERE id = $1`, f.tenantID, slug)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'public-read')
		ON CONFLICT (tenant_id, name) DO UPDATE SET visibility = 'public-read'`, f.tenantID, bucket)
	require.NoError(f.t, err)
	router := chi.NewRouter()
	router.Use(f.server.loggingMiddleware)
	router.Get("/cdn/{slug}/{bucket}/*", f.server.handleCDNRequest)
	return router, "/cdn/" + slug + "/" + bucket + "/"
}

func TestCDN_DemotedObjectIsServedAndComesBackHot(t *testing.T) {
	// Arrange: demoted yesterday — the hot copy is still inside its grace.
	f := setupClassFixture(t)
	f.demoted("pub", "logo.png", false)
	router, base := f.cdnRouter("pub")

	// Act
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", base+"logo.png", nil))

	// Assert: a public read is a read — it promotes like an S3 GET.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "blob:logo.png", string(body))
	assert.Equal(t, "idrive", f.backendOf("pub", "logo.png"), "flipped back by the public read")
}

func TestCDN_ReadKeepsAnObjectFromGoingIdle(t *testing.T) {
	// Arrange: an object served only through the CDN, last touched 20 days ago.
	f := setupClassFixture(t)
	f.object("pub", "hero.jpg", 100, 30, 20)
	router, base := f.cdnRouter("pub")

	// Act
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", base+"hero.jpg", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Assert: the read is recorded (at most once a day per object), so the
	// demotion job does not move an object the public is reading.
	require.Eventually(t, func() bool {
		var idle bool
		_ = f.db.QueryRow(`SELECT last_accessed < NOW() - INTERVAL '1 hour' FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = 'pub' AND object_key = 'hero.jpg'`, f.tenantID).Scan(&idle)
		return !idle
	}, 5*time.Second, 20*time.Millisecond)
}

func TestCDN_DemotedEvictedObjectIs503NotA500(t *testing.T) {
	// Arrange
	f := setupClassFixture(t)
	f.demoted("pub", "logo.png", true)
	f.stub.archived = true
	router, base := f.cdnRouter("pub")
	before := atomic.LoadInt64(&f.server.errorCount)

	// Act
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", base+"logo.png", nil))

	// Assert
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "120", w.Header().Get("Retry-After"))
	assert.Empty(t, w.Header().Get("Content-Length"), "the object's length must not frame the error body")
	assert.Empty(t, w.Header().Get("ETag"))
	assert.Contains(t, w.Header().Get("Cache-Control"), "no-store", "an edge must not cache the wait")
	assert.GreaterOrEqual(t, f.stub.restoreCalls, 1)
	assert.Equal(t, int64(0), atomic.LoadInt64(&f.server.errorCount)-before)
}

func TestInventoryCSV_ReportsTheCustomerClassNotTheBackend(t *testing.T) {
	// Arrange
	f := setupClassFixture(t)
	f.demoted("b", "old.jpg", true)
	f.vaultObject("b", "attic.zip")
	for _, b := range []string{"b", "inv"} {
		_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, f.tenantID, b)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID) })
	runner := NewInventoryRunner(f.db, f.eng, zap.NewNop())
	runner.SetWriter(newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop()))

	// Act
	runner.GenerateReportNow(context.Background(), f.tenantID, "b", "inv", "r/", "CSV")

	// Assert
	key := fmt.Sprintf("r/b/%sT00-00Z/manifest.csv", time.Now().UTC().Format("2006-01-02"))
	w := httptest.NewRecorder()
	f.server.handleGetObject(w, f.req("GET", "/inv/"+key), &S3Request{Bucket: "inv", Object: key, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	rows, err := csv.NewReader(bytes.NewReader(w.Body.Bytes())).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 3, w.Body.String())
	assert.Equal(t, "StorageClass", rows[0][6], "the column is the class a customer sees")
	got := map[string]string{rows[1][0]: rows[1][6], rows[2][0]: rows[2][6]}
	assert.Equal(t, map[string]string{"old.jpg": "STANDARD", "attic.zip": "GLACIER"}, got)
	assert.NotContains(t, w.Body.String(), "geyser", "backend names are ours, not the customer's")
}
