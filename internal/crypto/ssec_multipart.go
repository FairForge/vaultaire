package crypto

// SSE-C multipart (WP-SSEC-1, Stage 1a Prompt 17 Part B; plan 5.14.8a): blob
// format 0x05.
//
// In S3, CreateMultipartUpload and every UploadPart carry the customer's
// key; CompleteMultipartUpload carries none, and the key is never stored. So
// a part is sealed when its key is in the request — at UploadPart, on the
// staging disk — as one complete 0x04 stream of its own (its own salt, its
// segments numbered from 0, its final segment flagged last) whose AAD binds
// the object's identity AND the part number (StreamIdentity.Part: part 3's
// stream cannot be presented as part 2). Complete needs no key: it writes
// the sealed parts, in order, behind one self-describing header:
//
//	blob   = [0x05][kind 0x02 SSE-C][part count u32 BE]
//	         [per part: plaintext length u64 BE, sealed length u64 BE]
//	         stream_1 ‖ … ‖ stream_n          (each a 0x04 SSE-C stream)
//
// Nothing of the layout lives in the database: the head row records the
// object as an SSE-C object with the parts' -N ETag and the key's MD5, as
// a plain multipart object does. Every sealed length in the table is the
// pure function of its plaintext length (SSECSealedLength), and a table
// that says otherwise — or names 0 or more than 10,000 parts, or does not
// fit the bytes read — is refused before any buffer is sized from it.
//
// Reads: a whole GET opens the parts in order (MultipartOpener), the first
// part's header and first segment verified before the constructor returns,
// each part's plaintext checked against its table length as it ends; a
// Range is planned onto the parts it spans (PlanMultipartRange) and each
// span is read as a range of that part's stream (its own 29-byte header,
// then the segments — StreamRangeOpener under the part's identity). The key
// is needed for every part a read opens: MultipartOpener keeps its own copy
// until its last part is open, then clears it.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// MultipartFormat is the first byte of an SSE-C multipart blob.
	MultipartFormat byte = 0x05
	// MaxMultipartParts is the most parts a 0x05 header may name (S3's
	// limit on an upload).
	MaxMultipartParts = 10000

	multipartHeaderBase = 1 + 1 + 4 // format, kind, count
	multipartPartEntry  = 8 + 8     // plaintext length, sealed length
)

// ErrMultipartHeader: a 0x05 header that does not describe a blob —
// refused before any allocation sized from it.
var ErrMultipartHeader = errors.New("sse multipart: header does not describe a blob")

// MultipartPartSpan is one part of a 0x05 blob as its table records it.
type MultipartPartSpan struct {
	Plain, Sealed int64
}

// MultipartHeader is a parsed 0x05 header.
type MultipartHeader struct {
	Raw   []byte
	Kind  byte
	Parts []MultipartPartSpan
}

// MultipartHeaderLen is the length of a 0x05 header naming n parts.
func MultipartHeaderLen(n int) int { return multipartHeaderBase + n*multipartPartEntry }

// SSECMultipartSealedLength is the stored length of a 0x05 blob whose parts
// have the given plaintext lengths: a pure function of them, so Complete's
// backend PUT can carry it before the first byte.
func SSECMultipartSealedLength(plains []int64) int64 {
	n := int64(MultipartHeaderLen(len(plains)))
	for _, p := range plains {
		n += SSECSealedLength(p)
	}
	return n
}

// NewSSECMultipartHeader builds the 0x05 header of an SSE-C multipart blob
// whose parts have the given plaintext lengths (1..MaxMultipartParts, each
// ≥ 0).
func NewSSECMultipartHeader(plains []int64) ([]byte, error) {
	if len(plains) == 0 || len(plains) > MaxMultipartParts {
		return nil, fmt.Errorf("%w: %d parts", ErrMultipartHeader, len(plains))
	}
	h := make([]byte, 0, MultipartHeaderLen(len(plains)))
	h = append(h, MultipartFormat, StreamKindSSEC)
	h = binary.BigEndian.AppendUint32(h, uint32(len(plains))) // #nosec G115 -- bounded above
	for _, p := range plains {
		if p < 0 {
			return nil, fmt.Errorf("%w: negative part length %d", ErrMultipartHeader, p)
		}
		h = binary.BigEndian.AppendUint64(h, uint64(p))
		h = binary.BigEndian.AppendUint64(h, uint64(SSECSealedLength(p)))
	}
	return h, nil
}

