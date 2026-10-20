package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-SSEC-1 (Stage 1a Prompt 17 Part B): the 0x05 SSE-C multipart blob —
// parts sealed as their own 0x04 streams under the object's identity plus
// the part number, concatenated behind a self-describing part table.

var mpTestID = StreamIdentity{Tenant: "t-mp", Bucket: "b", Key: "k"}

// sealParts seals each plaintext as part i+1 of mpTestID under key and
// returns the 0x05 blob and the parts' plaintexts concatenated.
func sealParts(t *testing.T, key []byte, id StreamIdentity, parts ...[]byte) (blob, plain []byte) {
	t.Helper()
	plains := make([]int64, len(parts))
	for i, p := range parts {
		plains[i] = int64(len(p))
	}
	hdr, err := NewSSECMultipartHeader(plains)
	require.NoError(t, err)
	blob = append(blob, hdr...)
	for i, p := range parts {
		pid := id
		pid.Part = i + 1
		r, err := SSECSeal(key, pid, bytes.NewReader(p), int64(len(p)))
		require.NoError(t, err)
		sealed, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, SSECSealedLength(int64(len(p))), int64(len(sealed)))
		blob = append(blob, sealed...)
		plain = append(plain, p...)
	}
	return blob, plain
}

func randPart(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestSSECMultipart_RoundTripOfThreePartsAndTheLengthFunction(t *testing.T) {
	// Arrange: three parts — two segments and a bit, one under a segment,
	// one exactly a segment.
	key := testSSECKey(t)
	parts := [][]byte{randPart(2*StreamSegmentSize + 123), randPart(4567), randPart(StreamSegmentSize)}
	blob, plain := sealParts(t, key, mpTestID, parts...)

	// Assert: the length is the pure function of the plaintext lengths.
	assert.Equal(t, SSECMultipartSealedLength([]int64{int64(len(parts[0])), int64(len(parts[1])), int64(len(parts[2]))}), int64(len(blob)))
	assert.Equal(t, MultipartFormat, blob[0])
	h, err := ParseMultipartHeader(blob)
	require.NoError(t, err)
	assert.Len(t, h.Parts, 3)
	assert.Equal(t, int64(len(plain)), h.PlainSize())
	assert.Equal(t, int64(len(blob)), h.SealedSize())

	// Act: a whole open.
	o, err := SSECMultipartOpen(key, mpTestID, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
	assert.Nil(t, o.key, "the key copy is cleared once the last part is open")
}

func TestSSECMultipart_APartStreamOpensOnlyAsItsOwnPartNumber(t *testing.T) {
	// Arrange: two parts of the same size; swap their streams in the blob.
	key := testSSECKey(t)
	a, b := randPart(70000), randPart(70000)
	blob, _ := sealParts(t, key, mpTestID, a, b)
	h, err := ParseMultipartHeader(blob)
	require.NoError(t, err)
	s1, s2 := h.StreamOffset(0), h.StreamOffset(1)
	swapped := append([]byte(nil), blob[:s1]...)
	swapped = append(swapped, blob[s2:s2+h.Parts[1].Sealed]...)
	swapped = append(swapped, blob[s1:s2]...)

	// Act + Assert: part 2's stream presented as part 1 fails at once.
	_, err = SSECMultipartOpen(key, mpTestID, bytes.NewReader(swapped))
	require.ErrorIs(t, err, ErrSSECKeyMismatch)

	// And the whole blob copied to another key's address fails at part 1.
	other := mpTestID
	other.Key = "other"
	_, err = SSECMultipartOpen(key, other, bytes.NewReader(blob))
	require.ErrorIs(t, err, ErrSSECKeyMismatch)
	// A wrong key: the same answer.
	_, err = SSECMultipartOpen(testSSECKey(t), mpTestID, bytes.NewReader(blob))
	require.ErrorIs(t, err, ErrSSECKeyMismatch)

	// A whole-object (Part 0) stream of the same bytes is not part 1: the
	// AAD of a whole object is unchanged, a part's carries its number.
	whole, err := SSECSeal(key, mpTestID, bytes.NewReader(a), int64(len(a)))
	require.NoError(t, err)
	wholeBlob, _ := io.ReadAll(whole)
	hdr, _ := NewSSECMultipartHeader([]int64{int64(len(a))})
	_, err = SSECMultipartOpen(key, mpTestID, bytes.NewReader(append(hdr, wholeBlob...)))
	require.ErrorIs(t, err, ErrSSECKeyMismatch)
	p1 := mpTestID
	p1.Part = 1
	_, err = SSECOpen(key, mpTestID, bytes.NewReader(wholeBlob))
	require.NoError(t, err, "the whole object still opens as itself")
	_, err = SSECOpen(key, p1, bytes.NewReader(wholeBlob))
	require.ErrorIs(t, err, ErrSSECKeyMismatch)
}

func TestSSECMultipart_AnAbsurdHeaderIsRefusedBeforeAnyAllocation(t *testing.T) {
	key := testSSECKey(t)
	blob, _ := sealParts(t, key, mpTestID, randPart(100))
	mut := func(f func(b []byte)) []byte { b := append([]byte(nil), blob...); f(b); return b }

	cases := map[string][]byte{
		"zero parts":          mut(func(b []byte) { binary.BigEndian.PutUint32(b[2:], 0) }),
		"too many parts":      mut(func(b []byte) { binary.BigEndian.PutUint32(b[2:], MaxMultipartParts+1) }),
		"a billion parts":     mut(func(b []byte) { binary.BigEndian.PutUint32(b[2:], 1<<30) }),
		"sealed not the func": mut(func(b []byte) { binary.BigEndian.PutUint64(b[14:], uint64(SSECSealedLength(100))+1) }),
		"plain absurd":        mut(func(b []byte) { binary.BigEndian.PutUint64(b[6:], 1<<62) }),
		"table cut":           blob[:MultipartHeaderLen(1)-3],
		"wrong kind":          mut(func(b []byte) { b[1] = StreamKindSSES3 }),
		"not 0x05":            mut(func(b []byte) { b[0] = StreamFormat }),
	}
	for name, b := range cases {
		_, err := ParseMultipartHeader(b)
		assert.ErrorIs(t, err, ErrMultipartHeader, name)
		_, err = SSECMultipartOpen(key, mpTestID, bytes.NewReader(b))
		assert.Error(t, err, name)
	}
	_, err := NewSSECMultipartHeader(nil)
	assert.ErrorIs(t, err, ErrMultipartHeader)
	_, err = NewSSECMultipartHeader(make([]int64, MaxMultipartParts+1))
	assert.ErrorIs(t, err, ErrMultipartHeader)
}

func TestSSECMultipart_ARangeAcrossAPartBoundaryAndInsideTheLastSegment(t *testing.T) {
	// Arrange: parts of 1.5 and 2.25 segments.
	key := testSSECKey(t)
	parts := [][]byte{randPart(StreamSegmentSize + StreamSegmentSize/2), randPart(2*StreamSegmentSize + StreamSegmentSize/4)}
	blob, plain := sealParts(t, key, mpTestID, parts...)
	h, err := ParseMultipartHeader(blob)
	require.NoError(t, err)

	// readRange opens a plaintext range of the blob as the API does: the
	// part spans, each part's own header, then its segments.
	readRange := func(off, n int64) []byte {
		spans, err := PlanMultipartRange(h, off, n)
		require.NoError(t, err)
		var out []byte
		for _, sp := range spans {
			ph, err := ParseStreamHeader(blob[sp.StreamOffset : sp.StreamOffset+int64(StreamHeaderSSEC)])
			require.NoError(t, err)
			pid := mpTestID
			pid.Part = sp.Part
			opener, err := SSECRangeOpener(key, pid, ph)
			require.NoError(t, err)
			plan, err := PlanStreamRange(ph, h.Parts[sp.Part-1].Plain, sp.Offset, sp.Length)
			require.NoError(t, err)
			from := sp.StreamOffset + plan.CipherOffset
			r, err := opener.OpenRange(h.Parts[sp.Part-1].Plain, plan, bytes.NewReader(blob[from:from+plan.CipherLength]))
			require.NoError(t, err)
			got, err := io.ReadAll(r)
			require.NoError(t, err)
			out = append(out, got...)
		}
		return out
	}

	// Across the boundary between the parts.
	b := int64(len(parts[0]))
	assert.Equal(t, plain[b-1000:b+1000], readRange(b-1000, 2000))
	// Inside the last segment of part 2.
	last := int64(len(plain)) - 100
	assert.Equal(t, plain[last:last+50], readRange(last, 50))
	// The whole object as one range.
	assert.Equal(t, plain, readRange(0, int64(len(plain))))
	// Spans: two for the boundary range, one each otherwise.
	spans, _ := PlanMultipartRange(h, b-1000, 2000)
	assert.Len(t, spans, 2)
	assert.Equal(t, 1, spans[0].Part)
	assert.Equal(t, int64(1000), spans[0].Length)
	assert.Equal(t, 2, spans[1].Part)
	assert.Equal(t, int64(0), spans[1].Offset)
	_, err = PlanMultipartRange(h, int64(len(plain)), 1)
	assert.Error(t, err, "outside the object")
}

func TestSSECMultipart_ATamperedPartIsCutNeverWrongBytes(t *testing.T) {
	// Arrange: flip a byte in the second part's first segment.
	key := testSSECKey(t)
	parts := [][]byte{randPart(StreamSegmentSize + 5), randPart(StreamSegmentSize + 5)}
	blob, plain := sealParts(t, key, mpTestID, parts...)
	h, _ := ParseMultipartHeader(blob)
	blob[h.StreamOffset(1)+int64(StreamHeaderSSEC)+10] ^= 0xff

	// Act: the open succeeds (part 1 is fine); the read fails at part 2
	// having delivered exactly part 1.
	o, err := SSECMultipartOpen(key, mpTestID, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSECKeyMismatch)
	assert.Equal(t, plain[:len(parts[0])], got, "part 1 whole, nothing of part 2")

	// A part cut short (the blob truncated inside part 2): an error, never
	// a short end.
	cut := blob[:h.StreamOffset(1)+1000]
	o, err = SSECMultipartOpen(key, mpTestID, bytes.NewReader(cut))
	require.NoError(t, err)
	_, err = io.ReadAll(o)
	require.Error(t, err)
}

func TestSSECMultipart_AnEmptyLastPartAndASinglePart(t *testing.T) {
	key := testSSECKey(t)
	blob, plain := sealParts(t, key, mpTestID, randPart(10), []byte{})
	o, err := SSECMultipartOpen(key, mpTestID, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	blob, plain = sealParts(t, key, mpTestID, randPart(3))
	o, err = SSECMultipartOpen(key, mpTestID, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err = io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
}
