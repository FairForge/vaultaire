package catalogue

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite testdata/bloom_golden.json")

func hashOf(i int) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i))
	s := sha256.Sum256(b[:])
	return hex.EncodeToString(s[:])
}

func TestFilter_NoFalseNegativesAndTheFalsePositiveRate(t *testing.T) {
	const n = 10000
	f, err := New(n, 0.01, 3)
	require.NoError(t, err)
	for i := range n {
		require.NoError(t, f.Add(hashOf(i)))
	}
	for i := range n {
		ok, err := f.Contains(hashOf(i))
		require.NoError(t, err)
		require.True(t, ok, "never a false negative")
	}
	fp := 0
	const probes = 100000
	for i := n; i < n+probes; i++ {
		if ok, _ := f.Contains(hashOf(i)); ok {
			fp++
		}
	}
	rate := float64(fp) / probes
	assert.Less(t, rate, 0.015, "about the 1 %% asked for (%.4f)", rate)
	assert.Greater(t, rate, 0.002)
}

func TestFilter_TheByteFormatRoundTripsAndRefusesWhatIsNotOne(t *testing.T) {
	f, err := New(100, 0.01, 42)
	require.NoError(t, err)
	require.NoError(t, f.Add(hashOf(1)))
	b := f.Marshal()
	assert.Equal(t, byte(0x01), b[0], "format byte")
	assert.Equal(t, uint64(42), binary.BigEndian.Uint64(b[1:9]), "version")
	assert.Equal(t, f.M(), binary.BigEndian.Uint64(b[9:17]), "m")
	assert.Equal(t, f.K(), b[17], "k")
	assert.Len(t, b, 18+int(f.M()/8))

	g, err := Unmarshal(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), g.Version())
	ok, err := g.Contains(hashOf(1))
	require.NoError(t, err)
	assert.True(t, ok)

	for name, bad := range map[string][]byte{
		"empty":        nil,
		"short header": b[:10],
		"format 0x02":  append([]byte{0x02}, b[1:]...),
		"short bits":   b[:len(b)-1],
		"k 0":          append(append(append([]byte{}, b[:17]...), 0), b[18:]...),
	} {
		_, err := Unmarshal(bad)
		assert.Error(t, err, name)
	}
	_, err = f.Contains("not hex")
	assert.Error(t, err)
	_, err = New(0, 0.01, 1)
	assert.Error(t, err)
	_, err = New(10, 1.5, 1)
	assert.Error(t, err)
}

// golden is testdata/bloom_golden.json: what the Go filter answers, which
// the WASM build (sdk/wasm, edge/s3-ingest/test/catalogue.test.js) must
// answer byte for byte.
type golden struct {
	N            int      `json:"n"`
	FP           float64  `json:"fp"`
	Version      uint64   `json:"version"`
	M            uint64   `json:"m"`
	K            uint8    `json:"k"`
	Inserted     []string `json:"inserted"`
	Probes       []string `json:"probes"`
	ProbeResults []bool   `json:"probe_results"`
	FilterHex    string   `json:"filter_hex"`
}

func TestFilter_GoldenVectors(t *testing.T) {
	g := golden{N: 64, FP: 0.01, Version: 20261028}
	f, err := New(g.N, g.FP, g.Version)
	require.NoError(t, err)
	for i := range g.N {
		g.Inserted = append(g.Inserted, hashOf(i))
		require.NoError(t, f.Add(hashOf(i)))
	}
	for i := 1000; i < 1200; i++ {
		ok, err := f.Contains(hashOf(i))
		require.NoError(t, err)
		g.Probes, g.ProbeResults = append(g.Probes, hashOf(i)), append(g.ProbeResults, ok)
	}
	g.M, g.K, g.FilterHex = f.M(), f.K(), hex.EncodeToString(f.Marshal())
	path := "testdata/bloom_golden.json"
	if *update {
		b, err := json.MarshalIndent(g, "", " ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o600))
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var want golden
	require.NoError(t, json.Unmarshal(b, &want))
	assert.Equal(t, want, g, "the filter's bytes and answers are frozen (run -update only when the format version changes)")
}
