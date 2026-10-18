package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G401 — S3 spec requires MD5
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ssecTestID is the object the SSE-C codec tests seal for.
var ssecTestID = StreamIdentity{Tenant: "t", Bucket: "b", Key: "k"}

func testSSECKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func TestSSECEncryptDecrypt(t *testing.T) {
	key := testSSECKey(t)
	plaintext := []byte("hello, SSE-C world!")

	ciphertext, err := legacySSECEncrypt(key, plaintext)
	require.NoError(t, err)
	assert.Equal(t, len(plaintext)+SSECOverhead, len(ciphertext))

	decrypted, err := SSECDecrypt(key, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestSSECEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	key := testSSECKey(t)

	ciphertext, err := legacySSECEncrypt(key, []byte{})
	require.NoError(t, err)
	assert.Equal(t, SSECOverhead, len(ciphertext))

	decrypted, err := SSECDecrypt(key, ciphertext)
	require.NoError(t, err)
	assert.Empty(t, decrypted)
}

func TestSSECDecrypt_WrongKey(t *testing.T) {
	key := testSSECKey(t)
	wrongKey := testSSECKey(t)

	ciphertext, err := legacySSECEncrypt(key, []byte("secret data"))
	require.NoError(t, err)

	_, err = SSECDecrypt(wrongKey, ciphertext)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSECKeyMismatch)
}

