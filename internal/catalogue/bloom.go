// Package catalogue is the public-artefact catalogue's filter (WP-EDGE-4 PR
// D, Stage 1a Prompt 48; plan 10.3): a Bloom filter of the PLAIN sha256 of
// every chunk of the public artefacts the operator seeded (public_chunks,
// migration 114). The catalogue_filter job builds it daily and publishes it;
// a data-plane host (the Worker, the SDK — this package compiled to WASM in
// sdk/wasm) checks each chunk's plain hash locally and marks a match
// `public`, so the control plane can reference the one `_public` copy
// instead of storing the chunk again. No per-chunk round trip is added and
// the server learns only what was referenced. A false positive costs one
// lookup that misses: the chunk uploads as usual.
//
// The byte format is versioned and frozen by testdata/bloom_golden.json:
//
//	[0x01][version u64 BE][m u64 BE][k u8][m/8 bytes of bits]
//
// m is a multiple of 8. Bit i lives in byte i/8, mask 1<<(i%8). The k
// indexes of a hash are double hashing over FNV-1a 64 of the hash's 32 raw
// bytes split in two: h1 = the low 32 bits, h2 = the high 32 bits,
// index_i = (h1 + i·h2) mod m.
package catalogue

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
)

// FormatV1 is the first byte of a version-1 filter.
const FormatV1 = 0x01

const headerLen = 1 + 8 + 8 + 1

// maxBits bounds a filter (2^33 bits = 1 GiB): a header asking for more is
// not one of ours.
const maxBits = uint64(1) << 33

// Filter is a Bloom filter of plain sha256 hashes.
type Filter struct {
	version uint64
	m       uint64
	k       uint8
	bits    []byte
}

// New sizes a filter for n hashes at a false-positive rate fp (0 < fp < 1):
// m = ⌈−n·ln fp / ln²2⌉ rounded up to a byte, k = round(m/n · ln 2), 1..30.
func New(n int, fp float64, version uint64) (*Filter, error) {
	if n < 1 {
		return nil, errors.New("catalogue: a filter holds at least one hash")
	}
	if fp <= 0 || fp >= 1 {
		return nil, errors.New("catalogue: the false-positive rate is in (0, 1)")
	}
	m := uint64(math.Ceil(-float64(n) * math.Log(fp) / (math.Ln2 * math.Ln2)))
	m = max((m+7)/8*8, 64)
	if m > maxBits {
		return nil, fmt.Errorf("catalogue: %d bits is past the bound", m)
	}
	k := int(math.Round(float64(m) / float64(n) * math.Ln2))
	k = min(max(k, 1), 30)
	return &Filter{version: version, m: m, k: uint8(k), bits: make([]byte, m/8)}, nil // #nosec G115 -- k is 1..30
}

// Version is the filter's version (the catalogue's: the job's run).
func (f *Filter) Version() uint64 { return f.version }

// M is the number of bits.
func (f *Filter) M() uint64 { return f.m }

// K is the number of hash functions.
func (f *Filter) K() uint8 { return f.k }

// indexes calls fn with each of the k bit indexes of the hash (hex sha256).
func (f *Filter) indexes(hashHex string, fn func(uint64)) error {
	raw, err := hex.DecodeString(hashHex)
	if err != nil || len(raw) != 32 {
		return errors.New("catalogue: a hash is a sha256 in hex")
	}
	h := fnv.New64a()
	_, _ = h.Write(raw)
	sum := h.Sum64()
	h1, h2 := sum&0xffffffff, sum>>32
	for i := uint64(0); i < uint64(f.k); i++ {
		fn((h1 + i*h2) % f.m)
	}
	return nil
}

// Add puts a hash in the filter.
func (f *Filter) Add(hashHex string) error {
	return f.indexes(hashHex, func(i uint64) { f.bits[i/8] |= 1 << (i % 8) })
}

// Contains reports whether the filter may hold the hash (never false for
// one it holds).
func (f *Filter) Contains(hashHex string) (bool, error) {
	all := true
	err := f.indexes(hashHex, func(i uint64) {
		if f.bits[i/8]&(1<<(i%8)) == 0 {
			all = false
		}
	})
	return all && err == nil, err
}

// Marshal is the filter in its byte format.
func (f *Filter) Marshal() []byte {
	b := make([]byte, headerLen+len(f.bits))
	b[0] = FormatV1
	binary.BigEndian.PutUint64(b[1:9], f.version)
	binary.BigEndian.PutUint64(b[9:17], f.m)
	b[17] = f.k
	copy(b[headerLen:], f.bits)
	return b
}

// Unmarshal reads a filter of the byte format; anything else is refused.
func Unmarshal(b []byte) (*Filter, error) {
	if len(b) < headerLen {
		return nil, errors.New("catalogue: shorter than a filter's header")
	}
	if b[0] != FormatV1 {
		return nil, fmt.Errorf("catalogue: format 0x%02x is not one this build reads", b[0])
	}
	f := &Filter{version: binary.BigEndian.Uint64(b[1:9]), m: binary.BigEndian.Uint64(b[9:17]), k: b[17]}
	if f.m == 0 || f.m%8 != 0 || f.m > maxBits || f.k == 0 || f.k > 30 {
		return nil, errors.New("catalogue: the header is not a filter's")
	}
	if uint64(len(b)-headerLen) != f.m/8 {
		return nil, fmt.Errorf("catalogue: %d bytes of bits, the header says %d", len(b)-headerLen, f.m/8)
	}
	f.bits = append([]byte(nil), b[headerLen:]...)
	return f, nil
}
