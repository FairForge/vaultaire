package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R10-3c: an erasure reaches every byte. The runner's walk deletes what
// object_head_cache lists; bytes with no head row were never reached, and
// EraseRows then removed the last rows that named them (post-merge finding
// PM-3 of #529).

// --- test backends -------------------------------------------------------------

// fixedBucketDriver keys the way iDrive, Lyve, Geyser and R2 do: one store,
// every key `t-<tenant>/<container>/<artifact>`, the tenant taken from the
// context ("default" when it carries none). It implements engine.TenantWalker
// and can be told to fail the way a backend does.
type fixedBucketDriver struct {
	dir string

	failGet       atomic.Bool  // reads fail (what opens the engine's breaker)
	failWalkAfter atomic.Int32 // > 0: the listing fails after handing out this many objects
	failRemove    atomic.Bool  // every delete is refused
	hangRemove    atomic.Bool  // every delete waits for its context to give up
	walks         atomic.Int32 // WalkTenant calls
	removes       atomic.Int32 // deletes attempted
	onObject      func(n int)  // called before object n of a walk is handed out
}

func (d *fixedBucketDriver) tenant(ctx context.Context) string {
	if t, ok := ctx.Value(common.TenantIDKey).(string); ok && t != "" {
		return t
	}
	return "default"
}

func (d *fixedBucketDriver) path(tenant, container, artifact string) string {
	return filepath.Join(d.dir, "t-"+tenant, container, filepath.FromSlash(artifact))
}

func (d *fixedBucketDriver) Name() string { return "fixed" }

func (d *fixedBucketDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.failGet.Load() {
		return nil, errors.New("fixed: connection refused")
	}
	f, err := os.Open(d.path(d.tenant(ctx), container, artifact))
	if err != nil {
		return nil, engine.ErrNotFound(container, artifact)
	}
	return f, nil
}

func (d *fixedBucketDriver) Put(ctx context.Context, container, artifact string, data io.Reader, _ ...engine.PutOption) error {
	p := d.path(d.tenant(ctx), container, artifact)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func (d *fixedBucketDriver) Delete(ctx context.Context, container, artifact string) error {
	return os.Remove(d.path(d.tenant(ctx), container, artifact))
}

func (d *fixedBucketDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	var out []string
	root := filepath.Join(d.dir, "t-"+d.tenant(ctx), container)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if name := filepath.ToSlash(rel); strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
		return nil
	})
	return out, err
}

func (d *fixedBucketDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	return fileExists(d.path(d.tenant(ctx), container, artifact)), nil
}

func (d *fixedBucketDriver) HealthCheck(context.Context) error { return nil }

func (d *fixedBucketDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	d.walks.Add(1)
	if tenantID == "" || strings.Contains(tenantID, "/") {
		return drivers.ErrWalkTenantID
	}
	root := filepath.Join(d.dir, "t-"+tenantID)
	var files []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	for n, p := range files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("fixed: walk: %w", err)
		}
		if limit := int(d.failWalkAfter.Load()); limit > 0 && n >= limit {
			return errors.New("fixed: walk: 500 InternalError on the next page")
		}
		if d.onObject != nil {
			d.onObject(n)
		}
		rel, _ := filepath.Rel(root, p)
		container, artifact, _ := strings.Cut(filepath.ToSlash(rel), "/")
		err := fn(engine.TenantObject{Container: container, Artifact: artifact, Remove: func(rctx context.Context) error {
			d.removes.Add(1)
			if d.hangRemove.Load() {
				<-rctx.Done()
				return rctx.Err()
			}
			if d.failRemove.Load() {
				return errors.New("fixed: 503 SlowDown")
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}})
		if err != nil {
			return err
		}
	}
	return nil
}

// listOnlyDriver is a backend that cannot list a tenant (the plain `s3`
// driver, permafrost): the engine.Driver methods of a local driver and no
// WalkTenant. The sweep falls back to List + Delete per bucket.
type listOnlyDriver struct {
	inner    *drivers.LocalDriver
	failList atomic.Bool
	lists    atomic.Int32
}

