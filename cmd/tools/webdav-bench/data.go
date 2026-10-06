package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"math/rand/v2"
)

// genBlock is the unit the generator derives independently: block b of an
// object is the ChaCha8 stream keyed by (seed, b), so any range can be
// regenerated without the bytes before it and nothing is ever held whole.
const genBlock = 1 << 20

// gen streams `size` deterministic pseudo-random bytes for one seed. It is
// an io.ReadSeeker (the driver rewinds a PUT it retries) and, when hashing,
// keeps the sha256 of what it handed out — reset by a Seek to 0.
type gen struct {
	seed uint64
	size int64
	off  int64
	blk  int64
	buf  []byte
	h    hash.Hash
}

func newGen(seed uint64, size int64) *gen { return &gen{seed: seed, size: size, blk: -1} }

// hashing makes Read feed a sha256 (Sum = what was sent).
func (g *gen) hashing() *gen {
	g.h = sha256.New()
	return g
}

func (g *gen) fill(b int64) {
	if g.blk == b {
		return
	}
	n := int64(genBlock)
	if rem := g.size - b*genBlock; rem < n {
		n = rem
	}
	if int64(cap(g.buf)) < n {
		g.buf = make([]byte, n)
	}
	g.buf = g.buf[:n]
	var key [32]byte
	binary.LittleEndian.PutUint64(key[0:], g.seed)
	binary.LittleEndian.PutUint64(key[8:], uint64(b))
	c := rand.NewChaCha8(key)
	_, _ = c.Read(g.buf)
	g.blk = b
}

func (g *gen) Read(p []byte) (int, error) {
	if g.off >= g.size {
		return 0, io.EOF
	}
	b := g.off / genBlock
	g.fill(b)
	n := copy(p, g.buf[g.off-b*genBlock:])
	if g.h != nil {
		g.h.Write(p[:n])
	}
	g.off += int64(n)
	return n, nil
}

func (g *gen) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = g.off + offset
	case io.SeekEnd:
		abs = g.size + offset
	default:
		return 0, errors.New("gen: bad whence")
	}
	if abs < 0 {
		return 0, errors.New("gen: negative position")
	}
	if g.h != nil && abs != g.off {
		if abs != 0 {
			return 0, errors.New("gen: a hashing generator rewinds only to 0")
		}
		g.h.Reset()
	}
	g.off = abs
	return abs, nil
}

// Sum is the sha256 of the bytes read so far (hashing generators only).
func (g *gen) Sum() []byte { return g.h.Sum(nil) }

// expectedHash is the sha256 of a whole generated object.
func expectedHash(seed uint64, size int64) []byte {
	h := sha256.New()
	_, _ = io.Copy(h, newGen(seed, size))
	return h.Sum(nil)
}

// expectedRange is bytes [off, off+n) of a generated object.
func expectedRange(seed uint64, size, off, n int64) []byte {
	g := newGen(seed, size)
	_, _ = g.Seek(off, io.SeekStart)
	out := make([]byte, n)
	k, _ := io.ReadFull(g, out)
	return out[:k]
}

// seedFor derives an object's seed from the run seed and its key, so the
// same flags produce the same bytes (and v1 ≠ v2 of one key).
func seedFor(base int64, key string) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d/%s", base, key)
	return h.Sum64()
}

// verifyBody reads r to the end and compares its sha256 to want; it returns
// the byte count and whether it matched.
func verifyBody(r io.Reader, want []byte) (int64, bool, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return n, false, err
	}
	return n, bytes.Equal(h.Sum(nil), want), nil
}
