package api

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// withChunkingFlag attaches a real DB-backed flag service (default: chunking
// on) to the fixture's adapter and cleans up any rows the test writes.
func withChunkingFlag(t *testing.T, f *adapterTestFixture) *flags.Service {
	t.Helper()
	svc := flags.New(f.db, zap.NewNop())
	svc.Register(flagChunking, true)
	require.NoError(t, svc.Refresh(context.Background()))
	f.adapter.flags = svc
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", flagChunking)
	})
	return svc
}

func isChunkedInHeadCache(t *testing.T, f *adapterTestFixture, key string) bool {
	t.Helper()
	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", key).Scan(&isChunked))
	return isChunked
}

// TestHandlePut_ChunkingFlagOff — 1.13 kill-switch: flipping the `chunking`
// flag off routes the next PUT down the plain whole-object path (no manifest,
// no GCI refs) while GETs of previously chunked objects keep working
// (manifests are self-describing; reads are unaffected by the flag).
func TestHandlePut_ChunkingFlagOff(t *testing.T) {
	f := setupChunkingFixture(t)
	svc := withChunkingFlag(t, f)
	ctx := context.Background()

	// Flag on (default): objects above the threshold chunk as before.
	contentOn := generateTestData(8 * 1024)
	putChunkedObject(t, f, "flag-on.bin", contentOn, "application/octet-stream")
	require.True(t, isChunkedInHeadCache(t, f, "flag-on.bin"),
		"sanity: with the flag on, the object must chunk")

	// Kill switch: global off. The very next PUT stores whole-object.
	require.NoError(t, svc.Set(ctx, flagChunking, flags.GlobalTenant, false, "test"))
	contentOff := generateTestData(8 * 1024)
	putChunkedObject(t, f, "flag-off.bin", contentOff, "application/octet-stream")
	assert.False(t, isChunkedInHeadCache(t, f, "flag-off.bin"),
		"with the flag off, PUT must take the plain whole-object path")

	// The whole-object PUT round-trips.
	gw := getChunkedObject(t, f, "flag-off.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, contentOff, body)

	// GET of the PREVIOUSLY chunked object still works with the flag off.
	gw = getChunkedObject(t, f, "flag-on.bin")
	require.Equal(t, http.StatusOK, gw.Code,
		"chunked reads must be unaffected by the flag")
	body, _ = io.ReadAll(gw.Body)
	assert.Equal(t, contentOn, body)

	// Flip back on: chunking resumes.
	require.NoError(t, svc.Set(ctx, flagChunking, flags.GlobalTenant, true, "test"))
	contentBack := generateTestData(8 * 1024)
	putChunkedObject(t, f, "flag-back.bin", contentBack, "application/octet-stream")
	assert.True(t, isChunkedInHeadCache(t, f, "flag-back.bin"),
		"flipping the flag back on must restore chunking")
}

// TestHandlePut_ChunkingFlagPerTenant — the flag also carries per-tenant
// overrides: chunking stays globally on while one tenant is opted out.
func TestHandlePut_ChunkingFlagPerTenant(t *testing.T) {
	f := setupChunkingFixture(t)
	svc := withChunkingFlag(t, f)
	ctx := context.Background()

	// Opt THIS tenant out; the global state stays on (default true).
	require.NoError(t, svc.Set(ctx, flagChunking, f.tenantID, false, "test"))

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "tenant-off.bin", content, "application/octet-stream")
	assert.False(t, isChunkedInHeadCache(t, f, "tenant-off.bin"),
		"tenant override off must take the plain path for that tenant")

	// A different tenant still chunks.
	other := addTenant(t, f)
	putChunkedAs(t, f, other, "test-bucket", "other-on.bin", content)
	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		other.ID, "test-bucket", "other-on.bin").Scan(&isChunked))
	assert.True(t, isChunked, "tenants without an override follow the global state")
}

// nilFlagsAdapterStillChunks guards the test-fixture invariant: an adapter
// with no flag service (every pre-1.13 test) behaves as if chunking is on.
func TestHandlePut_NilFlagsAdapterStillChunks(t *testing.T) {
	f := setupChunkingFixture(t) // adapter.flags stays nil
	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "nil-flags.bin", content, "application/octet-stream")
	assert.True(t, isChunkedInHeadCache(t, f, "nil-flags.bin"))
}
