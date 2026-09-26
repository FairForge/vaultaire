package drivers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// R2-04: every engine-facing path builder must refuse a container/artifact
// that resolves outside the driver's base directory. Delete had no guard at
// all; Get/Put used a HasPrefix check that a sibling directory sharing the
// base name's prefix could pass.
func TestLocalDriver_PathEscapeRefused(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(base, 0750))
	victim := filepath.Join(root, "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("keep me"), 0600))
	sibling := filepath.Join(root, "data-sibling")
	require.NoError(t, os.MkdirAll(sibling, 0750))
	siblingFile := filepath.Join(sibling, "keep.txt")
	require.NoError(t, os.WriteFile(siblingFile, []byte("keep me too"), 0600))

	d := NewLocalDriver(base, zap.NewNop())
	ctx := context.Background()

	cases := map[string][2]string{
		"parent via artifact":     {"t_bucket", "../../victim.txt"},
		"parent via container":    {"../victim.txt", ""},
		"sibling prefix":          {"../data-sibling", "keep.txt"},
		"sibling via artifact":    {"t_bucket", "../../data-sibling/keep.txt"},
		"dot-dot inside artifact": {"t_bucket", "a/../../../victim.txt"},
		"the base itself":         {"", ""},
		"base via dot-dot":        {"t_bucket", ".."},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := d.Delete(ctx, c[0], c[1])
			require.Error(t, err, "Delete must refuse")
			assert.False(t, isBackendFailureForTest(err), "escape must not read as a backend failure")
			_, err = d.Get(ctx, c[0], c[1])
			require.Error(t, err, "Get must refuse")
			err = d.Put(ctx, c[0], c[1], strings.NewReader("x"))
			require.Error(t, err, "Put must refuse")
		})
	}
	b, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(b))
	b, err = os.ReadFile(siblingFile)
	require.NoError(t, err)
	assert.Equal(t, "keep me too", string(b))
}

// Legit keys with dot segments that stay inside the base still work — S3
// keys are arbitrary strings.
func TestLocalDriver_DotSegmentsInsideBaseAllowed(t *testing.T) {
	d := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()
	require.NoError(t, d.Put(ctx, "t_bucket", "dir/../file.txt", strings.NewReader("hello")))
	r, err := d.Get(ctx, "t_bucket", "file.txt")
	require.NoError(t, err)
	_ = r.Close()
	require.NoError(t, d.Delete(ctx, "t_bucket", "dir/../file.txt"))
	_, err = d.Get(ctx, "t_bucket", "file.txt")
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

// isBackendFailureForTest mirrors the engine's client-level classification:
// a typed NotFoundError or ErrInvalidInput never charges a breaker.
func isBackendFailureForTest(err error) bool {
	var nf engine.NotFoundError
	return !errors.As(err, &nf) && !errors.Is(err, engine.ErrInvalidInput)
}
