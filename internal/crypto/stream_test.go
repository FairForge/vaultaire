package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The codec without a key store: a fixed secret, the SSE-C kind's header
// (the codec is the same for both kinds; SSE-S3 has its own tests below).
var codecSecret = bytes.Repeat([]byte{7}, 32)

func codecSeal(t *testing.T, pt []byte, seg int) []byte {
	t.Helper()
	salt := make([]byte, streamSaltSize)
	_, _ = rand.Read(salt)
	header, err := newStreamHeader(StreamKindSSEC, salt, seg)
	require.NoError(t, err)
	aead, err := streamAEAD(codecSecret, salt, ssecStreamInfo, header)
	require.NoError(t, err)
	s, err := newStreamSealer(bytes.NewReader(pt), aead, header, seg, int64(len(pt)))
	require.NoError(t, err)
	blob, err := io.ReadAll(s)
	require.NoError(t, err)
	return blob
}

// codecOpen opens a blob; the error is the constructor's (header or first
// segment) when r is nil.
func codecOpen(blob []byte) (io.Reader, error) {
	src := bytes.NewReader(blob)
	h, err := ReadStreamHeader(src)
	if err != nil {
		return nil, err
	}
	aead, err := streamAEAD(codecSecret, h.Salt, ssecStreamInfo, h.Raw)
	if err != nil {
		return nil, err
	}
	o := newStreamOpener(src, aead, h)
	if err := o.prime(); err != nil {
		return nil, err
	}
	return o, nil
}

// readUntilErr is what a client would have received: every byte before the
// error.
func readUntilErr(r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	_, err := io.Copy(&out, r)
	return out.Bytes(), err
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestStream_RoundTrip_SegmentBoundaries(t *testing.T) {
	const s = 64
	for _, size := range []int{0, 1, s - 1, s, s + 1, 3 * s, 3*s + 17} {
		pt := randBytes(size)
		blob := codecSeal(t, pt, s)
		assert.Equal(t, StreamCiphertextLength(StreamHeaderSSEC, s, int64(size)), int64(len(blob)),
			"the length function at %d bytes", size)
		r, err := codecOpen(blob)
		require.NoError(t, err, "size %d", size)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, pt, got, "size %d", size)
	}
}

func TestStream_LengthFunction(t *testing.T) {
	// header + size + n × 16, n = max(1, ceil(size/S)).
	assert.Equal(t, int64(1105+16), StreamCiphertextLength(StreamHeaderSSES3, 1<<20, 0))
	assert.Equal(t, int64(1105+1+16), StreamCiphertextLength(StreamHeaderSSES3, 1<<20, 1))
	assert.Equal(t, int64(1105+(1<<20)+16), StreamCiphertextLength(StreamHeaderSSES3, 1<<20, 1<<20))
	assert.Equal(t, int64(1105+(1<<20)+1+32), StreamCiphertextLength(StreamHeaderSSES3, 1<<20, 1<<20+1))
	assert.Equal(t, int64(1105+(1<<30)+1024*16), StreamCiphertextLength(StreamHeaderSSES3, 1<<20, 1<<30))
	assert.Equal(t, 1105, StreamHeaderSSES3)
	assert.Equal(t, 29, StreamHeaderSSEC)
}

// A source that ends early or runs long is an error, never a short blob.
func TestStream_SealerHoldsTheSourceToItsSize(t *testing.T) {
	salt := randBytes(streamSaltSize)
	header, err := newStreamHeader(StreamKindSSEC, salt, 64)
	require.NoError(t, err)
	aead, err := streamAEAD(codecSecret, salt, ssecStreamInfo, header)
	require.NoError(t, err)

	short, err := newStreamSealer(bytes.NewReader(randBytes(100)), aead, header, 64, 200)
	require.NoError(t, err)
	_, err = io.ReadAll(short)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)

	long, err := newStreamSealer(bytes.NewReader(randBytes(300)), aead, header, 64, 200)
	require.NoError(t, err)
	_, err = io.ReadAll(long)
	assert.ErrorContains(t, err, "longer than its declared size")

	// An error that comes with the last bytes (a staged part's failed
	// check) is not lost behind a full buffer.
	bad := errors.New("part check failed")
	late, err := newStreamSealer(&lateErrReader{b: randBytes(200), err: bad}, aead, header, 64, 200)
	require.NoError(t, err)
	_, err = io.ReadAll(late)
	assert.ErrorIs(t, err, bad)
}

// lateErrReader returns its bytes with err on the read that delivers the
// last of them, then err again.
type lateErrReader struct {
	b   []byte
	err error
}

