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

// Sia through the s3d gateway ("sia", drivers/sia.go) holds objects only for
// a bucket whose tier_preference is `sia` (operator-set) of a tenant with the
// `sia_backend` flag — whole objects only, and only SEALED: a write there is
// SSE-S3 whatever the bucket says, or the customer's own key, and refused
// with no master key — so neither s3d's app key nor a Sia host can read it,
// and an account erasure is a crypto-shred at once although s3d unpins
// deleted data only on its hourly orphan loop. SIA_ALLOW_UNSEALED lifts it.

func siaGateFor(tenants ...string) tierGate {
	on := map[string]bool{}
	for _, t := range tenants {
		on[t] = true
	}
	return func(class, tenantID string) bool { return class == "SIA" && on[tenantID] }
}

func TestResolvePutStorageClass_SiaNeedsTheFlag(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	eng := placementEngine(t, true)
	eng.AddDriver("sia", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	ctx := context.Background()
	seedPlacementBucket(t, tenantID, "mine", "private", "sia")

	assert.Equal(t, "SIA", resolvePutStorageClass(ctx, db, eng, siaGateFor(tenantID), tenantID, "mine", ""))
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, siaGateFor("someone-else"), tenantID, "mine", ""))
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, blompGateFor(tenantID), tenantID, "mine", ""),
		"the blomp flag does not open the sia tier")
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, nil, tenantID, "mine", ""))
	seedPlacementBucket(t, tenantID, "auto", "private", "auto")
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, siaGateFor(tenantID), tenantID, "auto", "SIA"),
		"SIA is an internal class: never selected by the header")
}

func TestSiaClass_StoresWholeObjects_AndIsAStrictTarget(t *testing.T) {
	assert.True(t, storageClassDisablesChunking("SIA"), "whole objects: they flush to Sia 4.5x faster than chunks")
	assert.True(t, engine.IsStrictTarget("sia"), "never the primary in its place")
	assert.False(t, validTierPreferences["sia"], "operator-set only: the management API refuses it")
	backend, class := engine.ResolveStorageClass("SIA", "local", map[string]engine.Driver{})
	assert.Equal(t, "sia", backend, "returned even unregistered: the engine refuses it (503), never the primary")
	assert.Equal(t, "SIA", class)
	assert.Equal(t, "STANDARD", engine.BackendToStorageClass("sia"))
}

func TestSiaBackendFlag_RegisteredDefaultOff(t *testing.T) {
	fl := flags.New(nil, zap.NewNop())
	registerFlags(fl)
	assert.Equal(t, "sia_backend", flagSiaBackend)
	assert.True(t, fl.Registered(flagSiaBackend))
	assert.False(t, fl.Enabled(flagSiaBackend, "any-tenant"))
	assert.False(t, tierPlacementGate(fl)("SIA", "any-tenant"))
}

// The write onto the SIA class is sealed whatever the bucket says, and
// refused (503, nothing stored) on a process with no master key.
func TestDecidePlacement_SiaIsSealedOrRefused(t *testing.T) {
	f := setupAdapterFixture(t)
	req := placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "SIA", Size: 10}

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

	// BLOMP_ALLOW_UNSEALED does not lift the sia rule.
	f.adapter.allowUnsealedCleartext = true
	p = f.adapter.decidePlacement(context.Background(), req)
	require.NotNil(t, p.Refusal, "each tier has its own switch")
}

func TestSia_HasTheObjectCanary(t *testing.T) {
	assert.True(t, canaryBackend("sia"))
}

// SIA_ALLOW_UNSEALED lets the tier take plaintext; the bucket's own
// encryption rules still apply.
func TestDecidePlacement_SiaUnsealedWhenAllowed(t *testing.T) {
	f := setupAdapterFixture(t)
	f.adapter.sseService = nil
	f.adapter.allowUnsealedSia = true
	p := f.adapter.decidePlacement(context.Background(), placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "SIA", Size: 10})
	require.Nil(t, p.Refusal)
	assert.Equal(t, placeWhole, p.Kind)

	f.adapter.sseService = sseFor(t, f.db, f.tenantID)
	p = f.adapter.decidePlacement(context.Background(), placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "SIA", Size: 10})
	assert.Equal(t, placeWhole, p.Kind, "allowed means not forced: an unencrypted bucket stays unencrypted")
}

func TestAllowUnsealedSiaFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "true": true, "1": true, "false": false, "yes": false} {
		got := allowUnsealedSiaFromEnv(func(k string) string {
			if k == "SIA_ALLOW_UNSEALED" {
				return v
			}
			return ""
		}, zap.NewNop())
		assert.Equal(t, want, got, "%q", v)
	}
}

// The Vault parity job never makes `sia` its leg (shards of an unsealed
// object rebuild it) unless SIA_ALLOW_UNSEALED allows it.
func TestVaultParityLeg_SkipsSiaUnlessAllowed(t *testing.T) {
	eng := placementEngine(t, true)
	eng.AddDriver("sia", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("lyve", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	p := &VaultParity{eng: eng, Legs: []string{"sia", "lyve"}}

	name, _, ok := p.Leg()
	require.True(t, ok)
	assert.Equal(t, "lyve", name)

	p.AllowCleartextLegs = true
	name, _, ok = p.Leg()
	require.True(t, ok)
	assert.Equal(t, "lyve", name, "BLOMP_ALLOW_UNSEALED does not open the sia leg")

	p.AllowSiaLeg = true
	name, _, ok = p.Leg()
	require.True(t, ok)
	assert.Equal(t, "sia", name)
}
