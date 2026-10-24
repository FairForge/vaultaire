package realm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

type recordKey struct{ tenant, sub string }

type memEntry struct {
	rev uint64
	rec Record
}

// MemoryStore keeps records in memory (tests, labs). Safe for concurrent use.
type MemoryStore struct {
	mu  sync.Mutex
	m   map[recordKey]memEntry
	seq uint64 // the last revision handed out, store-wide (RP52-1)
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[recordKey]memEntry{}} }

func (s *MemoryStore) Load(_ context.Context, tenant, sub string) (Record, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[recordKey{tenant, sub}]
	return e.rec, e.rev, nil
}

func (s *MemoryStore) CompareAndSwap(_ context.Context, tenant, sub string, rev uint64, next Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := recordKey{tenant, sub}
	if s.m[k].rev != rev {
		return ErrConflict
	}
	if next.State == NotRegistered {
		delete(s.m, k)
		return nil
	}
	s.seq++
	s.m[k] = memEntry{rev: s.seq, rec: next}
	return nil
}

// recordsBucket is the one bbolt bucket.
var recordsBucket = []byte("records")

// BoltStore keeps records in a bbolt file; every CompareAndSwap is one
// transaction, fsync'd before it returns (bbolt's default; NoSync is
// refused).
//
// Erasure is logical: bbolt frees the pages of an overwritten or deleted
// record without zeroing them, so the bytes of an erased record can sit in
// free pages until reused. The realm's volume must be encrypted with a key
// that does not rest on the same disk (the attested VM of 41.4); a stolen
// disk is then ciphertext.
type BoltStore struct {
	db *bolt.DB
}

// OpenBoltStore opens (or creates) the file at path, mode 0600.
func OpenBoltStore(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open realm store %s: %w", path, err)
	}
	db.NoSync = false
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(recordsBucket)
		if err != nil {
			return err
		}
		// Revisions come from the bucket's sequence (RP52-1); a file written
		// before holds per-record revisions counted from 1, so the sequence
		// starts past the largest of them.
		var top uint64
		if err := b.ForEach(func(_, v []byte) error {
			rev, _, err := decodeEntry(v)
			top = max(top, rev)
			return err
		}); err != nil {
			return err
		}
		if top > b.Sequence() {
			return b.SetSequence(top)
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init realm store %s: %w", path, err)
	}
	return &BoltStore{db: db}, nil
}

// Close closes the file.
func (s *BoltStore) Close() error { return s.db.Close() }

// boltKey is tenant ‖ 0x00 ‖ sub; a tenant name never holds a NUL.
func boltKey(tenant, sub string) ([]byte, error) {
	if strings.IndexByte(tenant, 0) >= 0 {
		return nil, fmt.Errorf("%w: tenant name", ErrBadRequest)
	}
	return []byte(tenant + "\x00" + sub), nil
}

func (s *BoltStore) Load(ctx context.Context, tenant, sub string) (Record, uint64, error) {
	k, err := boltKey(tenant, sub)
	if err != nil {
		return Record{}, 0, err
	}
	var rec Record
	var rev uint64
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(recordsBucket).Get(k)
		if v == nil {
			return nil
		}
		rev, rec, err = decodeEntry(v)
		return err
	})
	if err == nil {
		err = ctx.Err()
	}
	return rec, rev, err
}

func (s *BoltStore) CompareAndSwap(ctx context.Context, tenant, sub string, rev uint64, next Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := boltKey(tenant, sub)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(recordsBucket)
		var cur uint64
		if v := b.Get(k); v != nil {
			if cur, _, err = decodeEntry(v); err != nil {
				return err
			}
		}
		if cur != rev {
			return ErrConflict
		}
		if next.State == NotRegistered {
			return b.Delete(k)
		}
		// A revision is never reused, a Delete included (RP52-1): counted
		// per record, a deleted-then-re-registered record was back at the
		// revision a stale Recover2 had loaded, and its write put the
		// deleted secret back.
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		return b.Put(k, encodeEntry(seq, next))
	})
}

// The on-disk entry: be64(rev) ‖ format 0x01 ‖ state ‖ the fields in
// declaration order ‖ be16(allowed) ‖ be16(attempted).
const entryFormat = 1

var entrySize = 8 + 1 + 1 + 16 + 32 + (32 + 32 + 64) + 32 + 16 + 32 + len(Record{}.EncryptedSecret) + 16 + 2 + 2

var errCorruptEntry = errors.New("realm: corrupt store entry")

func encodeEntry(rev uint64, r Record) []byte {
	b := make([]byte, 0, entrySize)
	b = binary.BigEndian.AppendUint64(b, rev)
	b = append(b, entryFormat, byte(r.State))
	b = append(b, r.Version[:]...)
	b = append(b, r.OPRFPrivateKey[:]...)
	b = append(b, r.OPRFSignedPublicKey.PublicKey[:]...)
	b = append(b, r.OPRFSignedPublicKey.VerifyingKey[:]...)
	b = append(b, r.OPRFSignedPublicKey.Signature[:]...)
	b = append(b, r.UnlockKeyCommitment[:]...)
	b = append(b, r.UnlockKeyTag[:]...)
	b = append(b, r.EncryptionKeyScalarShare[:]...)
	b = append(b, r.EncryptedSecret[:]...)
	b = append(b, r.EncryptedSecretCommitment[:]...)
	b = binary.BigEndian.AppendUint16(b, r.AllowedGuesses)
	return binary.BigEndian.AppendUint16(b, r.AttemptedGuesses)
}

func decodeEntry(v []byte) (uint64, Record, error) {
	var r Record
	if len(v) != entrySize || v[8] != entryFormat || v[9] > byte(NoGuesses) {
		return 0, r, errCorruptEntry
	}
	rev := binary.BigEndian.Uint64(v)
	r.State = State(v[9])
	p := v[10:]
	take := func(dst []byte) { p = p[copy(dst, p):] }
	take(r.Version[:])
	take(r.OPRFPrivateKey[:])
	take(r.OPRFSignedPublicKey.PublicKey[:])
	take(r.OPRFSignedPublicKey.VerifyingKey[:])
	take(r.OPRFSignedPublicKey.Signature[:])
	take(r.UnlockKeyCommitment[:])
	take(r.UnlockKeyTag[:])
	take(r.EncryptionKeyScalarShare[:])
	take(r.EncryptedSecret[:])
	take(r.EncryptedSecretCommitment[:])
	r.AllowedGuesses = binary.BigEndian.Uint16(p)
	r.AttemptedGuesses = binary.BigEndian.Uint16(p[2:])
	return rev, r, nil
}