func (d *listOnlyDriver) Name() string { return "plain" }
func (d *listOnlyDriver) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	return d.inner.Get(ctx, c, a)
}
func (d *listOnlyDriver) Put(ctx context.Context, c, a string, r io.Reader, o ...engine.PutOption) error {
	return d.inner.Put(ctx, c, a, r, o...)
}
func (d *listOnlyDriver) Delete(ctx context.Context, c, a string) error {
	return d.inner.Delete(ctx, c, a)
}
func (d *listOnlyDriver) List(ctx context.Context, c, p string) ([]string, error) {
	d.lists.Add(1)
	if d.failList.Load() {
		return nil, errors.New("plain: list: 403 AccessDenied")
	}
	return d.inner.List(ctx, c, p)
}
func (d *listOnlyDriver) Exists(ctx context.Context, c, a string) (bool, error) {
	return d.inner.Exists(ctx, c, a)
}
func (d *listOnlyDriver) HealthCheck(ctx context.Context) error { return d.inner.HealthCheck(ctx) }

// sweepFixture is the deletion fixture with two more backends: "fixed" (a
// fixed-bucket store with a tenant walk) and "plain" (List + Delete only).
type sweepFixture struct {
	*deletionFixture
	fixed    *fixedBucketDriver
	plain    *listOnlyDriver
	plainDir string
}

func setupSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	f := &sweepFixture{deletionFixture: setupDeletionFixture(t)}
	f.fixed = &fixedBucketDriver{dir: t.TempDir()}
	f.plainDir = t.TempDir()
	f.plain = &listOnlyDriver{inner: drivers.NewLocalDriver(f.plainDir, zap.NewNop())}
	f.eng.AddDriver("fixed", f.fixed)
	f.eng.AddDriver("plain", f.plain)
	f.eng.SetPrimary("local")
	f.seed(time.Now().Add(-time.Hour))
	return f
}

// onLocal / onFixed write a blob of any tenant and return its path.
func (f *sweepFixture) onLocal(dir, tenantID, bucket, key string) string {
	f.t.Helper()
	p := filepath.Join(dir, tenantID+"_"+bucket, filepath.FromSlash(key))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(f.t, os.WriteFile(p, []byte("bytes"), 0o600))
	return p
}

func (f *sweepFixture) onFixed(tenantID, container, key string) string {
	f.t.Helper()
	p := f.fixed.path(tenantID, container, key)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(f.t, os.WriteFile(p, []byte("bytes"), 0o600))
	return p
}

func (f *sweepFixture) run() TenantErasure {
	f.t.Helper()
	res, _ := f.runner.RunOnce(context.Background())
	require.Len(f.t, res.Tenants, 1, "%+v", res)
	return res.Tenants[0]
}

func (f *sweepFixture) userExists() bool {
	return f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID) == 1
}

func (f *sweepFixture) erasedRows() int {
	return f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID)
}

func assertGone(t *testing.T, msg string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		assert.False(t, fileExists(p), "%s: %s must be gone", msg, p)
	}
}

func assertKept(t *testing.T, msg string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		assert.True(t, fileExists(p), "%s: %s must survive", msg, p)
	}
}

// --- the gap ---------------------------------------------------------------------

// The proven case: on a versioning-enabled bucket a DELETE writes a marker,
// drops the head row and leaves the blob (it is what GET ?versionId serves).
// Same class: a blob whose version row went with DELETE ?versionId.
func TestErasureSweep_BlobsWithNoHeadRowAreErased(t *testing.T) {
	// Arrange
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	// A key deleted with a marker: a live version row, a marker on top, no head row.
	f.writeBlob(f.primary, "v/marked.txt", "bytes behind a delete marker")
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, is_latest, is_delete_marker, backend_name)
	        VALUES ($1, $2, 'v/marked.txt', 'v1', 28, 'e', FALSE, FALSE, 'local')`, f.tenantID, f.bucket)
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, is_latest, is_delete_marker)
	        VALUES ($1, $2, 'v/marked.txt', 'v2', 0, '', TRUE, TRUE)`, f.tenantID, f.bucket)
	// A key whose only version row went with DELETE ?versionId: no row names the blob at all.
	f.writeBlob(f.secDir, "v/purged.txt", "bytes no row remembers")

	// Act
	res, err := f.runner.RunOnce(ctx)

	// Assert
	require.NoError(t, err, "%+v", res)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.False(t, fileExists(filepath.Join(f.primary, f.container(), "v/marked.txt")), "the blob behind the delete marker must not survive the erasure")
	assert.False(t, fileExists(filepath.Join(f.secDir, f.container(), "v/purged.txt")), "a blob no row names must not survive the erasure")
	assert.Equal(t, map[string]int{"local": 1, "second": 1}, te.Swept, "one blob with no head row on each backend")
	assert.Zero(t, te.SweptAfterErase, "found by the sweep BEFORE the row erase, while a failure can still defer the tenant")
	assert.Equal(t, 4, te.ObjectsDeleted, "the walk's count is its own")
	assert.Equal(t, []string{"local", "second"}, te.SweptBackends)
	// The erasure record carries the sweep.
	var swept, backends string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'swept', metadata->>'swept_backends' FROM audit_logs
		WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID).Scan(&swept, &backends))
	assert.JSONEq(t, `{"local": 1, "second": 1}`, swept)
	assert.JSONEq(t, `["local", "second"]`, backends)
}

