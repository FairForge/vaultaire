package main

import (
	"bytes"
	"fmt"

	"github.com/klauspost/reedsolomon"
	xraptorq "github.com/xssnick/raptorq"
)

// codec is what the bench needs from an erasure code: n shards out of a
// payload, and the payload back from any k of them. Both codecs below are
// driven through the same shard slots, legs and read shapes, so a RaptorQ run
// and a Reed-Solomon run differ in nothing but the code.
type codec interface {
	Name() string
	// Shards splits + encodes: n equal-length shards, data shards first.
	Shards(payload []byte) ([][]byte, error)
	// Rebuild takes the n-slot slice with nil for shards that were not fetched
	// (at least k present) and returns the first `size` bytes of the payload.
	Rebuild(have [][]byte, size int64) ([]byte, error)
}

func newCodec(name string, k, m, symbol int, size int64) (codec, error) {
	return newCodecStripe(name, k, m, symbol, size, 0)
}

// newCodecStripe is newCodec with an interleave stripe for Reed-Solomon: 0 =
// contiguous shards (klauspost Split: shard j is the j-th k-th of the payload),
// otherwise the payload is cut into stripes of k×stripe bytes and shard j
// holds the j-th piece of EVERY stripe. The interleaved layout is what an
// edge reader needs to decode stripe by stripe inside a 128 MB isolate
// (tools/edge-ec-worker, mode=stripe) — any object size, bounded memory.
func newCodecStripe(name string, k, m, symbol int, size int64, stripe int) (codec, error) {
	switch name {
	case "rs":
		enc, err := reedsolomon.New(k, m)
		if err != nil {
			return nil, fmt.Errorf("reedsolomon: %w", err)
		}
		return &rsCodec{enc: enc, k: k, m: m, stripe: stripe}, nil
	case "raptorq":
		if symbol <= 0 || symbol > 65535 {
			return nil, fmt.Errorf("raptorq: symbol size %d outside 1..65535 (RFC 6330)", symbol)
		}
		return &rqCodec{k: k, m: m, symbol: symbol, size: size}, nil
	}
	return nil, fmt.Errorf("unknown codec %q (rs|raptorq)", name)
}

// --- Reed-Solomon (klauspost) -----------------------------------------------

type rsCodec struct {
	enc    reedsolomon.Encoder
	k, m   int
	stripe int // 0 = contiguous shards; else interleaved stripes of this many bytes per shard
}

func (c *rsCodec) Name() string {
	if c.stripe > 0 {
		return fmt.Sprintf("RS(%d,%d,stripe=%dK)", c.k, c.m, c.stripe>>10)
	}
	return fmt.Sprintf("RS(%d,%d)", c.k, c.m)
}

func (c *rsCodec) Shards(payload []byte) ([][]byte, error) {
	if c.stripe > 0 {
		return c.shardsInterleaved(payload)
	}
	shards, err := c.enc.Split(payload)
	if err != nil {
		return nil, fmt.Errorf("split: %w", err)
	}
	if err := c.enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return shards, nil
}

func (c *rsCodec) shardsInterleaved(payload []byte) ([][]byte, error) {
	n := c.k + c.m
	block := c.k * c.stripe
	nStripes := (len(payload) + block - 1) / block
	shards := make([][]byte, n)
	for j := range shards {
		shards[j] = make([]byte, 0, nStripes*c.stripe)
	}
	buf := make([]byte, block)
	for s := 0; s < nStripes; s++ {
		lo := s * block
		hi := lo + block
		if hi > len(payload) {
			hi = len(payload)
		}
		for i := range buf {
			buf[i] = 0
		}
		copy(buf, payload[lo:hi])
		sub, err := c.enc.Split(buf)
		if err != nil {
			return nil, fmt.Errorf("split stripe %d: %w", s, err)
		}
		if err := c.enc.Encode(sub); err != nil {
			return nil, fmt.Errorf("encode stripe %d: %w", s, err)
		}
		for j := 0; j < n; j++ {
			shards[j] = append(shards[j], sub[j]...)
		}
	}
	return shards, nil
}

func (c *rsCodec) rebuildInterleaved(have [][]byte, size int64) ([]byte, error) {
	n := c.k + c.m
	shardLen := 0
	for _, h := range have {
		if h != nil {
			shardLen = len(h)
			break
		}
	}
	if shardLen == 0 || shardLen%c.stripe != 0 {
		return nil, fmt.Errorf("interleaved rebuild: shard length %d is not a multiple of the stripe %d", shardLen, c.stripe)
	}
	out := make([]byte, 0, size)
	tmp := make([][]byte, n)
	for s := 0; s < shardLen/c.stripe; s++ {
		for j := 0; j < n; j++ {
			if have[j] == nil {
				tmp[j] = nil
				continue
			}
			tmp[j] = append([]byte(nil), have[j][s*c.stripe:(s+1)*c.stripe]...)
		}
		if err := c.enc.ReconstructData(tmp); err != nil {
			return nil, fmt.Errorf("reconstruct stripe %d: %w", s, err)
		}
		for j := 0; j < c.k; j++ {
			out = append(out, tmp[j]...)
		}
	}
	if int64(len(out)) < size {
		return nil, fmt.Errorf("interleaved rebuild: %d bytes < size %d", len(out), size)
	}
	return out[:size], nil
}

