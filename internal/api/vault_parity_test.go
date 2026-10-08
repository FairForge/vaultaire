package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 ETags are MD5 by definition
	"crypto/rand"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/google/uuid"
	"github.com/lib/pq"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-VAULT-1 part 2: the parity second copy. The tape backend ("geyser") and
// the free leg ("permafrost") are local drivers wrapped so a test can make
// Geyser fail (Get, GetRange, or both) and the leg fail one shard's Put.

// flakyGetDriver is the vault object's backend: a local driver whose Get and
// GetRange fail on demand. GetRange exists so the "data ranges that ARE
// readable" case is distinct from the whole-object Get.
type flakyGetDriver struct {
	*drivers.LocalDriver
	failGet, failRange atomic.Bool
	// truncateGet: Get answers, then the stream breaks after a few bytes —
	// the encoder fails mid-object.
	truncateGet atomic.Bool
	rangeGets   atomic.Int32
	dir         string
}

func (d *flakyGetDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.failGet.Load() {
		return nil, errors.New("geyser: connection reset by peer")
	}
	rc, err := d.LocalDriver.Get(ctx, container, artifact)
	if err == nil && d.truncateGet.Load() {
		return &brokenStream{Reader: io.LimitReader(rc, 4096), Closer: rc}, nil
	}
	return rc, err
}

// brokenStream serves its first bytes, then a read error.
type brokenStream struct {
	io.Reader
	io.Closer
}

func (b *brokenStream) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errors.New("geyser: stream reset mid-object")
	}
	return n, err
}

func (d *flakyGetDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	d.rangeGets.Add(1)
	if d.failRange.Load() {
		return nil, errors.New("geyser: connection reset by peer")
	}
	f, err := os.Open(filepath.Join(d.dir, container, artifact)) // #nosec G304 -- test fixture path
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, length), f}, nil
}

// flakyLegDriver is the parity leg: Put fails for artifacts with failSuffix.
type flakyLegDriver struct {
	*drivers.LocalDriver
	failSuffix atomic.Value // string
	failGet    atomic.Bool
	failDelete atomic.Bool
	puts       atomic.Int32
	// holdPuts: every Put announces itself on putEntered and waits for
	// putRelease to close before writing a byte — the window between the
	// row write and the shard writes, held open for a test.
	holdPuts   atomic.Bool
	putEntered chan struct{}
	putRelease chan struct{}
}

func (d *flakyLegDriver) Delete(ctx context.Context, container, artifact string) error {
	if d.failDelete.Load() {
		return errors.New("permafrost: 503 service unavailable")
	}
	return d.LocalDriver.Delete(ctx, container, artifact)
}

func (d *flakyLegDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.failGet.Load() {
		return nil, errors.New("permafrost: 503 service unavailable")
	}
	return d.LocalDriver.Get(ctx, container, artifact)
}

func (d *flakyLegDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	d.puts.Add(1)
	if d.holdPuts.Load() {
		d.putEntered <- struct{}{}
		<-d.putRelease
	}
	if s, _ := d.failSuffix.Load().(string); s != "" && strings.HasSuffix(artifact, s) {
		_, _ = io.Copy(io.Discard, io.LimitReader(data, 4096)) // read a little, then fail like a vendor would
		return errors.New("permafrost: 503 service unavailable")
	}
	return d.LocalDriver.Put(ctx, container, artifact, data, opts...)
}

type parityFixture struct {
	t                 *testing.T
	db                *sql.DB
	eng               *engine.CoreEngine
	geyser            *flakyGetDriver
	leg               *flakyLegDriver
	geyserDir, legDir string
	tenantID, bucket  string
	tn                *tenant.Tenant
	svc               *VaultParity
	flags             stubFlags
	logger            *zap.Logger
	stripe            int
}

func setupParityFixture(t *testing.T) *parityFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test")
	}
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	logger := zap.NewNop()
	f := &parityFixture{t: t, db: db, logger: logger, stripe: 64 << 10}
	f.geyserDir, f.legDir = t.TempDir(), t.TempDir()
	f.eng = engine.NewEngine(nil, logger, nil)
	f.eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), logger))
	f.geyser = &flakyGetDriver{LocalDriver: drivers.NewLocalDriver(f.geyserDir, logger), dir: f.geyserDir}
	f.eng.AddDriver("geyser", f.geyser)
	f.leg = &flakyLegDriver{LocalDriver: drivers.NewLocalDriver(f.legDir, logger),
		putEntered: make(chan struct{}, 8), putRelease: make(chan struct{})}
	f.eng.AddDriver("permafrost", f.leg)
	f.eng.SetPrimary("idrive")

	f.tenantID = uuid.New().String()
	f.tn = &tenant.Tenant{ID: f.tenantID}
	f.bucket = "attic"
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		f.tenantID, "parity test", "parity-"+f.tenantID[:8]+"@test.local", "AK-"+f.tenantID[:8], "SK-"+f.tenantID[:8])
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, tier_preference) VALUES ($1, $2, 'archive')`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM vault_parity WHERE tenant_id = $1`, `DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM object_locks WHERE tenant_id = $1`, `DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, f.tenantID)
		}
	})
	f.flags = stubFlags{on: map[string]bool{flagVaultParity + "/" + f.tenantID: true}}
	f.svc = NewVaultParity(db, f.eng, f.flags, logger)
	require.NotNil(t, f.svc)
	f.svc.scopeTenant = f.tenantID
	f.svc.JobName = "test_vault_parity_" + f.tenantID[:8]
	f.svc.Stripe = f.stripe
	return f
}

// object plants a vault-floor object: random bytes on "geyser" and a head row.
func (f *parityFixture) object(key string, size int) ([]byte, string) {
	f.t.Helper()
	body := make([]byte, size)
	_, err := rand.Read(body)
	require.NoError(f.t, err)
	sum := md5.Sum(body) // #nosec G401 -- the S3 ETag
	etag := hex.EncodeToString(sum[:])
	container := f.tn.NamespaceContainer(f.bucket)
	require.NoError(f.t, os.MkdirAll(filepath.Join(f.geyserDir, container, filepath.Dir(key)), 0o750))
	require.NoError(f.t, os.WriteFile(filepath.Join(f.geyserDir, container, key), body, 0o600))
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type)
		VALUES ($1,$2,$3,$4,$5,'geyser','vault',false,'application/octet-stream')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET size_bytes = EXCLUDED.size_bytes, etag = EXCLUDED.etag, updated_at = NOW()`,
		f.tenantID, f.bucket, key, size, etag)
	require.NoError(f.t, err)
	return body, etag
}