// The other entry point: the admin trigger starts the same job.
func TestErasureSweep_TheAdminTriggerSweepsToo(t *testing.T) {
	// Arrange
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	orphan := filepath.Join(f.primary, f.container(), "orphan.bin")
	f.writeBlob(f.primary, "orphan.bin", "no row")
	s := &Server{logger: zap.NewNop(), db: f.db, accountDeletion: f.runner, jobs: f.sched}

	// Act
	rr := doJSON(t, s.adminJobTrigger(f.runner.JobName), "POST", "/api/v1/admin/account-deletion")

	// Assert
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	require.Eventually(t, func() bool {
		rows, err := jobRunRows(ctx, f.db)
		return err == nil && rows[f.runner.JobName].Outcome == jobOutcomeOK
	}, 15*time.Second, 20*time.Millisecond)
	assert.False(t, fileExists(orphan), "the triggered run swept the orphan")
	var swept, after sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT metadata->'swept'->>'local', metadata->>'swept_after_erase' FROM audit_logs
		WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID).Scan(&swept, &after))
	assert.Equal(t, "1", swept.String)
	assert.Equal(t, "0", after.String, "by the sweep before the row erase")
}

// --- the dangerous half ------------------------------------------------------------

// Two tenants, two buckets, four backends of three shapes: everything of the
// erased tenant goes, everything of a neighbour whose id or bucket name shares
// a prefix stays, and the chunk container is not touched anywhere.
func TestErasureSweep_NeighboursAndTheChunkContainerSurvive(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	longer := T + "0"               // the erased id is a prefix of this one
	shorter := T[:len(T)-1]         // this one is a prefix of the erased id
	b, bc := f.bucket, f.bucket+"c" // two buckets whose names share a prefix
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, T, bc)

	var mine, theirs, chunks []string
	for _, dir := range []string{f.primary, f.secDir} {
		mine = append(mine, f.onLocal(dir, T, b, "orphan/a"), f.onLocal(dir, T, bc, "orphan/b"), f.onLocal(dir, T, "forgotten", "orphan/c"))
		for _, n := range []string{longer, shorter} {
			theirs = append(theirs, f.onLocal(dir, n, b, "theirs"), f.onLocal(dir, n, bc, "theirs"))
		}
		chunks = append(chunks, f.onLocal(dir, "", "global", "_chunks/h1")) // <dir>/_global/_chunks/h1
	}
	mine = append(mine, f.onFixed(T, T+"_"+b, "orphan/a"), f.onFixed(T, T+"_"+bc, "orphan/b"), f.onFixed(T, T+"_forgotten", "orphan/c"))
	for _, n := range []string{longer, shorter} {
		theirs = append(theirs, f.onFixed(n, n+"_"+b, "theirs"), f.onFixed(n, n+"_"+bc, "theirs"))
	}
	// On a fixed-bucket store the chunks a tenant's requests wrote are under
	// THAT tenant's prefix — inside the walk's reach.
	chunks = append(chunks, f.onFixed(T, chunkContainer, "_chunks/h2"), f.onFixed("default", chunkContainer, "_chunks/h3"), f.onFixed(longer, chunkContainer, "_chunks/h4"))
	// The backend that cannot list a tenant: the buckets the tables remember
	// are swept, a bucket no table remembers is out of its reach.
	mine = append(mine, f.onLocal(f.plainDir, T, b, "orphan/a"), f.onLocal(f.plainDir, T, bc, "orphan/b"))
	forgottenOnPlain := f.onLocal(f.plainDir, T, "forgotten", "orphan/c")
	theirs = append(theirs, f.onLocal(f.plainDir, longer, b, "theirs"), f.onLocal(f.plainDir, shorter, bc, "theirs"))

	// Act
	te := f.run()

	// Assert
	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assertGone(t, "the erased tenant's bytes", mine...)
	assertKept(t, "a neighbour's bytes", theirs...)
	assertKept(t, "the shared chunk container", chunks...)
	assert.Equal(t, map[string]int{"local": 3, "second": 3, "fixed": 3, "plain": 2}, te.Swept)
	assert.Equal(t, 1, te.ChunkBlobsLeft, "the chunk blob under the tenant's own prefix is seen, counted and left")
	assertKept(t, "a bucket no table remembers, on a backend that cannot list a tenant (documented limit)", forgottenOnPlain)
	assert.Equal(t, []string{"fixed", "local", "plain", "second"}, te.SweptBackends)
}