// ParseMultipartHeader parses a 0x05 header from b, which must start with it
// and hold the whole part table (it may hold more: a ranged reader's first
// fetch). The table is checked before anything is sized from it: 1..10,000
// parts, every sealed length the function of its plaintext length, no
// overflow.
func ParseMultipartHeader(b []byte) (*MultipartHeader, error) {
	if len(b) < multipartHeaderBase || b[0] != MultipartFormat {
		return nil, fmt.Errorf("%w: not a 0x05 blob", ErrMultipartHeader)
	}
	if b[1] != StreamKindSSEC {
		return nil, fmt.Errorf("%w: kind 0x%02x is not SSE-C", ErrMultipartHeader, b[1])
	}
	count := binary.BigEndian.Uint32(b[2:6])
	if count == 0 || count > MaxMultipartParts {
		return nil, fmt.Errorf("%w: %d parts", ErrMultipartHeader, count)
	}
	n := MultipartHeaderLen(int(count))
	if len(b) < n {
		return nil, fmt.Errorf("%w: table cut at %d of %d bytes", ErrMultipartHeader, len(b), n)
	}
	h := &MultipartHeader{Raw: append([]byte(nil), b[:n]...), Kind: b[1], Parts: make([]MultipartPartSpan, count)}
	var total int64
	for i := range h.Parts {
		off := multipartHeaderBase + i*multipartPartEntry
		plain := binary.BigEndian.Uint64(h.Raw[off:])
		sealed := binary.BigEndian.Uint64(h.Raw[off+8:])
		if plain > 1<<50 || int64(sealed) != SSECSealedLength(int64(plain)) { // #nosec G115 -- bounded first
			return nil, fmt.Errorf("%w: part %d: %d plaintext bytes, %d sealed", ErrMultipartHeader, i+1, plain, sealed)
		}
		h.Parts[i] = MultipartPartSpan{Plain: int64(plain), Sealed: int64(sealed)} // #nosec G115 -- bounded above
		total += int64(plain)                                                      // #nosec G115 -- bounded above
		if total > 1<<50 {
			return nil, fmt.Errorf("%w: more than 1 PiB", ErrMultipartHeader)
		}
	}
	return h, nil
}

// ReadMultipartHeader reads exactly one 0x05 header from r (the format
// byte NOT consumed): the fixed lead first, then the table the count
// implies — a count outside the bound is refused before the table is sized.
func ReadMultipartHeader(r io.Reader) (*MultipartHeader, error) {
	var lead [multipartHeaderBase]byte
	if _, err := io.ReadFull(r, lead[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrMultipartHeader, err)
	}
	if lead[0] != MultipartFormat || lead[1] != StreamKindSSEC {
		return nil, fmt.Errorf("%w: not a 0x05 SSE-C blob", ErrMultipartHeader)
	}
	count := binary.BigEndian.Uint32(lead[2:6])
	if count == 0 || count > MaxMultipartParts {
		return nil, fmt.Errorf("%w: %d parts", ErrMultipartHeader, count)
	}
	buf := make([]byte, MultipartHeaderLen(int(count)))
	copy(buf, lead[:])
	if _, err := io.ReadFull(r, buf[multipartHeaderBase:]); err != nil {
		return nil, fmt.Errorf("%w: table: %w", ErrMultipartHeader, err)
	}
	return ParseMultipartHeader(buf)
}

// PlainSize is the object's plaintext size.
func (h *MultipartHeader) PlainSize() int64 {
	var n int64
	for _, p := range h.Parts {
		n += p.Plain
	}
	return n
}

// SealedSize is the blob's stored length.
func (h *MultipartHeader) SealedSize() int64 {
	n := int64(len(h.Raw))
	for _, p := range h.Parts {
		n += p.Sealed
	}
	return n
}

// StreamOffset is where part i's (0-based) stream starts in the stored
// blob.
func (h *MultipartHeader) StreamOffset(i int) int64 {
	off := int64(len(h.Raw))
	for _, p := range h.Parts[:i] {
		off += p.Sealed
	}
	return off
}

// MultipartRangeSpan is the piece of one part a plaintext range covers:
// bytes [Offset, Offset+Length) of part Part (1-based), whose stream starts
// at StreamOffset in the stored blob.
type MultipartRangeSpan struct {
	Part         int
	StreamOffset int64
	Offset       int64
	Length       int64
}

