package translog_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog"
)

// FindBuild (WP-SUPPLY-1): the entry that lists a build is found with its
// inclusion PROVEN against the verified checkpoint — a log whose served
// tiles do not hash to the checkpoint's root (a stale or tampered mirror
// that still lists the build in an entry bundle) is refused, never "found".

func buildLog(t *testing.T, filler string, serverSum, wasmSum string) *translog.Memory {
	t.Helper()
	m := &translog.Memory{}
	for i := 0; i < 5; i++ {
		_, err := m.Append([]byte(fmt.Sprintf("%s-%d", filler, i)))
		require.NoError(t, err)
	}
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	body, err := translog.Encode(translog.ServerBuild{GitSHA: "abcdef123456", BinarySHA256: serverSum, BuildTime: "2026-10-25T00:00:00Z", GoVersion: "go1.26.10"}, at)
	require.NoError(t, err)
	_, err = m.Append(body)
	require.NoError(t, err)
	body, err = translog.Encode(translog.ClientBuild{Client: translog.ClientSDKWasm, Version: "0.2.0", SourceCommit: strings.Repeat("a", 40),
		SHA256: wasmSum, Toolchain: "go1.26.10", Recipe: "scripts/build-client.sh"}, at)
	require.NoError(t, err)
	_, err = m.Append(body)
	require.NoError(t, err)
	return m
}

func TestFindBuild_FoundWithInclusionAndRefusedWithout(t *testing.T) {
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := translog.NewSigner(skey)
	require.NoError(t, err)
	v, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	serverSum := strings.Repeat("5", 64)
	wasmSum := strings.Repeat("6", 64)

	honest := buildLog(t, "honest", serverSum, wasmSum)
	other := buildLog(t, "another-history", serverSum, wasmSum) // the same builds at the same indexes, other fillers: another root

	hh := translog.NewHandler(&translog.MemorySource{Log: honest, Signer: s})
	oh := translog.NewHandler(&translog.MemorySource{Log: other, Signer: s})
	// The honest log, and a "mirror" that serves the honest checkpoint with
	// the other history's tiles and entry bundles.
	mux := http.NewServeMux()
	mux.Handle("/honest/", http.StripPrefix("/honest/", hh))
	mux.Handle("/mirror/checkpoint", http.StripPrefix("/mirror/", hh))
	mux.Handle("/mirror/", http.StripPrefix("/mirror/", oh))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ctx := context.Background()

	c := &translog.Client{BaseURL: srv.URL + "/honest/", Verifier: v}
	cp, _, err := c.Checkpoint(ctx)
	require.NoError(t, err)
	found, err := translog.FindBuild(ctx, c, cp, serverSum, "")
	require.NoError(t, err)
	assert.Equal(t, int64(5), found.Index)
	assert.Contains(t, found.String(), "server build abcdef123456 listed at entry 5 — inclusion OK")
	found, err = translog.FindBuild(ctx, c, cp, wasmSum, translog.ClientSDKWasm)
	require.NoError(t, err)
	assert.Equal(t, int64(6), found.Index)
	assert.Contains(t, found.String(), "client sdk-wasm 0.2.0 listed at entry 6")

	_, err = translog.FindBuild(ctx, c, cp, wasmSum, "")
	require.Error(t, err, "a client's hash is no server build")
	_, err = translog.FindBuild(ctx, c, cp, serverSum, translog.ClientSDKWasm)
	require.Error(t, err, "the server's hash is no sdk-wasm build")
	_, err = translog.FindBuild(ctx, c, cp, strings.Repeat("7", 64), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build not in the log")

	// The mirror: the entry bundle it serves lists the build at entry 5, but
	// its tiles do not hash to the checkpoint's root — refused.
	mc := &translog.Client{BaseURL: srv.URL + "/mirror/", Verifier: v}
	mcp, _, err := mc.Checkpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, cp.Root, mcp.Root, "the mirror serves the honest checkpoint")
	_, err = translog.FindBuild(ctx, mc, mcp, serverSum, "")
	require.Error(t, err, "an entry whose inclusion does not prove is not a listed build")
	assert.NotContains(t, err.Error(), "build not in the log", "the refusal is the proof's, not a miss: %v", err)
}