func (f *parityFixture) shardFiles() []string {
	var out []string
	root := filepath.Join(f.legDir, parityContainer(f.tenantID))
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func (f *parityFixture) row(key string) (state string, legs []string, lastErr string, attempts int) {
	f.t.Helper()
	var l pq.StringArray
	var le sql.NullString
	err := f.db.QueryRow(`SELECT state, legs, last_error, attempts FROM vault_parity WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, f.bucket, key).Scan(&state, &l, &le, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, "", 0
	}
	require.NoError(f.t, err)
	return state, []string(l), le.String, attempts
}

func (f *parityFixture) run() VaultParityResult {
	f.t.Helper()
	res, err := f.svc.RunOnce(context.Background())
	require.NoError(f.t, err, "%+v", res)
	return res
}

// --- the job ------------------------------------------------------------------

func TestVaultParity_JobWritesFourParityShardsBehindTheCommit(t *testing.T) {
	f := setupParityFixture(t)
	size := 3<<20 + 777
	_, etag := f.object("films/one.mkv", size)
	complete := promtest.ToFloat64(vaultParityObjects.WithLabelValues("complete"))
	bytesBefore := promtest.ToFloat64(vaultParityBytes)

	res := f.run()
	assert.Equal(t, 1, res.Protected, "%+v", res)
	assert.Equal(t, "permafrost", res.Leg)
	assert.Empty(t, res.Errors)

	state, legs, lastErr, attempts := f.row("films/one.mkv")
	assert.Equal(t, "complete", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs)
	assert.Empty(t, lastErr)
	assert.Equal(t, 1, attempts)

	l := parityLayout{k: 4, m: 4, stripe: f.stripe, size: int64(size)}
	files := f.shardFiles()
	require.Len(t, files, 4, "four parity shards on the leg: %v", files)
	prefix := shardPrefix(f.bucket, "films/one.mkv", etag)
	for j := 0; j < 4; j++ {
		st, err := os.Stat(filepath.Join(f.legDir, parityContainer(f.tenantID), shardArtifact(prefix, j)))
		require.NoError(t, err, "shard p%d", j)
		assert.Equal(t, l.shardBytes(), st.Size(), "shard p%d is stripes × stripe bytes", j)
	}
	assert.Equal(t, 4*l.shardBytes(), res.BytesWritten, "the second copy costs the object's size, rounded up to stripes")
	assert.Equal(t, float64(1), promtest.ToFloat64(vaultParityObjects.WithLabelValues("complete"))-complete)
	assert.Equal(t, float64(4*l.shardBytes()), promtest.ToFloat64(vaultParityBytes)-bytesBefore)

	// Idempotent: a complete row is not rewritten.
	res = f.run()
	assert.Equal(t, 0, res.Protected, "%+v", res)
	assert.Equal(t, int32(4), f.leg.puts.Load(), "no second write")
}

func TestVaultParity_FlagOffTenantIsSkipped(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/two.mkv", 1<<20)
	f.svc.flags = stubFlags{on: map[string]bool{}}
	res := f.run()
	assert.Equal(t, 0, res.Protected)
	assert.Equal(t, 1, res.FlagOff)
	state, _, _, _ := f.row("films/two.mkv")
	assert.Empty(t, state, "no row for a tenant the flag is off for")
	assert.Empty(t, f.shardFiles())
}

func TestVaultParity_PartialWriteLeavesARowThatSaysSo(t *testing.T) {
	f := setupParityFixture(t)
	_, _ = f.object("films/three.mkv", 2<<20+1)
	f.leg.failSuffix.Store("/p2")
	partial := promtest.ToFloat64(vaultParityObjects.WithLabelValues("partial"))

	res := f.run()
	assert.Equal(t, 1, res.Partial, "%+v", res)
	assert.Equal(t, 0, res.Protected)
	require.Len(t, res.Errors, 1)
	assert.Contains(t, res.Errors[0], "p2")

	state, legs, lastErr, attempts := f.row("films/three.mkv")
	assert.Equal(t, "partial", state, "the row says which shards landed and which did not")
	assert.Equal(t, []string{"permafrost", "permafrost", "", "permafrost"}, legs)
	assert.Contains(t, lastErr, "shard p2")
	assert.Equal(t, 1, attempts)
	assert.Len(t, f.shardFiles(), 3, "the three shards that landed are on the leg")
	assert.Equal(t, float64(1), promtest.ToFloat64(vaultParityObjects.WithLabelValues("partial"))-partial)

	// The next pass retries and completes it.
	f.leg.failSuffix.Store("")
	res = f.run()
	assert.Equal(t, 1, res.Protected, "%+v", res)
	state, legs, lastErr, attempts = f.row("films/three.mkv")
	assert.Equal(t, "complete", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs)
	assert.Empty(t, lastErr)
	assert.Equal(t, 2, attempts)
	assert.Len(t, f.shardFiles(), 4)
}

func TestVaultParity_StaleShardsAreErased(t *testing.T) {
	f := setupParityFixture(t)
	_, etag1 := f.object("films/four.mkv", 1<<20+9)
	f.run()
	first := f.shardFiles()
	require.Len(t, first, 4)

	// Overwrite: a new etag. The old shards go, new ones are written.
	_, etag2 := f.object("films/four.mkv", 1<<20+1000)
	require.NotEqual(t, etag1, etag2)
	res := f.run()
	assert.Equal(t, 1, res.Erased, "%+v", res)
	assert.Equal(t, 1, res.Protected)
	second := f.shardFiles()
	require.Len(t, second, 4)
	for _, s := range second {
		assert.Contains(t, s, etag2)
		assert.NotContains(t, s, etag1)
	}

	// Delete: the head row goes; the next pass erases the shards and the row.
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "films/four.mkv")
	require.NoError(t, err)
	res = f.run()
	assert.Equal(t, 1, res.Erased, "%+v", res)
	assert.Empty(t, f.shardFiles())
	state, _, _, _ := f.row("films/four.mkv")
	assert.Empty(t, state)
}

func TestVaultParity_AChunkedObjectIsNeverVaultFloor(t *testing.T) {
	// The invariant the code asserts instead of handling: every class the
	// floor function maps to the attic disables chunking at the PUT.
	for _, class := range []string{"STANDARD", "STANDARD_IA", "GLACIER", "DEEP_ARCHIVE", "RESILIENT", "PUBLIC", "REDUCED_REDUNDANCY", ""} {
		if usage.FloorOf(class) == usage.FloorVault {
			assert.True(t, storageClassDisablesChunking(class), "class %q is vault-floor and must never chunk", class)
		}
	}
	assert.Equal(t, usage.FloorVault, usage.FloorOf("GLACIER"))
	assert.Equal(t, usage.FloorVault, usage.FloorOf("DEEP_ARCHIVE"))

	// A row that breaks it (by hand) is refused and counted, never encoded.
	f := setupParityFixture(t)
	f.object("films/chunked.mkv", 1<<20)
	_, err := f.db.Exec(`UPDATE object_head_cache SET is_chunked = true WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "films/chunked.mkv")
	require.NoError(t, err)
	res := f.run()
	assert.Equal(t, 1, res.ChunkedSkipped, "%+v", res)
	assert.Equal(t, 0, res.Protected)
	assert.Empty(t, f.shardFiles())
}

func TestVaultParity_JobSpecNotesItemFailures(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/five.mkv", 1<<20)
	f.leg.failSuffix.Store("/p0")
	rep, err := f.svc.spec().Run(context.Background())
	require.NoError(t, err, "one object that could not be protected is a note, not a failed run")
	assert.Contains(t, rep.Note, "1 item(s) failed")
	assert.Equal(t, "every 2m0s", f.svc.spec().schedule())
}

// --- the fallback reader -------------------------------------------------------

func TestVaultParity_OpenRebuildsTheObjectWhenGeyserIsGone(t *testing.T) {
	f := setupParityFixture(t)
	size := 2<<20 + 345
	body, etag := f.object("films/six.mkv", size)
	f.run()
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)
	ctx := context.Background()

	rc, err := f.svc.Open(ctx, f.tenantID, f.bucket, "films/six.mkv", etag, int64(size), 0, int64(size))
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.True(t, bytes.Equal(body, got), "rebuilt from the four parity shards alone")
	assert.Equal(t, int32(0), f.geyser.rangeGets.Load(), "tape was never asked")

	// A window.
	rc, err = f.svc.Open(ctx, f.tenantID, f.bucket, "films/six.mkv", etag, int64(size), 300_000, 500_000)
	require.NoError(t, err)
	got, err = io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.True(t, bytes.Equal(body[300_000:800_000], got))

	// The row is bound to the etag it was computed from.
	_, err = f.svc.Open(ctx, f.tenantID, f.bucket, "films/six.mkv", "someotheretag", int64(size), 0, int64(size))
	assert.True(t, errors.Is(err, errParityUnavailable), "got %v", err)
	// And to the flag.
	f.svc.flags = stubFlags{on: map[string]bool{}}
	_, err = f.svc.Open(ctx, f.tenantID, f.bucket, "films/six.mkv", etag, int64(size), 0, int64(size))
	assert.True(t, errors.Is(err, errParityUnavailable), "got %v", err)
}

