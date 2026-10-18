package crypto

// The streaming SSE codec (WP-R8-5, keystone K1): blob formats 0x03 and
// 0x04 (WP-R8-3 A.2). Every writer seals 0x04; 0x03 blobs still open.
//
// Before it, SSE-S3 (0x01/0x02) and SSE-C sealed an object with ONE AES-GCM
// over the whole plaintext, so every writer held the object (and its
// ciphertext) in memory and every reader read the whole blob before its
// first byte — hence a 256 MiB cap and 413s. This codec seals in segments:
// the STREAM construction of Hoang, Reyhanitabar, Rogaway and Vizár, "Online
// Authenticated-Encryption and its Nonce-Reuse Misuse-Resistance" (CRYPTO
// 2015), in the nonce-prefix shape Tink's AES-GCM-HKDF-STREAMING
// (developers.google.com/tink/streaming-aead/aes_gcm_hkdf_streaming) and
// age's payload (age-encryption.org/v1, "Payload") use:
//
//	header   = [format][kind u8][kind material][segment_size u32 BE][nonce_prefix 7]
//	format   = 0x04 (every writer since WP-R8-3 A.2) | 0x03 (before; opened only)
//	kind 0x01 SSE-S3: material = [key_version u32 BE][ML-KEM-768 ciphertext 1088]
//	kind 0x02 SSE-C:  material = [salt 16]
//	body     = segment_0 ‖ … ‖ segment_{n-1}
//	segment_i = AES-256-GCM(DEK, nonce_i, aad, plaintext_i)  (ct ‖ tag 16)
//	aad      = 0x04: header ‖ "vaultaire-sse-identity-v4" ‖ len‖tenant ‖ len‖bucket ‖ len‖key (len u32 BE)
//	           0x03: header
//	nonce_i  = nonce_prefix(7) ‖ i (u32 BE) ‖ last (0x01 on the final segment, else 0x00)
//
// The two formats differ only in their first byte and their AAD: a 0x04
// segment is authentic only as the object it was sealed for (StreamIdentity:
// tenant, bucket, key — the KEY, never the blob's address, so a retained
// version and a re-addressed blob still open). A 0x03 blob copied to another
// key's address under the same tenant decrypted and was served as that key
// (R8-10); a 0x04 one fails at its first segment. The format byte is inside
// the header, so rewriting 0x04 to 0x03 changes the DEK and the AAD: it fails
// too.
//
// Every segment but the last holds segment_size plaintext bytes; the last
// holds 0..segment_size (an empty plaintext is one empty final segment, so a
// blob is never a bare header), n = max(1, ceil(size / segment_size)), and
// the stored length is a pure function of the plaintext length:
// len(header) + size + n × 16 (StreamCiphertextLength).
//
// The DEK is fresh per object: SSE-S3 = HKDF-SHA256(the ML-KEM shared secret
// of a fresh encapsulation, info = "vaultaire-sse-stream-v3" ‖ header);
// SSE-C = HKDF-SHA256(the customer key, salt, info =
// "vaultaire-sse-c-stream-v3" ‖ header), the salt random per object, so one
// customer key over many objects never repeats a (key, nonce) pair. The
// nonce prefix is random too, as Tink's is, although with a fresh DEK per
// blob it only adds margin.
//
// Why each attack fails (each class has its test, stream_test.go):
//   - a segment modified, or its tag: GCM authentication;
//   - segments reordered, dropped or duplicated: segment i is only
//     authentic under nonce_i, which carries i — a segment moved to
//     position j ≠ i fails;
//   - truncated at a segment boundary: the new final segment was sealed
//     with last = 0 and is opened with last = 1; truncated inside a segment:
//     its tag fails; truncated to the header alone: no final segment at all
//     is an error, never an empty object;
//   - extended (bytes appended after the final segment): the final segment
//     is then read as a middle one (last = 0) and fails;
//   - the header changed (format, kind, key version, KEM ciphertext, segment
//     size, nonce prefix): the DEK is derived over the header and every
//     segment's AAD holds the header, so the first segment fails; a header
//     naming an absurd segment size is refused before any allocation;
//   - (0x04) the blob copied to another object's address — another key, another
//     bucket, another tenant's key store: every segment's AAD holds the
//     identity it was sealed for, so the first segment fails as the other
//     object (stream_identity_test.go).
//
// The opener verifies a segment before the first byte of its plaintext
// leaves it, and its constructors (SSEService.Open, OpenStreamRange,
// SSECOpen) verify the header and the first segment before they return: a
// caller that sends its status line after them never sends an unverified
// byte, and a failure later can only be a cut, never wrong bytes. At most one
// ciphertext and one plaintext segment are in memory per stream (≤ 2 MiB + the
// header at the default size), never the object.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// StreamFormat is the first byte of the streaming blob every writer
	// seals: 0x04, the identity-bound AAD (WP-R8-3 A.2).
	StreamFormat byte = 0x04
	// StreamFormatV3 is the format before it (header-only AAD): opened,
	// never written.
	StreamFormatV3 byte = 0x03
	// StreamKindSSES3 / StreamKindSSEC are the header's kind byte.
	StreamKindSSES3 byte = 0x01
	StreamKindSSEC  byte = 0x02
	// StreamSegmentSize is the plaintext bytes per segment every writer
	// uses. It is written into each header, so changing it never breaks a
	// reader of older blobs.
	StreamSegmentSize = 1 << 20

	streamTagSize         = 16
	streamNoncePrefixSize = 7
	streamSaltSize        = 16
	streamTail            = 4 + streamNoncePrefixSize
	// StreamHeaderSSES3 / StreamHeaderSSEC are the header lengths of the
	// two kinds (1105 and 29 bytes).
	StreamHeaderSSES3 = 2 + 4 + mlkem.CiphertextSize768 + streamTail
	StreamHeaderSSEC  = 2 + streamSaltSize + streamTail
	// MaxStreamHeader is the longest header: a ranged reader fetches this
	// many bytes first.
	MaxStreamHeader = StreamHeaderSSES3

	// A header's segment size outside these bounds is refused before any
	// buffer is sized from it.
	minStreamSegment = 16
	maxStreamSegment = 16 << 20
	// The segment index is a u32 in the nonce.
	maxStreamSegments = int64(1) << 32

	sseStreamInfo  = "vaultaire-sse-stream-v3"
	ssecStreamInfo = "vaultaire-sse-c-stream-v3"
	// streamIdentityLabel separates the header from the identity in a 0x04
	// AAD.
	streamIdentityLabel = "vaultaire-sse-identity-v4"
)

