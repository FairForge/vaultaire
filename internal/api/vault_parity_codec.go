package api

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

// The Vault parity codec (WP-VAULT-1 part 2): Reed-Solomon k+m, stripe by
// stripe, so an object of any size is encoded and rebuilt in bounded memory.
//
// A stripe is k × stripe bytes of the object: data piece i of stripe s is the
// object's bytes at s×k×S + i×S (the last stripe zero-padded), and the m
// parity pieces of the stripe are computed from those k. Parity shard j is
// the concatenation of parity piece j of every stripe — stripes × S bytes,
// written as one stream per shard. The data shards are NOT stored: the
// object's own backend (tape) holds them, and a reader that needs data piece
// (s, i) reads it as a range of the object.
//
// Any k of the k+m pieces of a stripe rebuild it. With k = m = 4 the four
// parity shards alone are a complete copy: the fallback reader rebuilds from
// them when the object's backend answers nothing, and from the data ranges it
// CAN read plus the parity when a parity shard is gone too.

type parityLayout struct {
	k, m   int
	stripe int   // bytes per piece
	size   int64 // the object's size
}

// stripes is the number of stripes an object of this size needs.
func (l parityLayout) stripes() int64 {
	block := int64(l.k * l.stripe)
	return (l.size + block - 1) / block
}

// shardBytes is the length of every parity shard.
func (l parityLayout) shardBytes() int64 { return l.stripes() * int64(l.stripe) }

// dataPieceRange is the object range data piece i of stripe s covers:
// offset and the number of real (unpadded) bytes, 0 for a piece that is all
// padding.
func (l parityLayout) dataPieceRange(stripe int64, shard int) (off, n int64) {
	off = stripe*int64(l.k*l.stripe) + int64(shard*l.stripe)
	if off >= l.size {
		return off, 0
	}
	n = int64(l.stripe)
	if off+n > l.size {
		n = l.size - off
	}
	return off, n
}

func (l parityLayout) encoder() (reedsolomon.Encoder, error) {
	enc, err := reedsolomon.New(l.k, l.m)
	if err != nil {
		return nil, fmt.Errorf("reedsolomon(%d,%d): %w", l.k, l.m, err)
	}
	return enc, nil
}

// encodeParity reads the object from src and writes parity shard j to
// out[j]. Memory: one stripe (k + m pieces).
func encodeParity(l parityLayout, src io.Reader, out []io.Writer) error {
	if len(out) != l.m {
		return fmt.Errorf("encode parity: %d writers for %d parity shards", len(out), l.m)
	}
	enc, err := l.encoder()
	if err != nil {
		return err
	}
	pieces := make([][]byte, l.k+l.m)
	for i := range pieces {
		pieces[i] = make([]byte, l.stripe)
	}
	var read int64
	for s := int64(0); s < l.stripes(); s++ {
		for i := 0; i < l.k; i++ {
			_, n := l.dataPieceRange(s, i)
			buf := pieces[i]
			if n > 0 {
				if _, err := io.ReadFull(src, buf[:n]); err != nil {
					return fmt.Errorf("encode parity: stripe %d piece %d: read %d bytes at %d of %d: %w", s, i, n, read, l.size, err)
				}
				read += n
			}
			for z := n; z < int64(l.stripe); z++ {
				buf[z] = 0
			}
		}
		if err := enc.Encode(pieces); err != nil {
			return fmt.Errorf("encode parity: stripe %d: %w", s, err)
		}
		for j := 0; j < l.m; j++ {
			if _, err := out[j].Write(pieces[l.k+j]); err != nil {
				return fmt.Errorf("encode parity: write shard %d stripe %d: %w", j, s, err)
			}
		}
	}
	// The object must be exactly as long as the layout says: a longer body
	// here would be an overwrite racing the job (the etag guard catches the
	// row; this catches the bytes).
	var extra [1]byte
	if n, _ := src.Read(extra[:]); n > 0 {
		return fmt.Errorf("encode parity: the object is longer than its recorded %d bytes", l.size)
	}
	return nil
}

// dataPieceFunc fetches data piece `shard` of stripe `stripe` of the object:
// exactly stripe bytes (zero-padded). It is how the rebuilder reaches the
// object's own backend for the pieces the parity cannot replace.
type dataPieceFunc func(ctx context.Context, stripe int64, shard int) ([]byte, error)

