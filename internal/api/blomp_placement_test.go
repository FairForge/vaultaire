package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
)

// Blomp's Swift storage ("blomp", drivers/swift_README.md) holds objects
// only for a bucket whose tier_preference is `blomp` (operator-set) of a
// tenant with the `blomp_backend` flag — and only SEALED: its storage URL is
// plain HTTP, so a write there is SSE-S3 whatever the bucket says, or the
// customer's own key, and refused with no master key.

func blompGateFor(tenants ...string) tierGate {
	on := map[string]bool{}
	for _, t := range tenants {
		on[t] = true
	}
	return func(class, tenantID string) bool { return class == "BLOMP" && on[tenantID] }
}

func TestResolvePutStorageClass_BlompNeedsTheFlag(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	eng := placementEngine(t, true)
	eng.AddDriver("blomp", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	ctx := context.Background()
	seedPlacementBucket(t, tenantID, "mine", "private", "blomp")

	assert.Equal(t, "BLOMP", resolvePutStorageClass(ctx, db, eng, blompGateFor(tenantID), tenantID, "mine", ""))
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, blompGateFor("someone-else"), tenantID, "mine", ""))
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, syncGateFor(tenantID), tenantID, "mine", ""),
		"the sync flag does not open the blomp tier")
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, nil, tenantID, "mine", ""))
	seedPlacementBucket(t, tenantID, "auto", "private", "auto")
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, blompGateFor(tenantID), tenantID, "auto", "BLOMP"),
		"BLOMP is an internal class: never selected by the header")
}

func TestBlompClass_StoresWholeObjects_AndIsAStrictTarget(t *testing.T) {
	assert.True(t, storageClassDisablesChunking("BLOMP"))
	assert.True(t, engine.IsStrictTarget("blomp"), "never the primary in its place")
	assert.False(t, validTierPreferences["blomp"], "operator-set only: the management API refuses it")
	backend, class := engine.ResolveStorageClass("BLOMP", "local", map[string]engine.Driver{})
	assert.Equal(t, "blomp", backend, "returned even unregistered: the engine refuses it (503), never the primary")
	assert.Equal(t, "BLOMP", class)
}

func TestBlompBackendFlag_RegisteredDefaultOff(t *testing.T) {
	fl := flags.New(nil, zap.NewNop())
	registerFlags(fl)
	assert.True(t, fl.Registered(flagBlompBackend))
	assert.False(t, fl.Enabled(flagBlompBackend, "any-tenant"))
	assert.False(t, tierPlacementGate(fl)("BLOMP", "any-tenant"))
}

// The write onto the BLOMP class is sealed whatever the bucket says, and
// refused (503, nothing stored) on a process with no master key.
func TestDecidePlacement_BlompIsSealedOrRefused(t *testing.T) {
	f := setupAdapterFixture(t)
	req := placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "BLOMP", Size: 10}

	f.adapter.sseService = nil
	p := f.adapter.decidePlacement(context.Background(), req)
	require.NotNil(t, p.Refusal)
	assert.Equal(t, http.StatusServiceUnavailable, p.Refusal.Status)

	f.adapter.sseService = sseFor(t, f.db, f.tenantID)
	p = f.adapter.decidePlacement(context.Background(), req)
	require.Nil(t, p.Refusal)
	assert.Equal(t, placeWholeSealed, p.Kind, "sealed although the bucket is not sse_enabled")

	ssec := req
	ssec.SSEC = true
	f.adapter.sseService = nil
	p = f.adapter.decidePlacement(context.Background(), ssec)
	require.Nil(t, p.Refusal, "the customer's key seals it")
	assert.Equal(t, placeWhole, p.Kind)

	std := req
	std.Class = ""
	p = f.adapter.decidePlacement(context.Background(), std)
	assert.Nil(t, p.Refusal, "other classes are untouched")
}

func TestBlomp_HasTheObjectCanary(t *testing.T) {
	assert.True(t, canaryBackend("blomp"))
}

// BLOMP_ALLOW_UNSEALED (the owner's testing switch, 2026-10-21) lets the
// tier take plaintext; the bucket's own encryption rules still apply.
func TestDecidePlacement_BlompUnsealedWhenAllowed(t *testing.T) {
	f := setupAdapterFixture(t)
	f.adapter.sseService = nil
	f.adapter.allowUnsealedCleartext = true
	p := f.adapter.decidePlacement(context.Background(), placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "BLOMP", Size: 10})
	require.Nil(t, p.Refusal)
	assert.Equal(t, placeWhole, p.Kind)

	f.adapter.sseService = sseFor(t, f.db, f.tenantID)
	p = f.adapter.decidePlacement(context.Background(), placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "BLOMP", Size: 10})
	assert.Equal(t, placeWhole, p.Kind, "allowed means not forced: an unencrypted bucket stays unencrypted")
}

func TestAllowUnsealedCleartextFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "true": true, "1": true, "false": false, "yes": false} {
		got := allowUnsealedCleartextFromEnv(func(string) string { return v }, zap.NewNop())
		assert.Equal(t, want, got, "%q", v)
	}
}

// cleartextLeg is a parity leg that declares a plain-HTTP transport.
type cleartextLeg struct{ engine.Driver }

func (cleartextLeg) CleartextTransport() bool { return true }

// The Vault parity job never makes a cleartext-transport driver its leg
// (shards of an unsealed object rebuild it) unless the switch allows it.
func TestVaultParityLeg_SkipsACleartextLegUnlessAllowed(t *testing.T) {
	eng := placementEngine(t, true)
	eng.AddDriver("blomp", cleartextLeg{drivers.NewLocalDriver(t.TempDir(), zap.NewNop())})
	eng.AddDriver("lyve", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	p := &VaultParity{eng: eng, Legs: []string{"blomp", "lyve"}}

	name, _, ok := p.Leg()
	require.True(t, ok)
	assert.Equal(t, "lyve", name)

	p.AllowCleartextLegs = true
	name, _, ok = p.Leg()
	require.True(t, ok)
	assert.Equal(t, "blomp", name)
}
