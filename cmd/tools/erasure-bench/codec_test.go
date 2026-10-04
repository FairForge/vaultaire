package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Both codecs rebuild the payload from ANY k of n shards, including every
// shard missing that the bench's "lose one backend" reads produce.
func TestCodecs_RebuildFromAnyK(t *testing.T) {
	const k, m = 4, 2
	size := int64(3<<20 + 12345) // not a multiple of anything
	payload := make([]byte, size)
	_, _ = rand.Read(payload)

	for _, name := range []string{"rs", "raptorq"} {
		t.Run(name, func(t *testing.T) {
			c, err := newCodec(name, k, m, 32<<10, size)
			require.NoError(t, err)
			shards, err := c.Shards(payload)
			require.NoError(t, err)
			require.Len(t, shards, k+m)
			for i := 1; i < len(shards); i++ {
				require.Equal(t, len(shards[0]), len(shards[i]), "equal shard lengths")
			}
			// Every pair of missing shards (15 combinations) must still decode.
			for a := 0; a < k+m; a++ {
				for b := a + 1; b < k+m; b++ {
					have := make([][]byte, k+m)
					for i := range shards {
						if i != a && i != b {
							have[i] = shards[i]
						}
					}
					out, err := c.Rebuild(have, size)
					require.NoError(t, err, "missing %d and %d", a, b)
					require.True(t, bytes.Equal(out, payload), fmt.Sprintf("missing %d and %d: payload differs", a, b))
				}
			}
			// Three missing is beyond m: must fail, not return garbage.
			have := make([][]byte, k+m)
			for i := 3; i < k+m; i++ {
				have[i] = shards[i]
			}
			out, err := c.Rebuild(have, size)
			if err == nil {
				require.False(t, bytes.Equal(out, payload), "k-1 shards must not decode")
			}
		})
	}
}

func TestNewCodec_Rejects(t *testing.T) {
	_, err := newCodec("lt", 4, 2, 32<<10, 1<<20)
	require.Error(t, err)
	_, err = newCodec("raptorq", 4, 2, 70000, 1<<20)
	require.ErrorContains(t, err, "65535")
}

// The interleaved RS layout rebuilds from any k shards too, and its data
// shards are NOT a contiguous slice of the payload — the whole point.
func TestRSInterleaved_RebuildFromAnyK(t *testing.T) {
	const k, m = 4, 2
	size := int64(3<<20 + 777)
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	c, err := newCodecStripe("rs", k, m, 0, size, 64<<10)
	require.NoError(t, err)
	shards, err := c.Shards(payload)
	require.NoError(t, err)
	require.Len(t, shards, k+m)
	require.Equal(t, 0, len(shards[0])%(64<<10))
	require.False(t, bytes.Equal(shards[0][:4096], payload[:4096]) && bytes.Equal(shards[1][:4096], payload[len(shards[0]):len(shards[0])+4096]),
		"interleaved shards are not the contiguous layout")
	// stripe 0 of shard 0 is the payload's first 64 KiB, stripe 0 of shard 1 the second
	require.True(t, bytes.Equal(shards[0][:64<<10], payload[:64<<10]))
	require.True(t, bytes.Equal(shards[1][:64<<10], payload[64<<10:128<<10]))
	for a := 0; a < k+m; a++ {
		for b := a + 1; b < k+m; b++ {
			have := make([][]byte, k+m)
			for i := range shards {
				if i != a && i != b {
					have[i] = shards[i]
				}
			}
			out, err := c.Rebuild(have, size)
			require.NoError(t, err, "missing %d and %d", a, b)
			require.True(t, bytes.Equal(out, payload), fmt.Sprintf("missing %d and %d: payload differs", a, b))
		}
	}
}
