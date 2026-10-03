package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// storeDriver is a stub that names the store it writes to (StoreIdentifier).
type storeDriver struct {
	stubDriver
	store string
}

func (d *storeDriver) StoreID() string { return d.store }

// WP-R7-5 (the invariant WP-R13-2 left as a comment): two registered names
// never share a store. A stale copy is deleted "on the backend the key does
// not route to" — when that backend is the same bucket under another name,
// the delete removes the live object.
func TestSharedStores_SameDriverRegisteredTwice(t *testing.T) {
	// Arrange: one driver value under two names (what a tool did with the
	// permafrost fleet as "onedrive" and "permafrost").
	e := NewEngine(nil, zap.NewNop(), nil)
	d := &storeDriver{store: "graph:fleet"}
	e.AddDriver("permafrost", d)
	e.AddDriver("onedrive", d)
	e.AddDriver("local", &storeDriver{store: "dir:/tmp/a"})

	// Act
	shared := e.SharedStores()

	// Assert: exactly one group, both names, sorted; local is alone.
	require.Len(t, shared, 1)
	assert.Equal(t, "graph:fleet", shared[0].Store)
	assert.Equal(t, []string{"onedrive", "permafrost"}, shared[0].Backends)
}

func TestSharedStores_TwoDriversOnTheSameEndpointAndBucket(t *testing.T) {
	// Arrange: two S3 drivers built separately on one endpoint + bucket (a
	// region pair pointing at the primary's bucket), and two on the same
	// endpoint but different buckets (fine).
	e := NewEngine(nil, zap.NewNop(), nil)
	e.AddDriver("idrive", &storeDriver{store: "s3://s3.us-central-1.idrivee2.com/vaultaire"})
	e.AddDriver("idrive-us-west-2", &storeDriver{store: "s3://s3.us-central-1.idrivee2.com/vaultaire"})
	e.AddDriver("r2", &storeDriver{store: "s3://acct.r2.cloudflarestorage.com/vaultaire-public"})
	e.AddDriver("r2-eu", &storeDriver{store: "s3://acct.r2.cloudflarestorage.com/vaultaire-eu"})

	// Act
	shared := e.SharedStores()

	// Assert
	require.Len(t, shared, 1)
	assert.Equal(t, []string{"idrive", "idrive-us-west-2"}, shared[0].Backends)
}

func TestSharedStores_NothingShared(t *testing.T) {
	e := NewEngine(nil, zap.NewNop(), nil)
	e.AddDriver("idrive", &storeDriver{store: "s3://a/vaultaire"})
	e.AddDriver("geyser", &storeDriver{store: "s3://b/tape"})
	// A driver without a StoreID is compared by identity only.
	e.AddDriver("plain", &stubDriver{})
	assert.Empty(t, e.SharedStores())
}
