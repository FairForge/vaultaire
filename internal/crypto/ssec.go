package crypto

import (
	"bytes"
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
	// SSECAlgorithm is the row value of an SSE-C object written before
	// WP-R8-5 Part B: [nonce 12][AES-GCM ct+tag] under the customer's key,
	// one GCM over the object. Such a blob is only opened (SSECDecrypt);
	// no writer makes it.
	SSECAlgorithm = "AES256-SSE-C"
	// SSECAlgorithmV3 is the row value of an SSE-C object in the streaming
	// format (stream.go, kind 0x02): what every SSE-C write makes since
	// WP-R8-5 Part B. The row says which format a blob is in: an old blob
	// starts with a random nonce byte, so a magic byte alone would collide
	// once in 256.
	SSECAlgorithmV3 = "AES256-SSE-C-v3"
	// SSECAlgorithmMultipart is the row value of an SSE-C object completed
	// from a multipart upload: the 0x05 container of the parts' streams
	// (ssec_multipart.go, WP-SSEC-1). Its stored length is not a function
	// of the object's size alone (the parts' lengths decide it), so readers
	// learn the layout from the blob's own header.
	SSECAlgorithmMultipart = "AES256-SSE-C-v3-mp"
	SSECOverhead           = 28 // 12 nonce + 16 GCM tag (the old format)
	// legacySSECMaxBlob bounds the old-format opener: no old SSE-C blob was
	// written over 256 MiB (the old PUT refused larger ones).
	legacySSECMaxBlob = 256<<20 + SSECOverhead
)

var ErrSSECKeyMismatch = errors.New("ssec: decryption failed — wrong key")

// IsSSEC reports whether a row's encryption_algorithm is SSE-C (either
// format): the customer's key is needed to read it.
func IsSSEC(algorithm string) bool {
	return algorithm == SSECAlgorithm || algorithm == SSECAlgorithmV3 || algorithm == SSECAlgorithmMultipart
}

// IsSSECMultipart reports the 0x05 container (a multipart-completed SSE-C
// object).
func IsSSECMultipart(algorithm string) bool { return algorithm == SSECAlgorithmMultipart }

// SSECSealedLength is the stored length of an SSE-C object of size bytes in
// the streaming format.
func SSECSealedLength(size int64) int64 {
	return StreamCiphertextLength(StreamHeaderSSEC, StreamSegmentSize, size)
}

// ssecAEAD derives the DEK of a streaming SSE-C blob: HKDF-SHA256 over the
// customer's key, the blob's random salt and its header.
func ssecAEAD(key []byte, h *StreamHeader) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("ssec: key must be 32 bytes, got %d", len(key))
	}
	if h.Kind != StreamKindSSEC {
		return nil, fmt.Errorf("%w: kind 0x%02x is not SSE-C", ErrStreamCorrupt, h.Kind)
	}
	return streamAEAD(key, h.Salt, ssecStreamInfo, h.Raw)
}

// SSECSeal returns a reader of the streaming (0x04) SSE-C blob of src
// (exactly size bytes) of the object id under the customer's key. The DEK is
// derived before it returns: the caller may zero key at once.
func SSECSeal(key []byte, id StreamIdentity, src io.Reader, size int64) (io.Reader, error) {
	if err := id.check(); err != nil {
		return nil, err
	}
	salt := make([]byte, streamSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("ssec: salt: %w", err)
	}
	raw, err := newStreamHeader(StreamKindSSEC, salt, StreamSegmentSize)
	if err != nil {
		return nil, err
	}
	h, err := ParseStreamHeader(raw)
	if err != nil {
		return nil, err
	}
	aead, err := ssecAEAD(key, h)
	if err != nil {
		return nil, err
	}
	return newStreamSealer(src, aead, raw, streamAAD(raw, id), StreamSegmentSize, size)
}

// SSECOpen opens a whole streaming SSE-C blob read from src. The header and
// the first segment are verified before it returns: a first segment that
// does not open is ErrSSECKeyMismatch (the key is not stored, so a wrong key
// and a damaged first segment look alike — as with the old format); a later
// one is ErrStreamCorrupt from the reader. A 0x04 blob opens only as the
// object id (a blob of another key, under the right customer key, is the
// same ErrSSECKeyMismatch). The DEK is derived before it returns: the caller
// may zero key at once.
func SSECOpen(key []byte, id StreamIdentity, src io.Reader) (io.Reader, error) {
	h, err := ReadStreamHeader(src)
	if err != nil {
		return nil, err
	}
	aad, err := openAAD(h, id)
	if err != nil {
		return nil, err
	}
	aead, err := ssecAEAD(key, h)
	if err != nil {
		return nil, err
	}
	o := newStreamOpener(src, aead, h, aad)
	if err := o.prime(); err != nil {
		if errors.Is(err, ErrStreamCorrupt) {
			return nil, ErrSSECKeyMismatch
		}
		return nil, err
	}
	return o, nil
}

