package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// Tape takes only sealed bytes. A delete on Geyser never erases a byte from
// the cartridge — a collection is the unit of reclaim
// (bench-results/GEYSER-LA2-TAPE-RECLAIM-2026-10-25.md) — and Geyser's own
// encryption is off on our account, so an object that reaches tape in the
// clear stays readable there for the life of the cartridge, and an account
// erasure can only be a crypto-shred. The archive classes are therefore
// sealed-only like BLOMP and SIA: SSE-S3 whatever the bucket says, or the
// customer's own key; refused (503, nothing stored) with no master key.
// GEYSER_ALLOW_UNSEALED lifts it (labs).

func TestSealedOnlyClass_ArchiveClasses(t *testing.T) {
	for _, class := range []string{"GLACIER", "DEEP_ARCHIVE"} {
		assert.True(t, sealedOnlyClass(class, false, false, false), class)
		assert.False(t, sealedOnlyClass(class, false, false, true), "%s: GEYSER_ALLOW_UNSEALED lifts it", class)
		assert.True(t, sealedOnlyClass(class, true, true, false), "%s: the other tiers' switches do not", class)
	}
	assert.False(t, sealedOnlyClass("STANDARD", false, false, false))
	assert.False(t, sealedOnlyClass("", false, false, false))
}

func TestDecidePlacement_TapeIsSealedOrRefused(t *testing.T) {
	f := setupAdapterFixture(t)
	for _, class := range []string{"GLACIER", "DEEP_ARCHIVE"} {
		req := placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: class, Size: 10}

		f.adapter.sseService = nil
		p := f.adapter.decidePlacement(context.Background(), req)
		require.NotNil(t, p.Refusal, class)
		assert.Equal(t, http.StatusServiceUnavailable, p.Refusal.Status, class)
		assert.Same(t, refuseTapeTierNoKey, p.Refusal, "%s: the tape's own sentence", class)

		empty := req
		empty.Size = 0
		p = f.adapter.decidePlacement(context.Background(), empty)
		require.Nil(t, p.Refusal, "%s: an empty object holds nothing to seal", class)

		ssec := req
		ssec.SSEC = true
		p = f.adapter.decidePlacement(context.Background(), ssec)
		require.Nil(t, p.Refusal, "%s: the customer's key seals it", class)
		assert.Equal(t, placeWhole, p.Kind, class)

		f.adapter.sseService = sseFor(t, f.db, f.tenantID)
		p = f.adapter.decidePlacement(context.Background(), req)
		require.Nil(t, p.Refusal, class)
		assert.Equal(t, placeWholeSealed, p.Kind, "%s: sealed although the bucket is not sse_enabled", class)
	}
}

func TestDecidePlacement_TapeUnsealedWhenAllowed(t *testing.T) {
	f := setupAdapterFixture(t)
	req := placementRequest{TenantID: f.tenantID, Bucket: "test-bucket", Class: "GLACIER", Size: 10}

	f.adapter.sseService = nil
	f.adapter.allowUnsealedCleartext = true
	f.adapter.allowUnsealedSia = true
	p := f.adapter.decidePlacement(context.Background(), req)
	require.NotNil(t, p.Refusal, "each tier has its own switch")

	f.adapter.allowUnsealedTape = true
	p = f.adapter.decidePlacement(context.Background(), req)
	require.Nil(t, p.Refusal)
	assert.Equal(t, placeWhole, p.Kind)
}

func TestAllowUnsealedTapeFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "true": true, "1": true, "false": false, "yes": false} {
		got := allowUnsealedTapeFromEnv(func(k string) string {
			if k == "GEYSER_ALLOW_UNSEALED" {
				return v
			}
			return ""
		}, zap.NewNop())
		assert.Equal(t, want, got, "%q", v)
	}
}

// Smart demotion moves bytes onto tape outside the placement decision: it
// demotes only an object whose stored bytes are already sealed, unless
// GEYSER_ALLOW_UNSEALED. A plaintext object stays hot (never a refusal, never
// a failed run — it is simply not a candidate).
func TestSmartDemotion_TapeTakesOnlySealedBytes(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.runner.AllowUnsealedCold = false
	f.object("b", "plain", 100*gb, 30, 20)
	f.object("b", "sse", 100*gb, 30, 20, encrypted(crypto.SSEAlgorithm))
	f.object("b", "ssec", 100*gb, 30, 20, encrypted(crypto.SSECAlgorithmV3))

	res, err := f.runner.RunOnce(context.Background(), false)

	require.NoError(t, err)
	assert.Equal(t, 2, res.Demoted, "%+v", res)
	assert.Equal(t, "idrive", f.backendOf("b", "plain"), "plaintext never reaches tape")
	assert.Equal(t, "geyser", f.backendOf("b", "sse"))
	assert.Equal(t, "geyser", f.backendOf("b", "ssec"))

	f.runner.AllowUnsealedCold = true
	res, err = f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Demoted, "%+v", res)
	assert.Equal(t, "geyser", f.backendOf("b", "plain"), "GEYSER_ALLOW_UNSEALED lifts it")
}

// A cold backend that is no tape is not this rule's.
func TestSmartDemotion_SealedRuleIsTheTapes(t *testing.T) {
	assert.True(t, coldBackendSealedOnly("geyser", false))
	assert.False(t, coldBackendSealedOnly("geyser", true))
	assert.False(t, coldBackendSealedOnly("lyve", false))
	assert.False(t, coldBackendSealedOnly("idrive", false))
}
