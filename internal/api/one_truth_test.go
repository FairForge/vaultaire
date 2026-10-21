package api

// WP-R6-1 Part B at the handlers: nothing on the read or write path reads
// or writes object_locations; the two #677 bypasses (a generated object
// into a sync-tier bucket, a region-pinned write) follow the PUT's rules.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestReadPath_DoesNotTouchObjectLocations(t *testing.T) {
	// Before (origin/main 9f3d0c1): every engine Put wrote an
	// object_locations row in a goroutine (one per chunk too, R8-19), a read
	// whose route was not in memory read it and touched last_accessed in
	// another goroutine (R6-14/15), and Delete removed it.
	f := setupLocFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_locations WHERE tenant_id = $1`, f.tenantID) })
	// A row the old engine would have written: nothing may read it now.
	_, err := f.db.Exec(`INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name, last_accessed)
		VALUES ($1, $2, 'stale.bin', 'other04', NOW() - interval '30 days')`, f.tenantID, f.tenant.NamespaceContainer("test-bucket"))
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	require.Equal(t, http.StatusOK, f.get(t, "k.bin", "").Code)
	require.Equal(t, http.StatusPartialContent, f.get(t, "k.bin", "bytes=0-9").Code)
	require.Equal(t, http.StatusNoContent, f.del(t, "k.bin").StatusCode)
	time.Sleep(200 * time.Millisecond) // the old writes were fire-and-forget goroutines

	var rows int
	var touched bool
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*), BOOL_OR(last_accessed > NOW() - interval '1 hour')
		FROM object_locations WHERE tenant_id = $1`, f.tenantID).Scan(&rows, &touched))
	assert.Equal(t, 1, rows, "no row written or removed: only the planted one")
	assert.False(t, touched, "and never touched")
}

// regionDouble is a region backend that counts its writes, consumes the
// body, and fails them while fail is set.
type regionDouble struct {
	*fixedBucketDriver
	puts atomic.Int32
	fail atomic.Bool
}

func (d *regionDouble) Put(ctx context.Context, c, a string, r io.Reader, opts ...engine.PutOption) error {
	d.puts.Add(1)
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if d.fail.Load() {
		return errors.New("dial tcp 203.0.113.9:443: connect: connection refused")
	}
	return nil
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("client connection reset") }

func TestPlaceObject_RegionDriver_BreakerAndStrictRules(t *testing.T) {
	// Before (#677): placeObject's region branch called the region driver
	// directly — outside the engine's breaker (a dead region endpoint was
	// asked by every PUT, its breaker never opened), its tenant rule and
	// its ErrCallerAborted tagging (a client that hung up mid-body was a
	// backend failure).
	db := cdnTestDB(t)
	tenantID := "region-" + t.Name()
	seedRegionBucket(t, tenantID, "eu", "eu-west-1")
	eng := regionEngine(t)
	region := &regionDouble{fixedBucketDriver: &fixedBucketDriver{dir: t.TempDir()}}
	eng.AddDriver("idrive-eu-west-1", region)
	ctx := common.WithTenantID(context.Background(), tenantID)
	put := func(ctx context.Context, body io.Reader) (string, error) {
		return placeObject(ctx, db, eng, tenantID, "eu", tenantID+"_eu", "k@00000000000000000000000000", body)
	}

	// A body the client stopped sending: the caller's, never a breaker charge.
	for i := 0; i < 6; i++ {
		_, err := put(ctx, brokenBody{})
		assert.ErrorIs(t, err, engine.ErrCallerAborted)
	}
	assert.Equal(t, "closed", eng.GetFailoverStatus()["idrive-eu-west-1"])

	// A dead region endpoint: five failures open its breaker, then it is not
	// asked: 503 (ErrAllBackendsUnavailable), never the primary.
	region.fail.Store(true)
	for i := 0; i < 5; i++ {
		_, err := put(ctx, bytes.NewReader([]byte("residency")))
		assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	}
	require.Equal(t, "open", eng.GetFailoverStatus()["idrive-eu-west-1"])
	before := region.puts.Load()
	backend, err := put(ctx, bytes.NewReader([]byte("residency")))
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	assert.Empty(t, backend)
	assert.Equal(t, before, region.puts.Load(), "an open breaker: the region backend is not asked")

	// No tenant: refused before the backend.
	expectNoTenantRefusals(t, func() {
		_, err = put(context.Background(), bytes.NewReader([]byte("x")))
	})
	assert.ErrorIs(t, err, engine.ErrNoTenant)
}

func TestGeneratedObject_SyncTierBucket_FollowsTheSyncGate(t *testing.T) {
	// Before (#677): generatedObjectWriter resolved the class with
	// allowTier = nil, so an access log or inventory report into a sync-tier
	// bucket of a tenant with the sync_backend flag landed on the primary —
	// off the tier the bucket promises — while a PUT there landed on sync.
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	seedPlacementBucket(t, tenantID, "mine", "private", "sync")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, tenantID)
	})
	eng := syncPlacementEngine(t)
	w := newGeneratedObjectWriter(db, eng, nil, nil, zap.NewNop())
	body := func(wr io.Writer) error { _, err := wr.Write([]byte("a report line\n")); return err }

	// The server wires the PUT's gate (server.go: tierPlacementGate).
	w.allowTier = syncGateFor(tenantID)
	got, err := w.write(context.Background(), generatedKindAccessLog, tenantID, "mine", "logs/a.log", "text/plain", body)
	require.NoError(t, err)
	assert.Equal(t, "sync", got.Backend, "where a PUT into this bucket lands")

	// The flag off for the tenant: the primary, as for a PUT.
	w.allowTier = syncGateFor("someone-else")
	got, err = w.write(context.Background(), generatedKindAccessLog, tenantID, "mine", "logs/b.log", "text/plain", body)
	require.NoError(t, err)
	assert.Equal(t, "local", got.Backend)
}
