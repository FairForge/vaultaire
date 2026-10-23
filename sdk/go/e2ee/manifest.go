package e2ee

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20poly1305"
)

// Objects and manifests (RP37-15d). An object key is 32 random bytes,
// wrapped under the space key and bound to the space and object ids. It
// seals the object's manifest — the one place that binds what the chunks do
// not (a chunk carries no AD: it must dedup across objects): which chunks,
// in which order, of which sizes, under which keys, of which object, at
// which version, after which previous manifest.
//
//	object key wrap  0x01 box under the space key, AD = label ‖ "object-key" ‖ lp(space id) ‖ lp(object id)
//	manifest         0x01 box under the object key,
//	                 AD = label ‖ "manifest" ‖ lp(space id) ‖ lp(object id) ‖ be64 version
//	  plaintext v1   01 ‖ lp(space id) ‖ lp(object id) ‖ be64 version ‖ previous(32) ‖ compressor(1) ‖
//	                 be64 total size ‖ be32 n ‖ n × (chunk id(32) ‖ SHA-256(chunk)(32) ‖ chunk key(32) ‖
//	                 be32 size ‖ be32 sealed size)
//	manifest hash    SHA-256 of the sealed manifest; version 1 names 32 zero bytes as its previous
//
// The chunk keys travel inside the sealed manifest: the manifest's box IS
// their wrap under the object key (one box, not one per chunk). The chunk's
// SHA-256 travels there too, so a reader can check every chunk against it
// (OpenChunk) without the space key.

const (
	// ManifestV1 is the manifest plaintext's version byte.
	ManifestV1 = byte(0x01)
	// CompressorNone is the only compressor v1 knows: a chunk is sealed as
	// it was cut. A compressor is a new id, never a reinterpretation.
	CompressorNone = byte(0x00)
	// ChunkOverhead is a sealed chunk's bytes beyond its plaintext.
	ChunkOverhead = 1 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead
	// MaxManifestChunks bounds one manifest (~2^22 chunks ≈ 8 TiB at the
	// chunker's 2 MiB average).
	MaxManifestChunks = 1 << 22

	labelObjectKey = label + "object-key"
	labelManifest  = label + "manifest"
	chunkRefSize   = 3*32 + 4 + 4
)

var (
	// ErrManifest: a manifest whose fields disagree with each other or with
	// the space, object and version it was opened for.
	ErrManifest = errors.New("e2ee: manifest is not consistent")
	// ErrRollback: a manifest older than, or forking from, the newest one
	// this client has seen for the object.
	ErrRollback = errors.New("e2ee: manifest is older than one already seen")
)

// ChunkRef is one manifest entry.
type ChunkRef struct {
	ID         []byte // ChunkID: what the server stores it under
	Hash       []byte // SHA-256 of the chunk (never leaves the device in clear)
	Key        []byte // the chunk key (convergent, or random under the paranoid flag)
	Size       uint32 // plaintext length
	SealedSize uint32 // stored length
}

// Manifest is an object's v1 manifest.
type Manifest struct {
	SpaceID    []byte
	ObjectID   []byte
	Version    uint64 // 1, 2, …: one per write of the object
	Previous   []byte // ManifestHash of version-1, or 32 zero bytes for version 1
	Compressor byte
	TotalSize  uint64
	Chunks     []ChunkRef
}

// ManifestHead is the newest manifest a client has seen for an object:
// what makes a rollback detectable.
type ManifestHead struct {
	Version uint64
	Hash    []byte
}

func objectKeyAD(spaceID, objectID []byte) []byte {
	return append(append([]byte(labelObjectKey), lp(spaceID)...), lp(objectID)...)
}

func manifestAD(spaceID, objectID []byte, version uint64) []byte {
	ad := append(append([]byte(labelManifest), lp(spaceID)...), lp(objectID)...)
	return binary.BigEndian.AppendUint64(ad, version)
}

// NewObjectKey is a fresh object key.
func NewObjectKey() ([]byte, error) { return RandomKey() }

