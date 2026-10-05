package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-VAULT-1 part 2: the parity codec. RS(4,4) stripe by stripe: the data
// shards of a stripe are four consecutive pieces of the object, the parity
// shards are written out as four streams; any four of the eight pieces of a
// stripe rebuild it. The tests use a 64 KiB stripe so an odd-sized object
// of a few MiB has a padded last stripe.

func parityTestObject(t *testing.T, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func encodeToBuffers(t *testing.T, l parityLayout, obj []byte) [][]byte {
	t.Helper()
	bufs := make([]*bytes.Buffer, l.m)
	ws := make([]io.Writer, l.m)
	for j := range bufs {
		bufs[j] = &bytes.Buffer{}
		ws[j] = bufs[j]
	}
	require.NoError(t, encodeParity(l, bytes.NewReader(obj), ws))
	out := make([][]byte, l.m)
	for j := range bufs {
		out[j] = bufs[j].Bytes()
		assert.Equal(t, l.shardBytes(), int64(len(out[j])), "parity shard %d is stripes × stripe bytes", j)
	}
	return out
}

func TestParityLayout_Geometry(t *testing.T) {
	l := parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 3<<20 + 777}
	assert.Equal(t, int64(13), l.stripes(), "3 MiB + 777 B over 256 KiB stripes = 12 full + 1 partial")
	assert.Equal(t, int64(13<<16), l.shardBytes())
	assert.Equal(t, int64(0), parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 0}.stripes())
}

func TestParityCodec_ParityAloneRebuildsTheObject(t *testing.T) {
	l := parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 3<<20 + 777}
	obj := parityTestObject(t, int(l.size))
	parity := encodeToBuffers(t, l, obj)

	// Every data piece unreadable (Geyser gone): the four parity streams
	// are enough — RS(4,4) rebuilds from ANY four of the eight.
	noData := func(context.Context, int64, int) ([]byte, error) { return nil, errors.New("geyser: unavailable") }
	r := newParityRebuilder(context.Background(), l, parityStreams(parity), noData, 0, l.size)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.True(t, bytes.Equal(obj, got), "rebuilt bytes differ")
}

func TestParityCodec_MissingParityUsesTheDataRangesThatAreReadable(t *testing.T) {
	l := parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 2<<20 + 5}
	obj := parityTestObject(t, int(l.size))
	parity := encodeToBuffers(t, l, obj)
	var dataReads int
	dataPiece := func(_ context.Context, stripe int64, shard int) ([]byte, error) {
		dataReads++
		return stripeDataPiece(l, obj, stripe, shard), nil
	}

	// Two parity shards gone: two data pieces per stripe come from the
	// object's own backend (a Geyser range each).
	streams := parityStreams(parity)
	streams[1], streams[3] = nil, nil
	r := newParityRebuilder(context.Background(), l, streams, dataPiece, 0, l.size)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(obj, got))
	assert.Equal(t, int(2*l.stripes()), dataReads, "exactly the pieces needed: two per stripe")

	// Three parity shards gone and the data unreadable: not rebuildable —
	// an error naming the shortfall, never silent zeros.
	streams = parityStreams(parity)
	streams[0], streams[1], streams[2] = nil, nil, nil
	r = newParityRebuilder(context.Background(), l, streams,
		func(context.Context, int64, int) ([]byte, error) { return nil, errors.New("geyser: 503") }, 0, l.size)
	_, err = io.ReadAll(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "of 4 pieces")
}

func TestParityCodec_WindowedReadDecodesOnlyTheStripesItNeeds(t *testing.T) {
	l := parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 4<<20 + 99}
	obj := parityTestObject(t, int(l.size))
	parity := encodeToBuffers(t, l, obj)
	block := int64(l.k * l.stripe)

	for _, w := range []struct{ off, n int64 }{{0, 10}, {block - 5, 10}, {3*block + 7, 2*block + 1}, {l.size - 1, 1}, {block, block}} {
		r := newParityRebuilder(context.Background(), l, parityStreams(parity),
			func(context.Context, int64, int) ([]byte, error) { return nil, errors.New("no data") }, w.off, w.n)
		got, err := io.ReadAll(r)
		require.NoError(t, err, "window %+v", w)
		assert.True(t, bytes.Equal(obj[w.off:w.off+w.n], got), "window %+v", w)
		_ = r.Close()
	}
}

func TestParityCodec_ACorruptParityStreamIsAnError(t *testing.T) {
	l := parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 1 << 20}
	obj := parityTestObject(t, int(l.size))
	parity := encodeToBuffers(t, l, obj)
	parity[2] = parity[2][:len(parity[2])-1000] // a shard that ends short
	r := newParityRebuilder(context.Background(), l, parityStreams(parity),
		func(context.Context, int64, int) ([]byte, error) { return nil, errors.New("no data") }, 0, l.size)
	_, err := io.ReadAll(r)
	require.Error(t, err)
}

// parityStreams wraps shard buffers as the readers the rebuilder takes.
func parityStreams(shards [][]byte) []io.ReadCloser {
	out := make([]io.ReadCloser, len(shards))
	for j, s := range shards {
		out[j] = io.NopCloser(bytes.NewReader(s))
	}
	return out
}

// stripeDataPiece is data shard `shard` of stripe `stripe`: the object's
// bytes at that position, zero-padded to the stripe size.
func stripeDataPiece(l parityLayout, obj []byte, stripe int64, shard int) []byte {
	piece := make([]byte, l.stripe)
	off := stripe*int64(l.k*l.stripe) + int64(shard*l.stripe)
	if off < int64(len(obj)) {
		copy(piece, obj[off:])
	}
	return piece
}

func TestParityCodec_StripeDataPieceAgreesWithTheEncoder(t *testing.T) {
	// The data shards are never stored: a reader that needs one fetches the
	// object's own bytes as a range. This pins the mapping the rebuilder's
	// dataPiece callers rely on (offset = stripe × k × S + shard × S).
	l := parityLayout{k: 4, m: 4, stripe: 1 << 16, size: 1<<20 + 3}
	obj := parityTestObject(t, int(l.size))
	for s := int64(0); s < l.stripes(); s++ {
		for i := 0; i < l.k; i++ {
			off, n := l.dataPieceRange(s, i)
			want := stripeDataPiece(l, obj, s, i)
			var got []byte
			if n > 0 {
				got = make([]byte, l.stripe)
				copy(got, obj[off:off+n])
			}
			if n == 0 {
				assert.Equal(t, make([]byte, l.stripe), want, fmt.Sprintf("stripe %d shard %d is all padding", s, i))
				continue
			}
			assert.Equal(t, want, got, "stripe %d shard %d", s, i)
		}
	}
}