// PlanMultipartRange maps the plaintext range [offset, offset+length) of a
// 0x05 object onto the parts it spans, in order. length > 0 and the range
// inside the object.
func PlanMultipartRange(h *MultipartHeader, offset, length int64) ([]MultipartRangeSpan, error) {
	size := h.PlainSize()
	if offset < 0 || length <= 0 || offset+length > size {
		return nil, fmt.Errorf("sse multipart: range %d+%d outside %d bytes", offset, length, size)
	}
	var spans []MultipartRangeSpan
	var plainOff, streamOff int64 = 0, int64(len(h.Raw))
	end := offset + length
	for i, p := range h.Parts {
		partEnd := plainOff + p.Plain
		if partEnd > offset && plainOff < end && p.Plain > 0 {
			from := max(offset, plainOff)
			to := min(end, partEnd)
			spans = append(spans, MultipartRangeSpan{Part: i + 1, StreamOffset: streamOff, Offset: from - plainOff, Length: to - from})
		}
		plainOff, streamOff = partEnd, streamOff+p.Sealed
		if plainOff >= end {
			break
		}
	}
	return spans, nil
}

// MultipartOpener opens a whole 0x05 blob part by part: the header's table
// verified, then part 1's stream (its first segment verified before the
// constructor returns), each further part's stream as the previous ends,
// each part's plaintext checked against the table. A part that does not
// open under the key — a wrong key, a part moved, a blob copied to another
// object — is ErrSSECKeyMismatch; a later segment that fails is
// ErrStreamCorrupt from Read, never wrong bytes.
type MultipartOpener struct {
	h    *MultipartHeader
	key  []byte // the opener's own copy; cleared once the last part is open
	id   StreamIdentity
	src  io.Reader
	i    int // the part being read (0-based)
	cur  io.Reader
	left int64 // plaintext bytes of the current part still to deliver
	err  error
}

// SSECMultipartOpen opens a whole 0x05 blob read from src as the object id
// under the customer's key. The header and part 1's first segment are
// verified before it returns. The caller may zero key when it returns: the
// opener keeps its own copy, cleared when its last part is open (or Close).
func SSECMultipartOpen(key []byte, id StreamIdentity, src io.Reader) (*MultipartOpener, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("ssec: key must be 32 bytes, got %d", len(key))
	}
	h, err := ReadMultipartHeader(src)
	if err != nil {
		return nil, err
	}
	o := &MultipartOpener{h: h, key: append([]byte(nil), key...), id: id, src: src}
	if err := o.openPart(0); err != nil {
		o.Close()
		return nil, err
	}
	return o, nil
}

// Header is the blob's parsed 0x05 header.
func (o *MultipartOpener) Header() *MultipartHeader { return o.h }

// openPart opens part i's stream (verifying its header and first segment).
func (o *MultipartOpener) openPart(i int) error {
	p := o.h.Parts[i]
	pid := o.id
	pid.Part = i + 1
	r, err := SSECOpen(o.key, pid, io.LimitReader(o.src, p.Sealed))
	if err != nil {
		if errors.Is(err, ErrSSECKeyMismatch) || errors.Is(err, ErrStreamCorrupt) {
			return fmt.Errorf("%w: part %d", ErrSSECKeyMismatch, i+1)
		}
		return fmt.Errorf("sse multipart: part %d: %w", i+1, err)
	}
	o.i, o.cur, o.left = i, r, p.Plain
	if i == len(o.h.Parts)-1 {
		clear(o.key)
		o.key = nil
	}
	return nil
}

// Read delivers the parts' plaintext in order.
func (o *MultipartOpener) Read(b []byte) (int, error) {
	for {
		if o.err != nil {
			return 0, o.err
		}
		if o.left == 0 {
			// The part must end exactly here: one more read of its stream
			// (bytes past its table length are not the part).
			var extra [1]byte
			if k, err := o.cur.Read(extra[:]); k > 0 || (err != nil && !errors.Is(err, io.EOF)) {
				if k > 0 {
					o.err = fmt.Errorf("%w: part %d is longer than its table length", ErrStreamCorrupt, o.i+1)
				} else {
					o.err = err
				}
				return 0, o.err
			}
			if o.i == len(o.h.Parts)-1 {
				o.err = io.EOF
				return 0, io.EOF
			}
			if err := o.openPart(o.i + 1); err != nil {
				o.err = err
				return 0, err
			}
			continue
		}
		if int64(len(b)) > o.left {
			b = b[:o.left]
		}
		n, err := o.cur.Read(b)
		o.left -= int64(n)
		switch {
		case errors.Is(err, io.EOF) && o.left > 0:
			o.err = fmt.Errorf("%w: part %d ended %d bytes short of its table length", ErrStreamCorrupt, o.i+1, o.left)
			return n, o.err
		case err != nil && !errors.Is(err, io.EOF):
			o.err = err
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
}

// Close clears the opener's key copy.
func (o *MultipartOpener) Close() {
	clear(o.key)
	o.key = nil
}