// WrapObjectKey wraps an object key under its space key.
func WrapObjectKey(spaceKey, spaceID, objectID, objectKey []byte) ([]byte, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	if err := checkID(objectID); err != nil {
		return nil, err
	}
	return Seal(spaceKey, objectKey, objectKeyAD(spaceID, objectID))
}

// OpenObjectKey opens it, for that space and object only.
func OpenObjectKey(spaceKey, spaceID, objectID, wrapped []byte) ([]byte, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	if err := checkID(objectID); err != nil {
		return nil, err
	}
	return open(FormatBox, spaceKey, wrapped, objectKeyAD(spaceID, objectID))
}

// PrepareChunk is a writer's step for one chunk cut by the frozen chunker:
// its convergent key, its id, its sealed bytes and its manifest entry. The
// chunk's SHA-256 stays in the entry (sealed in the manifest).
func PrepareChunk(spaceKey, chunk []byte) (ChunkRef, []byte, error) {
	if len(chunk) == 0 {
		return ChunkRef{}, nil, errors.New("e2ee: an empty chunk")
	}
	if len(chunk) > MaxChunk {
		return ChunkRef{}, nil, ErrChunkTooLarge
	}
	h := sha256.Sum256(chunk)
	k, err := ChunkKey(spaceKey, h[:])
	if err != nil {
		return ChunkRef{}, nil, err
	}
	id, err := ChunkID(spaceKey, h[:])
	if err != nil {
		return ChunkRef{}, nil, err
	}
	sealed, err := SealChunk(k, chunk)
	if err != nil {
		return ChunkRef{}, nil, err
	}
	size := uint32(len(chunk)) // ≤ MaxChunk, checked above
	return ChunkRef{ID: id, Hash: h[:], Key: k, Size: size, SealedSize: size + ChunkOverhead}, sealed, nil
}

// check is every rule of a v1 manifest, applied when it is sealed and when
// it is opened.
func (m *Manifest) check() error {
	if checkID(m.SpaceID) != nil || checkID(m.ObjectID) != nil {
		return ErrID
	}
	if m.Version == 0 || len(m.Previous) != sha256.Size {
		return ErrManifest
	}
	if zero := bytes.Equal(m.Previous, make([]byte, sha256.Size)); zero != (m.Version == 1) {
		return ErrManifest // version 1 has no previous; every later one names it
	}
	if m.Compressor != CompressorNone {
		return ErrFormat
	}
	if len(m.Chunks) > MaxManifestChunks {
		return ErrManifest
	}
	var total uint64
	for _, c := range m.Chunks {
		if len(c.ID) != 32 || len(c.Hash) != 32 || len(c.Key) != KeySize ||
			c.Size == 0 || c.Size > MaxChunk || c.SealedSize != c.Size+ChunkOverhead {
			return ErrManifest
		}
		total += uint64(c.Size)
	}
	if total != m.TotalSize {
		return ErrManifest
	}
	return nil
}

func (m *Manifest) encode() []byte {
	out := make([]byte, 0, 1+2+len(m.SpaceID)+2+len(m.ObjectID)+8+32+1+8+4+len(m.Chunks)*chunkRefSize)
	out = append(out, ManifestV1)
	out = append(out, lp(m.SpaceID)...)
	out = append(out, lp(m.ObjectID)...)
	out = binary.BigEndian.AppendUint64(out, m.Version)
	out = append(out, m.Previous...)
	out = append(out, m.Compressor)
	out = binary.BigEndian.AppendUint64(out, m.TotalSize)
	out = binary.BigEndian.AppendUint32(out, uint32(len(m.Chunks))) // ≤ MaxManifestChunks, checked
	for _, c := range m.Chunks {
		out = append(out, c.ID...)
		out = append(out, c.Hash...)
		out = append(out, c.Key...)
		out = binary.BigEndian.AppendUint32(out, c.Size)
		out = binary.BigEndian.AppendUint32(out, c.SealedSize)
	}
	return out
}