func TestVaultParity_OpenUsesTheDataRangesThatAreReadableWhenAShardIsGone(t *testing.T) {
	f := setupParityFixture(t)
	size := 1<<20 + 11
	body, etag := f.object("films/seven.mkv", size)
	f.run()
	// One parity shard lost on the leg; Geyser's whole-object GET is down
	// but its ranges answer.
	prefix := shardPrefix(f.bucket, "films/seven.mkv", etag)
	require.NoError(t, os.Remove(filepath.Join(f.legDir, parityContainer(f.tenantID), shardArtifact(prefix, 1))))
	f.geyser.failGet.Store(true)

	rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/seven.mkv", etag, int64(size), 0, int64(size))
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.True(t, bytes.Equal(body, got))
	l := parityLayout{k: 4, m: 4, stripe: f.stripe, size: int64(size)}
	assert.Equal(t, int32(l.stripes()), f.geyser.rangeGets.Load(), "one data range per stripe replaced the missing parity piece")

	// Ranges down too: the read fails, it does not serve zeros.
	f.geyser.failRange.Store(true)
	rc, err = f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/seven.mkv", etag, int64(size), 0, int64(size))
	require.NoError(t, err, "Open plans; the shortfall shows on read")
	_, err = io.ReadAll(rc)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errParityUnavailable), "got %v", err)
}

