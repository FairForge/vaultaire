// internal/drivers/quickxor.go
//
// QuickXorHash — the hash OneDrive for Business keeps for every file
// (driveItem file.hashes.quickXorHash; Business has no sha256Hash). A port
// of Microsoft's reference implementation ("QuickXorHash", OneDrive API
// docs, the C# HashAlgorithm): a 160-bit accumulator, each byte XORed in
// at a bit offset that advances 11 bits per byte, circularly; the result is
// the 20 bytes little-endian with the data length (int64, little-endian)
// XORed into the last 8. Graph answers it base64-encoded.
//
// WP-FLEET-1 (D-F1-7): the migration tool (cmd/tools/permafrost-migrate-drive)
// verifies a copied tree by it — Graph's hash of each side, and this one
// where Graph has not computed a hash yet. Its vectors are LIVE Graph
// answers (quickxor_test.go). The driver's write verification is WP-FLEET-4.
package drivers

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
)

const (
	qxWidthInBits    = 160
	qxShift          = 11
	qxBitsInLastCell = 32
	qxCells          = (qxWidthInBits-1)/64 + 1 // 3
	qxSize           = (qxWidthInBits-1)/8 + 1  // 20
)

type quickXorHash struct {
	data        [qxCells]uint64
	lengthSoFar int64
	shiftSoFar  int
}

// NewQuickXorHash is a QuickXorHash (Size 20; Graph base64-encodes Sum).
func NewQuickXorHash() hash.Hash { return &quickXorHash{} }

func (q *quickXorHash) Size() int      { return qxSize }
func (q *quickXorHash) BlockSize() int { return 64 }

func (q *quickXorHash) Reset() { *q = quickXorHash{} }

// Write is the reference HashCore: byte i of this call goes in at
// shiftSoFar + 11·i (mod 160); bytes 160 apart share that offset, so they
// are XORed together first.
func (q *quickXorHash) Write(p []byte) (int, error) {
	cbSize := len(p)
	currentShift := q.shiftSoFar
	vectorArrayIndex := currentShift / 64
	vectorOffset := currentShift % 64
	iterations := min(cbSize, qxWidthInBits)

	for i := 0; i < iterations; i++ {
		isLastCell := vectorArrayIndex == qxCells-1
		bitsInVectorCell := 64
		if isLastCell {
			bitsInVectorCell = qxBitsInLastCell
		}
		if vectorOffset <= bitsInVectorCell-8 {
			for j := i; j < cbSize; j += qxWidthInBits {
				q.data[vectorArrayIndex] ^= uint64(p[j]) << uint(vectorOffset)
			}
		} else {
			index1 := vectorArrayIndex
			index2 := 0
			if !isLastCell {
				index2 = vectorArrayIndex + 1
			}
			low := uint(bitsInVectorCell - vectorOffset)
			var xored byte
			for j := i; j < cbSize; j += qxWidthInBits {
				xored ^= p[j]
			}
			q.data[index1] ^= uint64(xored) << uint(vectorOffset)
			q.data[index2] ^= uint64(xored) >> low
		}
		vectorOffset += qxShift
		for vectorOffset >= bitsInVectorCell {
			if isLastCell {
				vectorArrayIndex = 0
			} else {
				vectorArrayIndex++
			}
			vectorOffset -= bitsInVectorCell // < 11 after one wrap: never twice
		}
	}
	q.shiftSoFar = (q.shiftSoFar + qxShift*(cbSize%qxWidthInBits)) % qxWidthInBits
	q.lengthSoFar += int64(cbSize)
	return cbSize, nil
}

// Sum is the reference HashFinal appended to b.
func (q *quickXorHash) Sum(b []byte) []byte {
	var rgb [qxSize]byte
	for i := 0; i < qxCells-1; i++ {
		binary.LittleEndian.PutUint64(rgb[i*8:], q.data[i])
	}
	var last [8]byte
	binary.LittleEndian.PutUint64(last[:], q.data[qxCells-1])
	copy(rgb[(qxCells-1)*8:], last[:qxSize-(qxCells-1)*8])

	var lengthBytes [8]byte
	binary.LittleEndian.PutUint64(lengthBytes[:], uint64(q.lengthSoFar)) // #nosec G115 -- a length is never negative
	for i := range lengthBytes {
		rgb[qxWidthInBits/8-len(lengthBytes)+i] ^= lengthBytes[i]
	}
	return append(b, rgb[:]...)
}

// QuickXorSelftestSeed seeds the self-test body.
const QuickXorSelftestSeed = "stored/quickxor-selftest/v1"

// QuickXorSelftestData is the self-test's body of n bytes: sha256(seed ‖
// counter) blocks — the same bytes on every machine and Go version (no
// PRNG in it). The tool uploads it; the test hashes it.
func QuickXorSelftestData(n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	var c [8]byte
	for i := uint64(0); len(out) < n; i++ {
		binary.BigEndian.PutUint64(c[:], i)
		sum := sha256.Sum256(append([]byte(QuickXorSelftestSeed), c[:]...))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

// QuickXorSelftestSizes are the body sizes the self-test uploads: the
// edges of the 160-byte stride and of the 64-bit cells, and 1 MiB (+7).
var QuickXorSelftestSizes = []int{1, 8, 159, 160, 161, 1000, 1 << 20, 1<<20 + 7}

// QuickXorGraphVector is one live Graph answer: the selftest body of size
// bytes and the quickXorHash Graph reported for it.
type QuickXorGraphVector struct {
	size  int
	graph string
}

// QuickXorGraphVectors are recorded by `permafrost-migrate-drive -selftest`
// (2026-11-05, slot 3, fromhis@fromhis.com, OneDrive for Business: every
// body uploaded by a simple PUT, the hash read from Graph's answer).
var QuickXorGraphVectors = []QuickXorGraphVector{
	{size: 1, graph: "hgAAAAAAAAAAAAAAAQAAAAAAAAA="},
	{size: 8, graph: "hjhBGRDBhVXEAgMACAAAAAAAAAA="},
	{size: 159, graph: "Bs9HNmAWrjM/1g1mJgFOODd7KW4="},
	{size: 160, graph: "Bs9HNmAWrjM/1g1mGQFOODd7CXk="},
	{size: 161, graph: "689HNmAWrjM/1g1mGAFOODd7CXk="},
	{size: 1000, graph: "qzehuEndMdHeF+OCFFn6Xtg/y1A="},
	{size: 1048576, graph: "qN659cMSpalMEcM58/3POcN896o="},
	{size: 1048583, graph: "WN659cMSpalMEcM5qPVIOKetdMk="},
}
