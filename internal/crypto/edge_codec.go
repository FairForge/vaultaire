package crypto

// The edge codec's cryptography (WP-EDGE-2 Part A, plan 40.10 slice 2;
// contract lines 4, 5, 6): what a data-plane host needs to chunk, hash and
// seal a write WITHOUT holding any long-lived key, and what the control
// plane and the origin derive from the one hierarchy (WP-R8-1's tenant key
// versions) to admit, index and read it.
//
// Per tenant key version, two derivations of the version's stored RANDOM
// chunk key — no new stored key, no new column (contract line 4: keys only
// through the one hierarchy; deleting the version's row shreds both):
//
//   - the INDEX key, HKDF-SHA256(chunk key, "stored/index/v1"): the dedup
//     index of codec chunks stores HMAC-SHA256(index key, sha256(chunk))
//     and never the plain hash (contract line 6 — dedup by a keyed index;
//     line 5 — the key is per tenant and version, so nothing converges
//     across tenants by construction);
//   - the CODEC key pair, an X-Wing (ML-KEM-768 + X25519 hybrid,
//     draft-ietf-hpke-pq) seed = HKDF-SHA256(chunk key,
//     "stored/edge-codec/v1"): the PUBLIC key goes to the data-plane host
//     in the admit answer; the private seed never leaves this package's
//     callers on SLC.
//
// A chunk is sealed as ONE 0x06 blob (stream.go's layout): a fresh RANDOM
// 32-byte chunk key per chunk, the 1 MiB segments of the 0x04 construction
// under a DEK derived from it, and the chunk key wrapped to the codec
// public key with HPKE (sdk/go/e2ee's 0x03 sealed box — RFC 9180, X-Wing,
// HKDF-SHA256, ChaCha20-Poly1305; the suite the client key core uses). A
// random key, not a convergent one: the HOST seals before it knows whether
// the chunk dedups, and a dedup hit reuses the FIRST writer's stored blob —
// openable by the tenant whatever host sealed it, because its header wraps
// its own key. The AAD is the header alone (a chunk has no bucket or key to
// bind; the keyed-hash check after the open binds the content, exactly as
// the plaintext-hash check binds an AES256-CE chunk).
//
// What each party knows: the host holds the plaintext it was sent (nothing
// new), one public key, and per-chunk keys it generated — none outlive the
// request. The control plane sees plaintext HASHES for the duration of an
// index call and applies the index key in memory (the RP66-8 correction:
// HMAC(K_tenant, h) cannot be computed from any per-admit HMAC without h,
// so the host sends sha256(chunk) over the service-token channel). The
// index stores keyed hashes only.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"

	"github.com/FairForge/vaultaire/sdk/go/e2ee"
)

const (
	// indexKeyInfo derives a version's index key from its chunk key.
	indexKeyInfo = "stored/index/v1"
	// edgeCodecSeedInfo derives a version's X-Wing seed from its chunk key.
	edgeCodecSeedInfo = "stored/edge-codec/v1"
	// edgeChunkKeyInfo is the e2ee sealed box's info of a wrapped chunk key.
	edgeChunkKeyInfo = "stored/edge-codec/chunk-key/v1"
	// edgeChunkStreamInfo is the DEK derivation's info of a 0x06 blob's
	// segments (HKDF over the chunk key ‖ the header, as every stream kind).
	edgeChunkStreamInfo = "stored/edge-codec/stream-v1"
)

// EdgeChunkEncryptionAlgo is what a 0x06 chunk's index row (and the head row
// of an object made of them) records as its encryption name, beside
// AES256-CE's convergent chunks: AES-256-GCM segments under a random
// per-chunk key wrapped with HPKE (X-Wing) to the tenant's codec key.
const EdgeChunkEncryptionAlgo = "AES256-EW"

// derive32 is HKDF-SHA256(secret, no salt, info) → 32 bytes.
func derive32(secret []byte, info string) ([]byte, error) {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte(info)), out); err != nil {
		return nil, fmt.Errorf("edge codec: derive %s: %w", info, err)
	}
	return out, nil
}

// IndexKeyFrom is a key version's index key. The caller clears it.
func IndexKeyFrom(kv *KeyVersion) ([]byte, error) {
	return derive32(kv.ChunkKey[:], indexKeyInfo)
}

