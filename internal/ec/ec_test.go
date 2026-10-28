package ec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
)

var update = flag.Bool("update", false, "rewrite testdata/ec_golden.json")

func encodeAll(t *testing.T, l Layout, body []byte) [][]byte {
	t.Helper()
	bufs := make([]*bytes.Buffer, l.K+l.M)
	ws := make([]io.Writer, l.K+l.M)
	for i := range bufs {
		bufs[i] = &bytes.Buffer{}
		ws[i] = bufs[i]
	}
	require.NoError(t, Encode(l, bytes.NewReader(body), ws))
	out := make([][]byte, len(bufs))
	for i, b := range bufs {
		out[i] = b.Bytes()
		require.Equal(t, l.ShardBytes(), int64(len(out[i])), "shard %d is stripes × S", i)
	}
	return out
}

func readAllFrom(t *testing.T, l Layout, shards [][]byte) ([]byte, error) {
	t.Helper()
	rs := make([]io.Reader, len(shards))
	for i, s := range shards {
		if s != nil {
			rs[i] = bytes.NewReader(s)
		}
	}
	r, err := NewReader(l, rs)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// Any k of the k+m shards rebuild the object; a stripe boundary falls mid-object.
func TestEC_AnyKOfKPlusMRebuild(t *testing.T) {
	l := Layout{K: 4, M: 2, Stripe: 2 << 20, Size: 20<<20 + 123}
	body := crypto.GoldenBody(4802, int(l.Size))
	shards := encodeAll(t, l, body)
	n := l.K + l.M
	for a := 0; a < n; a++ {
		for b := a + 1; b < n; b++ {
			lost := make([][]byte, n)
			copy(lost, shards)
			lost[a], lost[b] = nil, nil
			got, err := readAllFrom(t, l, lost)
			require.NoError(t, err, "lost %d and %d", a, b)
			require.True(t, bytes.Equal(body, got), "lost %d and %d", a, b)
		}
	}
	// k−1 left: not enough.
	lost := make([][]byte, n)
	copy(lost, shards)
	lost[0], lost[1], lost[2] = nil, nil, nil
	_, err := readAllFrom(t, l, lost)
	assert.ErrorIs(t, err, ErrTooFewShards)
	// The data shards alone are the object (a read touching data only).
	dataOnly := make([][]byte, n)
	copy(dataOnly, shards[:l.K])
	got, err := readAllFrom(t, l, dataOnly)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, got))
}

func TestEC_ABodyOfAnotherLengthIsRefused(t *testing.T) {
	l := Layout{K: 2, M: 1, Stripe: 1024, Size: 5000}
	ws := []io.Writer{io.Discard, io.Discard, io.Discard}
	assert.Error(t, Encode(l, bytes.NewReader(make([]byte, 4999)), ws), "short")
	assert.Error(t, Encode(l, bytes.NewReader(make([]byte, 5001)), ws), "long")
	assert.Error(t, Encode(l, bytes.NewReader(make([]byte, 5000)), ws[:2]), "too few writers")
}

type golden struct {
	K, M, Stripe int
	Size         int64
	Seed         uint64
	ShardSHA256  []string `json:"shard_sha256"`
	StripeHex    string   `json:"stripe0_parity_hex_head"`
}

// The shards are frozen: the WASM build (sdk/ecwasm, edge/s3-ingest/test/
// ec.test.js) must produce them byte for byte.
func TestEC_GoldenVectors(t *testing.T) {
	g := golden{K: 4, M: 2, Stripe: 64 << 10, Size: 3*4*(64<<10) + 777, Seed: 4848}
	l := Layout{K: g.K, M: g.M, Stripe: g.Stripe, Size: g.Size}
	shards := encodeAll(t, l, crypto.GoldenBody(g.Seed, int(g.Size)))
	for _, s := range shards {
		sum := sha256.Sum256(s)
		g.ShardSHA256 = append(g.ShardSHA256, hex.EncodeToString(sum[:]))
	}
	g.StripeHex = hex.EncodeToString(shards[g.K][:32])
	path := "testdata/ec_golden.json"
	if *update {
		b, err := json.MarshalIndent(g, "", " ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o600))
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var want golden
	require.NoError(t, json.Unmarshal(b, &want))
	assert.Equal(t, want, g)
}

// failAfter answers n bytes, then an error (a leg that dies mid-read).
type failAfter struct {
	r io.Reader
	n int64
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > f.n {
		p = p[:f.n]
	}
	k, err := f.r.Read(p)
	f.n -= int64(k)
	return k, err
}

// A data shard that dies after two stripes: the parity, never read before,
// is brought to the stripe it is needed at and the object rebuilds.
func TestEC_ADataShardThatDiesMidObject(t *testing.T) {
	l := Layout{K: 4, M: 2, Stripe: 4096, Size: 10*4*4096 + 99}
	body := crypto.GoldenBody(4803, int(l.Size))
	shards := encodeAll(t, l, body)
	rs := make([]io.Reader, len(shards))
	for i, s := range shards {
		rs[i] = bytes.NewReader(s)
	}
	rs[1] = &failAfter{r: bytes.NewReader(shards[1]), n: 2*4096 + 100}
	r, err := NewReader(l, rs)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, got))
}

// Step 0.2: the native rate at 4+2 with 8 MiB stripes.
func BenchmarkEncode_4plus2(b *testing.B) {
	l := Layout{K: 4, M: 2, Stripe: 2 << 20, Size: 64 << 20}
	body := crypto.GoldenBody(64, int(l.Size))
	ws := make([]io.Writer, 6)
	for i := range ws {
		ws[i] = io.Discard
	}
	b.SetBytes(l.Size)
	for b.Loop() {
		if err := Encode(l, bytes.NewReader(body), ws); err != nil {
			b.Fatal(err)
		}
	}
}
