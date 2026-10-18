package crypto

// WP-R8-3 A.2 (R8-10): a 0x03 segment's AAD is the stream header alone, so a
// sealed blob copied from key A to key B's address under the same tenant
// decrypts and is served as B. Format 0x04 binds every segment to the
// object's identity: AAD = header ‖ tenant ‖ bucket ‖ key (length-prefixed).
// 0x03 blobs still open; every writer seals 0x04.

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func idOf(tenant, bucket, key string) StreamIdentity {
	return StreamIdentity{Tenant: tenant, Bucket: bucket, Key: key}
}

func sealSSE(t *testing.T, svc *SSEService, id StreamIdentity, pt []byte) []byte {
	t.Helper()
	r, _, err := svc.Seal(context.Background(), id, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	return blob
}

func TestStreamV4_NewSSES3BlobsAreFormat0x04WithTheSameLayout(t *testing.T) {
	// Arrange
	svc, _, tenantID := newTestSSE(t)
	svc.SetSegmentSize(4096)
	pt := randBytes(3*4096 + 7)

	// Act
	blob := sealSSE(t, svc, idOf(tenantID, "b", "k"), pt)

	// Assert: the 0x03 header layout and length, one byte different.
	assert.Equal(t, byte(0x04), blob[0])
	assert.Equal(t, StreamFormat, blob[0], "StreamFormat is what every writer writes")
	assert.Equal(t, byte(0x03), StreamFormatV3)
	assert.Equal(t, svc.SealedLength(int64(len(pt))), int64(len(blob)), "SealedLength is unchanged")
	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	assert.Len(t, h.Raw, StreamHeaderSSES3)
	assert.Equal(t, 2+4+1088+4+7, StreamHeaderSSES3)
	assert.Equal(t, 2+16+4+7, StreamHeaderSSEC)
	assert.Equal(t, StreamHeaderSSES3, MaxStreamHeader)
	for _, f := range []byte{StreamFormatV3, StreamFormat} {
		raw := append([]byte{f}, blob[1:StreamHeaderSSES3]...)
		hf, err := ParseStreamHeader(raw)
		require.NoError(t, err, "format 0x%02x parses", f)
		assert.Len(t, hf.Raw, StreamHeaderSSES3, "both formats have one header length")
		assert.Equal(t, f, hf.Format)
	}
}

func TestStreamV4_ABlobOpensOnlyUnderItsOwnIdentity(t *testing.T) {
	// Arrange
	svc, _, tenantID := newTestSSE(t)
	svc.SetSegmentSize(4096)
	ctx := context.Background()
	pt := randBytes(2*4096 + 9)
	own := idOf(tenantID, "photos", "a.jpg")
	blob := sealSSE(t, svc, own, pt)

	// Act + Assert: its own identity opens it, whole and by range.
	o, err := svc.Open(ctx, own, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, pt, got)
	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	plan, err := PlanStreamRange(h, int64(len(pt)), 4090, 20)
	require.NoError(t, err)
	span := blob[plan.CipherOffset : plan.CipherOffset+plan.CipherLength]
	rr, err := svc.OpenRange(ctx, own, h, int64(len(pt)), plan, bytes.NewReader(span))
	require.NoError(t, err)
	part, err := io.ReadAll(rr)
	require.NoError(t, err)
	assert.Equal(t, pt[4090:4110], part)

	// Another key, another bucket, a split that concatenates the same:
	// the first segment does not open — before any byte.
	for name, other := range map[string]StreamIdentity{
		"another key":    idOf(tenantID, "photos", "b.jpg"),
		"another bucket": idOf(tenantID, "backup", "a.jpg"),
		"shifted split":  idOf(tenantID, "photo", "sa.jpg"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Open(ctx, other, bytes.NewReader(blob))
			assert.ErrorIs(t, err, ErrStreamCorrupt)
			_, err = svc.OpenRange(ctx, other, h, int64(len(pt)), plan, bytes.NewReader(span))
			assert.ErrorIs(t, err, ErrStreamCorrupt)
			ro, err := svc.RangeOpener(ctx, other, h)
			require.NoError(t, err, "the key is derived from the header alone")
			_, err = ro.OpenRange(int64(len(pt)), plan, bytes.NewReader(span))
			assert.ErrorIs(t, err, ErrStreamCorrupt)
		})
	}
}

func TestStreamV4_AFormatByteRewrittenTo0x03DoesNotOpen(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	id := idOf(tenantID, "b", "k")
	blob := sealSSE(t, svc, id, randBytes(5000))
	blob[0] = StreamFormatV3

	_, err := svc.Open(context.Background(), id, bytes.NewReader(blob))
	assert.ErrorIs(t, err, ErrStreamCorrupt, "the header is in the AAD and the DEK's derivation")
}

