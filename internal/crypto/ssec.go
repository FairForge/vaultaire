package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G401 G501 — S3 spec requires MD5 for SSE-C key validation
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	SSECAlgorithm = "AES256-SSE-C"
	SSECOverhead  = 28 // 12 nonce + 16 GCM tag
)

var ErrSSECKeyMismatch = errors.New("ssec: decryption failed — wrong key")

func SSECEncrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("ssec: key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("ssec: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("ssec: create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("ssec: generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	buf := make([]byte, 0, len(nonce)+len(ciphertext))
	buf = append(buf, nonce...)
	buf = append(buf, ciphertext...)
	return buf, nil
}

func SSECDecrypt(key, data []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("ssec: key must be 32 bytes, got %d", len(key))
	}
	if len(data) < SSECOverhead {
		return nil, fmt.Errorf("ssec: data too short: %d bytes (minimum %d)", len(data), SSECOverhead)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("ssec: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("ssec: create GCM: %w", err)
	}

	nonce := data[:gcm.NonceSize()]
	ciphertext := data[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrSSECKeyMismatch
	}
	return plaintext, nil
}

func HasSSECHeaders(r *http.Request) bool {
	return r.Header.Get("x-amz-server-side-encryption-customer-algorithm") != ""
}

func ParseSSECHeaders(r *http.Request) ([]byte, error) {
	algo := r.Header.Get("x-amz-server-side-encryption-customer-algorithm")
	keyB64 := r.Header.Get("x-amz-server-side-encryption-customer-key")
	md5B64 := r.Header.Get("x-amz-server-side-encryption-customer-key-MD5")

	if algo == "" && keyB64 == "" && md5B64 == "" {
		return nil, nil
	}

	if algo == "" || keyB64 == "" || md5B64 == "" {
		return nil, fmt.Errorf("ssec: incomplete SSE-C headers — all three are required")
	}

	if algo != "AES256" {
		return nil, fmt.Errorf("ssec: unsupported algorithm %q, expected AES256", algo)
	}

	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("ssec: invalid base64 key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("ssec: key must be 32 bytes, got %d", len(key))
	}

	expectedMD5, err := base64.StdEncoding.DecodeString(md5B64)
	if err != nil {
		return nil, fmt.Errorf("ssec: invalid base64 MD5: %w", err)
	}

	actualMD5 := md5.Sum(key) // #nosec G401 — S3 spec requires MD5 for key validation
	if !equal(actualMD5[:], expectedMD5) {
		return nil, fmt.Errorf("ssec: key MD5 mismatch")
	}

	return key, nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