func TestErasureSweep_RefusesATenantIDThatCouldWidenAListing(t *testing.T) {
	f := setupSweepFixture(t)
	ctx := context.Background()

	// The shapes: empty, a path, a container-name separator, the chunk container.
	for _, bad := range []string{"", "a/b", "a_b", "_global", "/", "-", "t x"} {
		_, err := f.runner.planSweep(ctx, bad)
		assert.Error(t, err, "planSweep(%q)", bad)
		te := TenantErasure{}
		_, err = f.runner.sweepTenant(ctx, account.Due{TenantID: bad, UserID: f.userID}, sweepPlan{backends: []string{"fixed", "local", "plain"}, buckets: []string{"b"}}, &te, zap.NewNop(), true)
		assert.Error(t, err, "sweepTenant(%q)", bad)
	}
	assert.Zero(t, f.fixed.walks.Load(), "nothing was listed for a refused tenant")
	assert.Zero(t, f.plain.lists.Load())

	// Another tenant whose id is this id plus a separator: its containers
	// start with this tenant's container prefix on a container-keyed backend.
	collider := f.tenantID + "_x"
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Collider', $2, $3, $4)`,
		collider, "collider-"+f.suffix+"@test.local", "VKCL"+f.suffix, "SKCL"+f.suffix)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, collider) })
	colliderBlob := f.onLocal(f.primary, collider, "bkt", "theirs")
	orphan := f.onLocal(f.primary, f.tenantID, f.bucket, "orphan")

	te := f.run()

	assert.Equal(t, outcomeDeferred, te.Outcome)
	assert.Contains(t, te.Error, "sweep plan")
	assertKept(t, "nothing is swept while the ids collide", colliderBlob, orphan)
	assert.True(t, f.userExists(), "the account is not erased")
}

