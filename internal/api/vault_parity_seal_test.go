package api

import (
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-FLEET next chain Part 3 (D-F-SEAL-1): the permafrost fleet joins the
// sealed-only leg rule. A cleartext-transport leg (blomp) and sia were
// skipped unless BLOMP_/SIA_ALLOW_UNSEALED; permafrost (TLS) was not — so
// the four RS shards of a PLAINTEXT Vault object, a complete readable copy,
// went to drives whose data app (one credential per tenant) can read them.
// Now an unsealed object's shards are not written to permafrost unless
// PERMAFROST_ALLOW_UNSEALED=true; an SSE-S3 or SSE-C object's (ciphertext)
// go as before. With no master key on prod (F-14), that is SSE-C only until
// the key ceremony — the correct state.

// sealedObject plants an SSE-C vault object: stored bytes = the plaintext
// size + the SSE-C overhead (the job encodes the stored bytes, never opens
// them), the head row naming its algorithm.
func (f *parityFixture) sealedObject(key string, size int) {
	f.t.Helper()
	stored := make([]byte, size+crypto.SSECOverhead)
	_, err := rand.Read(stored)
	require.NoError(f.t, err)
	sum := md5.Sum(stored) // #nosec G401 -- the S3 ETag
	container := f.tn.NamespaceContainer(f.bucket)
	require.NoError(f.t, os.MkdirAll(filepath.Join(f.geyserDir, container, filepath.Dir(key)), 0o750))
	require.NoError(f.t, os.WriteFile(filepath.Join(f.geyserDir, container, key), stored, 0o600))
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked,
		                               content_type, encryption_algorithm)
		VALUES ($1,$2,$3,$4,$5,'geyser','vault',false,'application/octet-stream',$6)`,
		f.tenantID, f.bucket, key, size, hex.EncodeToString(sum[:]), crypto.SSECAlgorithm)
	require.NoError(f.t, err)
}

func TestVaultParity_AnUnsealedObjectNeverGoesToPermafrostUnlessAllowed(t *testing.T) {
	// Arrange: permafrost is the only leg; the switch off (prod's default).
	f := setupParityFixture(t)
	f.svc.AllowUnsealedPermafrost = false
	f.object("plain.bin", 1<<20)

	// Act
	res := f.run()

	// Assert: no row, no shard, counted as unprotected for that reason.
	assert.Equal(t, 0, res.Protected, "%+v", res)
	assert.Equal(t, 1, res.UnsealedLegSkipped, "%+v", res)
	state, _, _, _ := f.row("plain.bin")
	assert.Empty(t, state, "no row: nothing was written")
	assert.Empty(t, f.shardFiles())
	assert.Equal(t, int32(0), f.leg.puts.Load())
	assert.Equal(t, float64(1), promtest.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("unsealed_leg")))
	assert.NotEmpty(t, res.Notes)

	// With the switch (a lab), it protects.
	f.svc.AllowUnsealedPermafrost = true
	res = f.run()
	assert.Equal(t, 1, res.Protected, "%+v", res)
	state, _, _, _ = f.row("plain.bin")
	assert.Equal(t, "complete", state)
	assert.Equal(t, float64(0), promtest.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("unsealed_leg")))
}

func TestVaultParity_ASealedObjectGoesToPermafrostRegardless(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.AllowUnsealedPermafrost = false
	f.sealedObject("sealed.bin", 1<<20)

	res := f.run()

	assert.Equal(t, 1, res.Protected, "%+v", res)
	assert.Equal(t, 0, res.UnsealedLegSkipped)
	state, legs, _, _ := f.row("sealed.bin")
	assert.Equal(t, "complete", state)
	assert.Equal(t, []string{"permafrost", "permafrost", "permafrost", "permafrost"}, legs)
}

func TestVaultParityLeg_AnUnsealedObjectFallsToTheNextLegThatMayTakeIt(t *testing.T) {
	eng := placementEngine(t, true)
	eng.AddDriver("permafrost", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("lyve", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	p := &VaultParity{eng: eng, Legs: []string{"permafrost", "lyve"}}

	name, _, ok := p.legForObject("", true)
	require.True(t, ok)
	assert.Equal(t, "permafrost", name, "ciphertext shards go to the first leg")

	name, _, ok = p.legForObject("", false)
	require.True(t, ok)
	assert.Equal(t, "lyve", name, "a plaintext object's shards skip the fleet")

	p.Legs = []string{"permafrost"}
	_, _, ok = p.legForObject("", false)
	assert.False(t, ok, "no leg may take them: unprotected, never on the fleet")
	name, _, ok = p.Leg()
	require.True(t, ok, "the job still runs: sealed objects have a leg")
	assert.Equal(t, "permafrost", name)

	p.AllowUnsealedPermafrost = true
	name, _, ok = p.legForObject("", false)
	require.True(t, ok)
	assert.Equal(t, "permafrost", name)
}

func TestAllowUnsealedPermafrostFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "1": true, "yes": false} {
		core, logs := observer.New(zap.WarnLevel)
		got := allowUnsealedPermafrostFromEnv(func(k string) string {
			if k == "PERMAFROST_ALLOW_UNSEALED" {
				return v
			}
			return ""
		}, zap.New(core))
		assert.Equal(t, want, got, "PERMAFROST_ALLOW_UNSEALED=%q", v)
		if v == "" || v == "false" || v == "0" {
			assert.Equal(t, 0, logs.Len(), "%q is silent", v)
		} else {
			assert.Equal(t, 1, logs.Len(), "%q: one Warn", v)
		}
	}
}