func (l *lateErrReader) Read(p []byte) (int, error) {
	n := copy(p, l.b)
	l.b = l.b[n:]
	if len(l.b) == 0 {
		return n, l.err
	}
	return n, nil
}

// A 1 GiB stream in -short: equal end to end, and the allocations of
// sealing and opening it are a few segments, never the object.
func TestStream_OneGiB_BoundedMemory(t *testing.T) {
	const size = 1 << 30
	const seg = 1 << 20
	src := io.LimitReader(&patternReader{}, size)
	inHash := sha256.New()
	salt := randBytes(streamSaltSize)
	header, err := newStreamHeader(StreamKindSSEC, salt, seg)
	require.NoError(t, err)
	aead, err := streamAEAD(codecSecret, salt, ssecStreamInfo, header)
	require.NoError(t, err)

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	sealer, err := newStreamSealer(io.TeeReader(src, inHash), aead, header, seg, size)
	require.NoError(t, err)
	pr, pw := io.Pipe()
	written := make(chan int64, 1)
	go func() {
		n, err := io.Copy(pw, sealer)
		pw.CloseWithError(err)
		written <- n
	}()
	h, err := ReadStreamHeader(pr)
	require.NoError(t, err)
	oa, err := streamAEAD(codecSecret, h.Salt, ssecStreamInfo, h.Raw)
	require.NoError(t, err)
	o := newStreamOpener(pr, oa, h)
	require.NoError(t, o.prime())
	outHash := sha256.New()
	n, err := io.Copy(outHash, o)
	require.NoError(t, err)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	assert.Equal(t, int64(size), n)
	assert.Equal(t, inHash.Sum(nil), outHash.Sum(nil))
	assert.Equal(t, StreamCiphertextLength(StreamHeaderSSEC, seg, size), <-written, "the length function")
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(32<<20),
		"sealing and opening 1 GiB allocates a few segments, not the object")
}

// patternReader repeats a 64 KiB random block (copy, not a byte loop: -race).
type patternReader struct {
	block []byte
	off   int
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.block == nil {
		p.block = randBytes(64 << 10)
	}
	n := 0
	for n < len(b) {
		k := copy(b[n:], p.block[p.off:])
		n += k
		p.off = (p.off + k) % len(p.block)
	}
	return n, nil
}

// The tamper classes (stream.go's file comment). A blob of 5 segments of 64
// bytes, the last 44: segment i sits at 29 + i × 80.
const tamperSeg = 64

func tamperFixture(t *testing.T) (pt, blob []byte) {
	pt = randBytes(4*tamperSeg + 44)
	return pt, codecSeal(t, pt, tamperSeg)
}

func segAt(i int) (int, int) {
	start := StreamHeaderSSEC + i*(tamperSeg+streamTagSize)
	return start, start + tamperSeg + streamTagSize
}

// assertCutAt: the opener returned exactly the plaintext of the segments
// before `at`, then ErrStreamCorrupt — no byte of a bad segment got out.
func assertCutAt(t *testing.T, pt, blob []byte, at int) {
	t.Helper()
	r, err := codecOpen(blob)
	if at == 0 {
		require.ErrorIs(t, err, ErrStreamCorrupt, "the constructor refuses a bad first segment")
		return
	}
	require.NoError(t, err)
	got, err := readUntilErr(r)
	require.ErrorIs(t, err, ErrStreamCorrupt)
	assert.Equal(t, pt[:at*tamperSeg], got, "only verified segments were returned")
}

func TestStream_Tamper_SegmentModified(t *testing.T) {
	pt, blob := tamperFixture(t)
	s, _ := segAt(2)
	blob[s+5] ^= 1
	assertCutAt(t, pt, blob, 2)
}

func TestStream_Tamper_TagModified(t *testing.T) {
	pt, blob := tamperFixture(t)
	_, e := segAt(1)
	blob[e-1] ^= 0x80
	assertCutAt(t, pt, blob, 1)
}

func TestStream_Tamper_SegmentsReordered(t *testing.T) {
	pt, blob := tamperFixture(t)
	s1, e1 := segAt(1)
	s2, e2 := segAt(2)
	seg1 := append([]byte(nil), blob[s1:e1]...)
	copy(blob[s1:e1], blob[s2:e2])
	copy(blob[s2:e2], seg1)
	assertCutAt(t, pt, blob, 1)
}

func TestStream_Tamper_SegmentDropped(t *testing.T) {
	pt, blob := tamperFixture(t)
	s, e := segAt(2)
	cut := append(append([]byte(nil), blob[:s]...), blob[e:]...)
	assertCutAt(t, pt, cut, 2)
}

