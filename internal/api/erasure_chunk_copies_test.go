package api

// RP76-3: a codec tenant's chunk copies on a mirror leg are named only by
// their blob_copies rows (the sweep never reaches _global, account.EraseRows
// deletes the rows): the deletion runner deletes each copy on its leg
// THROUGH the rows before the row erase, deferring the tenant as the sweep
// does when a leg cannot be asked.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// flakyLeg is a local mirror leg whose Get and Delete fail on demand.
type flakyLeg struct {
	*drivers.LocalDriver
	fail bool
}

func (d *flakyLeg) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.fail {
		return nil, errors.New("leg: connection reset by peer")
	}
	return d.LocalDriver.Get(ctx, container, artifact)
}

func (d *flakyLeg) Delete(ctx context.Context, container, artifact string) error {
	if d.fail {
		return errors.New("leg: connection reset by peer")
	}
	return d.LocalDriver.Delete(ctx, container, artifact)
}

// leg registers the mirror leg the fixture's chunk copies sit on.
func (f *deletionFixture) leg() *flakyLeg {
	f.t.Helper()
	l := &flakyLeg{LocalDriver: drivers.NewLocalDriver(f.t.TempDir(), zap.NewNop())}
	f.eng.AddDriver("mirrorleg", l)
	return l
}

func (f *deletionFixture) chunkCopy(l *flakyLeg, name string) string {
	f.t.Helper()
	key := "_chunks/" + f.tenantID + "/" + name + strings.Repeat("0", 60) + ".k1"
	require.NoError(f.t, l.Put(engine.ChunkContext(context.Background()), engine.ChunkContainer, key, strings.NewReader("sealed "+name)))
	f.exec(`INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, state, etag, size)
		VALUES ($1, '', '', $2, 'local', 'mirrorleg', 'mirror', 'ready', '', 12)`, f.tenantID, key)
	return key
}

func chunkCopyHeld(t *testing.T, l *flakyLeg, key string) bool {
	t.Helper()
	ok, err := l.LocalDriver.Exists(engine.ChunkContext(context.Background()), engine.ChunkContainer, key)
	require.NoError(t, err)
	return ok
}

// eraseOnce runs the runner once and returns the fixture tenant's entry.
func (f *deletionFixture) eraseOnce() TenantErasure {
	f.t.Helper()
	res, err := f.runner.RunOnce(context.Background())
	if err != nil && len(res.Tenants) == 0 {
		require.NoError(f.t, err)
	}
	require.Len(f.t, res.Tenants, 1)
	return res.Tenants[0]
}

func TestErasure_DeletesChunkCopiesOnTheirLegsBeforeTheRows(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	l := f.leg()
	a, b := f.chunkCopy(l, "aa"), f.chunkCopy(l, "bb")

	// A leg that refuses the delete defers the tenant; nothing is erased.
	l.fail = true
	te := f.eraseOnce()
	assert.Equal(t, outcomeDeferred, te.Outcome, "%+v", te)
	assert.Contains(t, te.Error, "chunk copies")
	assert.True(t, chunkCopyHeld(t, l, a) && chunkCopyHeld(t, l, b), "the copies are still on the leg")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id = $1`, f.userID), "the rows stay")

	// The leg answers: both copies go, then the rows.
	l.fail = false
	te = f.eraseOnce()
	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.Equal(t, 2, te.ChunkCopiesDeleted)
	assert.False(t, chunkCopyHeld(t, l, a))
	assert.False(t, chunkCopyHeld(t, l, b))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM blob_copies WHERE tenant_id = $1`, f.tenantID))
}

func TestErasure_AChunkCopyLegWithAnOpenBreakerDefers(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	l := f.leg()
	a := f.chunkCopy(l, "cc")
	l.fail = true
	for i := 0; i < 10 && f.eng.GetFailoverStatus()["mirrorleg"] != "open"; i++ {
		_, _ = f.eng.Get(engine.WithRecordedBackend(common.WithTenantID(context.Background(), "probe-tenant"), "mirrorleg"), "c", "probe")
	}
	require.Equal(t, "open", f.eng.GetFailoverStatus()["mirrorleg"], "fixture: the breaker must be open")
	l.fail = false

	te := f.eraseOnce()
	assert.Equal(t, outcomeDeferred, te.Outcome, "%+v", te)
	assert.True(t, chunkCopyHeld(t, l, a), "a leg whose breaker is open is not asked")
}
