package drivers

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// QuickXorHash is the only hash OneDrive for Business keeps for a file
// (file.hashes.quickXorHash; no sha256Hash on Business) — the migration tool
// verifies a copied tree by it (WP-FLEET-1, D-F1-7).

func TestQuickXor_EmptyIsTwentyZeroBytes(t *testing.T) {
	h := NewQuickXorHash()
	assert.Equal(t, 20, h.Size())
	assert.Equal(t, "AAAAAAAAAAAAAAAAAAAAAAAAAAA=", base64.StdEncoding.EncodeToString(h.Sum(nil)))
}

func TestQuickXor_OneByteIsTheByteThenTheLength(t *testing.T) {
	// One byte sits in bits 0..7; the length (1, little-endian) is XORed
	// into the last 8 bytes.
	h := NewQuickXorHash()
	_, _ = h.Write([]byte{0xAB})
	want := make([]byte, 20)
	want[0] = 0xAB
	want[12] = 0x01
	assert.Equal(t, want, h.Sum(nil))
}

func TestQuickXor_AnySplitOfTheStreamIsTheSameHash(t *testing.T) {
	data := QuickXorSelftestData(70_001)
	one := NewQuickXorHash()
	_, _ = one.Write(data)
	want := one.Sum(nil)
	for _, step := range []int{1, 7, 159, 160, 161, 320, 4096, 65536} {
		h := NewQuickXorHash()
		for off := 0; off < len(data); off += step {
			_, _ = h.Write(data[off:min(off+step, len(data))])
		}
		require.Equal(t, want, h.Sum(nil), "written %d bytes at a time", step)
	}
}

func TestQuickXor_ResetStartsOver(t *testing.T) {
	h := NewQuickXorHash()
	_, _ = h.Write([]byte("something"))
	h.Reset()
	assert.Equal(t, make([]byte, 20), h.Sum(nil))
}

// The vectors below were recorded from LIVE Graph responses by
// `permafrost-migrate-drive -selftest` (2026-11-05, OneDrive for Business):
// the body QuickXorSelftestData(size) uploaded, Graph's
// file.hashes.quickXorHash of the item copied here.
func TestQuickXor_MatchesGraphOnTheSelftestVectors(t *testing.T) {
	require.Len(t, QuickXorGraphVectors, len(QuickXorSelftestSizes), "one live vector per self-test size")
	for i, v := range QuickXorGraphVectors {
		require.Equal(t, QuickXorSelftestSizes[i], v.size)
		h := NewQuickXorHash()
		_, _ = h.Write(QuickXorSelftestData(v.size))
		assert.Equal(t, v.graph, base64.StdEncoding.EncodeToString(h.Sum(nil)), "size %d", v.size)
	}
}
