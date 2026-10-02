package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R8-7: the one address of a chunk blob, and the single-backend calls the
// chunk store and the chunk move address it with.

func TestChunkContext_IsTheReservedTenant(t *testing.T) {
	ctx := ChunkContext(common.WithTenantID(context.Background(), "tenant-a"))
	assert.Equal(t, ChunkAddressTenant, common.GetTenantID(ctx), "whoever was asking")
	assert.True(t, IsReservedTenantID(ChunkAddressTenant))
	assert.Equal(t, "tenant-a", common.GetTenantID(LegacyChunkContext(context.Background(), "tenant-a")))
}

func TestPut_RefusesAChunkBlobOutsideTheChunkContext(t *testing.T) {
	// Arrange
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "p"})
	d := &stubDriver{name: "p", data: map[string][]byte{}, healthy: true}
	eng.AddDriver("p", d)

	// Act
	_, errTenant := eng.Put(common.WithTenantID(context.Background(), "tenant-a"), ChunkContainer, "_chunks/h", bytes.NewReader([]byte("x")))
	_, errNone := eng.Put(context.Background(), ChunkContainer, "_chunks/h", bytes.NewReader([]byte("x")))
	errOn := eng.PutOn(common.WithTenantID(context.Background(), "tenant-a"), "p", ChunkContainer, "_chunks/h", bytes.NewReader([]byte("x")))
	bn, errOK := eng.Put(ChunkContext(context.Background()), ChunkContainer, "_chunks/h", bytes.NewReader([]byte("x")))

	// Assert: refused as the caller's error (never a breaker charge), nothing
	// stored; the one context goes through.
	for _, err := range []error{errTenant, errNone, errOn} {
		require.ErrorIs(t, err, ErrChunkAddress)
		assert.ErrorIs(t, err, ErrInvalidInput)
		assert.False(t, isBackendFailure(err))
	}
	require.NoError(t, errOK)
	assert.Equal(t, "p", bn)
	assert.Len(t, d.data, 1)
	assert.Equal(t, "closed", eng.GetFailoverStatus()["p"])
}

// The *On calls ask ONE backend and report its own verdict.
func TestOn_AsksTheNamedBackendOnly(t *testing.T) {
	// Arrange: the blob is on "other" only; "p" is the primary.
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "p"})
	p := &stubDriver{name: "p", data: map[string][]byte{}, healthy: true}
	other := &stubDriver{name: "other", data: map[string][]byte{ChunkContainer + "/_chunks/h": []byte("blob")}, healthy: true}
	eng.AddDriver("p", p)
	eng.AddDriver("other", other)
	ctx := ChunkContext(context.Background())

	// Act + Assert: found on the backend that has it.
	rc, err := eng.GetOn(ctx, "other", ChunkContainer, "_chunks/h")
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "blob", string(b))

	// A miss on the named backend is a miss — Get's candidate walk would
	// have gone on to "other" and found it; this must not.
	_, err = eng.GetOn(ctx, "p", ChunkContainer, "_chunks/h")
	var nf *NotFoundError
	require.True(t, errors.As(err, &nf), "%v", err)
	assert.False(t, errors.Is(err, ErrAllBackendsUnavailable))

	// A backend nobody registered is not a miss.
	_, err = eng.GetOn(ctx, "nope", ChunkContainer, "_chunks/h")
	require.ErrorIs(t, err, ErrBackendNotRegistered)
	assert.False(t, errors.As(err, &nf))
	_, err = eng.ExistsOn(ctx, "nope", ChunkContainer, "_chunks/h")
	assert.ErrorIs(t, err, ErrBackendNotRegistered)
	assert.ErrorIs(t, eng.DeleteOn(ctx, "nope", ChunkContainer, "_chunks/h"), ErrBackendNotRegistered)
}

func TestOn_GoesThroughTheBackendsBreaker(t *testing.T) {
	// Arrange: a backend that fails hard, five times.
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "p"})
	d := &mockDriver{name: "p", getErr: errors.New("connection refused")}
	eng.AddDriver("p", d)
	ctx := ChunkContext(context.Background())
	for i := 0; i < 5; i++ {
		_, err := eng.GetOn(ctx, "p", ChunkContainer, "_chunks/h")
		require.ErrorIs(t, err, ErrAllBackendsUnavailable, "a hard failure of the only backend asked")
	}

	// Act: the breaker is open now; the backend has recovered.
	d.getErr = nil
	_, err := eng.GetOn(ctx, "p", ChunkContainer, "_chunks/h")

	// Assert: unavailable — never a miss.
	require.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Equal(t, StateOpen.String(), eng.GetFailoverStatus()["p"])
}