// --- the S3 GET ----------------------------------------------------------------

func (f *parityFixture) adapter() *S3ToEngine {
	a := NewS3ToEngine(f.eng, f.db, f.logger)
	a.vaultParity = f.svc
	return a
}

func (f *parityFixture) get(a *S3ToEngine, key, rangeHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/"+f.bucket+"/"+key, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tn))
	w := httptest.NewRecorder()
	a.HandleGet(w, req, f.bucket, key)
	return w
}

func TestHandleGet_VaultObjectIsServedFromParityWhenItsBackendFails(t *testing.T) {
	f := setupParityFixture(t)
	size := 1<<20 + 77
	body, etag := f.object("films/eight.mkv", size)
	f.run()
	served := promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("served"))
	a := f.adapter()

	// Healthy: the object's own backend serves it, no parity involved.
	w := f.get(a, "films/eight.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))

	// Geyser answers an error: the same bytes, from the parity.
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)
	w = f.get(a, "films/eight.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.Equal(t, `"`+etag+`"`, w.Header().Get("ETag"))
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"), "the class the customer sees is unchanged")
	assert.Equal(t, float64(1), promtest.ToFloat64(vaultParityFallbacks.WithLabelValues("served"))-served)

	// A ranged read decodes only the stripes it needs and carries the
	// identity headers on the 206 (the 2026-10-05 rule).
	w = f.get(a, "films/eight.mkv", "bytes=100000-200000")
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body[100000:200001], w.Body.Bytes()))
	assert.Equal(t, fmt.Sprintf("bytes 100000-200000/%d", size), w.Header().Get("Content-Range"))
	assert.NotEmpty(t, w.Header().Get("ETag"))
	assert.NotEmpty(t, w.Header().Get("Last-Modified"))
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))

	// The flag off: the failure is the customer's answer, as before.
	f.svc.flags = stubFlags{on: map[string]bool{}}
	w = f.get(a, "films/eight.mkv", "")
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))
}

func TestHandleGet_VaultObjectIsServedFromParityWhenGeysersBreakerIsOpen(t *testing.T) {
	f := setupParityFixture(t)
	size := 1 << 20
	body, _ := f.object("films/nine.mkv", size)
	f.run()
	// Trip the breaker: five backend failures through the engine.
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	container := f.tn.NamespaceContainer(f.bucket)
	for i := 0; i < 5; i++ {
		f.eng.HintBackend(container, "films/nine.mkv", "geyser")
		_, _ = f.eng.Get(ctx, container, "films/nine.mkv")
	}
	require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["geyser"], "the breaker is open")
	// With the breaker open the engine never asks Geyser: the failover
	// chain ends at the primary, which does not have the object.
	f.geyser.failGet.Store(false)

	w := f.get(f.adapter(), "films/nine.mkv", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
}

func TestHandleGet_ArchivedVaultObjectKeepsTheRestoreSemantics(t *testing.T) {
	// On tape is the object's state, not a backend failure: a GET must
	// answer the restore semantics, never quietly rebuild from parity and
	// make "restore" meaningless.
	f := setupParityFixture(t)
	f.object("films/ten.mkv", 1<<20)
	f.run()
	archived := &archivedDriver{LocalDriver: drivers.NewLocalDriver(f.geyserDir, f.logger)}
	f.eng.AddDriver("geyser", archived)
	w := f.get(f.adapter(), "films/ten.mkv", "")
	// No body in the message: a rebuilt object would be a megabyte of binary.
	require.Equal(t, http.StatusForbidden, w.Code, "an archived object must answer the restore semantics, not be rebuilt")
	assert.Contains(t, w.Body.String(), "InvalidObjectState")
	assert.Empty(t, w.Header().Get("x-vaultaire-served-from"))
}

