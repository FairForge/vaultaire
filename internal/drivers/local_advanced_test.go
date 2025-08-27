package drivers

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestLocalDriver_Checksums(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	testContent := []byte("test content for checksum")

	t.Run("GetChecksum", func(t *testing.T) {
		err := driver.Put(ctx, "container", "test.txt", bytes.NewReader(testContent))
		require.NoError(t, err)

		// Get MD5 checksum
		checksum, err := driver.GetChecksum(ctx, "container", "test.txt", ChecksumMD5)
		require.NoError(t, err)
		assert.NotEmpty(t, checksum)

		// Get SHA256 checksum
		checksum256, err := driver.GetChecksum(ctx, "container", "test.txt", ChecksumSHA256)
		require.NoError(t, err)
		assert.NotEmpty(t, checksum256)
		assert.NotEqual(t, checksum, checksum256) // Different algorithms = different results
	})

	t.Run("VerifyChecksum", func(t *testing.T) {
		// First get the correct checksum
		checksum, err := driver.GetChecksum(ctx, "container", "test.txt", ChecksumSHA256)
		require.NoError(t, err)

		// Verify with correct checksum
		err = driver.VerifyChecksum(ctx, "container", "test.txt", checksum, ChecksumSHA256)
		assert.NoError(t, err)

		// Verify with wrong checksum
		err = driver.VerifyChecksum(ctx, "container", "test.txt", "wrongchecksum", ChecksumSHA256)
		assert.Error(t, err)
	})
}

