package dashboard

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMFAPendingStore_Create(t *testing.T) {
	store := NewMFAPendingStore()

	token, err := store.Create(MFAPending{
		UserID: "user-1", Email: "a@b.com", TenantID: "t-1", Role: "user",
	})
	require.NoError(t, err)
	assert.Len(t, token, 64) // 32 bytes hex-encoded
}

func TestMFAPendingStore_GetConsumes(t *testing.T) {
	store := NewMFAPendingStore()

	token, err := store.Create(MFAPending{
		UserID: "user-1", Email: "a@b.com", TenantID: "t-1", Role: "user",
	})
	require.NoError(t, err)

	// First get succeeds.
	p := store.Get(token)
	require.NotNil(t, p)
	assert.Equal(t, "user-1", p.UserID)
	assert.Equal(t, "a@b.com", p.Email)

	// Second get returns nil (consumed).
	p = store.Get(token)
	assert.Nil(t, p)
}

func TestMFAPendingStore_Expired(t *testing.T) {
	store := NewMFAPendingStore()

	token, err := store.Create(MFAPending{
		UserID: "user-1", Email: "a@b.com",
	})
	require.NoError(t, err)

	// Force expiry.
	store.mu.Lock()
	store.entries[token].Expires = time.Now().Add(-1 * time.Second)
	store.mu.Unlock()

	p := store.Get(token)
	assert.Nil(t, p)
}

func TestMFAPendingStore_Peek(t *testing.T) {
	store := NewMFAPendingStore()

	token, err := store.Create(MFAPending{
		UserID: "user-1", Email: "a@b.com",
	})
	require.NoError(t, err)

	// Peek does not consume.
	p := store.Peek(token)
	require.NotNil(t, p)

	p = store.Peek(token)
	require.NotNil(t, p)

	// Get still works.
	p = store.Get(token)
	require.NotNil(t, p)
}

// R5-15d: expired challenges used to stay in memory until their own token
// was presented again — an attacker who could pass the first factor grew the
// store by one entry per attempt forever. Create now sweeps inline.
func TestMFAPendingStore_SweepsExpiredOnCreate(t *testing.T) {
	store := NewMFAPendingStore()
	for i := 0; i < mfaPendingSweepAt; i++ {
		tok, err := store.Create(MFAPending{UserID: "u"})
		require.NoError(t, err)
		store.mu.Lock()
		store.entries[tok].Expires = time.Now().Add(-time.Second)
		store.mu.Unlock()
	}
	require.Equal(t, mfaPendingSweepAt, store.Len())
	_, err := store.Create(MFAPending{UserID: "fresh"})
	require.NoError(t, err)
	assert.Equal(t, 1, store.Len(), "only the fresh challenge survives the sweep")
}