type archivedDriver struct{ *drivers.LocalDriver }

func (d *archivedDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("%w (on tape)", engine.ErrArchived)
}

// --- delete and erasure ---------------------------------------------------------

func TestHandleDelete_ErasesTheParityShardsWithTheObject(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/eleven.mkv", 1<<20)
	f.run()
	require.Len(t, f.shardFiles(), 4)

	a := f.adapter()
	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/films/eleven.mkv", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tn))
	w := httptest.NewRecorder()
	a.HandleDelete(w, req, f.bucket, "films/eleven.mkv")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Empty(t, f.shardFiles(), "the shards go with the object")
	state, _, _, _ := f.row("films/eleven.mkv")
	assert.Empty(t, state)
}

func TestVaultParity_EraseTenantDefersWhenTheLegCannotBeReached(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/twelve.mkv", 1<<20)
	f.object("films/thirteen.mkv", 1<<20)
	f.run()
	require.Len(t, f.shardFiles(), 8)
	ctx := context.Background()

	t.Run("leg not registered", func(t *testing.T) {
		eng := engine.NewEngine(nil, f.logger, nil)
		eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), f.logger))
		eng.SetPrimary("idrive")
		svc := NewVaultParity(f.db, eng, f.flags, f.logger)
		n, err := svc.EraseTenant(ctx, f.tenantID)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errParityLegUnavailable), "got %v", err)
		assert.Contains(t, err.Error(), "permafrost is not registered")
		assert.Equal(t, 0, n)
		assert.Len(t, f.shardFiles(), 8, "nothing was touched, nothing was forgotten")
		assert.Equal(t, 2, f.countRows())
	})

	t.Run("leg breaker open", func(t *testing.T) {
		// Five backend failures through the engine open the leg's breaker.
		f.leg.failGet.Store(true)
		ctx := common.WithTenantID(ctx, f.tenantID)
		for i := 0; i < 5; i++ {
			f.eng.HintBackend("c", "k", "permafrost")
			_, _ = f.eng.Get(ctx, "c", "k")
		}
		require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["permafrost"], "the breaker is open")
		n, err := f.svc.EraseTenant(ctx, f.tenantID)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errParityLegUnavailable), "got %v", err)
		assert.Contains(t, err.Error(), "circuit breaker open")
		assert.Equal(t, 0, n)
		assert.Len(t, f.shardFiles(), 8)
		f.leg.failGet.Store(false)
	})

	t.Run("leg reachable", func(t *testing.T) {
		// A fresh engine: the breaker the previous case opened is not this one's.
		eng := engine.NewEngine(nil, f.logger, nil)
		eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), f.logger))
		eng.AddDriver("permafrost", f.leg)
		eng.SetPrimary("idrive")
		svc := NewVaultParity(f.db, eng, f.flags, f.logger)
		n, err := svc.EraseTenant(ctx, f.tenantID)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Empty(t, f.shardFiles())
		assert.Equal(t, 0, f.countRows())
	})
}

func (f *parityFixture) countRows() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM vault_parity WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

func TestAccountDeletion_ErasesParityShardsBeforeTheSweepAndDefersWithoutTheLeg(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	legDir := t.TempDir()
	leg := drivers.NewLocalDriver(legDir, zap.NewNop())
	// Two parity rows whose shards sit on the leg.
	container := parityContainer(f.tenantID)
	for _, key := range []string{"v/one.bin", "v/two.bin"} {
		prefix := shardPrefix(f.bucket, key, "etag-"+key)
		for j := 0; j < 4; j++ {
			require.NoError(t, leg.Put(common.WithTenantID(ctx, f.tenantID), container, shardArtifact(prefix, j), strings.NewReader("parity")))
		}
		f.exec(`INSERT INTO vault_parity (tenant_id, bucket, object_key, etag, size_bytes, stripe_bytes, shard_bytes, shard_prefix, legs, state)
		        VALUES ($1,$2,$3,$4,100,64,6,$5,$6,'complete')`, f.tenantID, f.bucket, key, "etag-"+key, prefix, pq.Array([]string{"permafrost", "permafrost", "permafrost", "permafrost"}))
	}
	shardCount := func() int {
		n := 0
		_ = filepath.Walk(filepath.Join(legDir, container), func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	require.Equal(t, 8, shardCount())

	// Without the leg registered: the tenant is deferred at the parity
	// stage, the rows and shards untouched.
	f.runner.VaultParity = NewVaultParity(f.db, f.eng, stubFlags{}, zap.NewNop())
	res, err := f.runner.RunOnce(ctx)
	require.Error(t, err, "a deferred tenant fails the run on purpose")
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeDeferred, res.Tenants[0].Outcome, "%+v", res.Tenants[0])
	assert.Contains(t, res.Tenants[0].Error, "parity:")
	assert.Contains(t, res.Tenants[0].Error, "permafrost is not registered")
	assert.Equal(t, 8, shardCount(), "nothing erased on a deferral")
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM vault_parity WHERE tenant_id = $1`, f.tenantID))

	// With the leg: shards erased before the sweep, rows erased with the
	// account, counted on the record.
	f.eng.AddDriver("permafrost", leg)
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	assert.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.Equal(t, 2, te.ParityErased)
	assert.Equal(t, 0, shardCount())
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM vault_parity WHERE tenant_id = $1`, f.tenantID))
}