func TestStream_Tamper_SegmentDuplicated(t *testing.T) {
	pt, blob := tamperFixture(t)
	s, e := segAt(1)
	dup := append(append(append([]byte(nil), blob[:e]...), blob[s:e]...), blob[e:]...)
	assertCutAt(t, pt, dup, 2)
}

func TestStream_Tamper_TruncatedAtASegmentBoundary(t *testing.T) {
	pt, blob := tamperFixture(t)
	s, _ := segAt(4) // drop the final segment: segment 3 is read as final
	assertCutAt(t, pt, blob[:s], 3)
}

func TestStream_Tamper_TruncatedInsideTheFinalSegment(t *testing.T) {
	pt, blob := tamperFixture(t)
	assertCutAt(t, pt, blob[:len(blob)-3], 4)
}

func TestStream_Tamper_TruncatedToTheHeader(t *testing.T) {
	pt, blob := tamperFixture(t)
	assertCutAt(t, pt, blob[:StreamHeaderSSEC], 0)
}

func TestStream_Tamper_Extended(t *testing.T) {
	pt, blob := tamperFixture(t)
	assertCutAt(t, pt, append(append([]byte(nil), blob...), 0), 4)
	s, e := segAt(4)
	assertCutAt(t, pt, append(append([]byte(nil), blob...), blob[s:e-20]...), 4)
}

func TestStream_Tamper_HeaderFields(t *testing.T) {
	cases := map[string]func(b []byte){
		"salt":           func(b []byte) { b[2] ^= 1 },
		"segment size":   func(b []byte) { binary.BigEndian.PutUint32(b[2+streamSaltSize:], 65) },
		"nonce prefix":   func(b []byte) { b[StreamHeaderSSEC-1] ^= 1 },
		"kind":           func(b []byte) { b[1] = StreamKindSSES3 },
		"format byte":    func(b []byte) { b[0] = 0x02 },
		"absurd segment": func(b []byte) { binary.BigEndian.PutUint32(b[2+streamSaltSize:], 0xFFFFFFFF) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pt, blob := tamperFixture(t)
			mutate(blob)
			assertCutAt(t, pt, blob, 0)
		})
	}
}

// Ranged open: the segments a range spans, verified, the leading bytes of
// the first discarded.
func TestStream_RangeAtTheBoundaries(t *testing.T) {
	const s = 64
	pt := randBytes(4*s + 44)
	blob := codecSeal(t, pt, s)
	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	size := int64(len(pt))
	for name, rg := range map[string][2]int64{
		"first byte":          {0, 1},
		"crossing a boundary": {s - 3, 6},
		"inside one segment":  {s + 5, 10},
		"last byte":           {size - 1, 1},
		"whole last segment":  {4 * s, 44},
		"whole object":        {0, size},
		"two whole segments":  {s, 2 * s},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := PlanStreamRange(h, size, rg[0], rg[1])
			require.NoError(t, err)
			aead, err := streamAEAD(codecSecret, h.Salt, ssecStreamInfo, h.Raw)
			require.NoError(t, err)
			span := blob[plan.CipherOffset : plan.CipherOffset+plan.CipherLength]
			r, err := openRange(bytes.NewReader(span), aead, h, size, plan)
			require.NoError(t, err)
			got, err := io.ReadAll(r)
			require.NoError(t, err)
			assert.Equal(t, pt[rg[0]:rg[0]+rg[1]], got)
			assert.Equal(t, plan.LastSegment-plan.FirstSegment+1,
				(plan.CipherLength+s+streamTagSize-1)/(s+streamTagSize), "only the spanned segments are fetched")
		})
	}
}

func TestStream_Range_TamperedOrShortSpanFails(t *testing.T) {
	const s = 64
	pt := randBytes(4*s + 44)
	blob := codecSeal(t, pt, s)
	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	size := int64(len(pt))
	plan, err := PlanStreamRange(h, size, s+1, 2*s)
	require.NoError(t, err)
	aead, err := streamAEAD(codecSecret, h.Salt, ssecStreamInfo, h.Raw)
	require.NoError(t, err)
	span := append([]byte(nil), blob[plan.CipherOffset:plan.CipherOffset+plan.CipherLength]...)

	// The span of other segments (shifted by one): the first fails.
	shifted := blob[plan.CipherOffset+s+streamTagSize : plan.CipherOffset+plan.CipherLength+s+streamTagSize]
	_, err = openRange(bytes.NewReader(shifted), aead, h, size, plan)
	assert.ErrorIs(t, err, ErrStreamCorrupt)

	// A span cut short: the segment is an error, never a short answer.
	r, err := openRange(bytes.NewReader(span[:len(span)-10]), aead, h, size, plan)
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	assert.ErrorIs(t, err, ErrStreamCorrupt)

	// The object's final segment opened as if it were a middle one.
	_, err = PlanStreamRange(h, size, size-1, 2)
	assert.Error(t, err, "a range past the end is refused")
}

