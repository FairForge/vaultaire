package crypto

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGCICache_EntryExpires: a row another process repointed (the chunk move
// on the admin slot, the key re-encrypt job, the inactive slot during a
// switch) is read again once its cached copy is gciCacheTTL old — it used to
// be served from memory until a restart (Stage 1a Part A3).
func TestGCICache_EntryExpires(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)
	ctx := context.Background()

	gci := NewGlobalContentIndex(db)
	clock := time.Now()
	gci.cache.now = func() time.Time { return clock }

	hash := "ttl0000000000000000000000000000000000000000000000000000000000001"
	require.NoError(t, gci.InsertChunk(ctx, &GCIEntry{
		DedupScope: scope, PlaintextHash: hash, BackendID: "idrive",
		StorageKey: "_chunks/" + hash, SizeBytes: 10, RefCount: 1,
	}))
	first, err := gci.LookupChunk(ctx, scope, hash)
	require.NoError(t, err)
	require.Equal(t, "idrive", first.Entry.BackendID)

	// Another process moves the blob and repoints the row.
	_, err = db.Exec(`UPDATE global_content_index SET backend_id = 'lyve' WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash)
	require.NoError(t, err)

	clock = clock.Add(gciCacheTTL - time.Second)
	cached, err := gci.LookupChunk(ctx, scope, hash)
	require.NoError(t, err)
	assert.Equal(t, "idrive", cached.Entry.BackendID, "inside the TTL the cached row serves (the cache is still a cache)")
	many, err := gci.LookupChunks(ctx, scope, []string{hash})
	require.NoError(t, err)
	assert.Equal(t, "idrive", many[hash].Entry.BackendID)

	clock = clock.Add(2 * time.Second)
	fresh, err := gci.LookupChunk(ctx, scope, hash)
	require.NoError(t, err)
	assert.Equal(t, "lyve", fresh.Entry.BackendID, "past the TTL the row is read again")

	_, err = db.Exec(`UPDATE global_content_index SET backend_id = 'geyser' WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash)
	require.NoError(t, err)
	clock = clock.Add(gciCacheTTL + time.Second)
	many, err = gci.LookupChunks(ctx, scope, []string{hash})
	require.NoError(t, err)
	assert.Equal(t, "geyser", many[hash].Entry.BackendID, "the batch lookup honours the TTL too")
}