func TestSweepPlan_AlwaysListsTheParityContainer(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	plan, err := f.runner.planSweep(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Contains(t, plan.buckets, parityBucket, "a shard whose row is gone must still be found: %v", plan.buckets)
}

// Sync is the first parity leg when it is registered (owner decision
// 2026-10-07: Sync approved the reseller use; its parity rebuilds at
// ~110 MB/s vs ~60 for the OneDrive fleet). VAULT_PARITY_LEGS overrides the
// order; unknown names are ignored; an empty override keeps the default.
func TestVaultParity_LegPreference(t *testing.T) {
	local := func() engine.Driver { return drivers.NewLocalDriver(t.TempDir(), zap.NewNop()) }
	newEng := func(names ...string) *engine.CoreEngine {
		eng := engine.NewEngine(nil, zap.NewNop(), nil)
		for _, n := range names {
			eng.AddDriver(n, local())
		}
		return eng
	}
	cases := []struct {
		name, env string
		drivers   []string
		want      string
	}{
		{"sync first by default", "", []string{"lyve", "permafrost", "sync"}, "sync"},
		{"permafrost without sync", "", []string{"lyve", "permafrost"}, "permafrost"},
		{"lyve last", "", []string{"lyve"}, "lyve"},
		{"override order", "lyve,sync", []string{"lyve", "permafrost", "sync"}, "lyve"},
		{"unknown names skipped", "nope, permafrost", []string{"permafrost", "sync"}, "permafrost"},
		{"blank override = default", " , ", []string{"permafrost", "sync"}, "sync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VAULT_PARITY_LEGS", tc.env)
			db, err := sql.Open("postgres", "") // never dialled: Leg() reads no row
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			p := NewVaultParity(db, newEng(tc.drivers...), nil, zap.NewNop())
			require.NotNil(t, p)
			got, _, ok := p.Leg()
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- both delete paths share one aftermath ------------------------------------

// server is the Server the S3 handlers run on, wired the way the product
// wires it (vault parity, the Smart promoter, the engine): the single
// DELETE and DeleteObjects paths are both driven through it.
func (f *parityFixture) server() *Server {
	return &Server{engine: f.eng, db: f.db, logger: f.logger, vaultParity: f.svc,
		smartPromoter: NewSmartPromoter(f.db, f.eng, f.logger)}
}

func (f *parityFixture) lockRow(key string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, $2, $3, 'GOVERNANCE', NOW() - INTERVAL '1 day')`, f.tenantID, f.bucket, key)
	require.NoError(f.t, err)
}

func (f *parityFixture) lockRows() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM object_locks WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

func (f *parityFixture) ledgerRow(key string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
		VALUES ($1,$2,$3,'e',100,'idrive','geyser','idle',NOW())`, f.tenantID, f.bucket, key)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM smart_demotions WHERE tenant_id = $1`, f.tenantID) })
}

func (f *parityFixture) openLedgerRows() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM smart_demotions WHERE tenant_id = $1 AND hot_deleted_at IS NULL`, f.tenantID).Scan(&n))
	return n
}

// Everything that happens after an object's bytes are gone — the parity
// shards and their row, the expired lock row, the Smart ledger — is ONE
// helper both delete paths call (object_delete_shared.go), the way the
// write paths share object_write_shared.go. Post-merge review of #621/#629:
// DeleteObjects had drifted from single DELETE (no parity erase, no lock
// row cleanup), and the single DELETE handler built its adapter without
// the Smart promoter, so only the batch path settled a demoted object.
func TestObjectDelete_BothPathsEraseParityShardsLockRowsAndTheSmartLedger(t *testing.T) {
	f := setupParityFixture(t)
	srv := f.server()
	for _, key := range []string{"films/single.mkv", "films/batch-a.mkv", "films/batch-b.mkv"} {
		f.object(key, 1<<20)
		f.lockRow(key)
		f.ledgerRow(key)
	}
	f.run()
	require.Len(t, f.shardFiles(), 12)
	require.Equal(t, 3, f.lockRows())
	require.Equal(t, 3, f.openLedgerRows())

	t.Run("single DELETE through the server", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/"+f.bucket+"/films/single.mkv", nil)
		req = req.WithContext(s3Ctx(req.Context(), f.tn))
		w := httptest.NewRecorder()
		srv.handleDeleteObject(w, req, &S3Request{Bucket: f.bucket, Object: "films/single.mkv", TenantID: f.tenantID})
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		assert.Len(t, f.shardFiles(), 8, "the shards of the deleted object are gone")
		state, _, _, _ := f.row("films/single.mkv")
		assert.Empty(t, state, "and its row")
		assert.Equal(t, 2, f.lockRows(), "the expired lock row goes with the object")
		assert.Equal(t, 2, f.openLedgerRows(), "the Smart ledger row is settled")
	})

	t.Run("DeleteObjects through the server", func(t *testing.T) {
		body := `<Delete><Object><Key>films/batch-a.mkv</Key></Object><Object><Key>films/batch-b.mkv</Key></Object></Delete>`
		req := httptest.NewRequest("POST", "/"+f.bucket+"?delete", strings.NewReader(body))
		req = req.WithContext(s3Ctx(req.Context(), f.tn))
		w := httptest.NewRecorder()
		srv.handleDeleteObjects(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "<Error>")
		assert.Empty(t, f.shardFiles(), "the shards of both objects are gone at once, not at the job's stale pass")
		assert.Equal(t, 0, f.countRows())
		assert.Equal(t, 0, f.lockRows())
		assert.Equal(t, 0, f.openLedgerRows())
	})
}