// IsStreamFormat reports whether b is the first byte of a streaming blob
// (0x03 or 0x04).
func IsStreamFormat(b byte) bool { return b == StreamFormat || b == StreamFormatV3 }

// StreamIdentity is the object a 0x04 blob is sealed for: the tenant, the
// bucket and the KEY (never the blob's address — a retained version, a
// re-addressed or re-encrypted blob of the key keeps its identity).
type StreamIdentity struct {
	Tenant, Bucket, Key string
}

// ErrStreamIdentity is a seal, or an open of a 0x04 blob, without a whole
// identity: a caller's bug, never the blob's.
var ErrStreamIdentity = errors.New("sse stream: the object identity (tenant, bucket, key) is incomplete")

func (id StreamIdentity) check() error {
	if id.Tenant == "" || id.Bucket == "" || id.Key == "" {
		return fmt.Errorf("%w: tenant %q, bucket %q, key %q", ErrStreamIdentity, id.Tenant, id.Bucket, id.Key)
	}
	return nil
}

// streamAAD is every segment's associated data: the header, and for a 0x04
// header the identity, each field length-prefixed (so "a"+"bc" ≠ "ab"+"c").
func streamAAD(header []byte, id StreamIdentity) []byte {
	if len(header) == 0 || header[0] != StreamFormat {
		return header
	}
	aad := make([]byte, 0, len(header)+len(streamIdentityLabel)+12+len(id.Tenant)+len(id.Bucket)+len(id.Key))
	aad = append(append(aad, header...), streamIdentityLabel...)
	for _, f := range []string{id.Tenant, id.Bucket, id.Key} {
		aad = binary.BigEndian.AppendUint32(aad, uint32(len(f))) // #nosec G115 -- names are far below 4 GiB
		aad = append(aad, f...)
	}
	return aad
}