// SSECOpenRange opens a planned range of a streaming SSE-C object of size
// bytes whose header is h; src is the stored bytes the plan names. A first
// segment that does not open is ErrSSECKeyMismatch.
func SSECOpenRange(key []byte, id StreamIdentity, h *StreamHeader, size int64, rg StreamRange, src io.Reader) (io.Reader, error) {
	aad, err := openAAD(h, id)
	if err != nil {
		return nil, err
	}
	aead, err := ssecAEAD(key, h)
	if err != nil {
		return nil, err
	}
	r, err := openRange(src, aead, h, aad, size, rg)
	if err != nil {
		if errors.Is(err, ErrStreamCorrupt) {
			return nil, ErrSSECKeyMismatch
		}
		return nil, err
	}
	return r, nil
}

// SSECRangeOpener derives the DEK of a streaming SSE-C blob whose header is
// h once, for many ranges of it. The caller may zero key when it returns.
func SSECRangeOpener(key []byte, id StreamIdentity, h *StreamHeader) (*StreamRangeOpener, error) {
	aad, err := openAAD(h, id)
	if err != nil {
		return nil, err
	}
	aead, err := ssecAEAD(key, h)
	if err != nil {
		return nil, err
	}
	return &StreamRangeOpener{aead: aead, h: h, aad: aad, ssec: true}, nil
}

// SSECOpenLegacy opens a whole old-format SSE-C blob read from src (≤ 256
// MiB by construction): the compatibility opener, bounded.
func SSECOpenLegacy(key []byte, src io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(io.LimitReader(src, legacySSECMaxBlob+1))
	if err != nil {
		return nil, fmt.Errorf("ssec: read: %w", err)
	}
	if len(data) > legacySSECMaxBlob {
		return nil, errors.New("ssec: old-format blob longer than any old write")
	}
	pt, err := SSECDecrypt(key, data)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(pt), nil
}

// SSECDecrypt opens an old-format SSE-C blob ([nonce][ct+tag]).
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

// HasSSECCopySourceHeaders reports a copy source's customer key on the
// request (x-amz-copy-source-server-side-encryption-customer-*, WP-SSEC-1
// B.4).
func HasSSECCopySourceHeaders(r *http.Request) bool {
	return r.Header.Get("x-amz-copy-source-server-side-encryption-customer-algorithm") != ""
}

// ParseSSECHeaders reads the request's own SSE-C headers (the object being
// written or read): nil, nil when none; the 32-byte key when all three are
// present and consistent; an error otherwise.
func ParseSSECHeaders(r *http.Request) ([]byte, error) {
	return parseSSECKey(r.Header.Get("x-amz-server-side-encryption-customer-algorithm"),
		r.Header.Get("x-amz-server-side-encryption-customer-key"),
		r.Header.Get("x-amz-server-side-encryption-customer-key-MD5"))
}

// ParseSSECCopySourceHeaders reads the copy SOURCE's SSE-C headers
// (x-amz-copy-source-server-side-encryption-customer-*): the same rules.
func ParseSSECCopySourceHeaders(r *http.Request) ([]byte, error) {
	return parseSSECKey(r.Header.Get("x-amz-copy-source-server-side-encryption-customer-algorithm"),
		r.Header.Get("x-amz-copy-source-server-side-encryption-customer-key"),
		r.Header.Get("x-amz-copy-source-server-side-encryption-customer-key-MD5"))
}

func parseSSECKey(algo, keyB64, md5B64 string) ([]byte, error) {
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

// SSECKeyMD5 is the base64 MD5 of a customer key — what the
// x-amz-server-side-encryption-customer-key-MD5 header carries and what the
// row keeps (RP14-11, migration 094), never the key.
func SSECKeyMD5(key []byte) string {
	sum := md5.Sum(key) // #nosec G401 — S3's own key-identity digest
	return base64.StdEncoding.EncodeToString(sum[:])
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