func TestStreamV4_A0x03BlobStillOpensAndIsCountedAsSuch(t *testing.T) {
	// Arrange: a blob in the format written before (labs and tests only —
	// prod has no master key, so no sealed blob of either format).
	svc, _, tenantID := newTestSSE(t)
	seen := map[string]int{}
	svc.OnOpen(func(f string) { seen[f]++ })
	pt := randBytes(9000)
	blob := sealSSEV3(t, svc, tenantID, pt)
	require.Equal(t, StreamFormatV3, blob[0])

	// Act: opened under any identity (0x03 binds none).
	o, err := svc.Open(context.Background(), idOf(tenantID, "b", "k"), bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.NoError(t, err)

	// Assert
	assert.Equal(t, pt, got)
	assert.Equal(t, 1, seen["0x03"])
	v, err := SSEBlobKeyVersion(blob[:StreamHeaderSSES3])
	require.NoError(t, err)
	assert.Equal(t, 1, v)

	_ = sealSSE(t, svc, idOf(tenantID, "b", "k2"), pt)
	o4, err := svc.Open(context.Background(), idOf(tenantID, "b", "k2"), bytes.NewReader(sealSSE(t, svc, idOf(tenantID, "b", "k2"), pt)))
	require.NoError(t, err)
	_, _ = io.ReadAll(o4)
	assert.Equal(t, 1, seen["0x04"])
}

func TestStreamV4_SSECBindsTheIdentityToo(t *testing.T) {
	// Arrange
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	pt := randBytes(StreamSegmentSize + 100)
	own := idOf("t1", "b", "a.bin")
	r, err := SSECSeal(key, own, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte{StreamFormat, StreamKindSSEC}, blob[:2])
	assert.Equal(t, SSECSealedLength(int64(len(pt))), int64(len(blob)))

	// Act + Assert: its own identity opens it.
	o, err := SSECOpen(key, own, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, pt, got)

	// Another key's address: the right customer key, the wrong object.
	_, err = SSECOpen(key, idOf("t1", "b", "b.bin"), bytes.NewReader(blob))
	assert.Error(t, err)
	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	plan, err := PlanStreamRange(h, int64(len(pt)), 10, 10)
	require.NoError(t, err)
	_, err = SSECOpenRange(key, idOf("t1", "b", "b.bin"), h, int64(len(pt)), plan,
		bytes.NewReader(blob[plan.CipherOffset:plan.CipherOffset+plan.CipherLength]))
	assert.Error(t, err)
}

func TestStreamV4_AnEmptyIdentityIsRefused(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	ctx := context.Background()
	for name, id := range map[string]StreamIdentity{
		"empty":     {},
		"no key":    {Tenant: tenantID, Bucket: "b"},
		"no bucket": {Tenant: tenantID, Key: "k"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := svc.Seal(ctx, id, bytes.NewReader([]byte("x")), 1)
			assert.ErrorIs(t, err, ErrStreamIdentity)
			_, err = SSECSeal(make([]byte, 32), id, bytes.NewReader([]byte("x")), 1)
			assert.ErrorIs(t, err, ErrStreamIdentity)
		})
	}
	blob := sealSSE(t, svc, idOf(tenantID, "b", "k"), []byte("data"))
	_, err := svc.Open(ctx, StreamIdentity{Tenant: tenantID}, bytes.NewReader(blob))
	assert.ErrorIs(t, err, ErrStreamIdentity, "a 0x04 blob is never opened without its identity")
}

func TestStreamIdentity_AADIsLengthPrefixed(t *testing.T) {
	h := []byte{StreamFormat, StreamKindSSEC}
	a := streamAAD(h, idOf("t", "a", "bc"))
	b := streamAAD(h, idOf("t", "ab", "c"))
	assert.NotEqual(t, a, b)
	assert.True(t, bytes.HasPrefix(a, h))
	assert.Equal(t, []byte{StreamFormatV3, 1}, streamAAD([]byte{StreamFormatV3, 1}, idOf("t", "a", "b")),
		"a 0x03 header's AAD is the header alone")
}

// sealSSEV3 seals pt in the format written before 0x04.
func sealSSEV3(t *testing.T, svc *SSEService, tenantID string, pt []byte) []byte {
	t.Helper()
	r, _, err := svc.sealFormat(context.Background(), idOf(tenantID, "any", "any"), bytes.NewReader(pt), int64(len(pt)), StreamFormatV3)
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	return blob
}
