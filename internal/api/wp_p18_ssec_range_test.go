package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP19-2 (Stage 1a Prompt 18, Part 0.4): a Range over a 0x05 blob opened
// every spanned part's header up front — a range over N parts cost N small
// ranged reads before the first byte. The next part's header is now read as
// the previous span ends; the FIRST span's header is still opened up front
// (a wrong key stays a 403 before the status line), and the copy of the
// customer key the later spans need lives until the stream closes.

// sealMultipartBlob is a 0x05 blob of parts sealed under key for id, and
// the plaintext.
func sealMultipartBlob(t *testing.T, key []byte, id crypto.StreamIdentity, parts ...[]byte) (blob, plain []byte) {
	t.Helper()
	plains := make([]int64, len(parts))
	for i, p := range parts {
		plains[i] = int64(len(p))
	}
	hdr, err := crypto.NewSSECMultipartHeader(plains)
	require.NoError(t, err)
	blob = append(blob, hdr...)
	for i, p := range parts {
		pid := id
		pid.Part = i + 1
		r, err := crypto.SSECSeal(key, pid, bytes.NewReader(p), int64(len(p)))
		require.NoError(t, err)
		sealed, err := io.ReadAll(r)
		require.NoError(t, err)
		blob = append(blob, sealed...)
		plain = append(plain, p...)
	}
	return blob, plain
}

// countingSealedSource serves a blob held in memory and counts the stored
// reads it is asked for.
func countingSealedSource(blob []byte, reads *int) sealedSource {
	return sealedSource{
		whole: func() (io.ReadCloser, error) {
			*reads++
			return io.NopCloser(bytes.NewReader(blob)), nil
		},
		ranged: func(off, n int64) (io.ReadCloser, error) {
			*reads++
			end := min(off+n, int64(len(blob)))
			return io.NopCloser(bytes.NewReader(blob[off:end])), nil
		},
	}
}

func TestSSECMultipartRange_OpensTheNextPartsHeaderOnlyWhenItsTurnComes(t *testing.T) {
	// Arrange: three parts; a range from inside part 1 to inside part 3.
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	id := crypto.StreamIdentity{Tenant: "t-p18", Bucket: "b", Key: "three-parts"}
	part := func(n int) []byte { b := make([]byte, n); _, _ = rand.Read(b); return b }
	blob, plain := sealMultipartBlob(t, key, id, part(1000), part(1000), part(1000))
	var reads int
	src := countingSealedSource(blob, &reads)
	callerKey := bytes.Clone(key)

	// Act: open as the GET does, then clear the caller's key at once (the
	// handler's clear(ssecKey) after the open).
	rc, err := openSealed(context.Background(), newSSECOpener(callerKey, crypto.SSECAlgorithmMultipart, id), src,
		sealedRead{Size: 3000, Ranged: true, Off: 500, N: 2000})
	require.NoError(t, err)
	clear(callerKey)
	readsBeforeFirstByte := reads
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	// Assert: lead + table + the first span's header + its segments
	// before the first byte (it was lead + table + 3 headers + segments);
	// the later parts' headers are read as their turn comes; the body is
	// right although the caller's key was cleared after the open; the
	// chain's own copy is zeroed by Close.
	assert.Equal(t, 4, readsBeforeFirstByte, "lead, table, part 1's header, part 1's segments")
	assert.Equal(t, 8, reads, "+ part 2's header and segments, part 3's header and segments")
	assert.Equal(t, plain[500:2500], got)
	chain, ok := rc.(*readCloser).Closer.(*spanChain)
	require.True(t, ok)
	assert.Len(t, chain.key, 32)
	assert.Equal(t, make([]byte, len(chain.key)), chain.key, "the key copy is zeroed when the stream closes")
}

func TestSSECMultipartRange_AWrongKeyIsRefusedBeforeTheStatusLine(t *testing.T) {
	// Arrange
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	id := crypto.StreamIdentity{Tenant: "t-p18", Bucket: "b", Key: "wrong-key"}
	blob, _ := sealMultipartBlob(t, key, id, bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 1000))
	var reads int
	other := make([]byte, 32)

	// Act
	_, err = openSealed(context.Background(), newSSECOpener(other, crypto.SSECAlgorithmMultipart, id), countingSealedSource(blob, &reads),
		sealedRead{Size: 2000, Ranged: true, Off: 10, N: 1500})

	// Assert: the open itself fails (the 403 is decided before any byte).
	require.ErrorIs(t, err, crypto.ErrSSECKeyMismatch)
}
