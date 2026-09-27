package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R7-06: the local List ignored `prefix`, returned in-flight `.tmp-*`
// temp files as objects, and did not resolve the container through
// resolvePath. R7-20: Exists did not either.
func TestLocalDriver_List_PrefixTempFilesAndTraversal(t *testing.T) {
	base := t.TempDir()
	d := NewLocalDriver(base, zap.NewNop())
	ctx := context.Background()

	// ".tmp-notes" is a legitimate customer key that merely resembles a temp name.
	for _, k := range []string{"a/1", "a/2", "b/3", "top", ".tmp-notes"} {
		require.NoError(t, d.Put(ctx, "t_bucket", k, strings.NewReader(k)))
	}
	// An upload in flight: AtomicWrite's sibling temp file.
	require.NoError(t, os.WriteFile(filepath.Join(base, "t_bucket", "a", ".tmp-123456"), []byte("partial"), 0600))
	// A legacy sidecar the old code already hid.
	require.NoError(t, os.WriteFile(filepath.Join(base, "t_bucket", "top.meta"), []byte("{}"), 0600))
	// A file outside the base that a traversing container must never list.
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(base), "outside-"+filepath.Base(base)), []byte("x"), 0600))

	all, err := d.List(ctx, "t_bucket", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a/1", "a/2", "b/3", "top", ".tmp-notes"}, all, "no temp files, no sidecars, slash-separated; look-alike customer keys stay")

	onlyA, err := d.List(ctx, "t_bucket", "a/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a/1", "a/2"}, onlyA)

	none, err := d.List(ctx, "t_bucket", "zzz")
	require.NoError(t, err)
	assert.Empty(t, none)

	missing, err := d.List(ctx, "no_such_container", "")
	require.NoError(t, err, "a missing container is an empty listing, not an error")
	assert.Empty(t, missing)

	escaped, err := d.List(ctx, "..", "")
	require.NoError(t, err, "an escaping container cannot exist: empty, not an error")
	assert.Empty(t, escaped)
}

func TestLocalDriver_Exists_UsesResolvePath(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(base, 0750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "victim.txt"), []byte("keep"), 0600))
	d := NewLocalDriver(base, zap.NewNop())
	ctx := context.Background()

	require.NoError(t, d.Put(ctx, "t_bucket", "here.txt", strings.NewReader("x")))
	ok, err := d.Exists(ctx, "t_bucket", "here.txt")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = d.Exists(ctx, "t_bucket", "../../victim.txt")
	require.NoError(t, err, "a key that cannot exist is a miss, not an error")
	assert.False(t, ok, "must not report a file outside the data directory")

	ok, err = d.Exists(ctx, "t_bucket", "absent.txt")
	require.NoError(t, err)
	assert.False(t, ok)
}