func TestLocalDriver_DirectoryOperations(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("CreateAndRemoveDirectory", func(t *testing.T) {
		// Create directory
		err := driver.CreateDirectory(ctx, "container", "mydir")
		require.NoError(t, err)

		// Verify it exists
		exists, err := driver.DirectoryExists(ctx, "container", "mydir")
		require.NoError(t, err)
		assert.True(t, exists)

		// Remove directory
		err = driver.RemoveDirectory(ctx, "container", "mydir")
		require.NoError(t, err)

		// Verify it's gone
		exists, err = driver.DirectoryExists(ctx, "container", "mydir")
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("ListDirectory", func(t *testing.T) {
		// Create structure
		driver.Put(ctx, "container", "dir1/file1.txt", strings.NewReader("content1"))
		driver.Put(ctx, "container", "dir1/file2.txt", strings.NewReader("content2"))
		driver.Put(ctx, "container", "dir1/subdir/file3.txt", strings.NewReader("content3"))

		// List directory
		entries, err := driver.ListDirectory(ctx, "container", "dir1")
		require.NoError(t, err)
		assert.Len(t, entries, 3) // 2 files + 1 subdir
	})
}

func TestLocalDriver_DirectoryTraversal(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	// Setup test structure
	driver.Put(ctx, "container", "file1.txt", strings.NewReader("root"))
	driver.Put(ctx, "container", "dir1/file2.txt", strings.NewReader("level1"))
	driver.Put(ctx, "container", "dir1/dir2/file3.txt", strings.NewReader("level2"))
	driver.Put(ctx, "container", "dir1/dir2/dir3/file4.txt", strings.NewReader("level3"))

	t.Run("WalkDirectory", func(t *testing.T) {
		var files []string
		err := driver.WalkDirectory(ctx, "container", "", func(path string, entry os.DirEntry) error {
			if !entry.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		require.NoError(t, err)
		assert.Len(t, files, 4)
		assert.Contains(t, files, "file1.txt")
		assert.Contains(t, files, filepath.Join("dir1", "file2.txt"))
	})

	t.Run("GetDirectorySize", func(t *testing.T) {
		size, err := driver.GetDirectorySize(ctx, "container", "dir1")
		require.NoError(t, err)
		assert.Greater(t, size, int64(0))
	})
}

func TestLocalDriver_DirectoryIndexing(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	// Create test structure
	driver.Put(ctx, "container", "docs/readme.txt", strings.NewReader("readme"))
	driver.Put(ctx, "container", "docs/guide.pdf", strings.NewReader("guide"))
	driver.Put(ctx, "container", "images/photo1.jpg", strings.NewReader("photo1"))
	driver.Put(ctx, "container", "images/photo2.png", strings.NewReader("photo2"))

	t.Run("IndexDirectory", func(t *testing.T) {
		index, err := driver.IndexDirectory(ctx, "container", "")
		require.NoError(t, err)
		assert.NotNil(t, index)
		assert.Equal(t, 4, index.FileCount)
		assert.Equal(t, 2, index.DirCount)
	})

	t.Run("FindFilesByExtension", func(t *testing.T) {
		files, err := driver.FindFilesByExtension(ctx, "container", ".txt")
		require.NoError(t, err)
		assert.Len(t, files, 1)
		assert.Contains(t, files[0], "readme.txt")
	})

	t.Run("FindFilesByPattern", func(t *testing.T) {
		files, err := driver.FindFilesByPattern(ctx, "container", "photo*")
		require.NoError(t, err)
		assert.Len(t, files, 2)
	})
}

func TestLocalDriver_DirectorySync(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("SyncDirectories", func(t *testing.T) {
		// Create source structure
		driver.Put(ctx, "source", "file1.txt", strings.NewReader("content1"))
		driver.Put(ctx, "source", "dir1/file2.txt", strings.NewReader("content2"))

		// Sync to destination
		err := driver.SyncDirectory(ctx, "source", "", "dest", "")
		require.NoError(t, err)

		// Verify destination has same structure
		reader, err := driver.Get(ctx, "dest", "file1.txt")
		require.NoError(t, err)
		content, _ := io.ReadAll(reader)
		reader.Close()
		assert.Equal(t, "content1", string(content))

		reader, err = driver.Get(ctx, "dest", "dir1/file2.txt")
		require.NoError(t, err)
		reader.Close()
	})

	t.Run("CompareDirectories", func(t *testing.T) {
		// Compare identical directories
		diff, err := driver.CompareDirectories(ctx, "source", "", "dest", "")
		require.NoError(t, err)
		assert.Empty(t, diff.Added)
		assert.Empty(t, diff.Modified)
		assert.Empty(t, diff.Deleted)

		// Add file to dest
		driver.Put(ctx, "dest", "extra.txt", strings.NewReader("extra"))

		// Compare again
		diff, err = driver.CompareDirectories(ctx, "source", "", "dest", "")
		require.NoError(t, err)
		assert.Len(t, diff.Added, 1)
	})
}

func TestLocalDriver_DirectoryMonitoring(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("GetDirectoryModTime", func(t *testing.T) {
		// Create directory with files
		driver.Put(ctx, "container", "testdir/file1.txt", strings.NewReader("content"))

		// Get directory modification time
		modTime, err := driver.GetDirectoryModTime(ctx, "container", "testdir")
		require.NoError(t, err)
		assert.NotZero(t, modTime)

		// Sleep briefly then add new file
		time.Sleep(10 * time.Millisecond)
		driver.Put(ctx, "container", "testdir/file2.txt", strings.NewReader("new"))

		// Mod time should be newer
		newModTime, err := driver.GetDirectoryModTime(ctx, "container", "testdir")
		require.NoError(t, err)
		assert.True(t, newModTime.After(modTime))
	})

	t.Run("HasDirectoryChanged", func(t *testing.T) {
		// Get initial state
		since := time.Now()

		// Add file after timestamp
		time.Sleep(10 * time.Millisecond)
		driver.Put(ctx, "container", "testdir/file3.txt", strings.NewReader("newer"))

		// Check if changed
		changed, err := driver.HasDirectoryChanged(ctx, "container", "testdir", since)
		require.NoError(t, err)
		assert.True(t, changed)
	})
}

func TestLocalDriver_AtomicOperations(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("AtomicWrite", func(t *testing.T) {
		// Atomic write should either fully succeed or fully fail
		err := driver.AtomicWrite(ctx, "container", "atomic.txt", strings.NewReader("atomic content"))
		require.NoError(t, err)

		// Verify file exists with correct content
		reader, err := driver.Get(ctx, "container", "atomic.txt")
		require.NoError(t, err)
		content, _ := io.ReadAll(reader)
		reader.Close()
		assert.Equal(t, "atomic content", string(content))
	})

	t.Run("AtomicRename", func(t *testing.T) {
		// Create original file
		err := driver.Put(ctx, "container", "original.txt", strings.NewReader("original content"))
		require.NoError(t, err)

		// Atomic rename
		err = driver.AtomicRename(ctx, "container", "original.txt", "renamed.txt")
		require.NoError(t, err)

		// Original should not exist
		_, err = driver.Get(ctx, "container", "original.txt")
		assert.Error(t, err)

		// Renamed should exist with same content
		reader, err := driver.Get(ctx, "container", "renamed.txt")
		require.NoError(t, err)
		content, _ := io.ReadAll(reader)
		reader.Close()
		assert.Equal(t, "original content", string(content))

	t.Run("AtomicDelete", func(t *testing.T) {
		// Create file
		err := driver.Put(ctx, "container", "to-delete.txt", strings.NewReader("delete me"))
		require.NoError(t, err)
		
		// Atomic delete with trash/backup
		err = driver.AtomicDelete(ctx, "container", "to-delete.txt")
		require.NoError(t, err)
		
		// File should not exist
		_, err = driver.Get(ctx, "container", "to-delete.txt")
		assert.Error(t, err)
	})	})
}
