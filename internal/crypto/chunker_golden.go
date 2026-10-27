package crypto

// GoldenBody is the deterministic pseudo-random body of the chunker's golden
// vectors (WP-EDGE-2 Part A): splitmix64 from seed, 8 little-endian bytes per
// step, cut to n. The 100 MiB body itself is never a file in the repo — the
// Go golden test (TestChunker_GoldenBoundaries) and the WASM chunker's node
// test (edge/s3-ingest/test/chunker.test.js, which gets the body from the
// module's own goldenBody export — this code compiled to WASM) both generate
// it and must cut it at the same recorded offsets, byte-identical.
func GoldenBody(seed uint64, n int) []byte {
	out := make([]byte, n)
	x := seed
	for i := 0; i < n; i += 8 {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		for j := 0; j < 8 && i+j < n; j++ {
			out[i+j] = byte(z >> (8 * j))
		}
	}
	return out
}