// openAAD is the AAD a blob with header h opens under: a 0x04 blob needs the
// whole identity.
func openAAD(h *StreamHeader, id StreamIdentity) ([]byte, error) {
	if h.Format == StreamFormat {
		if err := id.check(); err != nil {
			return nil, err
		}
	}
	return streamAAD(h.Raw, id), nil
}

// ErrStreamCorrupt is a streaming blob that does not authenticate: a segment
// changed, moved, dropped, duplicated, truncated or appended to, or a header
// changed. It is never the caller's fault.
var ErrStreamCorrupt = errors.New("sse stream: blob does not authenticate")

// StreamSegments is the number of segments of a plaintext of size bytes.
func StreamSegments(size int64, segmentSize int) int64 {
	if size <= 0 {
		return 1
	}
	return (size + int64(segmentSize) - 1) / int64(segmentSize)
}

// StreamCiphertextLength is the stored length of a streaming blob: a pure
// function of the plaintext length, so a backend PUT can carry it as its
// Content-Length before the first byte is sealed.
func StreamCiphertextLength(headerLen, segmentSize int, size int64) int64 {
	if size < 0 {
		size = 0
	}
	return int64(headerLen) + size + StreamSegments(size, segmentSize)*streamTagSize
}

// StreamHeader is a parsed 0x03 / 0x04 header.
type StreamHeader struct {
	Raw         []byte
	Format      byte // 0x03 or 0x04
	Kind        byte
	KeyVersion  int    // SSE-S3
	KEMCipher   []byte // SSE-S3
	Salt        []byte // SSE-C
	SegmentSize int
	NoncePrefix []byte
}

// streamHeaderLen is the header length a kind byte implies.
func streamHeaderLen(kind byte) (int, error) {
	switch kind {
	case StreamKindSSES3:
		return StreamHeaderSSES3, nil
	case StreamKindSSEC:
		return StreamHeaderSSEC, nil
	}
	return 0, fmt.Errorf("%w: unknown kind 0x%02x", ErrStreamCorrupt, kind)
}

// ParseStreamHeader parses a header from b, which must start with it (it may
// hold more bytes: a ranged reader's first fetch). The header is only
// authenticated by the first segment that opens under it.
func ParseStreamHeader(b []byte) (*StreamHeader, error) {
	if len(b) < 2 || !IsStreamFormat(b[0]) {
		return nil, fmt.Errorf("%w: not a streaming (0x03/0x04) blob", ErrStreamCorrupt)
	}
	n, err := streamHeaderLen(b[1])
	if err != nil {
		return nil, err
	}
	if len(b) < n {
		return nil, fmt.Errorf("%w: header cut at %d of %d bytes", ErrStreamCorrupt, len(b), n)
	}
	raw := append([]byte(nil), b[:n]...)
	h := &StreamHeader{Raw: raw, Format: raw[0], Kind: raw[1]}
	off := 2
	switch h.Kind {
	case StreamKindSSES3:
		h.KeyVersion = int(binary.BigEndian.Uint32(raw[off:]))
		off += 4
		h.KEMCipher = raw[off : off+mlkem.CiphertextSize768]
		off += mlkem.CiphertextSize768
		if h.KeyVersion < 1 {
			return nil, fmt.Errorf("%w: key version %d", ErrStreamCorrupt, h.KeyVersion)
		}
	case StreamKindSSEC:
		h.Salt = raw[off : off+streamSaltSize]
		off += streamSaltSize
	}
	seg := binary.BigEndian.Uint32(raw[off:])
	off += 4
	if seg < minStreamSegment || seg > maxStreamSegment {
		return nil, fmt.Errorf("%w: segment size %d", ErrStreamCorrupt, seg)
	}
	h.SegmentSize = int(seg)
	h.NoncePrefix = raw[off : off+streamNoncePrefixSize]
	return h, nil
}