// decodeManifest parses a v1 plaintext; check() validates it.
func decodeManifest(b []byte) (*Manifest, error) {
	r := &cursor{b: b}
	if v := r.next(1); v == nil || v[0] != ManifestV1 {
		return nil, ErrFormat
	}
	m := &Manifest{SpaceID: r.field(), ObjectID: r.field()}
	m.Version = r.u64()
	m.Previous = r.next(32)
	if c := r.next(1); c != nil {
		m.Compressor = c[0]
	}
	m.TotalSize = r.u64()
	n := r.u32()
	if r.bad || n > MaxManifestChunks || uint64(n)*chunkRefSize != uint64(len(r.b)) {
		return nil, ErrManifest // short, a truncated list, or bytes after it
	}
	m.Chunks = make([]ChunkRef, n)
	for i := range m.Chunks {
		m.Chunks[i] = ChunkRef{ID: r.next(32), Hash: r.next(32), Key: r.next(32), Size: r.u32(), SealedSize: r.u32()}
	}
	return m, nil
}

// cursor reads a fixed encoding front to back; a short read sets bad.
type cursor struct {
	b   []byte
	bad bool
}

func (c *cursor) next(n int) []byte {
	if c.bad || n > len(c.b) {
		c.bad = true
		return nil
	}
	out := c.b[:n:n]
	c.b = c.b[n:]
	return out
}

func (c *cursor) field() []byte {
	n := c.next(2)
	if n == nil {
		return nil
	}
	return c.next(int(binary.BigEndian.Uint16(n)))
}

func (c *cursor) u32() uint32 {
	if b := c.next(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (c *cursor) u64() uint64 {
	if b := c.next(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}

// SealManifest seals a manifest under its object key, its space, object and
// version in the AD.
func SealManifest(objectKey []byte, m *Manifest) ([]byte, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return Seal(objectKey, m.encode(), manifestAD(m.SpaceID, m.ObjectID, m.Version))
}

// ManifestHash is what the next version names as its previous, and what a
// client remembers in its ManifestHead.
func ManifestHash(sealed []byte) []byte {
	h := sha256.Sum256(sealed)
	return h[:]
}

// OpenManifest opens the manifest of version `version` of object objectID
// in space spaceID. A manifest of another space, object or version does not
// open (the AD). With seen — the newest manifest this client has seen for
// the object — an older version, the same version with other bytes (a
// fork) and a next version that does not name seen as its previous are
// ErrRollback. The caller then remembers {version, ManifestHash(sealed)}.
func OpenManifest(objectKey, sealed, spaceID, objectID []byte, version uint64, seen *ManifestHead) (*Manifest, error) {
	if err := checkID(spaceID); err != nil {
		return nil, err
	}
	if err := checkID(objectID); err != nil {
		return nil, err
	}
	pt, err := open(FormatBox, objectKey, sealed, manifestAD(spaceID, objectID, version))
	if err != nil {
		return nil, err
	}
	m, err := decodeManifest(pt)
	if err != nil {
		return nil, err
	}
	if err := m.check(); err != nil {
		return nil, err
	}
	if !bytes.Equal(m.SpaceID, spaceID) || !bytes.Equal(m.ObjectID, objectID) || m.Version != version {
		return nil, ErrManifest
	}
	if seen != nil {
		hash := ManifestHash(sealed)
		switch {
		case version < seen.Version:
			return nil, ErrRollback
		case version == seen.Version && subtle.ConstantTimeCompare(hash, seen.Hash) != 1:
			return nil, ErrRollback
		case version == seen.Version+1 && subtle.ConstantTimeCompare(m.Previous, seen.Hash) != 1:
			return nil, ErrRollback
		}
	}
	return m, nil
}

// OpenChunkAt opens the i-th chunk of the object as the server served it:
// its stored length, its key and its SHA-256 must all be the manifest's
// i-th entry, so a chunk served out of order, from another object, or
// poisoned by a member is refused.
func (m *Manifest) OpenChunkAt(i int, sealed []byte) ([]byte, error) {
	if i < 0 || i >= len(m.Chunks) {
		return nil, ErrManifest
	}
	c := m.Chunks[i]
	if uint64(len(sealed)) != uint64(c.SealedSize) {
		return nil, ErrChunkMismatch
	}
	return OpenChunk(c.Key, sealed, c.Hash)
}