func TestErasureSweep_RefusesAnEmptyBucketName(t *testing.T) {
	f := setupSweepFixture(t)
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag) VALUES ($1, '', 'k', 'v1', 1, 'e')`, f.tenantID)
	orphan := f.onLocal(f.plainDir, f.tenantID, f.bucket, "orphan")

	te := f.run()

	assert.Equal(t, outcomeDeferred, te.Outcome)
	assert.Contains(t, te.Error, "sweep plan: object_versions holds an empty bucket name")
	assertKept(t, "nothing is listed with an empty bucket name", orphan)
	assert.Zero(t, f.plain.lists.Load())

	// The second guard, where the listing is built: a plan handed in with an
	// empty name is refused before List (`<tenant>_` is the prefix of every
	// container of the tenant).
	got := TenantErasure{}
	_, err := f.runner.sweepTenant(context.Background(), account.Due{TenantID: f.tenantID, UserID: f.userID},
		sweepPlan{backends: []string{"plain"}, buckets: []string{""}}, &got, zap.NewNop(), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to list")
	assert.Zero(t, f.plain.lists.Load())
}

// The plan's bucket names never include the chunk container: object_locations
// records `_global` as the "bucket" of every chunk store.
func TestErasureSweep_PlanTakesBucketNamesFromEveryTableAndNeverTheChunkContainer(t *testing.T) {
	f := setupSweepFixture(t)
	T := f.tenantID
	f.exec(`INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name) VALUES ($1, $2, '_chunks/h', 'local')`, T, chunkContainer)
	f.exec(`INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name) VALUES ($1, $2, 'k', 'onedrive')`, T, T+"_from-locations")
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, backend_name) VALUES ($1, 'from-versions', 'k', 'v1', 1, 'e', 'local')`, T)
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date) VALUES ($1, 'from-locks', 'k', 'GOVERNANCE', NOW())`, T)
	f.exec(`INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url) VALUES ($1, 'from-notifications', 's3:ObjectCreated:*', 'webhook', 'https://example.com/n')`, T)

	plan, err := f.runner.planSweep(context.Background(), T)

	require.NoError(t, err)
	for _, want := range []string{f.bucket, "from-locations", "from-versions", "from-locks", "from-notifications"} {
		assert.Contains(t, plan.buckets, want)
	}
	for _, b := range plan.buckets {
		assert.NotContains(t, b, "global", "bucket %q", b)
		assert.NotEqual(t, chunkContainer, T+"_"+b)
	}
	assert.Equal(t, []string{"onedrive"}, plan.unregistered, "a backend the rows name and nobody registered")
	assert.Equal(t, []string{"fixed", "local", "plain", "second"}, plan.backends)
}

// Every tenant table with a bucket column is a source of the sweep's bucket
// list or is excluded with a reason — a migration cannot add one unnoticed.
func TestSweepBucketSourcesCoverSchema(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	listed := map[string]bool{"buckets": true}
	for _, s := range sweepBucketSources {
		listed[s.table] = true
		stmt, err := db.PrepareContext(ctx, s.query)
		require.NoError(t, err, "source %s does not prepare", s.table)
		_ = stmt.Close()
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT c.table_name FROM information_schema.columns c
		 WHERE c.table_schema = 'public'
		   AND c.column_name IN ('bucket', 'bucket_name', 'container', 'container_name')
		   AND EXISTS (SELECT 1 FROM information_schema.columns t
		                WHERE t.table_schema = 'public' AND t.table_name = c.table_name AND t.column_name = 'tenant_id')
		 ORDER BY 1`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		seen++
		_, excluded := sweepBucketTablesExcluded[table]
		assert.True(t, listed[table] || excluded, "table %s has tenant_id and a bucket column: add it to sweepBucketSources or to sweepBucketTablesExcluded with the reason", table)
		assert.False(t, listed[table] && excluded, "table %s is both a source and excluded", table)
	}
	require.NoError(t, rows.Err())
	assert.Greater(t, seen, 10, "the walk saw the schema")
}

// --- a backend that fails ----------------------------------------------------------

// A listing that fails part-way (page 3) is not the end of the listing: the
// tenant is deferred, the account is not reported erased, the next run
// finishes the job.
func TestErasureSweep_AListingThatFailsDefersAndTheNextRunResumes(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	blobs := []string{f.onFixed(T, T+"_"+f.bucket, "o/1"), f.onFixed(T, T+"_"+f.bucket, "o/2"), f.onFixed(T, T+"_"+f.bucket, "o/3"), f.onFixed(T, T+"_"+f.bucket, "o/4")}
	onLocal := f.onLocal(f.primary, T, f.bucket, "orphan")
	f.fixed.failWalkAfter.Store(2)

	// Act
	te := f.run()

	// Assert
	assert.Equal(t, outcomeDeferred, te.Outcome, "%+v", te)
	assert.Contains(t, te.Error, "sweep: fixed:")
	assert.Contains(t, te.Error, "next page")
	assert.True(t, f.userExists(), "not erased while bytes may remain")
	assert.Zero(t, f.erasedRows(), "no erasure record")
	assertGone(t, "handed out before the failure", blobs[:2]...)
	assertKept(t, "behind the failed page", blobs[2:]...)
	assertGone(t, "the other backends are still swept: the run makes progress", onLocal)
	assert.Equal(t, 2, te.Swept["fixed"])

	// The next run starts over: what is gone is not listed.
	f.fixed.failWalkAfter.Store(0)
	te = f.run()
	assert.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assertGone(t, "the rest", blobs...)
	assert.Equal(t, 2, te.Swept["fixed"])
	assert.False(t, f.userExists())
}

