package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP19-1 (Stage 1a Prompt 18, Part 0.1): a 0x05 SSE-C multipart object's
// stored length is not a function of its size (storedBlobLength = 0 =
// unknown). The jobs that size a copy by it skip the object instead of
// writing it at 0 bytes, until they can read the length from the blob's
// own header.

func TestVaultParity_SSECMultipartObjectIsSkippedAndCounted(t *testing.T) {
	// Arrange: a vault-floor object recorded as the 0x05 container.
	f := setupParityFixture(t)
	f.object("films/sealed-mp.mkv", 1<<20)
	_, err := f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $3 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, "films/sealed-mp.mkv", crypto.SSECAlgorithmMultipart)
	require.NoError(t, err)

	// Act
	res := f.run()

	// Assert: never encoded at 0 bytes — no row, no shard, counted and noted.
	assert.Equal(t, 0, res.Protected, "%+v", res)
	assert.Equal(t, 0, res.Failed, "%+v", res)
	assert.Contains(t, resultJSON(t, res), `"unknown_length_skipped":1`)
	assert.NotEmpty(t, res.Notes, "the skip is a note")
	assert.Empty(t, res.Errors)
	state, _, _, _ := f.row("films/sealed-mp.mkv")
	assert.Empty(t, state, "no parity row for an object whose stored length is unknown")
	assert.Empty(t, f.shardFiles())
	assert.Equal(t, float64(1), promtest.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("unknown_length")))
	assert.Equal(t, float64(0), promtest.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("other")),
		"counted under its own reason, not other")
}

func TestSmartDemotion_NeverNamesAnSSECMultipartObject(t *testing.T) {
	// Arrange: two idle objects that would be demoted, one recorded as 0x05.
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "sealed-mp", 512*gb, 30, 20)
	f.object("b", "plain", 512*gb, 30, 20)
	_, err := f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $2 WHERE tenant_id = $1 AND object_key = 'sealed-mp'`,
		f.tenantID, crypto.SSECAlgorithmMultipart)
	require.NoError(t, err)

	// Act
	res, err := f.runner.RunOnce(context.Background(), false)

	// Assert: the plain one moves, the 0x05 one is never a candidate.
	require.NoError(t, err)
	assert.Equal(t, 1, res.Candidates, "%+v", res)
	assert.Equal(t, "geyser", f.backendOf("b", "plain"))
	assert.Equal(t, "idrive", f.backendOf("b", "sealed-mp"))
	_, _, _, ok := f.ledger("b", "sealed-mp")
	assert.False(t, ok, "no ledger row for a 0x05 object")
}

func TestSmartPromotion_CopyBackRefusesAnSSECMultipartObject(t *testing.T) {
	// Arrange: a demoted, reclaimed object whose row (same etag) says 0x05.
	f := setupDemotionFixture(t, 1*tb, "standard")
	p := newPromoter(f)
	f.object("b", "doc", 100, 30, 20)
	etag := f.demoteNow("b", "doc")
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "doc")))
	_, err := f.db.Exec(`UPDATE smart_demotions SET hot_deleted_at=NOW(), hot_outcome='deleted' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $2 WHERE tenant_id = $1 AND object_key = 'doc'`,
		f.tenantID, crypto.SSECAlgorithmMultipart)
	require.NoError(t, err)

	// Act
	outcome, err := p.promote(context.Background(), f.tenantID, "b", "doc", etag, "", 100)

	// Assert: nothing written at a 0 content length; the object stays cold.
	require.Error(t, err)
	assert.NotEqual(t, "promoted", outcome)
	assert.False(t, f.hotExists("b", "doc"), "no copy-back sized at 0")
	assert.Equal(t, "geyser", f.backendOf("b", "doc"))
}

func resultJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
