// Package ec is the Reed-Solomon stripe codec of the second copy at the edge
// (WP-EDGE-4 PR B, Stage 1a Prompt 48; plan 40.10 slice 3), the WP-VAULT-1
// codec (internal/api vault_parity_codec.go) lifted out of the API package so
// a data-plane host and the WASM build (sdk/ecwasm) run the same code.
//
// The layout is vault_parity's: a stripe is K × Stripe bytes of the object —
// data piece i of stripe s is the object's bytes at s·K·S + i·S (the last
// stripe zero-padded) — and its M parity pieces are computed from those K.
// Shard j is piece j of every stripe concatenated (stripes × S bytes): the K
// data shards hold the object, the M parity shards its redundancy, and any K
// of the K+M rebuild it. A reader that holds the K data shards touches no
// parity (no read penalty). Memory: one stripe.
package ec

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

// ErrTooFewShards: fewer than K shards can be read.
var ErrTooFewShards = errors.New("ec: fewer than k shards remain")

// Layout is an object's stripe geometry.
type Layout struct {
	K, M   int
	Stripe int   // bytes per piece
	Size   int64 // the object's size
}

// Stripes is the number of stripes an object of this size needs.
func (l Layout) Stripes() int64 {
	block := int64(l.K * l.Stripe)
	return (l.Size + block - 1) / block
}

// ShardBytes is the length of every shard.
func (l Layout) ShardBytes() int64 { return l.Stripes() * int64(l.Stripe) }

// DataPieceRange is the object range data piece i of stripe s covers:
// offset and the number of real (unpadded) bytes, 0 for an all-padding piece.
func (l Layout) DataPieceRange(stripe int64, shard int) (off, n int64) {
	off = stripe*int64(l.K*l.Stripe) + int64(shard*l.Stripe)
	if off >= l.Size {
		return off, 0
	}
	n = int64(l.Stripe)
	if off+n > l.Size {
		n = l.Size - off
	}
	return off, n
}

func (l Layout) check() error {
	if l.K < 1 || l.M < 1 || l.K+l.M > 256 || l.Stripe < 1 || l.Size < 0 {
		return fmt.Errorf("ec: layout %d+%d × %d B of %d B", l.K, l.M, l.Stripe, l.Size)
	}
	return nil
}

func (l Layout) encoder() (reedsolomon.Encoder, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	enc, err := reedsolomon.New(l.K, l.M)
	if err != nil {
		return nil, fmt.Errorf("ec: reedsolomon(%d,%d): %w", l.K, l.M, err)
	}
	return enc, nil
}

// EncodeStripe computes the M parity pieces of one stripe in place:
// pieces[0..K) are the data (each Stripe bytes), pieces[K..K+M) are written.
func (l Layout) EncodeStripe(pieces [][]byte) error {
	enc, err := l.encoder()
	if err != nil {
		return err
	}
	return enc.Encode(pieces)
}

// Encode reads the object (exactly Size bytes) from src and writes shard j
// to out[j] — the K data shards, then the M parity shards — stripe by
// stripe, so the K+M writers fill in step (a host PUTs them at once).
func Encode(l Layout, src io.Reader, out []io.Writer) error {
	if len(out) != l.K+l.M {
		return fmt.Errorf("ec: %d writers for %d shards", len(out), l.K+l.M)
	}
	enc, err := l.encoder()
	if err != nil {
		return err
	}
	pieces := make([][]byte, l.K+l.M)
	for i := range pieces {
		pieces[i] = make([]byte, l.Stripe)
	}
	var read int64
	for s := int64(0); s < l.Stripes(); s++ {
		for i := 0; i < l.K; i++ {
			_, n := l.DataPieceRange(s, i)
			buf := pieces[i]
			if n > 0 {
				if _, err := io.ReadFull(src, buf[:n]); err != nil {
					return fmt.Errorf("ec: stripe %d piece %d: read %d bytes at %d of %d: %w", s, i, n, read, l.Size, err)
				}
				read += n
			}
			clear(buf[n:])
		}
		if err := enc.Encode(pieces); err != nil {
			return fmt.Errorf("ec: stripe %d: %w", s, err)
		}
		for j, w := range out {
			if _, err := w.Write(pieces[j]); err != nil {
				return fmt.Errorf("ec: write shard %d stripe %d: %w", j, s, err)
			}
		}
	}
	var extra [1]byte
	if n, _ := src.Read(extra[:]); n > 0 {
		return fmt.Errorf("ec: the body is longer than its %d bytes", l.Size)
	}
	return nil
}

