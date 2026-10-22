// Package translog is the transparency log (keystone K4, plan Phase 43.6,
// decision F-16, docs/ARCHITECTURE_CONTRACT.md rule 7): an append-only
// Merkle log in the RFC 6962 / RFC 9162 shape whose checkpoints are C2SP
// signed notes (c2sp.org/tlog-checkpoint, c2sp.org/signed-note) and whose
// tiles are served in the c2sp.org/tlog-tiles layout.
//
// No Merkle or proof arithmetic is written here. Hashing, stored hashes,
// roots, proofs and tile contents are golang.org/x/mod/sumdb/tlog (the Go
// checksum database's log, RFC 6962 hashing); notes are
// golang.org/x/mod/sumdb/note; the checkpoint body is
// github.com/transparency-dev/formats/log; proofs are VERIFIED by
// github.com/transparency-dev/merkle/proof — a second, independent
// implementation, so a proof this package produces is checked by code it
// did not come from. This package is pure: storage is the caller's
// (internal/translog/store for Postgres, Memory for tests and tools).
package translog

import (
	"errors"
	"fmt"

	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"
	"golang.org/x/mod/sumdb/tlog"
)

const (
	// Origin is the log's name: the first line of every checkpoint and the
	// name of its signing key, so it is inside every signature. Decided
	// once (Stage 1a Prompt 28); a new origin is a new log.
	Origin = "stored.cloud/translog/v1"
	// HashSize is the size of a node hash (SHA-256).
	HashSize = tlog.HashSize
	// MaxEntrySize: an entry bundle prefixes each entry with a uint16
	// length (c2sp.org/tlog-tiles), so no entry may be longer.
	MaxEntrySize = 1<<16 - 1
)

// Hash is a node hash.
type Hash = tlog.Hash

// HashReader returns stored hashes by storage index (tlog's layout:
// StoredHashIndex(level, n)).
type HashReader = tlog.HashReader

// HashReaderFunc adapts a function to HashReader.
type HashReaderFunc = tlog.HashReaderFunc

var (
	// ErrEntryTooLarge: a body longer than MaxEntrySize.
	ErrEntryTooLarge = errors.New("translog: entry larger than 65535 bytes")
	// ErrBadSize: a tree size or index out of range.
	ErrBadSize = errors.New("translog: invalid tree size or index")
)

// LeafHash is RFC 6962's leaf hash, SHA-256(0x00 ‖ body).
func LeafHash(body []byte) Hash { return tlog.RecordHash(body) }

// EmptyRoot is the root of the empty tree, SHA-256 of nothing (RFC 6962 § 2.1).
func EmptyRoot() Hash {
	h, _ := tlog.TreeHash(0, nil)
	return h
}

// StoredHashCount is how many hashes a log of n entries has stored.
func StoredHashCount(n int64) int64 { return tlog.StoredHashCount(n) }

// StoredHashes returns what appending body as entry n must persist: the
// hashes to store at storage indexes first, first+1, …; r must hold the
// hashes stored for entries 0…n-1.
func StoredHashes(n int64, body []byte, r HashReader) (first int64, hashes []Hash, err error) {
	if n < 0 {
		return 0, nil, ErrBadSize
	}
	if len(body) > MaxEntrySize {
		return 0, nil, ErrEntryTooLarge
	}
	hashes, err = tlog.StoredHashes(n, body, r)
	if err != nil {
		return 0, nil, fmt.Errorf("translog: stored hashes of entry %d: %w", n, err)
	}
	return tlog.StoredHashCount(n), hashes, nil
}

// Root is the root hash of the tree of the first size entries.
func Root(size int64, r HashReader) (Hash, error) {
	if size < 0 {
		return Hash{}, ErrBadSize
	}
	h, err := tlog.TreeHash(size, r)
	if err != nil {
		return Hash{}, fmt.Errorf("translog: root of size %d: %w", size, err)
	}
	return h, nil
}

// InclusionProof proves entry index is in the tree of size entries.
func InclusionProof(index, size int64, r HashReader) ([]Hash, error) {
	if index < 0 || index >= size {
		return nil, ErrBadSize
	}
	p, err := tlog.ProveRecord(size, index, r)
	if err != nil {
		return nil, fmt.Errorf("translog: inclusion proof of %d in %d: %w", index, size, err)
	}
	return p, nil
}

// ConsistencyProof proves the tree of oldSize entries is a prefix of the
// tree of newSize entries. Empty for oldSize 0 or oldSize == newSize.
func ConsistencyProof(oldSize, newSize int64, r HashReader) ([]Hash, error) {
	if oldSize < 0 || oldSize > newSize {
		return nil, ErrBadSize
	}
	if oldSize == 0 || oldSize == newSize {
		return []Hash{}, nil
	}
	p, err := tlog.ProveTree(newSize, oldSize, r)
	if err != nil {
		return nil, fmt.Errorf("translog: consistency proof %d→%d: %w", oldSize, newSize, err)
	}
	return p, nil
}

var hasher = rfc6962.DefaultHasher

// VerifyInclusion checks an inclusion proof with transparency-dev/merkle.
func VerifyInclusion(p []Hash, index, size int64, leaf, root Hash) error {
	if index < 0 || size < 0 {
		return ErrBadSize
	}
	if err := proof.VerifyInclusion(hasher, uint64(index), uint64(size), leaf[:], slices(p), root[:]); err != nil {
		return fmt.Errorf("translog: inclusion of %d in %d: %w", index, size, err)
	}
	return nil
}

// VerifyConsistency checks a consistency proof with transparency-dev/merkle:
// nil means the tree of newSize entries with newRoot extends the tree of
// oldSize entries with oldRoot — nothing in the old tree was rewritten.
func VerifyConsistency(p []Hash, oldSize, newSize int64, oldRoot, newRoot Hash) error {
	if oldSize < 0 || newSize < 0 {
		return ErrBadSize
	}
	if err := proof.VerifyConsistency(hasher, uint64(oldSize), uint64(newSize), slices(p), oldRoot[:], newRoot[:]); err != nil {
		return fmt.Errorf("translog: consistency %d→%d: %w", oldSize, newSize, err)
	}
	return nil
}

func slices(p []Hash) [][]byte {
	out := make([][]byte, len(p))
	for i := range p {
		out[i] = p[i][:]
	}
	return out
}

// Memory is an in-process log: the reference store for tests and tools
// (translog-verify rebuilds a tree from fetched entries with it).
type Memory struct {
	hashes []Hash
	bodies [][]byte
}

// Append adds body as the next entry and returns its index.
func (m *Memory) Append(body []byte) (int64, error) {
	n := int64(len(m.bodies))
	_, hs, err := StoredHashes(n, body, m)
	if err != nil {
		return 0, err
	}
	m.hashes = append(m.hashes, hs...)
	m.bodies = append(m.bodies, append([]byte(nil), body...))
	return n, nil
}

// Size is the number of entries.
func (m *Memory) Size() int64 { return int64(len(m.bodies)) }

// Entry returns entry i's body.
func (m *Memory) Entry(i int64) []byte { return m.bodies[i] }

// ReadHashes implements HashReader.
func (m *Memory) ReadHashes(indexes []int64) ([]Hash, error) {
	out := make([]Hash, len(indexes))
	for i, x := range indexes {
		if x < 0 || x >= int64(len(m.hashes)) {
			return nil, fmt.Errorf("translog: no stored hash %d", x)
		}
		out[i] = m.hashes[x]
	}
	return out, nil
}