func TestErasureSweep_AListFailureOnABackendThatCannotWalkDefers(t *testing.T) {
	f := setupSweepFixture(t)
	orphan := f.onLocal(f.plainDir, f.tenantID, f.bucket, "orphan")
	f.plain.failList.Store(true)

	te := f.run()

	assert.Equal(t, outcomeDeferred, te.Outcome)
	assert.Contains(t, te.Error, `plain: list bucket "`+f.bucket+`"`)
	assert.Contains(t, te.Error, "AccessDenied", "a List error is never read as an empty container")
	assertKept(t, "not listed, not deleted", orphan)
	assert.True(t, f.userExists())

	f.plain.failList.Store(false)
	te = f.run()
	assert.Equal(t, outcomeErased, te.Outcome)
	assertGone(t, "the next run", orphan)
}

// A delete that hangs is cut by its own deadline; a backend that keeps
// failing is given up after SweepMaxFailures, not one timeout per object.
func TestErasureSweep_ADeleteThatHangsIsCutAndDefers(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	var blobs []string
	for i := 0; i < 6; i++ {
		blobs = append(blobs, f.onFixed(T, T+"_"+f.bucket, fmt.Sprintf("o/%d", i)))
	}
	f.fixed.hangRemove.Store(true)
	f.runner.SweepDeleteTimeout = 40 * time.Millisecond
	f.runner.SweepMaxFailures = 3

	// Act
	start := time.Now()
	te := f.run()
	elapsed := time.Since(start)

	// Assert
	assert.Equal(t, outcomeDeferred, te.Outcome, "%+v", te)
	assert.Contains(t, te.Error, "3 deletes failed")
	assert.Contains(t, te.Error, "deadline exceeded")
	assert.Equal(t, int32(3), f.fixed.removes.Load(), "the backend is given up after SweepMaxFailures")
	assert.Equal(t, 3, te.SweepFailures)
	assert.Less(t, elapsed, 5*time.Second, "three deadlines, not a hang")
	assertKept(t, "nothing was deleted", blobs...)
	assert.True(t, f.userExists())

	// A backend that refuses (no hang) below the limit: every object is tried, then deferred.
	f.fixed.hangRemove.Store(false)
	f.fixed.failRemove.Store(true)
	f.fixed.removes.Store(0)
	f.runner.SweepMaxFailures = 25
	te = f.run()
	assert.Equal(t, outcomeDeferred, te.Outcome)
	assert.Contains(t, te.Error, "6 deletes failed")
	assert.Equal(t, int32(6), f.fixed.removes.Load())

	f.fixed.failRemove.Store(false)
	te = f.run()
	assert.Equal(t, outcomeErased, te.Outcome)
	assertGone(t, "once the backend answers", blobs...)
}

// A backend whose circuit breaker is open is not asked at all; the tenant is
// deferred (the job retries hourly).
func TestErasureSweep_AnOpenBreakerDefersWithoutAskingTheBackend(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	blob := f.onFixed(T, T+"_"+f.bucket, "orphan")
	onLocal := f.onLocal(f.primary, T, f.bucket, "orphan")
	f.fixed.failGet.Store(true)
	for i := 0; i < 10 && f.eng.GetFailoverStatus()["fixed"] != "open"; i++ {
		f.eng.HintBackend("c", "probe", "fixed")
		_, _ = f.eng.Get(context.Background(), "c", "probe")
	}
	require.Equal(t, "open", f.eng.GetFailoverStatus()["fixed"], "fixture: the breaker must be open")

	// Act
	te := f.run()

	// Assert
	assert.Equal(t, outcomeDeferred, te.Outcome, "%+v", te)
	assert.Contains(t, te.Error, "fixed: unavailable (circuit breaker open)")
	assert.Zero(t, f.fixed.walks.Load(), "a backend whose breaker is open is not listed")
	assertKept(t, "on the unavailable backend", blob)
	assertGone(t, "the reachable backends are swept", onLocal)
	assert.True(t, f.userExists())
}

