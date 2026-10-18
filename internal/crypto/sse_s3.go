package crypto

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/mlkem"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// SSEVersion is the whole-buffer format written from WP-R8-1 until
	// WP-R8-5: [0x02][key_version uint32 BE][ML-KEM ct 1088][nonce 12]
	// [AES-GCM ct+tag], the first five bytes bound to the ciphertext as AAD.
	// Every writer seals the streaming format (stream.go: 0x03 from WP-R8-5,
	// 0x04 since WP-R8-3 A.2); 0x01 and 0x02 blobs are only opened, by the
	// compatibility opener below.
	SSEVersion byte = 0x02
	// SSEVersionV1 is the format before it ([0x01][KEM ct][nonce][ct], no
	// version, no AAD); such a blob was sealed under key version 1.
	SSEVersionV1       byte = 0x01
	sseHeaderBytes          = 1 + 4
	SSEOverheadBytes        = sseHeaderBytes + mlkem.CiphertextSize768 + 12 + 16 // 1121
	sseV1OverheadBytes      = 1 + mlkem.CiphertextSize768 + 12 + 16              // 1117
	// legacySSEMaxPlaintext bounds what the compatibility opener reads: no
	// 0x01/0x02 blob was ever sealed over 256 MiB (the old writers' cap).
	legacySSEMaxPlaintext int64 = 256 << 20
	SSEAlgorithm                = "ML-KEM-768+AES-256-GCM"
)

// SSEService is SSE-S3: a fresh ML-KEM-768 encapsulation per object against
// the tenant's current key version, the object sealed by the streaming codec
// (stream.go, format 0x04) under a DEK derived from the shared secret. The
// decapsulation key is the version's random seed, wrapped under the master
// key in tenant_key_versions (TenantKeys, WP-R8-1).
type SSEService struct {
	keys    *TenantKeys
	segment int
	onOpen  func(format string)
}

// NewSSEService builds the service on the tenant key store.
func NewSSEService(keys *TenantKeys) *SSEService {
	return &SSEService{keys: keys, segment: StreamSegmentSize, onOpen: func(string) {}}
}

// SetSegmentSize changes the segment size this service seals with (tests;
// every writer uses StreamSegmentSize). Readers take it from each header.
func (s *SSEService) SetSegmentSize(n int) { s.segment = n }

// OnOpen sets the function called once per blob opened, with its format
// ("0x01", "0x02", "0x03", "0x04"): vaultaire_sse_blobs_opened_total.
func (s *SSEService) OnOpen(fn func(format string)) { s.onOpen = fn }

// Current makes sure the tenant has a key and returns its current version
// (it was EnsureTenantKey; a PUT calls it before reading the body).
func (s *SSEService) Current(ctx context.Context, tenantID string) (int, error) {
	kv, err := s.keys.Current(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return kv.Version, nil
}

// SealedLength is the stored length of the blob Seal makes of size bytes.
func (s *SSEService) SealedLength(size int64) int64 {
	return StreamCiphertextLength(StreamHeaderSSES3, s.segment, size)
}

// Seal returns a reader of the 0x04 blob of src for the object id — exactly
// size bytes; a source that ends early or runs long fails the read — and the
// key version it is sealed under (id.Tenant's current one; the caller
// records it on the row). Nothing is read from src before the first Read.
func (s *SSEService) Seal(ctx context.Context, id StreamIdentity, src io.Reader, size int64) (io.Reader, int, error) {
	return s.sealFormat(ctx, id, src, size, StreamFormat)
}

// sealFormat is Seal in a given streaming format (tests make 0x03 blobs).
func (s *SSEService) sealFormat(ctx context.Context, id StreamIdentity, src io.Reader, size int64, format byte) (io.Reader, int, error) {
	if err := id.check(); err != nil {
		return nil, 0, err
	}
	kv, err := s.keys.Current(ctx, id.Tenant)
	if err != nil {
		return nil, 0, fmt.Errorf("tenant key: %w", err)
	}
	sharedKey, kemCiphertext := kv.Encap.Encapsulate()
	defer clear(sharedKey)
	material := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(kemCiphertext)), uint32(kv.Version)) // #nosec G115 -- versions are small
	material = append(material, kemCiphertext...)
	header, err := newStreamHeaderFormat(format, StreamKindSSES3, material, s.segment)
	if err != nil {
		return nil, 0, err
	}
	aead, err := streamAEAD(sharedKey, nil, sseStreamInfo, header)
	if err != nil {
		return nil, 0, err
	}
	sealer, err := newStreamSealer(src, aead, header, streamAAD(header, id), s.segment, size)
	if err != nil {
		return nil, 0, err
	}
	return sealer, kv.Version, nil
}