func TestLegacySSECEncrypt_InvalidKeyLength(t *testing.T) {
	_, err := legacySSECEncrypt(make([]byte, 16), []byte("data"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestSSECDecrypt_InvalidKeyLength(t *testing.T) {
	_, err := SSECDecrypt(make([]byte, 16), make([]byte, SSECOverhead))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestSSECDecrypt_DataTooShort(t *testing.T) {
	key := testSSECKey(t)
	_, err := SSECDecrypt(key, []byte("short"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestLegacySSECEncrypt_DifferentCiphertexts(t *testing.T) {
	key := testSSECKey(t)
	plaintext := []byte("same data")

	ct1, err := legacySSECEncrypt(key, plaintext)
	require.NoError(t, err)
	ct2, err := legacySSECEncrypt(key, plaintext)
	require.NoError(t, err)

	assert.NotEqual(t, ct1, ct2, "different nonces should produce different ciphertexts")
}

func ssecHeaders(t *testing.T, key []byte) http.Header {
	t.Helper()
	h := http.Header{}
	h.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	h.Set("x-amz-server-side-encryption-customer-key", base64.StdEncoding.EncodeToString(key))
	digest := md5.Sum(key) // #nosec G401
	h.Set("x-amz-server-side-encryption-customer-key-MD5", base64.StdEncoding.EncodeToString(digest[:]))
	return h
}

func TestParseSSECHeaders_Valid(t *testing.T) {
	key := testSSECKey(t)
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	for k, v := range ssecHeaders(t, key) {
		req.Header[k] = v
	}

	parsed, err := ParseSSECHeaders(req)
	require.NoError(t, err)
	assert.Equal(t, key, parsed)
}

func TestParseSSECHeaders_Missing(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	parsed, err := ParseSSECHeaders(req)
	require.NoError(t, err)
	assert.Nil(t, parsed)
}

func TestParseSSECHeaders_MD5Mismatch(t *testing.T) {
	key := testSSECKey(t)
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	req.Header.Set("x-amz-server-side-encryption-customer-key", base64.StdEncoding.EncodeToString(key))
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", base64.StdEncoding.EncodeToString([]byte("wrongmd5wrongmd5")))

	_, err := ParseSSECHeaders(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MD5 mismatch")
}

func TestParseSSECHeaders_PartialHeaders(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")

	_, err := ParseSSECHeaders(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete")
}

func TestParseSSECHeaders_WrongAlgorithm(t *testing.T) {
	key := testSSECKey(t)
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES128")
	req.Header.Set("x-amz-server-side-encryption-customer-key", base64.StdEncoding.EncodeToString(key))
	digest := md5.Sum(key) // #nosec G401
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", base64.StdEncoding.EncodeToString(digest[:]))

	_, err := ParseSSECHeaders(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported algorithm")
}

func TestParseSSECHeaders_InvalidBase64Key(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	req.Header.Set("x-amz-server-side-encryption-customer-key", "not-valid-base64!!!")
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", "also-bad")

	_, err := ParseSSECHeaders(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid base64")
}

func TestParseSSECHeaders_WrongKeyLength(t *testing.T) {
	shortKey := make([]byte, 16)
	_, _ = rand.Read(shortKey)
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	req.Header.Set("x-amz-server-side-encryption-customer-key", base64.StdEncoding.EncodeToString(shortKey))
	digest := md5.Sum(shortKey) // #nosec G401
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", base64.StdEncoding.EncodeToString(digest[:]))

	_, err := ParseSSECHeaders(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestHasSSECHeaders(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		assert.True(t, HasSSECHeaders(req))
	})

	t.Run("absent", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		assert.False(t, HasSSECHeaders(req))
	})
}

func TestSSECOverheadConstant(t *testing.T) {
	assert.Equal(t, 28, SSECOverhead)
}

// legacySSECEncrypt is the SSE-C writer before WP-R8-5 Part B (SSECEncrypt):
// [nonce 12][AES-GCM ct+tag], one GCM over the object. A fixture: no
// production code writes this format.
func legacySSECEncrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("ssec: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// WP-R8-5 Part B: SSE-C is kind 0x02 of the streaming codec.
func TestSSECStream_RoundTripRangeWrongKey(t *testing.T) {
	key := testSSECKey(t)
	pt := make([]byte, 3*StreamSegmentSize+777)
	_, _ = rand.Read(pt)

	r, err := SSECSeal(key, ssecTestID, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, SSECSealedLength(int64(len(pt))), int64(len(blob)))
	assert.Equal(t, StreamFormat, blob[0])
	assert.Equal(t, StreamKindSSEC, blob[1])

	o, err := SSECOpen(key, ssecTestID, bytes.NewReader(blob))
	require.NoError(t, err)
	got, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, pt, got)

	h, err := ParseStreamHeader(blob)
	require.NoError(t, err)
	plan, err := PlanStreamRange(h, int64(len(pt)), StreamSegmentSize-5, 10)
	require.NoError(t, err)
	rr, err := SSECOpenRange(key, ssecTestID, h, int64(len(pt)), plan, bytes.NewReader(blob[plan.CipherOffset:plan.CipherOffset+plan.CipherLength]))
	require.NoError(t, err)
	part, err := io.ReadAll(rr)
	require.NoError(t, err)
	assert.Equal(t, pt[StreamSegmentSize-5:StreamSegmentSize+5], part)

	wrong := testSSECKey(t)
	_, err = SSECOpen(wrong, ssecTestID, bytes.NewReader(blob))
	assert.ErrorIs(t, err, ErrSSECKeyMismatch, "decided at the first segment, before any byte")
	_, err = SSECOpenRange(wrong, ssecTestID, h, int64(len(pt)), plan, bytes.NewReader(blob[plan.CipherOffset:plan.CipherOffset+plan.CipherLength]))
	assert.ErrorIs(t, err, ErrSSECKeyMismatch)
}

// One customer key over many objects never repeats a (key, nonce): every
// blob has its own salt, so its own DEK.
func TestSSECStream_SameKeySameBytes_DifferentBlobs(t *testing.T) {
	key := testSSECKey(t)
	pt := []byte("the same plaintext under the same customer key")
	a, err := SSECSeal(key, ssecTestID, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	b, err := SSECSeal(key, ssecTestID, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	ab, _ := io.ReadAll(a)
	bb, _ := io.ReadAll(b)
	assert.NotEqual(t, ab[2:18], bb[2:18], "a fresh salt per object")
	assert.NotEqual(t, ab[StreamHeaderSSEC:], bb[StreamHeaderSSEC:])
}

// The old format still opens, through its bounded opener; the row says which
// format a blob is in, so an old blob whose random nonce starts like a 0x03
// SSE-C header is opened as what it is.
func TestSSECStream_OldBlobAndTheMagicCollision(t *testing.T) {
	key := testSSECKey(t)
	pt := []byte("an SSE-C object written before WP-R8-5 Part B")
	var old []byte
	for {
		b, err := legacySSECEncrypt(key, pt)
		require.NoError(t, err)
		if b[0] == StreamFormat {
			old = b
			break
		}
	}
	old[1] = StreamKindSSEC // and the kind byte too: a nonce is any 12 bytes
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	old = gcm.Seal(append([]byte(nil), old[:12]...), old[:12], pt, nil)
	require.Equal(t, []byte{StreamFormat, StreamKindSSEC}, old[:2])

	r, err := SSECOpenLegacy(key, bytes.NewReader(old))
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, pt, got, "the row said old format: opened as one")
	_, err = SSECOpen(key, ssecTestID, bytes.NewReader(old))
	assert.Error(t, err, "read as a streaming blob it does not open: the row is what decides")

	assert.True(t, IsSSEC(SSECAlgorithm) && IsSSEC(SSECAlgorithmV3))
	assert.False(t, IsSSEC(SSEAlgorithm) || IsSSEC(""))
}