// --- the user, the writer, the deploy ------------------------------------------------

func TestErasureSweep_ACancelBeforeOrDuringTheSweepStopsIt(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		f := setupSweepFixture(t)
		orphan := f.onLocal(f.primary, f.tenantID, f.bucket, "orphan")
		f.runner.beforeSweep = func() { require.NoError(t, f.runner.account.Cancel(context.Background(), f.userID)) }

		te := f.run()

		assert.Equal(t, outcomeCancelled, te.Outcome, "%+v", te)
		assertKept(t, "a cancel wins before the first listing", orphan)
		assert.Zero(t, f.fixed.walks.Load())
		assert.True(t, f.userExists())
	})
	t.Run("during", func(t *testing.T) {
		f := setupSweepFixture(t)
		T := f.tenantID
		blobs := []string{f.onFixed(T, T+"_"+f.bucket, "o/1"), f.onFixed(T, T+"_"+f.bucket, "o/2"), f.onFixed(T, T+"_"+f.bucket, "o/3")}
		f.runner.SweepCancelEvery = 1
		f.fixed.onObject = func(n int) {
			if n == 1 {
				require.NoError(t, f.runner.account.Cancel(context.Background(), f.userID))
			}
		}

		te := f.run()

		assert.Equal(t, outcomeCancelled, te.Outcome, "%+v", te)
		assertGone(t, "deleted before the cancel", blobs[0])
		assertKept(t, "the sweep stopped", blobs[1:]...)
		assert.True(t, f.userExists())
		assert.Zero(t, f.erasedRows())
	})
}

// A deploy in the middle of a sweep: the run is `interrupted`, never the
// day's success, nothing is reported erased, and the next run finishes.
func TestErasureSweep_AShutdownMidSweepIsInterruptedAndResumes(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	blobs := []string{f.onFixed(T, T+"_"+f.bucket, "o/1"), f.onFixed(T, T+"_"+f.bucket, "o/2"), f.onFixed(T, T+"_"+f.bucket, "o/3")}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	f.fixed.onObject = func(n int) {
		if n == 1 {
			stop() // SIGTERM while the second blob is being handed out
		}
	}

	// Act
	_, err := f.sj.runWith(ctx, func(ctx context.Context) (jobReport, error) {
		r, runErr := f.runner.RunOnce(ctx)
		return jobReport{Rows: int64(r.Erased)}, runErr
	})

	// Assert
	require.Error(t, err)
	rows, rerr := jobRunRows(context.Background(), f.db)
	require.NoError(t, rerr)
	assert.Equal(t, jobOutcomeInterrupted, rows[f.runner.JobName].Outcome)
	assert.False(t, rows[f.runner.JobName].LastSuccess.Valid, "an interrupted run is never the day's success")
	assert.True(t, f.userExists(), "not erased")
	assert.Zero(t, f.erasedRows(), "no erasure record")
	assertGone(t, "deleted before the stop", blobs[0])
	assertKept(t, "not reached", blobs[2])

	// The next boot's catch-up.
	f.fixed.onObject = nil
	te := f.run()
	assert.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assertGone(t, "after the restart", blobs...)
}