// parityRebuilder streams a window of the object rebuilt stripe by stripe
// from the parity streams (nil = that shard is unavailable) and, when fewer
// than k parity pieces are present, data pieces from dataPiece. It is pull
// based: one stripe is decoded per refill, no goroutines.
type parityRebuilder struct {
	ctx       context.Context
	l         parityLayout
	enc       reedsolomon.Encoder
	parity    []io.ReadCloser
	dataPiece dataPieceFunc
	pieces    [][]byte // k + m slots, nil = absent for the current stripe
	stripe    int64    // next stripe to decode
	endStripe int64    // one past the last stripe of the window
	skip      int64    // bytes of the first decoded stripe to drop (window offset)
	left      int64    // bytes of the window still to deliver
	cur       []byte   // decoded bytes of the current stripe not yet delivered
	err       error
	closed    bool
}

// newParityRebuilder reads [off, off+n) of the object.
func newParityRebuilder(ctx context.Context, l parityLayout, parity []io.ReadCloser, dataPiece dataPieceFunc, off, n int64) *parityRebuilder {
	r := &parityRebuilder{ctx: ctx, l: l, parity: parity, dataPiece: dataPiece, left: n}
	block := int64(l.k * l.stripe)
	r.stripe = off / block
	r.skip = off - r.stripe*block
	r.endStripe = (off + n + block - 1) / block
	if n <= 0 {
		r.endStripe = r.stripe
	}
	r.pieces = make([][]byte, l.k+l.m)
	var err error
	if r.enc, err = l.encoder(); err != nil {
		r.err = err
	}
	if len(parity) != l.m {
		r.err = fmt.Errorf("parity rebuild: %d streams for %d parity shards", len(parity), l.m)
	}
	// The parity streams start at the first stripe of the window.
	if r.err == nil && r.stripe > 0 {
		for j, s := range parity {
			if s == nil {
				continue
			}
			if _, err := io.CopyN(io.Discard, s, r.stripe*int64(l.stripe)); err != nil {
				r.parity[j] = nil // from here on this shard counts as absent
				_ = s.Close()
			}
		}
	}
	return r
}

func (r *parityRebuilder) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for len(r.cur) == 0 {
		if r.left <= 0 || r.stripe >= r.endStripe {
			return 0, io.EOF
		}
		if err := r.decodeNext(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	r.left -= int64(n)
	return n, nil
}

// decodeNext rebuilds stripe r.stripe into r.cur (window-trimmed).
func (r *parityRebuilder) decodeNext() error {
	s := r.stripe
	present := 0
	for j := 0; j < r.l.m; j++ {
		r.pieces[r.l.k+j] = nil
		stream := r.parity[j]
		if stream == nil {
			continue
		}
		buf := make([]byte, r.l.stripe)
		if _, err := io.ReadFull(stream, buf); err != nil {
			// A shard that ends short or errors is gone for the rest of
			// the read, not silently zero.
			r.parity[j] = nil
			_ = stream.Close()
			continue
		}
		r.pieces[r.l.k+j] = buf
		present++
	}
	for i := 0; i < r.l.k; i++ {
		r.pieces[i] = nil
	}
	var dataErr error
	for i := 0; i < r.l.k && present < r.l.k; i++ {
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		piece, err := r.dataPiece(r.ctx, s, i)
		if err != nil {
			dataErr = err
			continue
		}
		if len(piece) != r.l.stripe {
			dataErr = fmt.Errorf("data piece %d of stripe %d is %d bytes, want %d", i, s, len(piece), r.l.stripe)
			continue
		}
		r.pieces[i] = piece
		present++
	}
	if present < r.l.k {
		return fmt.Errorf("parity rebuild: stripe %d has %d of %d pieces (parity streams and data ranges both short): %w",
			s, present, r.l.k, errors.Join(dataErr, errParityUnavailable))
	}
	if err := r.enc.Reconstruct(r.pieces); err != nil {
		return fmt.Errorf("parity rebuild: stripe %d: %w", s, err)
	}
	block := int64(r.l.k * r.l.stripe)
	out := make([]byte, 0, block)
	for i := 0; i < r.l.k; i++ {
		out = append(out, r.pieces[i]...)
	}
	// Trim the padding of the last stripe and the window's edges.
	if end := r.l.size - s*block; end < int64(len(out)) {
		out = out[:end]
	}
	if r.skip > 0 {
		if r.skip >= int64(len(out)) {
			out = out[:0]
		} else {
			out = out[r.skip:]
		}
		r.skip = 0
	}
	if int64(len(out)) > r.left {
		out = out[:r.left]
	}
	r.cur = out
	r.stripe++
	return nil
}

// Close closes every parity stream.
func (r *parityRebuilder) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	for j, s := range r.parity {
		if s != nil {
			_ = s.Close()
			r.parity[j] = nil
		}
	}
	return nil
}

// errParityUnavailable: the parity copy cannot serve this read.
var errParityUnavailable = errors.New("the parity copy cannot rebuild this object")
