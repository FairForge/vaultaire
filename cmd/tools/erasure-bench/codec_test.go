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