// A PUT that lands during the sweep. Its bytes are swept like any blob with
// no head row; when its row commits, EraseRows refuses (ErrObjectsRemain) and
// the tenant is deferred — the next run deletes the row and erases.
func TestErasureSweep_AWriteThatLandsDuringTheSweepDefersTheErase(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	f.onFixed(T, T+"_"+f.bucket, "o/1")
	var late string
	f.fixed.onObject = func(n int) {
		if n == 0 && late == "" {
			// "fixed" is swept before "local": the bytes land now, the row commits now.
			late = f.onLocal(f.primary, T, f.bucket, "late/put.bin")
			f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, 'late/put.bin', 5, 'e', 'local')`, T, f.bucket)
		}
	}

	// Act
	te := f.run()

	// Assert
	assert.Equal(t, outcomeDeferred, te.Outcome, "%+v", te)
	assert.Contains(t, te.Error, "erase rows")
	assert.True(t, f.userExists(), "EraseRows refused: a head row names an object")
	assertGone(t, "the late blob had no row when the sweep listed it", late)

	te = f.run()
	assert.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, T))
}

// Bytes that land after the sweep with no row (the write was in flight when
// its container was listed) are found by the pass after the row erase.
func TestErasureSweep_ThePassAfterTheEraseFindsBytesThatLandedLate(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	T := f.tenantID
	var late []string
	f.runner.beforeFinalSweep = func() {
		late = append(late, f.onFixed(T, T+"_"+f.bucket, "late/1"), f.onLocal(f.plainDir, T, f.bucket, "late/2"))
	}

	// Act
	te := f.run()

	// Assert
	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	require.Len(t, late, 2)
	assertGone(t, "found after the rows were erased (the plan was read before)", late...)
	assert.Equal(t, 2, te.SweptAfterErase)
	var after string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'swept_after_erase' FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, T).Scan(&after))
	assert.Equal(t, "2", after)
}

// When that last pass fails the account IS erased (the rows are gone, nothing
// can defer) and the record says the pass did not complete.
func TestErasureSweep_AFailedPassAfterTheEraseIsRecordedNotHidden(t *testing.T) {
	f := setupSweepFixture(t)
	T := f.tenantID
	var late string
	f.runner.beforeFinalSweep = func() {
		late = f.onFixed(T, T+"_"+f.bucket, "late/1")
		f.fixed.failRemove.Store(true)
	}

	te := f.run()

	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.Contains(t, te.Error, "sweep after the erase")
	assertKept(t, "the backend refused", late)
	var note string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(metadata->>'note', '') FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, T).Scan(&note))
	assert.Contains(t, note, "sweep after the erase", "the erasure record names what was left")
}

// A backend the tenant's rows name and nobody registered cannot be swept at
// all: the erasure goes through and the record says so.
func TestErasureSweep_AnUnregisteredBackendIsNamedInTheRecord(t *testing.T) {
	f := setupSweepFixture(t)
	T := f.tenantID
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, backend_name) VALUES ($1, $2, 'gone/k', 'v1', 1, 'e', 'onedrive')`, T, f.bucket)

	te := f.run()

	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.Equal(t, []string{"onedrive"}, te.UnsweptBackends)
	var unswept string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'unswept_backends' FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, T).Scan(&unswept))
	assert.JSONEq(t, `["onedrive"]`, unswept)
}

func TestErasureSweep_MetricsExistAtZeroForEveryRegisteredBackend(t *testing.T) {
	// Arrange: a runner over an engine with two backends nobody else names.
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("zero-a", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("zero-b", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	require.NotNil(t, NewAccountDeletionRunner(db, zap.NewNop(), eng, nil, nil, account.NewService(db, zap.NewNop())))
	s := &Server{}
	s.initMetrics()

	// Act
	body := scrapeMetrics(t, s)

	// Assert
	for _, backend := range []string{"zero-a", "zero-b"} {
		for _, result := range []string{"deleted", "failed", "shared_left"} {
			assert.Contains(t, body, `vaultaire_account_deletion_swept_total{backend="`+backend+`",result="`+result+`"} 0`)
		}
	}
}

// Post-merge review: "default" is the tenant id every driver and
// common.GetTenantID fall back to when a context carries no tenant. On a
// fixed-bucket backend `t-default/` therefore holds whatever ANY tenant's
// context-less path wrote (and is where dedup GC addresses chunk blobs). No
// registration mints that id, but a tenants row with it — a dev database, a
// hand-made row — would have been swept like any other: every backend's
// shared fallback prefix, deleted.
func TestErasureSweep_RefusesTheFallbackTenantID(t *testing.T) {
	// Arrange
	f := setupSweepFixture(t)
	ctx := context.Background()

	// Act
	_, planErr := f.runner.planSweep(ctx, "default")
	te := TenantErasure{}
	_, sweepErr := f.runner.sweepTenant(ctx, account.Due{TenantID: "default", UserID: f.userID},
		sweepPlan{backends: []string{"fixed", "local", "plain"}, buckets: []string{"b"}}, &te, zap.NewNop(), true)

	// Assert
	assert.Error(t, planErr)
	assert.Error(t, sweepErr)
	assert.Zero(t, f.fixed.walks.Load(), "nothing may be listed under the shared fallback prefix")
	assert.Zero(t, f.plain.lists.Load())
}