// --- shards never outlive their row (post-merge review of #629) ------------------

// openLegBreaker opens the parity leg's circuit breaker: five backend
// failures through the engine.
func (f *parityFixture) openLegBreaker() {
	f.t.Helper()
	f.leg.failGet.Store(true)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	for i := 0; i < 5; i++ {
		f.eng.HintBackend("c", "k", "permafrost")
		_, _ = f.eng.Get(ctx, "c", "k")
	}
	require.Equal(f.t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["permafrost"], "the breaker is open")
	f.leg.failGet.Store(false)
}

// (3) The row of an overwritten object whose stale shards could not be
// erased this run (the leg's breaker is open) used to be rewritten by the
// protect pass with the new etag and empty legs — the old shards were
// orphaned on the leg with no row naming them. The candidate is skipped
// instead, the row kept, and the stale pass tries again next run.
func TestVaultParity_ProtectNeverRewritesARowWhoseShardsAreNotProvablyGone(t *testing.T) {
	f := setupParityFixture(t)
	_, etag1 := f.object("films/fourteen.mkv", 1<<20+9)
	f.run()
	require.Len(t, f.shardFiles(), 4)
	_, etag2 := f.object("films/fourteen.mkv", 1<<20+1000)
	require.NotEqual(t, etag1, etag2)
	f.openLegBreaker()

	res := f.run()
	assert.Equal(t, 1, res.EraseFailed, "%+v", res)
	assert.Equal(t, 0, res.Protected, "the candidate is skipped while its old shards may still be on the leg")
	assert.Equal(t, 0, res.Erased)
	require.NotEmpty(t, res.Errors)
	assert.Contains(t, strings.Join(res.Errors, "\n"), "circuit breaker open")
	files := f.shardFiles()
	assert.Len(t, files, 4, "nothing new written, nothing orphaned: %v", files)
	for _, s := range files {
		assert.Contains(t, s, etag1)
	}
	state, legs, _, _ := f.row("films/fourteen.mkv")
	assert.Equal(t, "complete", state, "the row still names the old shards")
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs)
	var rowETag string
	require.NoError(t, f.db.QueryRow(`SELECT etag FROM vault_parity WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "films/fourteen.mkv").Scan(&rowETag))
	assert.Equal(t, etag1, rowETag)
}

// (3b) The same for the same etag on a different leg: a partial row whose
// shards sit on permafrost, retried after `sync` became the first leg. The
// permafrost shards are erased before the sync write; when they cannot be,
// the candidate is skipped and the row keeps naming them.
func TestVaultParity_RetryOnANewLegErasesTheOldLegsShardsFirst(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/fifteen.mkv", 2<<20+1)
	f.leg.failSuffix.Store("/p2")
	res := f.run()
	require.Equal(t, 1, res.Partial, "%+v", res)
	require.Len(t, f.shardFiles(), 3)
	f.leg.failSuffix.Store("")

	// The leg order switches: sync is registered and first.
	syncDir := t.TempDir()
	f.eng.AddDriver("sync", drivers.NewLocalDriver(syncDir, f.logger))
	syncFiles := func() int {
		n := 0
		_ = filepath.Walk(filepath.Join(syncDir, parityContainer(f.tenantID)), func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				n++
			}
			return nil
		})
		return n
	}

	t.Run("old leg unreachable: skipped, row kept", func(t *testing.T) {
		f.openLegBreaker()
		res := f.run()
		assert.Equal(t, "sync", res.Leg)
		assert.Equal(t, 0, res.Protected, "%+v", res)
		assert.Contains(t, strings.Join(res.Errors, "\n"), "circuit breaker open")
		assert.Len(t, f.shardFiles(), 3, "the permafrost shards are still named by the row")
		assert.Equal(t, 0, syncFiles(), "nothing written to sync while permafrost's shards are not provably gone")
		state, legs, _, attempts := f.row("films/fifteen.mkv")
		assert.Equal(t, "partial", state)
		assert.Equal(t, []string{"permafrost", "permafrost", "", "permafrost"}, legs)
		assert.Equal(t, 1, attempts, "a skipped candidate is not an attempt")
	})

	t.Run("old leg reachable: erased, then written to the new leg", func(t *testing.T) {
		// A fresh engine: the breaker the previous case opened is not this one's.
		eng := engine.NewEngine(nil, f.logger, nil)
		eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), f.logger))
		eng.AddDriver("geyser", f.geyser)
		eng.AddDriver("permafrost", f.leg)
		eng.AddDriver("sync", drivers.NewLocalDriver(syncDir, f.logger))
		eng.SetPrimary("idrive")
		f.svc.eng = eng
		res := f.run()
		assert.Equal(t, 1, res.Protected, "%+v", res)
		assert.Empty(t, f.shardFiles(), "no shard left on permafrost")
		assert.Equal(t, 4, syncFiles())
		state, legs, _, attempts := f.row("films/fifteen.mkv")
		assert.Equal(t, "complete", state)
		assert.Equal(t, []string{"sync", "sync", "sync", "sync"}, legs)
		assert.Equal(t, 2, attempts)
	})
}

// (4) A DELETE while the job streams the shards: the row sat 'partial' with
// empty legs, OnObjectDeleted found nothing to erase and dropped the row,
// then the four shards landed with no row naming them. The row now records
// the intended leg of every shard before the first byte (so the erase knows
// where to look), and a finish that updates no row — the row was deleted
// meanwhile — deletes the shards it just wrote. Zero shards left, either way.
func TestVaultParity_DeleteDuringTheShardWriteLeavesNoShard(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/sixteen.mkv", 1<<20+5)
	f.leg.holdPuts.Store(true)

	done := make(chan VaultParityResult, 1)
	go func() {
		res, _ := f.svc.RunOnce(context.Background())
		done <- res
	}()
	// The job has written its row and is inside Put on every shard, no byte
	// written yet.
	for i := 0; i < 4; i++ {
		select {
		case <-f.leg.putEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("the job never reached the leg")
		}
	}
	state, legs, _, _ := f.row("films/sixteen.mkv")
	assert.Equal(t, "partial", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs, "the intent: every shard's leg is on the row before it is written")

	// The object is deleted now: single DELETE's aftermath.
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "films/sixteen.mkv")
	require.NoError(t, err)
	f.svc.OnObjectDeleted(context.Background(), f.tenantID, f.bucket, "films/sixteen.mkv")
	assert.Equal(t, 0, f.countRows(), "the delete dropped the row")

	f.leg.holdPuts.Store(false)
	close(f.leg.putRelease)
	select {
	case res := <-done:
		assert.Equal(t, 0, res.Protected, "%+v", res)
	case <-time.After(30 * time.Second):
		t.Fatal("the job never finished")
	}
	assert.Empty(t, f.shardFiles(), "the shards written after the delete are gone")
	assert.Equal(t, 0, f.countRows())
}

// (5) An encode failure (the source stream broke) deletes what it wrote,
// best effort; a shard whose delete failed stays recorded on the row so the
// next pass retries it in place rather than forgetting it.
func TestVaultParity_EncodeFailureKeepsAnUndeletedShardOnTheRow(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/seventeen.mkv", 1<<20+3)
	f.geyser.truncateGet.Store(true)
	f.leg.failDelete.Store(true)

	res := f.run()
	assert.Equal(t, 1, res.Failed, "%+v", res)
	assert.Equal(t, 0, res.Protected)
	state, legs, lastErr, _ := f.row("films/seventeen.mkv")
	assert.Equal(t, "partial", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs,
		"a shard whose delete failed is still the row's to erase")
	assert.Contains(t, lastErr, "stream reset")

	// The next pass, with the source and the leg healthy, completes it in place.
	f.geyser.truncateGet.Store(false)
	f.leg.failDelete.Store(false)
	res = f.run()
	assert.Equal(t, 1, res.Protected, "%+v", res)
	state, legs, _, _ = f.row("films/seventeen.mkv")
	assert.Equal(t, "complete", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs)
	assert.Len(t, f.shardFiles(), 4)
}

// (6) A retry whose source read fails must keep naming the shards the
// earlier attempt wrote: clearPriorShards left them in place for the write
// to overwrite, and a row that then says "no shard anywhere" orphans them —
// OnObjectDeleted (and the stale pass) erase only what the row names.
func TestVaultParity_ARetryThatCannotReadTheObjectKeepsTheEarlierShardsOnTheRow(t *testing.T) {
	f := setupParityFixture(t)
	f.object("films/eighteen.mkv", 2<<20+9)
	f.leg.failSuffix.Store("/p2")
	res := f.run()
	require.Equal(t, 1, res.Partial, "%+v", res)
	require.Len(t, f.shardFiles(), 3, "three shards of the first attempt are on the leg")

	// The retry: the leg is fine, the object cannot be read.
	f.leg.failSuffix.Store("")
	f.geyser.failGet.Store(true)
	res = f.run()
	assert.Equal(t, 1, res.Failed, "%+v", res)
	state, legs, lastErr, attempts := f.row("films/eighteen.mkv")
	assert.Equal(t, "partial", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs,
		"the row still names every shard the earlier attempt may have left on the leg")
	assert.Contains(t, lastErr, "read the object")
	assert.Equal(t, 2, attempts)
	assert.Len(t, f.shardFiles(), 3, "the retry wrote nothing and deleted nothing")

	// The object is deleted: every shard goes with it.
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "films/eighteen.mkv")
	require.NoError(t, err)
	f.svc.OnObjectDeleted(context.Background(), f.tenantID, f.bucket, "films/eighteen.mkv")
	assert.Empty(t, f.shardFiles(), "no shard outlives the row")
	assert.Equal(t, 0, f.countRows())
}
