package drivers

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLocalDriver_SymlinkSupport(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("DetectSymlinkSupport", func(t *testing.T) {
		supported := driver.SupportsSymlinks()
		t.Logf("Filesystem symlink support: %v", supported)
		// Don't fail if unsupported, just log
	})

	t.Run("CreateAndFollowSymlink", func(t *testing.T) {
		if !driver.SupportsSymlinks() {
			t.Skip("Filesystem doesn't support symlinks")
		}

		// Create real file
		err := driver.Put(ctx, "container1", "realfile.txt", strings.NewReader("real content"))
		require.NoError(t, err)

		// Create symlink
		realPath := filepath.Join(testDir, "container1", "realfile.txt")
		linkPath := filepath.Join(testDir, "container1", "link.txt")
		err = os.Symlink(realPath, linkPath)
		require.NoError(t, err)

		// Get via symlink with follow option
		reader, err := driver.GetWithOptions(ctx, "container1", "link.txt", GetOptions{
			FollowSymlinks: true,
		})
		require.NoError(t, err)
		defer reader.Close()

		content, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, "real content", string(content))
	})

	t.Run("DetectSymlinkWithoutFollowing", func(t *testing.T) {
		if !driver.SupportsSymlinks() {
			t.Skip("Filesystem doesn't support symlinks")
		}

		// Ensure symlink exists from previous test or create new
		linkPath := filepath.Join(testDir, "container1", "link.txt")
		if _, err := os.Lstat(linkPath); os.IsNotExist(err) {
			realPath := filepath.Join(testDir, "container1", "realfile.txt")
			os.Symlink(realPath, linkPath)
		}

		info, err := driver.GetInfo(ctx, "container1", "link.txt")
		require.NoError(t, err)
		assert.True(t, info.IsSymlink)
		assert.NotEmpty(t, info.SymlinkTarget)
	})

	t.Run("RejectSymlinkWhenNotFollowing", func(t *testing.T) {
		if !driver.SupportsSymlinks() {
			t.Skip("Filesystem doesn't support symlinks")
		}

		// Try to get symlink without following
		_, err := driver.GetWithOptions(ctx, "container1", "link.txt", GetOptions{
			FollowSymlinks: false,
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "symlink")
	})
}

func TestLocalDriver_FileInfo(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("RegularFileInfo", func(t *testing.T) {
		// Create test file
		testData := []byte("test content")
		err := driver.Put(ctx, "container", "test.txt", bytes.NewReader(testData))
		require.NoError(t, err)

		info, err := driver.GetInfo(ctx, "container", "test.txt")
		require.NoError(t, err)

		assert.Equal(t, "test.txt", info.Name)
		assert.Equal(t, int64(len(testData)), info.Size)
		assert.False(t, info.IsDir)
		assert.False(t, info.IsSymlink)
		assert.NotZero(t, info.ModTime)
	})

	t.Run("NonExistentFileInfo", func(t *testing.T) {
		_, err := driver.GetInfo(ctx, "container", "nonexistent.txt")
		assert.Error(t, err)
	})
}

func TestLocalDriver_FilePermissions(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("SetAndGetPermissions", func(t *testing.T) {
		// Create test file
		err := driver.Put(ctx, "container", "test.txt", strings.NewReader("content"))
		require.NoError(t, err)

		// Set permissions
		err = driver.SetPermissions(ctx, "container", "test.txt", 0644)
		require.NoError(t, err)

		// Get permissions
		mode, err := driver.GetPermissions(ctx, "container", "test.txt")
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0644), mode)

		// Change permissions
		err = driver.SetPermissions(ctx, "container", "test.txt", 0600)
		require.NoError(t, err)

		mode, err = driver.GetPermissions(ctx, "container", "test.txt")
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), mode)
	})

	t.Run("PermissionsOnNonExistent", func(t *testing.T) {
		// Should error on non-existent file
		_, err := driver.GetPermissions(ctx, "container", "nonexistent.txt")
		assert.Error(t, err)

		err = driver.SetPermissions(ctx, "container", "nonexistent.txt", 0644)
		assert.Error(t, err)
	})
}

func TestLocalDriver_FileOwnership(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Ownership tests require root privileges")
	}

	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("SetAndGetOwnership", func(t *testing.T) {
		err := driver.Put(ctx, "container", "test.txt", strings.NewReader("content"))
		require.NoError(t, err)

		// Set ownership (using current user/group as safe test)
		uid := os.Getuid()
		gid := os.Getgid()
		err = driver.SetOwnership(ctx, "container", "test.txt", uid, gid)
		require.NoError(t, err)

		// Get ownership
		gotUid, gotGid, err := driver.GetOwnership(ctx, "container", "test.txt")
		require.NoError(t, err)
		assert.Equal(t, uid, gotUid)
		assert.Equal(t, gid, gotGid)
	})
}

func TestLocalDriver_ExtendedAttributes(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("SetAndGetXAttr", func(t *testing.T) {
		err := driver.Put(ctx, "container", "test.txt", strings.NewReader("content"))
		require.NoError(t, err)

		// Set extended attribute
		err = driver.SetXAttr(ctx, "container", "test.txt", "user.myattr", []byte("myvalue"))
		if err != nil {
			t.Skip("Extended attributes not supported on this filesystem")
		}

		// Get extended attribute
		value, err := driver.GetXAttr(ctx, "container", "test.txt", "user.myattr")
		require.NoError(t, err)
		assert.Equal(t, []byte("myvalue"), value)

		// List extended attributes
		attrs, err := driver.ListXAttrs(ctx, "container", "test.txt")
		require.NoError(t, err)
		assert.Contains(t, attrs, "user.myattr")
	})
}