// ReadStreamHeader reads exactly one header from r (whose format byte has NOT
// been consumed).
func ReadStreamHeader(r io.Reader) (*StreamHeader, error) {
	var lead [2]byte
	if _, err := io.ReadFull(r, lead[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrStreamCorrupt, err)
	}
	n, err := streamHeaderLen(lead[1])
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	copy(buf, lead[:])
	if _, err := io.ReadFull(r, buf[2:]); err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrStreamCorrupt, err)
	}
	return ParseStreamHeader(buf)
}

// newStreamHeader builds a header of a kind around its material, with a
// fresh nonce prefix, in the format every writer seals.
func newStreamHeader(kind byte, material []byte, segmentSize int) ([]byte, error) {
	return newStreamHeaderFormat(StreamFormat, kind, material, segmentSize)
}

// newStreamHeaderFormat is newStreamHeader in a given format (tests make 0x03
// blobs with it).
func newStreamHeaderFormat(format, kind byte, material []byte, segmentSize int) ([]byte, error) {
	if segmentSize < minStreamSegment || segmentSize > maxStreamSegment {
		return nil, fmt.Errorf("sse stream: segment size %d out of bounds", segmentSize)
	}
	if !IsStreamFormat(format) {
		return nil, fmt.Errorf("sse stream: format 0x%02x", format)
	}
	h := make([]byte, 0, 2+len(material)+streamTail)
	h = append(h, format, kind)
	h = append(h, material...)
	h = binary.BigEndian.AppendUint32(h, uint32(segmentSize)) // #nosec G115 -- bounded above
	prefix := make([]byte, streamNoncePrefixSize)
	if _, err := io.ReadFull(rand.Reader, prefix); err != nil {
		return nil, fmt.Errorf("sse stream: nonce prefix: %w", err)
	}
	return append(h, prefix...), nil
}

// streamAEAD derives the DEK of a blob (HKDF-SHA256 over secret, salt and
// info ‖ header) and returns its AES-256-GCM.
func streamAEAD(secret, salt []byte, info string, header []byte) (cipher.AEAD, error) {
	inf := make([]byte, 0, len(info)+len(header))
	inf = append(append(inf, info...), header...)
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, inf), key); err != nil {
		return nil, fmt.Errorf("sse stream: HKDF: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sse stream: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sse stream: GCM: %w", err)
	}
	return gcm, nil
}

// streamNonce writes nonce_i into dst (12 bytes).
func streamNonce(dst, prefix []byte, i uint32, last bool) {
	copy(dst, prefix)
	binary.BigEndian.PutUint32(dst[streamNoncePrefixSize:], i)
	dst[11] = 0
	if last {
		dst[11] = 1
	}
}

// streamSealer is the sealing reader: the header, then each segment as soon
// as its plaintext has been read. It reads exactly size bytes from src and
// fails when src ends early or holds more.
type streamSealer struct {
	src       io.Reader
	aead      cipher.AEAD
	header    []byte
	aad       []byte
	prefix    []byte
	seg       int
	remaining int64
	n, i      int64
	pt, out   []byte
	pending   []byte
	nonce     [12]byte
	err       error
}

func newStreamSealer(src io.Reader, aead cipher.AEAD, header, aad []byte, segmentSize int, size int64) (*streamSealer, error) {
	if size < 0 {
		return nil, fmt.Errorf("sse stream: negative size %d", size)
	}
	n := StreamSegments(size, segmentSize)
	if n > maxStreamSegments {
		return nil, fmt.Errorf("sse stream: %d bytes is more than %d segments", size, maxStreamSegments)
	}
	return &streamSealer{
		src: src, aead: aead, header: header, aad: aad, prefix: header[len(header)-streamNoncePrefixSize:],
		seg: segmentSize, remaining: size, n: n,
		pt: make([]byte, segmentSize), out: make([]byte, 0, segmentSize+streamTagSize),
		pending: header,
	}, nil
}

func (s *streamSealer) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		if s.i == s.n {
			s.err = io.EOF
			continue
		}
		s.err = s.sealNext()
	}
	k := copy(p, s.pending)
	s.pending = s.pending[k:]
	return k, nil
}

