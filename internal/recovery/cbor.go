package recovery

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The subset of CBOR (RFC 8949) the Juicebox wire uses, exactly as
// ciborium writes it (docs/reviews/K5-PROBE.md § The wire): unsigned
// integers, byte strings, text strings and maps, every length in its
// shortest form, definite lengths only. Nothing else is accepted.

const (
	cborUint  = 0
	cborBytes = 2
	cborText  = 3
	cborMap   = 5
)

// maxCBORDepth bounds nesting; the deepest message (a ClientRequest
// carrying a handshake) is 4 levels.
const maxCBORDepth = 8

// ErrDecode is returned for any input that is not a well-formed message of
// the expected shape.
var ErrDecode = errors.New("recovery: malformed message")

func appendHead(b []byte, major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return append(b, m|byte(n))
	case n <= 0xff:
		return append(b, m|24, byte(n))
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16(append(b, m|25), uint16(n))
	case n <= 0xffffffff:
		return binary.BigEndian.AppendUint32(append(b, m|26), uint32(n))
	default:
		return binary.BigEndian.AppendUint64(append(b, m|27), n)
	}
}

func appendUint(b []byte, n uint64) []byte { return appendHead(b, cborUint, n) }
func appendBytes(b, v []byte) []byte       { return append(appendHead(b, cborBytes, uint64(len(v))), v...) }
func appendText(b []byte, s string) []byte {
	return append(appendHead(b, cborText, uint64(len(s))), s...)
}
func appendMapHead(b []byte, n int) []byte       { return appendHead(b, cborMap, uint64(n)) }
func appendVariant(b []byte, name string) []byte { return appendText(appendMapHead(b, 1), name) }

// item is one decoded CBOR data item.
type item struct {
	major byte
	n     uint64 // the value of a uint
	raw   []byte // the content of a byte or text string
	pairs []pair // a map, in wire order
}

type pair struct {
	key string
	val item
}

func decodeCBOR(b []byte) (item, error) {
	it, rest, err := decodeItem(b, 0)
	if err != nil {
		return item{}, err
	}
	if len(rest) != 0 {
		return item{}, fmt.Errorf("%w: %d trailing bytes", ErrDecode, len(rest))
	}
	return it, nil
}

func decodeItem(b []byte, depth int) (item, []byte, error) {
	if depth > maxCBORDepth {
		return item{}, nil, fmt.Errorf("%w: nested too deep", ErrDecode)
	}
	if len(b) == 0 {
		return item{}, nil, fmt.Errorf("%w: truncated", ErrDecode)
	}
	major, ai := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var n uint64
	switch {
	case ai < 24:
		n = uint64(ai)
	case ai == 24 && len(b) >= 1:
		n, b = uint64(b[0]), b[1:]
		if n < 24 {
			return item{}, nil, fmt.Errorf("%w: non-shortest length", ErrDecode)
		}
	case ai == 25 && len(b) >= 2:
		n, b = uint64(binary.BigEndian.Uint16(b)), b[2:]
		if n <= 0xff {
			return item{}, nil, fmt.Errorf("%w: non-shortest length", ErrDecode)
		}
	case ai == 26 && len(b) >= 4:
		n, b = uint64(binary.BigEndian.Uint32(b)), b[4:]
		if n <= 0xffff {
			return item{}, nil, fmt.Errorf("%w: non-shortest length", ErrDecode)
		}
	case ai == 27 && len(b) >= 8:
		n, b = binary.BigEndian.Uint64(b), b[8:]
		if n <= 0xffffffff {
			return item{}, nil, fmt.Errorf("%w: non-shortest length", ErrDecode)
		}
	default:
		return item{}, nil, fmt.Errorf("%w: unsupported head 0x%02x", ErrDecode, major<<5|ai)
	}
	switch major {
	case cborUint:
		return item{major: major, n: n}, b, nil
	case cborBytes, cborText:
		if uint64(len(b)) < n {
			return item{}, nil, fmt.Errorf("%w: truncated string", ErrDecode)
		}
		return item{major: major, raw: b[:n]}, b[n:], nil
	case cborMap:
		if n > uint64(len(b)) { // every pair is at least two bytes
			return item{}, nil, fmt.Errorf("%w: truncated map", ErrDecode)
		}
		it := item{major: major, pairs: make([]pair, 0, n)}
		for i := uint64(0); i < n; i++ {
			k, rest, err := decodeItem(b, depth+1)
			if err != nil {
				return item{}, nil, err
			}
			if k.major != cborText {
				return item{}, nil, fmt.Errorf("%w: map key is not text", ErrDecode)
			}
			v, rest, err := decodeItem(rest, depth+1)
			if err != nil {
				return item{}, nil, err
			}
			for _, p := range it.pairs {
				if p.key == string(k.raw) {
					return item{}, nil, fmt.Errorf("%w: duplicate key %q", ErrDecode, k.raw)
				}
			}
			it.pairs = append(it.pairs, pair{key: string(k.raw), val: v})
			b = rest
		}
		return it, b, nil
	default:
		return item{}, nil, fmt.Errorf("%w: unsupported major type %d", ErrDecode, major)
	}
}

// fields reads a map with exactly the named keys (any order) into a
// lookup; a missing or unknown key is an error.
func (it item) fields(names ...string) (map[string]item, error) {
	if it.major != cborMap || len(it.pairs) != len(names) {
		return nil, fmt.Errorf("%w: want a map of %d fields", ErrDecode, len(names))
	}
	out := make(map[string]item, len(names))
	for _, p := range it.pairs {
		out[p.key] = p.val
	}
	for _, n := range names {
		if _, ok := out[n]; !ok {
			return nil, fmt.Errorf("%w: missing field %q", ErrDecode, n)
		}
	}
	return out, nil
}

// variant reads an enum value: a bare text string (a unit variant, data
// is the zero item) or a one-entry map {name: data}.
func (it item) variant() (string, item, bool, error) {
	switch {
	case it.major == cborText:
		return string(it.raw), item{}, false, nil
	case it.major == cborMap && len(it.pairs) == 1:
		return it.pairs[0].key, it.pairs[0].val, true, nil
	default:
		return "", item{}, false, fmt.Errorf("%w: want an enum variant", ErrDecode)
	}
}

func (it item) uint(bits int) (uint64, error) {
	if it.major != cborUint || (bits < 64 && it.n >= 1<<bits) {
		return 0, fmt.Errorf("%w: want a u%d", ErrDecode, bits)
	}
	return it.n, nil
}

func (it item) bytesN(dst []byte) error {
	if it.major != cborBytes || len(it.raw) != len(dst) {
		return fmt.Errorf("%w: want %d bytes", ErrDecode, len(dst))
	}
	copy(dst, it.raw)
	return nil
}

func (it item) bytesAny() ([]byte, error) {
	if it.major != cborBytes {
		return nil, fmt.Errorf("%w: want bytes", ErrDecode)
	}
	out := make([]byte, len(it.raw))
	copy(out, it.raw)
	return out, nil
}

func (it item) text() (string, error) {
	if it.major != cborText {
		return "", fmt.Errorf("%w: want text", ErrDecode)
	}
	return string(it.raw), nil
}
