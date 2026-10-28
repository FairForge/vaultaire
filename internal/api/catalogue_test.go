package api

// WP-EDGE-4 PR D (plan 10.3): the public catalogue. A `_public` chunk is the
// operator's seeded copy of public content: no tenant's reference keeps it,
// so dedup GC must never collect it, whatever its ref_count says.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"testing"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/catalogue"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
)

func TestDedupGC_NeverCollectsAPublicChunk(t *testing.T) {
	runner, f := setupGCFixture(t)
	sum := sha256.Sum256([]byte("public chunk " + uuid.New().String()))
	hash := hex.EncodeToString(sum[:])
	_, err := f.db.Exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, encrypted, ref_count, state, marked_for_deletion, marked_at, last_accessed_at)
		VALUES ($1, $2, 'local', $3, 13, FALSE, 0, 'ready', TRUE, NOW() - interval '30 days', NOW() - interval '30 days')`,
		crypto.PublicDedupScope, hash, "_chunks/_public/"+hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, crypto.PublicDedupScope, hash)
	})

	_, err = runner.RunOnce(context.Background())
	require.NoError(t, err)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		crypto.PublicDedupScope, hash).Scan(&n))
	assert.Equal(t, 1, n, "a seeded public chunk outlives every reference")
}

// The catalogue_filter job builds the filter from public_chunks and
// publishes catalogue/<version>.bloom and catalogue/latest on the r2
// backend under the reserved tenant _catalogue; without r2 it is a note.
func TestCatalogueFilterJob_PublishesTheFilter(t *testing.T) {
	f := setupEdgeFixture(t)
	r2 := drivers.NewLocalDriver(t.TempDir(), zap.NewNop())
	f.srv.engine.AddDriver("r2", r2)
	object := func(artifact string) []byte {
		rc, err := r2.Get(common.WithTenantID(context.Background(), catalogueTenant), catalogueContainer, artifact)
		if err != nil {
			return nil
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return b
	}

	var id int64
	require.NoError(t, f.db.QueryRow(`INSERT INTO public_artefacts (name, sha256) VALUES ('test artefact', $1) RETURNING id`, uuid.New().String()).Scan(&id))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM public_artefacts WHERE id = $1`, id) })
	var hashes []string
	for i := range 50 {
		s := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", id, i)))
		hashes = append(hashes, hex.EncodeToString(s[:]))
		f.exec(`INSERT INTO public_chunks (artefact_id, chunk_index, plain_hash, size_bytes) VALUES ($1, $2, $3, 100)`, id, i, hashes[i])
	}

	j := newCatalogueFilterJob(f.srv)
	j.JobName = "catalogue_filter_test_" + f.tenantID
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Chunks, int64(50))
	require.NotZero(t, res.Version)

	latest := object("catalogue/latest")
	require.NotNil(t, latest)
	assert.Equal(t, fmt.Sprint(res.Version), string(latest))
	raw := object(fmt.Sprintf("catalogue/%d.bloom", res.Version))
	require.NotNil(t, raw)
	filter, err := catalogue.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, res.Version, filter.Version())
	for _, h := range hashes {
		ok, err := filter.Contains(h)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	assert.Equal(t, float64(res.Version), promtestutil.ToFloat64(catalogueFilterVersion))
}