// sealNext seals segment i into s.pending (or sets an error).
func (s *streamSealer) sealNext() error {
	want := int64(s.seg)
	if s.remaining < want {
		want = s.remaining
	}
	got, err := io.ReadFull(s.src, s.pt[:want])
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("sse stream: source ended %d bytes short: %w", s.remaining-int64(got), io.ErrUnexpectedEOF)
		}
		return err
	}
	s.remaining -= want
	last := s.i == s.n-1
	if last {
		// One more read: bytes past the size are an error, and so is an
		// error that came with the bytes that filled the buffer (ReadFull
		// drops it; a staged multipart part reports its failed check with
		// its last bytes) — only io.EOF ends the source cleanly.
		var extra [1]byte
		k, err := io.ReadFull(s.src, extra[:])
		if k > 0 {
			return errors.New("sse stream: source is longer than its declared size")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	streamNonce(s.nonce[:], s.prefix, uint32(s.i), last) // #nosec G115 -- n ≤ 2^32
	s.out = s.aead.Seal(s.out[:0], s.nonce[:], s.pt[:want], s.aad)
	s.pending = s.out
	s.i++
	if last {
		clear(s.pt)
	}
	return nil
}

// streamOpener is the opening reader over the segments of one blob (the
// header already read). In full mode (finalIndex < 0) it finds the final
// segment by reading one byte past each segment; in ranged mode it reads the
// segments first .. last of an object whose final segment is finalIndex.
type streamOpener struct {
	src        io.Reader
	aead       cipher.AEAD
	aad        []byte
	prefix     []byte
	seg        int
	i          int64
	finalIndex int64 // -1 = unknown (full mode)
	lastLen    int   // ranged: ciphertext length of the object's final segment
	stopAfter  int64 // ranged: the last index to read
	buf        []byte
	have       int
	pt         []byte
	pending    []byte
	nonce      [12]byte
	done       bool
	err        error
}

func newStreamOpener(src io.Reader, aead cipher.AEAD, h *StreamHeader, aad []byte) *streamOpener {
	return &streamOpener{
		src: src, aead: aead, aad: aad, prefix: h.NoncePrefix, seg: h.SegmentSize,
		finalIndex: -1, stopAfter: -1,
		buf: make([]byte, h.SegmentSize+streamTagSize+1),
		pt:  make([]byte, 0, h.SegmentSize),
	}
}

func (o *streamOpener) Read(p []byte) (int, error) {
	for len(o.pending) == 0 {
		if o.err != nil {
			return 0, o.err
		}
		if o.done {
			o.err = io.EOF
			continue
		}
		o.err = o.openNext()
	}
	k := copy(p, o.pending)
	o.pending = o.pending[k:]
	return k, nil
}

// prime opens the first segment now, so a constructor returns its failure
// before the caller has sent anything.
func (o *streamOpener) prime() error {
	if err := o.openNext(); err != nil {
		o.err = err
		return err
	}
	return nil
}

func (o *streamOpener) openNext() error {
	if o.i >= maxStreamSegments {
		return fmt.Errorf("%w: more than %d segments", ErrStreamCorrupt, maxStreamSegments)
	}
	var ct []byte
	var last bool
	if o.finalIndex < 0 {
		// Full mode: fill the buffer to one byte past a full segment.
		n, err := io.ReadFull(o.src, o.buf[o.have:])
		o.have += n
		switch {
		case err == nil:
			ct, last = o.buf[:o.seg+streamTagSize], false
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			ct, last = o.buf[:o.have], true
		default:
			return err
		}
	} else {
		last = o.i == o.finalIndex
		want := o.seg + streamTagSize
		if last {
			want = o.lastLen
		}
		if _, err := io.ReadFull(o.src, o.buf[:want]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: segment %d cut short", ErrStreamCorrupt, o.i)
			}
			return err
		}
		ct = o.buf[:want]
	}
	if len(ct) < streamTagSize {
		return fmt.Errorf("%w: segment %d cut short", ErrStreamCorrupt, o.i)
	}
	streamNonce(o.nonce[:], o.prefix, uint32(o.i), last) // #nosec G115 -- bounded above
	pt, err := o.aead.Open(o.pt[:0], o.nonce[:], ct, o.aad)
	if err != nil {
		return fmt.Errorf("%w: segment %d", ErrStreamCorrupt, o.i)
	}
	o.pt, o.pending = pt, pt
	if o.finalIndex < 0 && !last {
		// Carry the look-ahead byte to the front.
		o.buf[0] = o.buf[o.seg+streamTagSize]
		o.have = 1
	}
	if last || (o.stopAfter >= 0 && o.i == o.stopAfter) {
		o.done = true
	}
	o.i++
	return nil
}

