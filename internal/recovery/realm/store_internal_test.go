package realm

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// RP52-1: a file written before the fix holds per-record revisions counted
// from 1. Opened now, the store's sequence starts past the largest of them,
// so a stale write that loaded one of those revisions still conflicts once
// the record is deleted and registered anew.
func TestBoltStore_SequenceStartsPastRevisionsOnDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "realm.db")
	db, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(recordsBucket)
		if err != nil {
			return err
		}
		k, _ := boltKey("stored", "old")
		return b.Put(k, encodeEntry(7, Record{State: Registered, AllowedGuesses: 3}))
	}))
	require.NoError(t, db.Close())

	st, err := OpenBoltStore(path)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	_, rev, err := st.Load(ctx, "stored", "old")
	require.NoError(t, err)
	require.Equal(t, uint64(7), rev)

	require.NoError(t, st.CompareAndSwap(ctx, "stored", "old", rev, Record{}))
	for i := 0; i < 10; i++ { // re-register enough times to reach 7 if counting restarted
		_, r, err := st.Load(ctx, "stored", "old")
		require.NoError(t, err)
		require.NoError(t, st.CompareAndSwap(ctx, "stored", "old", r, Record{State: Registered, AllowedGuesses: 3}))
		assert.NotEqual(t, uint64(7), r, "revision 7 handed out again")
	}
	_, now, err := st.Load(ctx, "stored", "old")
	require.NoError(t, err)
	assert.Greater(t, now, uint64(7))
}
