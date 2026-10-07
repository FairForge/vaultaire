package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// pack_gc is a job of this process only when the pack store's backend
// (`sync`) is registered; without it there is nothing to collect.
func TestPackGC_OnlyWithItsBackend(t *testing.T) {
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), logger))
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	assert.Nil(t, newPackGC(db, eng, t.TempDir(), logger), "no sync backend: no job")
	assert.Nil(t, newPackGC(nil, eng, t.TempDir(), logger), "no database: no job")

	eng.AddDriver(packStoreBackend, drivers.NewLocalDriver(t.TempDir(), logger))
	g := newPackGC(db, eng, t.TempDir(), logger)
	require.NotNil(t, g)
	assert.Equal(t, "pack_gc", g.spec().Name)
	assert.Equal(t, packStoreBackend, g.store.Backend())
}

// The job's run is one packstore GC pass: an orphan pack file on the backend
// is deleted and counted in the report.
func TestPackGC_SpecRunsOneGCPass(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)
	// a backend name of this test's own: packs rows are keyed by backend
	backend := fmt.Sprintf("sync-pkgc-%d", time.Now().UnixNano())
	drv := drivers.NewLocalDriver(t.TempDir(), logger)
	eng.AddDriver(backend, drv)
	g := newPackGCFor(db, eng, backend, t.TempDir(), logger)
	require.NotNil(t, g)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM packs WHERE backend = $1`, backend) })

	orphan := fmt.Sprintf("%064x", 99)
	require.NoError(t, drv.Put(ctx, "_packs", orphan[:2]+"/"+orphan+".pack", bytes.NewReader([]byte("x"))))

	rep, err := g.spec().Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rep.Rows)
	ok, err := drv.Exists(ctx, "_packs", orphan[:2]+"/"+orphan+".pack")
	require.NoError(t, err)
	assert.False(t, ok)
}
