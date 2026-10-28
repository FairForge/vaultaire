package api

// WP-EDGE-4 PR B: internal/ec is the WP-VAULT-1 codec lifted out of this
// package. Its parity shards are vault_parity's, byte for byte, on the
// vault layout (4+4, 1 MiB pieces) — one codec, not two.

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/ec"
)

func TestEC_ParityIsVaultParitys(t *testing.T) {
	size := int64(3*4*(1<<20) + 12345)
	body := crypto.GoldenBody(7777, int(size))
	vl := parityLayout{k: 4, m: 4, stripe: 1 << 20, size: size}
	vault := make([]*bytes.Buffer, 4)
	vw := make([]io.Writer, 4)
	for i := range vault {
		vault[i] = &bytes.Buffer{}
		vw[i] = vault[i]
	}
	require.NoError(t, encodeParity(vl, bytes.NewReader(body), vw))

	el := ec.Layout{K: 4, M: 4, Stripe: 1 << 20, Size: size}
	all := make([]*bytes.Buffer, 8)
	aw := make([]io.Writer, 8)
	for i := range all {
		all[i] = &bytes.Buffer{}
		aw[i] = all[i]
	}
	require.NoError(t, ec.Encode(el, bytes.NewReader(body), aw))
	for j := 0; j < 4; j++ {
		require.True(t, bytes.Equal(vault[j].Bytes(), all[4+j].Bytes()), "parity shard %d", j)
	}
	require.Equal(t, vl.shardBytes(), el.ShardBytes())
}
