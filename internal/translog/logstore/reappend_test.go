package logstore

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// K4b PR 3 (RP38-6): after a database restore behind the high-water file,
// the only way to keep the log's origin is to put the lost entries back BYTE
// FOR BYTE — then the tree at the file's size has the file's root and the
// checkpoint job passes by itself. ReAppend is that write: one transaction,
// raw bodies, the recomputed root checked against the signed one, never a
// signature.

// signedLog is a log of 9 entries signed at 5 (the dump's moment) and at 9
// (the high-water): the high-water file, the checkpoint at 9, the store.
func signedLog(t *testing.T) (hw string, at9 translog.Checkpoint, s *Store) {
	t.Helper()
	ctx := context.Background()
	signer, _ := testSigner(t)
	hw = filepath.Join(t.TempDir(), "highwater")
	s = New(logstoretest.PrivateLog(t), signer).WithHighwater(hw)
	for i := 0; i < 9; i++ {
		var e translog.Entry = ceremony(t)
		if i == 6 {
			e = uniqueBuild(t) // a kind with a dedup key
		}
		_, _, err := s.Append(ctx, e)
		require.NoError(t, err)
		if i == 4 {
			res, err := s.Checkpoint(ctx)
			require.NoError(t, err)
			require.True(t, res.Signed)
		}
	}
	res, err := s.Checkpoint(ctx)
	require.NoError(t, err)
	require.True(t, res.Signed)
	at9, ok, err := ReadHighwater(hw)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(9), at9.Size)
	return hw, at9, s
}

func TestReAppend_RestoresTheExactTree(t *testing.T) {
	// Arrange: the dump was taken at 5; entries 5…8 are lost.
	ctx := context.Background()
	hw, at9, live := signedLog(t)
	lost, err := live.Entries(ctx, 5, 4)
	require.NoError(t, err)
	restored := restoreOf(t, live.db, 5)
	r := New(restored, live.signer).WithHighwater(hw)
	res, err := r.Checkpoint(ctx)
	require.Error(t, err, "before: the restored log is behind the high-water")
	require.Equal(t, RefusedBehindHighwater, res.Refused)

	// Act
	require.NoError(t, r.ReAppend(ctx, lost, at9))

	// Assert: the tree at 9 is the high-water's, byte for byte; the next
	// checkpoint pass signs (the same tree the key signed) with no refusal.
	size, err := r.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(9), size)
	root, err := r.Root(ctx, 9)
	require.NoError(t, err)
	assert.Equal(t, at9.Root, root)
	assert.Equal(t, at9.Root, recomputeRoot(t, restored, 9), "the bodies alone give the signed root")
	res, err = r.Checkpoint(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Refused)
	assert.True(t, res.Signed)
	var kinds []string
	rows, err := restored.Query(`SELECT kind FROM translog_entries WHERE idx >= 5 ORDER BY idx`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		kinds = append(kinds, k)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"key_ceremony", "server_build", "key_ceremony", "key_ceremony"}, kinds)
	// The dedup key comes back from the body: the same build appends nothing.
	d, err := translog.Decode(lost[1])
	require.NoError(t, err)
	idx, appended, err := r.Append(ctx, d.Entry)
	require.NoError(t, err)
	assert.False(t, appended)
	assert.Equal(t, int64(6), idx)
}

func TestReAppend_WrongBytes_RollsBack(t *testing.T) {
	ctx := context.Background()
	hw, at9, live := signedLog(t)
	lost, err := live.Entries(ctx, 5, 4)
	require.NoError(t, err)
	restored := restoreOf(t, live.db, 5)
	r := New(restored, live.signer).WithHighwater(hw)
	// One byte of one body altered — still a valid entry (a different
	// participant count), so only the root can tell.
	forged := append([][]byte(nil), lost...)
	forged[2] = bytes.Replace(lost[2], []byte(`"participants":1`), []byte(`"participants":2`), 1)
	require.NotEqual(t, lost[2], forged[2])
	_, err = translog.Decode(forged[2])
	require.NoError(t, err, "the forged body is a valid entry")

	err = r.ReAppend(ctx, forged, at9)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReAppendRoot)
	size, err := r.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), size, "nothing kept")
	var hashes int64
	require.NoError(t, restored.QueryRow(`SELECT COUNT(*) FROM translog_hashes`).Scan(&hashes))
	assert.Equal(t, translog.StoredHashCount(5), hashes, "no partial rows")
}

func TestReAppend_RefusesPastCheckpoint(t *testing.T) {
	ctx := context.Background()
	_, at9, live := signedLog(t)
	lost, err := live.Entries(ctx, 5, 4)
	require.NoError(t, err)
	r := New(restoreOf(t, live.db, 5), nil)

	// More bodies than the checkpoint holds, fewer, and a log already at it.
	assert.ErrorIs(t, r.ReAppend(ctx, append(lost, lost[0]), at9), ErrReAppendSize)
	assert.ErrorIs(t, r.ReAppend(ctx, lost[:3], at9), ErrReAppendSize)
	assert.ErrorIs(t, live.ReAppend(ctx, nil, at9), ErrReAppendSize, "M ≥ N: nothing to re-append")
	size, err := r.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), size)
}

func TestReAppend_RefusesUndecodableBody(t *testing.T) {
	ctx := context.Background()
	_, at9, live := signedLog(t)
	lost, err := live.Entries(ctx, 5, 4)
	require.NoError(t, err)
	r := New(restoreOf(t, live.db, 5), nil)
	bad := append([][]byte(nil), lost...)
	bad[1] = []byte(`{"kind":"le_production","v":1,"at":"2026-10-22T00:00:00Z","entry":{}}`) // a kind this build does not know (admin_read is one since K4c-1)

	err = r.ReAppend(ctx, bad, at9)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry 6")
	size, err := r.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), size)
}
