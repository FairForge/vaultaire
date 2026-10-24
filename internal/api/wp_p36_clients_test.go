package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// K4c-3b (Stage 1a Prompt 36): the client-build manifest's rows are appended
// at boot — one client_build entry per distinct (client, sha256), like
// server_build — and a row the frozen log refuses stays pending for the
// translog_checkpoint job's retry (RP42-1's rule), never lost.

func clientRow(client, sha string) translog.ClientBuild {
	tool := "go1.26.10"
	if client == translog.ClientSDKJS {
		tool = "none"
	}
	return translog.ClientBuild{Client: client, Version: "0.1.0",
		SourceCommit: strings.Repeat("ab", 20), SHA256: sha,
		Toolchain: tool, Recipe: "scripts/build-client.sh"}
}

func TestClientBuildEntry_AppendedOncePerHash(t *testing.T) {
	// Arrange: a private log and two manifest rows.
	ctx := context.Background()
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	store := logstore.New(logstoretest.PrivateLog(t), signer)
	rows := []translog.ClientBuild{
		clientRow(translog.ClientSDKWasm, strings.Repeat("07", 32)),
		clientRow(translog.ClientSDKJS, strings.Repeat("47", 32)),
	}

	// Act: one boot appends, a second boot (the other slot, the same
	// manifest) appends nothing more.
	first := &clientBuilds{}
	first.set(rows)
	first.try(ctx, store, zap.NewNop(), false)
	second := &clientBuilds{}
	second.set(rows)
	second.try(ctx, store, zap.NewNop(), false)

	// Assert: two entries, not four; neither boot stays pending.
	assert.False(t, first.Pending())
	assert.False(t, second.Pending())
	size, err := store.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), size, "one entry per (client, sha256), never twice")

	// A direct second append of the same row answers the first entry's index.
	idx, appended, err := store.Append(ctx, rows[0])
	require.NoError(t, err)
	assert.False(t, appended)
	assert.Equal(t, int64(0), idx)

	// A new release of the same client (another sha256) is its own entry.
	_, appended, err = store.Append(ctx, clientRow(translog.ClientSDKWasm, strings.Repeat("08", 32)))
	require.NoError(t, err)
	assert.True(t, appended)
}

func TestClientBuild_FrozenLogRetriedNotLost(t *testing.T) {
	// Arrange: the log frozen behind a high-water file (a restore being
	// repaired), one manifest row waiting.
	ctx := context.Background()
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	hw := filepath.Join(t.TempDir(), "highwater")
	root := base64.StdEncoding.EncodeToString(make([]byte, 32))
	require.NoError(t, os.WriteFile(hw, []byte(translog.Origin+"\n5\n"+root+"\n"), 0o600))
	store := logstore.New(logstoretest.PrivateLog(t), signer).WithHighwater(hw)
	cb := &clientBuilds{}
	cb.set([]translog.ClientBuild{clientRow(translog.ClientSDKWasm, strings.Repeat("07", 32))})

	// Act: the boot's try is refused; the row stays pending and nothing is
	// written.
	cb.try(ctx, store, zap.NewNop(), false)
	require.True(t, cb.Pending(), "the frozen log keeps the row pending")
	size, err := store.Size(ctx)
	require.NoError(t, err)
	require.Zero(t, size)

	// The log is repaired (here: the file is this lab's first run); the
	// checkpoint job's retry lands the entry.
	require.NoError(t, os.Remove(hw))
	cb.try(ctx, store, zap.NewNop(), true)

	// Assert
	assert.False(t, cb.Pending())
	size, err = store.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), size)
	// And the retry path is wired: the checkpoint job tries pending client
	// builds before it checkpoints.
	job := newTranslogCheckpointer(store, zap.NewNop())
	job.clients = cb
	_, err = job.spec().Run(ctx)
	require.NoError(t, err)
}