// streamAEADFor decapsulates a streaming SSE-S3 header under the version it
// names.
func (s *SSEService) streamAEADFor(ctx context.Context, tenantID string, h *StreamHeader) (cipher.AEAD, error) {
	if h.Kind != StreamKindSSES3 {
		return nil, fmt.Errorf("%w: kind 0x%02x is not SSE-S3", ErrStreamCorrupt, h.Kind)
	}
	kv, err := s.keys.Version(ctx, tenantID, h.KeyVersion)
	if err != nil {
		return nil, fmt.Errorf("tenant key: %w", err)
	}
	sharedKey, err := kv.KEM.Decapsulate(h.KEMCipher)
	if err != nil {
		return nil, fmt.Errorf("%w: ML-KEM decapsulation: %w", ErrStreamCorrupt, err)
	}
	defer clear(sharedKey)
	return streamAEAD(sharedKey, nil, sseStreamInfo, h.Raw)
}

// Open opens a whole SSE-S3 blob of the object id read from src. A streaming
// blob (0x03, 0x04) streams: the header and the first segment are verified
// before Open returns, each later segment before its first byte is read; a
// 0x04 blob opens only as id (its segments' AAD carries it). A 0x01/0x02 blob
// (written before WP-R8-5, ≤ 256 MiB by construction) is read whole and
// opened by the compatibility opener — the only whole-object buffer left on
// an SSE-S3 path.
func (s *SSEService) Open(ctx context.Context, id StreamIdentity, src io.Reader) (io.Reader, error) {
	tenantID := id.Tenant
	var first [1]byte
	if _, err := io.ReadFull(src, first[:]); err != nil {
		return nil, fmt.Errorf("encrypted data too short: %w", err)
	}
	r := io.MultiReader(bytes.NewReader(first[:]), src)
	if !IsStreamFormat(first[0]) {
		blob, err := io.ReadAll(io.LimitReader(r, legacySSEMaxPlaintext+SSEOverheadBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read SSE blob: %w", err)
		}
		if int64(len(blob)) > legacySSEMaxPlaintext+SSEOverheadBytes {
			return nil, fmt.Errorf("SSE blob 0x%02x longer than any whole-buffer seal", first[0])
		}
		plaintext, err := s.DecryptBytes(ctx, tenantID, blob)
		if err != nil {
			return nil, err
		}
		s.onOpen(fmt.Sprintf("0x%02x", first[0]))
		return bytes.NewReader(plaintext), nil
	}
	h, err := ReadStreamHeader(r)
	if err != nil {
		return nil, err
	}
	aad, err := openAAD(h, id)
	if err != nil {
		return nil, err
	}
	aead, err := s.streamAEADFor(ctx, tenantID, h)
	if err != nil {
		return nil, err
	}
	o := newStreamOpener(r, aead, h, aad)
	if err := o.prime(); err != nil {
		return nil, err
	}
	s.onOpen(fmt.Sprintf("0x%02x", h.Format))
	return o, nil
}

// OpenRange opens plaintext [rg.Offset, +rg.Length) of a streaming object
// id of size bytes whose header is h (PlanStreamRange made rg); src is the
// stored bytes [rg.CipherOffset, +rg.CipherLength). The first segment is
// verified before it returns.
func (s *SSEService) OpenRange(ctx context.Context, id StreamIdentity, h *StreamHeader, size int64, rg StreamRange, src io.Reader) (io.Reader, error) {
	aad, err := openAAD(h, id)
	if err != nil {
		return nil, err
	}
	aead, err := s.streamAEADFor(ctx, id.Tenant, h)
	if err != nil {
		return nil, err
	}
	r, err := openRange(src, aead, h, aad, size, rg)
	if err != nil {
		return nil, err
	}
	s.onOpen(fmt.Sprintf("0x%02x", h.Format))
	return r, nil
}

// RangeOpener derives the key of a streaming SSE-S3 blob of the object id
// whose header is h once, for many ranges of it (a parallel ranged read).
// Counted as one open.
func (s *SSEService) RangeOpener(ctx context.Context, id StreamIdentity, h *StreamHeader) (*StreamRangeOpener, error) {
	aad, err := openAAD(h, id)
	if err != nil {
		return nil, err
	}
	aead, err := s.streamAEADFor(ctx, id.Tenant, h)
	if err != nil {
		return nil, err
	}
	s.onOpen(fmt.Sprintf("0x%02x", h.Format))
	return &StreamRangeOpener{aead: aead, h: h, aad: aad}, nil
}

// SSEBlobKeyVersion reads the key version a blob was sealed under from its
// first bytes (a 0x03/0x04 header, or a whole 0x01/0x02 blob): the header's
// or the 0x02 field, 1 for a 0x01 blob.
func SSEBlobKeyVersion(data []byte) (int, error) {
	switch {
	case len(data) > 0 && IsStreamFormat(data[0]):
		h, err := ParseStreamHeader(data)
		if err != nil {
			return 0, err
		}
		if h.Kind != StreamKindSSES3 {
			return 0, fmt.Errorf("SSE blob kind 0x%02x has no key version", h.Kind)
		}
		return h.KeyVersion, nil
	case len(data) >= SSEOverheadBytes && data[0] == SSEVersion:
		v := int(binary.BigEndian.Uint32(data[1:sseHeaderBytes]))
		if v < 1 {
			return 0, fmt.Errorf("SSE blob names key version %d", v)
		}
		return v, nil
	case len(data) >= sseV1OverheadBytes && data[0] == SSEVersionV1:
		return 1, nil
	case len(data) > 0 && data[0] != SSEVersion && data[0] != SSEVersionV1:
		return 0, fmt.Errorf("unsupported SSE version: 0x%02x", data[0])
	default:
		return 0, fmt.Errorf("encrypted data too short: %d bytes", len(data))
	}
}

// DecryptBytes opens a whole 0x01/0x02 blob under the key version it
// records: the compatibility opener (Open calls it; no writer makes these).
func (s *SSEService) DecryptBytes(ctx context.Context, tenantID string, data []byte) ([]byte, error) {
	if len(data) > 0 && IsStreamFormat(data[0]) {
		return nil, errors.New("DecryptBytes: a streaming (0x03/0x04) blob opens through Open")
	}
	version, err := SSEBlobKeyVersion(data)
	if err != nil {
		return nil, err
	}
	off, aad := 1, []byte(nil)
	if data[0] == SSEVersion {
		off, aad = sseHeaderBytes, data[:sseHeaderBytes]
	}
	kemCT := data[off : off+mlkem.CiphertextSize768]
	off += mlkem.CiphertextSize768
	nonce := data[off : off+12]
	off += 12
	aesCT := data[off:]

	kv, err := s.keys.Version(ctx, tenantID, version)
	if err != nil {
		return nil, fmt.Errorf("tenant key: %w", err)
	}

	sharedKey, err := kv.KEM.Decapsulate(kemCT)
	if err != nil {
		return nil, fmt.Errorf("ML-KEM decapsulation failed: %w", err)
	}

	aesKey, err := deriveSSEKey(sharedKey)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, aesCT, aad)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM decryption failed: %w", err)
	}

	return plaintext, nil
}

func deriveSSEKey(sharedKey []byte) ([]byte, error) {
	r := hkdf.New(sha256.New, sharedKey, nil, []byte("vaultaire-sse-s3"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("HKDF key derivation: %w", err)
	}
	return key, nil
}