// StreamRange is the ciphertext span that holds the plaintext bytes
// [Offset, Offset+Length) of a streaming blob.
type StreamRange struct {
	// CipherOffset / CipherLength: the bytes to fetch from the stored blob.
	CipherOffset, CipherLength int64
	// FirstSegment / LastSegment: the segments they hold.
	FirstSegment, LastSegment int64
	// Skip: plaintext bytes of the first segment before Offset.
	Skip int64
	// Length: plaintext bytes to return.
	Length int64
}

// PlanStreamRange maps a plaintext range of an object of size bytes onto its
// segments. length > 0 and offset+length ≤ size.
func PlanStreamRange(h *StreamHeader, size, offset, length int64) (StreamRange, error) {
	if offset < 0 || length <= 0 || offset+length > size {
		return StreamRange{}, fmt.Errorf("sse stream: range %d+%d outside %d bytes", offset, length, size)
	}
	s := int64(h.SegmentSize)
	first, last := offset/s, (offset+length-1)/s
	stride := s + streamTagSize
	off := int64(len(h.Raw)) + first*stride
	end := int64(len(h.Raw)) + last*stride + segmentPlainLen(size, s, last) + streamTagSize
	return StreamRange{CipherOffset: off, CipherLength: end - off, FirstSegment: first, LastSegment: last,
		Skip: offset - first*s, Length: length}, nil
}

// segmentPlainLen is the plaintext length of segment i of an object.
func segmentPlainLen(size, seg, i int64) int64 {
	if i < StreamSegments(size, int(seg))-1 {
		return seg
	}
	return size - i*seg
}

// openRange opens the segments of rg read from src (the bytes at
// rg.CipherOffset), verifies the first before returning, and yields exactly
// rg.Length plaintext bytes.
func openRange(src io.Reader, aead cipher.AEAD, h *StreamHeader, aad []byte, size int64, rg StreamRange) (io.Reader, error) {
	o := newStreamOpener(src, aead, h, aad)
	n := StreamSegments(size, h.SegmentSize)
	o.i, o.finalIndex, o.stopAfter = rg.FirstSegment, n-1, rg.LastSegment
	o.lastLen = int(segmentPlainLen(size, int64(h.SegmentSize), n-1)) + streamTagSize
	if err := o.prime(); err != nil {
		return nil, err
	}
	if rg.Skip > 0 {
		o.pending = o.pending[rg.Skip:]
	}
	return io.LimitReader(o, rg.Length), nil
}

// StreamRangeOpener opens planned ranges of ONE streaming blob under a key
// derived once — a parallel ranged read opens many ranges of the same blob
// (Prompt 8 Part B) and an SSE-C reader zeroes the customer's key as soon
// as the opener exists. Safe for concurrent use: AES-GCM's Open keeps no
// state between calls.
type StreamRangeOpener struct {
	aead cipher.AEAD
	h    *StreamHeader
	aad  []byte
	ssec bool
}

// Header is the blob's parsed header.
func (o *StreamRangeOpener) Header() *StreamHeader { return o.h }

// OpenRange opens plaintext [rg.Offset, +rg.Length) of the object (size
// bytes); src is the stored bytes [rg.CipherOffset, +rg.CipherLength). The
// first segment is verified before it returns; for SSE-C a first segment
// that does not open is ErrSSECKeyMismatch, as SSECOpenRange answers.
func (o *StreamRangeOpener) OpenRange(size int64, rg StreamRange, src io.Reader) (io.Reader, error) {
	r, err := openRange(src, o.aead, o.h, o.aad, size, rg)
	if err != nil && o.ssec && errors.Is(err, ErrStreamCorrupt) {
		return nil, ErrSSECKeyMismatch
	}
	return r, err
}
