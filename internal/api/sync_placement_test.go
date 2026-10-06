package api

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
)

// The Sync.com WebDAV bridge ("sync") holds customer data only for a bucket
// whose tier_preference is `sync` (set by an operator; no customer-facing
// control offers it) AND whose tenant has the `sync_backend` flag. Sync's
// terms forbid reselling the service without its written consent, so with
// either missing the bucket places exactly as an `auto` one.

func syncPlacementEngine(t *testing.T) *engine.CoreEngine {
	t.Helper()
	eng := placementEngine(t, true)
	eng.AddDriver("sync", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	return eng
}

func syncGateFor(tenants ...string) func(string) bool {
	on := map[string]bool{}
	for _, t := range tenants {
		on[t] = true
	}
	return func(tenantID string) bool { return on[tenantID] }
}

func TestResolvePutStorageClass_SyncNeedsTheFlag(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	eng := syncPlacementEngine(t)
	ctx := context.Background()
	seedPlacementBucket(t, tenantID, "mine", "private", "sync")

	// Flagged tenant: the bucket's objects go to the bridge, whatever class
	// the client asks for (a placement promise, like resilient).
	gate := syncGateFor(tenantID)
	assert.Equal(t, "SYNC", resolvePutStorageClass(ctx, db, eng, gate, tenantID, "mine", ""))
	assert.Equal(t, "SYNC", resolvePutStorageClass(ctx, db, eng, gate, tenantID, "mine", "STANDARD"))
	assert.Equal(t, "SYNC", resolvePutStorageClass(ctx, db, eng, gate, tenantID, "mine", "GLACIER"))

	// Without the flag (another tenant's gate, or no gate at all) the tier is
	// ignored: the primary, or what the client asked for.
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, syncGateFor("someone-else"), tenantID, "mine", ""))
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, nil, tenantID, "mine", ""))
	assert.Equal(t, "GLACIER", resolvePutStorageClass(ctx, db, eng, nil, tenantID, "mine", "GLACIER"))

	// A public-read sync bucket without the flag is an ordinary public bucket.
	seedPlacementBucket(t, tenantID, "pub", "public-read", "sync")
	assert.Equal(t, "PUBLIC", resolvePutStorageClass(ctx, db, eng, nil, tenantID, "pub", ""))
	assert.Equal(t, "SYNC", resolvePutStorageClass(ctx, db, eng, gate, tenantID, "pub", ""))
}

func TestResolvePutStorageClass_ClientCannotAskForSync(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	eng := syncPlacementEngine(t)
	seedPlacementBucket(t, tenantID, "auto", "private", "auto")

	assert.Equal(t, "", resolvePutStorageClass(context.Background(), db, eng, syncGateFor(tenantID), tenantID, "auto", "SYNC"),
		"SYNC is an internal class: the header never selects it, flag or not")
}

func TestSyncClass_StoresWholeObjects(t *testing.T) {
	assert.True(t, storageClassDisablesChunking("SYNC"),
		"chunk blobs live on the primary: a chunked object would break the bucket's placement")
}

func TestSyncPlacementEndToEnd_ObjectLandsOnSyncAndReadsBack(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	eng := syncPlacementEngine(t)
	seedPlacementBucket(t, tenantID, "mine", "private", "sync")

	class := resolvePutStorageClass(context.Background(), db, eng, syncGateFor(tenantID), tenantID, "mine", "")
	require.Equal(t, "SYNC", class)
	ctx := context.Background()
	backend, err := eng.Put(ctx, tenantID+"_mine", "k", strings.NewReader("on the bridge"), engine.WithStorageClass(class))
	require.NoError(t, err)
	assert.Equal(t, "sync", backend, "the engine records the backend the API stores in object_head_cache.backend_name")
	assert.Equal(t, "STANDARD", engine.CustomerStorageClass("standard", backend))

	sync, _ := eng.GetDriver("sync")
	ok, err := sync.Exists(ctx, tenantID+"_mine", "k")
	require.NoError(t, err)
	assert.True(t, ok)
	primary, _ := eng.GetDriver("local")
	ok, err = primary.Exists(ctx, tenantID+"_mine", "k")
	require.NoError(t, err)
	assert.False(t, ok, "nothing on the primary")

	// Reads follow the recorded backend (the API hints it from the head row).
	eng.HintBackend(tenantID+"_mine", "k", backend)
	rc, err := eng.Get(ctx, tenantID+"_mine", "k")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
}

func TestSyncBackendFlag_RegisteredDefaultOff(t *testing.T) {
	fl := flags.New(nil, zap.NewNop())
	registerFlags(fl)
	assert.Equal(t, "sync_backend", flagSyncBackend)
	assert.True(t, fl.Registered(flagSyncBackend))
	assert.False(t, fl.Enabled(flagSyncBackend, "any-tenant"))
	assert.False(t, syncPlacementGate(fl)("any-tenant"))
	assert.Nil(t, syncPlacementGate(nil), "no flag service = no gate = never sync")
}

func TestSyncTier_NotSettableByCustomers(t *testing.T) {
	assert.False(t, validTierPreferences["sync"], "PUT /api/v1/manage/buckets/{name}/tier refuses it: operator-set only")
}
