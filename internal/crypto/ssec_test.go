package crypto

import (
	"crypto/md5" // #nosec G401 — S3 spec requires MD5
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	ciphertext, err := SSECEncrypt(key, plaintext)
	require.NoError(t, err)
	assert.Equal(t, len(plaintext)+SSECOverhead, len(ciphertext))

	decrypted, err := SSECDecrypt(key, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestSSECEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	key := testSSECKey(t)

	ciphertext, err := SSECEncrypt(key, []byte{})
	require.NoError(t, err)
	assert.Equal(t, SSECOverhead, len(ciphertext))

	decrypted, err := SSECDecrypt(key, ciphertext)
	require.NoError(t, err)
	assert.Empty(t, decrypted)
}

func TestSSECDecrypt_WrongKey(t *testing.T) {
	key := testSSECKey(t)
	wrongKey := testSSECKey(t)

	ciphertext, err := SSECEncrypt(key, []byte("secret data"))
	require.NoError(t, err)

	_, err = SSECDecrypt(wrongKey, ciphertext)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSSECKeyMismatch)
}

func TestSSECEncrypt_InvalidKeyLength(t *testing.T) {
	_, err := SSECEncrypt(make([]byte, 16), []byte("data"))
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

func TestSSECEncrypt_DifferentCiphertexts(t *testing.T) {
	key := testSSECKey(t)
	plaintext := []byte("same data")

	ct1, err := SSECEncrypt(key, plaintext)
	require.NoError(t, err)
	ct2, err := SSECEncrypt(key, plaintext)
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