func (c *rsCodec) Rebuild(have [][]byte, size int64) ([]byte, error) {
	if c.stripe > 0 {
		return c.rebuildInterleaved(have, size)
	}
	if err := c.enc.ReconstructData(have); err != nil {
		return nil, fmt.Errorf("reconstruct: %w", err)
	}
	var out bytes.Buffer
	out.Grow(int(size))
	if err := c.enc.Join(&out, have, int(size)); err != nil {
		return nil, fmt.Errorf("join: %w", err)
	}
	return out.Bytes(), nil
}

// --- RaptorQ (RFC 6330, xssnick/raptorq) ------------------------------------
//
// RaptorQ is rateless: the encoder emits symbols with ids (ESIs) without end,
// and any K' slightly above the K source symbols decodes. To fit the bench's
// "n shards on n backend slots, any k decode" shape, the symbols are dealt
// round-robin to the n shards: symbol with ESI e sits at position e/n of shard
// e%n. Every shard holds S/n symbols, so any k shards together hold k·S/n
// symbols — S is chosen so that is at least K+extra, the surplus RaptorQ
// wants for a decode to succeed every time (the probability of needing more
// than 2 extra symbols is 1e-6 by the RFC's own analysis).
//
// So the "shard" is a group of symbols, not a contiguous slice of the payload
// — the trade: no shard is plain data (RS shards 0..k-1 are), but ANY k shards
// decode and the repair count is not fixed at encode time. Shard length is
// (S/n)·T bytes, which rounds up a little against RS's size/k.

type rqCodec struct {
	k, m   int
	symbol int
	size   int64
	perSh  int // symbols per shard
}

const rqExtraSymbols = 4

func (c *rqCodec) Name() string {
	return fmt.Sprintf("RaptorQ(%d,%d,T=%dK)", c.k, c.m, c.symbol>>10)
}

func (c *rqCodec) Shards(payload []byte) ([][]byte, error) {
	n := c.k + c.m
	rq := xraptorq.NewRaptorQ(uint32(c.symbol))
	enc, err := rq.CreateEncoder(payload)
	if err != nil {
		return nil, fmt.Errorf("raptorq encoder: %w", err)
	}
	K := int(enc.BaseSymbolsNum())
	// any k shards must carry ≥ K+extra symbols → per shard ⌈(K+extra)/k⌉
	c.perSh = (K + rqExtraSymbols + c.k - 1) / c.k
	shards := make([][]byte, n)
	for i := range shards {
		shards[i] = make([]byte, 0, c.perSh*c.symbol)
	}
	for pos := 0; pos < c.perSh; pos++ {
		for sh := 0; sh < n; sh++ {
			esi := uint32(pos*n + sh)
			shards[sh] = append(shards[sh], enc.GenSymbol(esi)...)
		}
	}
	return shards, nil
}

func (c *rqCodec) Rebuild(have [][]byte, size int64) ([]byte, error) {
	n := c.k + c.m
	rq := xraptorq.NewRaptorQ(uint32(c.symbol))
	dec, err := rq.CreateDecoder(uint32(size))
	if err != nil {
		return nil, fmt.Errorf("raptorq decoder: %w", err)
	}
	if c.perSh == 0 && len(have) > 0 {
		for _, h := range have {
			if h != nil {
				c.perSh = len(h) / c.symbol
				break
			}
		}
	}
	done := false
	added := 0
outer:
	for pos := 0; pos < c.perSh; pos++ {
		for sh := 0; sh < n; sh++ {
			if have[sh] == nil {
				continue
			}
			off := pos * c.symbol
			if off+c.symbol > len(have[sh]) {
				continue
			}
			esi := uint32(pos*n + sh)
			if done, err = dec.AddSymbol(esi, have[sh][off:off+c.symbol]); err != nil {
				return nil, fmt.Errorf("raptorq add symbol %d: %w", esi, err)
			}
			added++
			if done {
				break outer
			}
		}
	}
	out := make([]byte, size)
	ok, err := dec.DecodeInto(out)
	if err != nil {
		return nil, fmt.Errorf("raptorq decode after %d symbols: %w", added, err)
	}
	if !ok {
		return nil, fmt.Errorf("raptorq: %d symbols were not enough to decode", added)
	}
	return out, nil
}