// SSE-S3 through the key store: Seal/Open round trip, the header names the
// version, a range, the old formats still open.
func TestSSEStream_SealOpen_KeyVersionInHeader(t *testing.T) {
	svc, keys, tenantID := newTestSSE(t)
	svc.SetSegmentSize(4096)
	ctx := context.Background()
	pt := randBytes(3*4096 + 100)

	r, v, err := svc.Seal(ctx, tenantID, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	assert.Equal(t, 1, v)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, svc.SealedLength(int64(len(pt))), int64(len(blob)))
	assert.Equal(t, StreamFormat, blob[0])
	got, err := SSEBlobKeyVersion(blob[:StreamHeaderSSES3])
	require.NoError(t, err)
	assert.Equal(t, 1, got)

	_, err = keys.Rotate(ctx, tenantID)
	require.NoError(t, err)
	o, err := svc.Open(ctx, tenantID, bytes.NewReader(blob))
	require.NoError(t, err, "a blob opens under the version it names, after a rotation too")
	out, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, pt, out)

	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	plan, err := PlanStreamRange(h, int64(len(pt)), 4090, 20)
	require.NoError(t, err)
	rr, err := svc.OpenRange(ctx, tenantID, h, int64(len(pt)), plan,
		bytes.NewReader(blob[plan.CipherOffset:plan.CipherOffset+plan.CipherLength]))
	require.NoError(t, err)
	part, err := io.ReadAll(rr)
	require.NoError(t, err)
	assert.Equal(t, pt[4090:4110], part)

	// The key version is authenticated: pointing the header at version 2
	// fails at the constructor.
	moved := append([]byte(nil), blob...)
	binary.BigEndian.PutUint32(moved[2:6], 2)
	_, err = svc.Open(ctx, tenantID, bytes.NewReader(moved))
	assert.ErrorIs(t, err, ErrStreamCorrupt)
}

func TestSSEStream_WrongTenantFails(t *testing.T) {
	svc, _, tenant1 := newTestSSE(t)
	tenant2 := keyTenant(t, getTestDB(t))
	ctx := context.Background()
	_, err := svc.Current(ctx, tenant2)
	require.NoError(t, err)
	r, _, err := svc.Seal(ctx, tenant1, bytes.NewReader([]byte("secret")), 6)
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	_, err = svc.Open(ctx, tenant2, bytes.NewReader(blob))
	assert.Error(t, err)
}

// The compatibility opener: 0x01 and 0x02 blobs, as the old writers made
// them, open through Open; the metric hook sees each format.
func TestSSEStream_OldBlobs_0x01_0x02_StillOpen(t *testing.T) {
	svc, keys, tenantID := newTestSSE(t)
	ctx := context.Background()
	seen := map[string]int{}
	svc.OnOpen(func(f string) { seen[f]++ })

	v2, _, err := legacyEncryptV2(ctx, svc, tenantID, []byte("a 0x02 blob"))
	require.NoError(t, err)
	r, err := svc.Open(ctx, tenantID, bytes.NewReader(v2))
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "a 0x02 blob", string(got))

	kv1, err := keys.Version(ctx, tenantID, 1)
	require.NoError(t, err)
	v1 := legacyEncryptV1(t, kv1, []byte("a 0x01 blob"))
	r, err = svc.Open(ctx, tenantID, bytes.NewReader(v1))
	require.NoError(t, err)
	got, err = io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "a 0x01 blob", string(got))

	s, _, err := svc.Seal(ctx, tenantID, bytes.NewReader([]byte("new")), 3)
	require.NoError(t, err)
	_, err = svc.Open(ctx, tenantID, s)
	require.NoError(t, err)

	assert.Equal(t, map[string]int{"0x01": 1, "0x02": 1, "0x03": 1}, seen)

	_, err = svc.DecryptBytes(ctx, tenantID, []byte{StreamFormat, 1, 2})
	assert.Error(t, err, "a 0x03 blob never goes through the whole-buffer opener")
	_, err = svc.Open(ctx, tenantID, bytes.NewReader(nil))
	assert.Error(t, err)
	_, err = svc.Open(ctx, tenantID, bytes.NewReader([]byte{0x7f, 1, 2, 3}))
	assert.True(t, err != nil && !errors.Is(err, io.EOF))
}