// Reader rebuilds the object from its shards.
type Reader struct {
	l      Layout
	shards []io.Reader // nil = missing (or failed: dropped on its first error)
	enc    reedsolomon.Encoder
	pieces [][]byte
	pos    []int64 // stripes each shard's reader has passed (a parity shard is read only when needed)
	stripe int64
	out    []byte // the current stripe's object bytes not yet read
}

// NewReader reads the object from shards (K+M entries, nil for a shard that
// is not there): the data shards when they all answer, a reconstruction from
// any K otherwise. A shard whose read fails is dropped for the rest of the
// object; fewer than K left is ErrTooFewShards.
func NewReader(l Layout, shards []io.Reader) (*Reader, error) {
	if len(shards) != l.K+l.M {
		return nil, fmt.Errorf("ec: %d readers for %d shards", len(shards), l.K+l.M)
	}
	enc, err := l.encoder()
	if err != nil {
		return nil, err
	}
	have := 0
	for _, s := range shards {
		if s != nil {
			have++
		}
	}
	if have < l.K {
		return nil, ErrTooFewShards
	}
	pieces := make([][]byte, l.K+l.M)
	return &Reader{l: l, shards: append([]io.Reader(nil), shards...), enc: enc, pieces: pieces, pos: make([]int64, l.K+l.M)}, nil
}

func (r *Reader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.stripe >= r.l.Stripes() {
			return 0, io.EOF
		}
		if err := r.nextStripe(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// skipTo advances shard j's reader to the current stripe (a parity shard
// not needed by the stripes before).
func (r *Reader) skipTo(j int) error {
	if behind := r.stripe - r.pos[j]; behind > 0 {
		if _, err := io.CopyN(io.Discard, r.shards[j], behind*int64(r.l.Stripe)); err != nil {
			return err
		}
		r.pos[j] = r.stripe
	}
	return nil
}

// nextStripe reads one piece from every live shard (parity only when a data
// shard is missing) and decodes the stripe.
func (r *Reader) nextStripe() error {
	l := r.l
	dataMissing := false
	for i := 0; i < l.K; i++ {
		if r.shards[i] == nil {
			dataMissing = true
		}
	}
	for j := range r.shards {
		r.pieces[j] = nil
		if r.shards[j] == nil || (j >= l.K && !dataMissing) {
			continue
		}
		buf := make([]byte, l.Stripe)
		err := r.skipTo(j)
		if err == nil {
			_, err = io.ReadFull(r.shards[j], buf)
		}
		if err != nil {
			// Dropped: a later stripe reconstructs without it.
			r.shards[j] = nil
			if j < l.K {
				dataMissing = true
				// The parity of THIS stripe is read now (j ascending: every
				// parity shard comes after the data shards).
			}
			continue
		}
		r.pieces[j] = buf
		r.pos[j]++
	}
	have := 0
	for _, p := range r.pieces {
		if p != nil {
			have++
		}
	}
	if have < l.K {
		return ErrTooFewShards
	}
	if dataMissing {
		if err := r.enc.ReconstructData(r.pieces); err != nil {
			return fmt.Errorf("ec: stripe %d: %w", r.stripe, err)
		}
	}
	r.out = r.out[:0]
	for i := 0; i < l.K; i++ {
		_, n := l.DataPieceRange(r.stripe, i)
		r.out = append(r.out, r.pieces[i][:n]...)
	}
	r.stripe++
	return nil
}