// KeyedChunkHash is a chunk's index identity under a tenant's index key:
// hex of HMAC-SHA256(index key, sha256(chunk plaintext)). The index stores
// THIS, never the plain hash.
func KeyedChunkHash(indexKey []byte, plainHash []byte) (string, error) {
	if len(indexKey) != 32 {
		return "", errors.New("edge codec: an index key is 32 bytes")
	}
	if len(plainHash) != sha256.Size {
		return "", errors.New("edge codec: a chunk hash is SHA-256 (32 bytes)")
	}
	m := hmac.New(sha256.New, indexKey)
	m.Write(plainHash)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// edgeCodecSeed is a key version's X-Wing seed. The caller clears it.
func edgeCodecSeed(kv *KeyVersion) ([]byte, error) {
	return derive32(kv.ChunkKey[:], edgeCodecSeedInfo)
}

// EdgeCodecPublicKey is the tenant's CURRENT codec public key (1216 bytes,
// X-Wing) and the key version it belongs to — what the admit answer carries.
func (k *TenantKeys) EdgeCodecPublicKey(ctx context.Context, tenantID string) ([]byte, int, error) {
	kv, err := k.Current(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	seed, err := edgeCodecSeed(kv)
	if err != nil {
		return nil, 0, err
	}
	defer clear(seed)
	pub, err := e2ee.PublicKey(seed)
	if err != nil {
		return nil, 0, fmt.Errorf("edge codec: public key: %w", err)
	}
	return pub, kv.Version, nil
}

// EdgeCodecPrivateKey is the codec private seed of a RECORDED version — what
// the origin's read path unwraps a 0x06 chunk's key with. The caller clears
// it. A shredded version is ErrNoTenantKey: the crypto-shred covers the
// codec exactly as it covers the chunks and the SSE blobs.
func (k *TenantKeys) EdgeCodecPrivateKey(ctx context.Context, tenantID string, version int) ([]byte, error) {
	kv, err := k.Version(ctx, tenantID, version)
	if err != nil {
		return nil, err
	}
	return edgeCodecSeed(kv)
}

// IndexKey is a version's index key (version < 1 = the current version,
// whose number is returned). The caller clears it.
func (k *TenantKeys) IndexKey(ctx context.Context, tenantID string, version int) ([]byte, int, error) {
	var kv *KeyVersion
	var err error
	if version < 1 {
		kv, err = k.Current(ctx, tenantID)
	} else {
		kv, err = k.Version(ctx, tenantID, version)
	}
	if err != nil {
		return nil, 0, err
	}
	key, err := IndexKeyFrom(kv)
	if err != nil {
		return nil, 0, err
	}
	return key, kv.Version, nil
}

// WrappedChunkSealedLength is the stored length of a 0x06 blob of size
// plaintext bytes — a pure function, so the control plane signs it as a
// presigned PUT's Content-Length before any byte is sealed.
func WrappedChunkSealedLength(size int64) int64 {
	return StreamCiphertextLength(StreamHeaderWrapped, StreamSegmentSize, size)
}

// SealWrappedChunk seals exactly size bytes of src as one 0x06 blob under a
// fresh random chunk key wrapped to codecPub (the tenant's codec public key
// of keyVersion). The returned reader yields the header, then each segment
// as its plaintext is read; a source that ends early or runs past size
// fails the stream, never a short blob.
func SealWrappedChunk(codecPub []byte, keyVersion int, src io.Reader, size int64) (io.Reader, error) {
	if keyVersion < 1 {
		return nil, fmt.Errorf("edge codec: key version %d", keyVersion)
	}
	chunkKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, chunkKey); err != nil {
		return nil, fmt.Errorf("edge codec: chunk key: %w", err)
	}
	defer clear(chunkKey)
	wrap, err := e2ee.SealTo(codecPub, chunkKey, []byte(edgeChunkKeyInfo), nil)
	if err != nil {
		return nil, fmt.Errorf("edge codec: wrap chunk key: %w", err)
	}
	if len(wrap) != StreamWrapLen {
		return nil, fmt.Errorf("edge codec: a wrapped chunk key is %d bytes, sealed %d", StreamWrapLen, len(wrap))
	}
	material := make([]byte, 0, 4+StreamWrapLen)
	material = binary.BigEndian.AppendUint32(material, uint32(keyVersion)) // #nosec G115 -- versions are small positive ints
	material = append(material, wrap...)
	header, err := newStreamHeaderFormat(StreamFormatWrapped, StreamKindWrapped, material, StreamSegmentSize)
	if err != nil {
		return nil, err
	}
	aead, err := streamAEAD(chunkKey, nil, edgeChunkStreamInfo, header)
	if err != nil {
		return nil, err
	}
	return newStreamSealer(src, aead, header, streamAAD(header, StreamIdentity{}), StreamSegmentSize, size)
}

// OpenWrappedChunk opens a whole 0x06 blob: the header read and its wrapped
// chunk key opened under codecPriv (the codec private seed of the version
// the READER's row records — the header's version is a belt, not the
// authority), then the segments as a 0x04-layout stream. The header and the
// first segment are verified before it returns; the caller checks the
// plaintext's keyed hash against the row, which is what binds the content.
func OpenWrappedChunk(codecPriv []byte, src io.Reader) (io.Reader, *StreamHeader, error) {
	h, err := ReadStreamHeader(src)
	if err != nil {
		return nil, nil, err
	}
	if h.Kind != StreamKindWrapped {
		return nil, nil, fmt.Errorf("%w: kind 0x%02x is not a wrapped chunk", ErrStreamCorrupt, h.Kind)
	}
	chunkKey, err := e2ee.OpenSealed(codecPriv, h.Wrap, []byte(edgeChunkKeyInfo), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: the chunk key does not unwrap", ErrStreamCorrupt)
	}
	defer clear(chunkKey)
	aead, err := streamAEAD(chunkKey, nil, edgeChunkStreamInfo, h.Raw)
	if err != nil {
		return nil, nil, err
	}
	o := newStreamOpener(src, aead, h, streamAAD(h.Raw, StreamIdentity{}))
	if err := o.prime(); err != nil {
		return nil, nil, err
	}
	return o, h, nil
}
