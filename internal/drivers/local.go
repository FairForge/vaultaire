package drivers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DriverStats tracks driver statistics
type DriverStats struct {
	Reads  int64
	Writes int64
	Errors int64
}

// LocalDriver implements the Driver interface for local filesystem
type LocalDriver struct {
	basePath     string
	mu           sync.RWMutex
	stats        DriverStats
	transactions map[string]*Transaction
	logger       *zap.Logger
	readerPool   *sync.Pool
	filePool     *sync.Pool
}

// NewLocalDriver creates a new local filesystem driver
func NewLocalDriver(basePath string, logger *zap.Logger) *LocalDriver {
	return &LocalDriver{
		basePath:     basePath,
		transactions: make(map[string]*Transaction),
		logger:       logger,
		readerPool: &sync.Pool{
			New: func() interface{} {
				return bufio.NewReaderSize(nil, 64*1024)
			},
		},
		filePool: &sync.Pool{
			New: func() interface{} {
				return nil
			},
		},
	}
}

// Name returns the driver name
func (d *LocalDriver) Name() string {
	return "local"
}

// Get retrieves an artifact from a container
func (d *LocalDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	d.logger.Debug("LocalDriver.Get",
		zap.String("container", container),
		zap.String("artifact", artifact),
		zap.String("fullPath", fullPath))

	return os.Open(fullPath)
}

// Put stores an artifact in a container
func (d *LocalDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	containerPath := filepath.Join(d.basePath, container)
	if err := os.MkdirAll(containerPath, 0750); err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	fullPath := filepath.Join(d.basePath, container, artifact)

	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			d.logger.Error("failed to close file",
				zap.String("path", fullPath),
				zap.Error(err))
		}
	}()

	_, err = io.Copy(file, data)
	if err != nil {
		return fmt.Errorf("failed to copy data: %w", err)
	}

	return nil
}

// Delete removes an artifact from a container
func (d *LocalDriver) Delete(ctx context.Context, container, artifact string) error {
	fullPath := filepath.Join(d.basePath, container, artifact)
	return os.Remove(fullPath)
}

// List lists artifacts in a container
func (d *LocalDriver) List(ctx context.Context, container string) ([]string, error) {
	containerPath := filepath.Join(d.basePath, container)
	var artifacts []string

	err := filepath.Walk(containerPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			if rel, err := filepath.Rel(containerPath, path); err == nil {
				artifacts = append(artifacts, rel)
			}
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	return artifacts, nil
}

// HealthCheck verifies the driver is working
func (d *LocalDriver) HealthCheck(ctx context.Context) error {
	_, err := os.Stat(d.basePath)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	return nil
}

// PooledReader wraps a file with pooled buffer
type PooledReader struct {
	file   *os.File
	buffer *bufio.Reader
	driver *LocalDriver
}

// Read implements io.Reader
func (pr *PooledReader) Read(p []byte) (n int, err error) {
	return pr.buffer.Read(p)
}

// Close returns resources to pool
func (pr *PooledReader) Close() error {
	return pr.driver.ReturnPooledReader(pr)
}

// GetPooled retrieves a file using pooled resources
func (d *LocalDriver) GetPooled(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	d.mu.Lock()
	d.stats.Reads++
	d.mu.Unlock()

	fullPath := filepath.Join(d.basePath, container, artifact)

	if !strings.HasPrefix(fullPath, d.basePath) {
		return nil, fmt.Errorf("path traversal detected")
	}

	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact not found: %w", err)
		}
		return nil, fmt.Errorf("open failed: %w", err)
	}

	buffer := d.readerPool.Get().(*bufio.Reader)
	buffer.Reset(file)

	return &PooledReader{
		file:   file,
		buffer: buffer,
		driver: d,
	}, nil
}

// ReturnPooledReader returns resources to pool
func (d *LocalDriver) ReturnPooledReader(pr io.ReadCloser) error {
	pooled, ok := pr.(*PooledReader)
	if !ok {
		return pr.Close()
	}

	err := pooled.file.Close()

	pooled.buffer.Reset(nil)
	d.readerPool.Put(pooled.buffer)

	return err
}

// GetPoolStats returns pool metrics
func (d *LocalDriver) GetPoolStats() map[string]interface{} {
	return map[string]interface{}{
		"reader_pool_size": "unknown",
		"total_reads":      d.stats.Reads,
	}
}

// GetOptions configures Get behavior
type GetOptions struct {
	FollowSymlinks bool
}

// FileInfo extends basic file information
type FileInfo struct {
	Name          string
	Size          int64
	IsDir         bool
	IsSymlink     bool
	SymlinkTarget string
	Mode          os.FileMode
	ModTime       time.Time
}

// SupportsSymlinks checks if the filesystem supports symlinks
func (d *LocalDriver) SupportsSymlinks() bool {
	testFile := filepath.Join(d.basePath, ".symlink-test")
	testLink := filepath.Join(d.basePath, ".symlink-test-link")

	_ = os.Remove(testLink)
	_ = os.Remove(testFile)
	defer func() { _ = os.Remove(testLink) }()
	defer func() { _ = os.Remove(testFile) }()

	if err := os.WriteFile(testFile, []byte("test"), 0600); err != nil {
		return false
	}

	if err := os.Symlink(testFile, testLink); err != nil {
		return false
	}

	return true
}

// GetWithOptions retrieves an artifact with configurable options
func (d *LocalDriver) GetWithOptions(ctx context.Context, container, artifact string, opts GetOptions) (io.ReadCloser, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("lstat failed: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if !opts.FollowSymlinks {
			return nil, fmt.Errorf("artifact is a symlink and FollowSymlinks is false")
		}
		return os.Open(fullPath)
	}

	return os.Open(fullPath)
}

// GetInfo returns detailed information about an artifact
func (d *LocalDriver) GetInfo(ctx context.Context, container, artifact string) (*FileInfo, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("lstat failed: %w", err)
	}

	fi := &FileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		IsDir:   info.IsDir(),
		Mode:    info.Mode(),
		ModTime: info.ModTime(),
	}

	if info.Mode()&os.ModeSymlink != 0 {
		fi.IsSymlink = true
		if target, err := os.Readlink(fullPath); err == nil {
			fi.SymlinkTarget = target
		}
	}

	return fi, nil
}

// SetPermissions sets the file permissions for an artifact
func (d *LocalDriver) SetPermissions(ctx context.Context, container, artifact string, mode os.FileMode) error {
	fullPath := filepath.Join(d.basePath, container, artifact)

	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}

	if err := os.Chmod(fullPath, mode); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	return nil
}

// GetPermissions retrieves the file permissions for an artifact
func (d *LocalDriver) GetPermissions(ctx context.Context, container, artifact string) (os.FileMode, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("artifact not found: %w", err)
		}
		return 0, fmt.Errorf("stat failed: %w", err)
	}

	return info.Mode() & os.ModePerm, nil
}

// SetOwnership sets the owner and group for an artifact
func (d *LocalDriver) SetOwnership(ctx context.Context, container, artifact string, uid, gid int) error {
	fullPath := filepath.Join(d.basePath, container, artifact)

	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}

	if err := os.Chown(fullPath, uid, gid); err != nil {
		return fmt.Errorf("chown failed: %w", err)
	}

	return nil
}

// GetOwnership retrieves the owner and group for an artifact
func (d *LocalDriver) GetOwnership(ctx context.Context, container, artifact string) (uid, gid int, err error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, -1, fmt.Errorf("artifact not found: %w", err)
		}
		return -1, -1, fmt.Errorf("stat failed: %w", err)
	}

	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid), nil
	}

	return -1, -1, fmt.Errorf("unable to get ownership information")
}

// ChecksumAlgorithm represents a hashing algorithm
type ChecksumAlgorithm string

const (
	ChecksumMD5    ChecksumAlgorithm = "md5"
	ChecksumSHA256 ChecksumAlgorithm = "sha256"
	ChecksumSHA512 ChecksumAlgorithm = "sha512"
)

// GetChecksum calculates the checksum of an artifact
func (d *LocalDriver) GetChecksum(ctx context.Context, container, artifact string, algorithm ChecksumAlgorithm) (string, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("artifact not found: %w", err)
		}
		return "", fmt.Errorf("open failed: %w", err)
	}
	defer file.Close()

	var h hash.Hash
	switch algorithm {
	case ChecksumMD5:
		h = md5.New()
	case ChecksumSHA256:
		h = sha256.New()
	case ChecksumSHA512:
		h = sha512.New()
	default:
		return "", fmt.Errorf("unsupported algorithm: %s", algorithm)
	}

	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("read failed: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyChecksum verifies an artifact matches the expected checksum
func (d *LocalDriver) VerifyChecksum(ctx context.Context, container, artifact string, expected string, algorithm ChecksumAlgorithm) error {
	actual, err := d.GetChecksum(ctx, container, artifact, algorithm)
	if err != nil {
		return err
	}

	if actual != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}

	return nil
}

// CreateDirectory creates a directory within a container
func (d *LocalDriver) CreateDirectory(ctx context.Context, container, dir string) error {
	fullPath := filepath.Join(d.basePath, container, dir)
	if err := os.MkdirAll(fullPath, 0750); err != nil {
		return fmt.Errorf("create directory failed: %w", err)
	}
	return nil
}

// RemoveDirectory removes a directory from a container
func (d *LocalDriver) RemoveDirectory(ctx context.Context, container, dir string) error {
	fullPath := filepath.Join(d.basePath, container, dir)
	if err := os.RemoveAll(fullPath); err != nil {
		return fmt.Errorf("remove directory failed: %w", err)
	}
	return nil
}

// DirectoryExists checks if a directory exists
func (d *LocalDriver) DirectoryExists(ctx context.Context, container, dir string) (bool, error) {
	fullPath := filepath.Join(d.basePath, container, dir)
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat failed: %w", err)
	}
	return info.IsDir(), nil
}

// ListDirectory lists entries in a directory
func (d *LocalDriver) ListDirectory(ctx context.Context, container, dir string) ([]os.DirEntry, error) {
	fullPath := filepath.Join(d.basePath, container, dir)
	entries, err := os.ReadDir(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("directory not found: %w", err)
		}
		return nil, fmt.Errorf("read directory failed: %w", err)
	}
	return entries, nil
}

// WalkDirectory walks a directory tree and calls fn for each entry
func (d *LocalDriver) WalkDirectory(ctx context.Context, container, dir string, fn func(path string, entry os.DirEntry) error) error {
	basePath := filepath.Join(d.basePath, container, dir)
	return filepath.WalkDir(basePath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(filepath.Join(d.basePath, container), path)
		if err != nil {
			return fmt.Errorf("get relative path failed: %w", err)
		}
		return fn(relPath, entry)
	})
}

// GetDirectorySize calculates the total size of a directory
func (d *LocalDriver) GetDirectorySize(ctx context.Context, container, dir string) (int64, error) {
	var totalSize int64
	err := d.WalkDirectory(ctx, container, dir, func(path string, entry os.DirEntry) error {
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk directory failed: %w", err)
	}
	return totalSize, nil
}

// DirectoryIndex represents indexed directory information
type DirectoryIndex struct {
	FileCount int
	DirCount  int
	TotalSize int64
	Files     []string
	Dirs      []string
}

// IndexDirectory creates an index of a directory
func (d *LocalDriver) IndexDirectory(ctx context.Context, container, dir string) (*DirectoryIndex, error) {
	index := &DirectoryIndex{
		Files: make([]string, 0),
		Dirs:  make([]string, 0),
	}
	err := d.WalkDirectory(ctx, container, dir, func(path string, entry os.DirEntry) error {
		if entry.IsDir() {
			if path != dir && path != "." {
				index.DirCount++
				index.Dirs = append(index.Dirs, path)
			}
		} else {
			index.FileCount++
			index.Files = append(index.Files, path)
			if info, err := entry.Info(); err == nil {
				index.TotalSize += info.Size()
			}
		}
		return nil
	})
	return index, err
}

// FindFilesByExtension finds all files with a specific extension
func (d *LocalDriver) FindFilesByExtension(ctx context.Context, container, ext string) ([]string, error) {
	var files []string
	err := d.WalkDirectory(ctx, container, "", func(path string, entry os.DirEntry) error {
		if !entry.IsDir() && strings.HasSuffix(path, ext) {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// FindFilesByPattern finds files matching a glob pattern
func (d *LocalDriver) FindFilesByPattern(ctx context.Context, container, pattern string) ([]string, error) {
	var files []string
	err := d.WalkDirectory(ctx, container, "", func(path string, entry os.DirEntry) error {
		if !entry.IsDir() {
			matched, err := filepath.Match(pattern, filepath.Base(path))
			if err != nil {
				return err
			}
			if matched {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, err
}

// DirectoryDiff represents differences between two directories
type DirectoryDiff struct {
	Added    []string
	Modified []string
	Deleted  []string
}

// SyncDirectory synchronizes files from source to destination
func (d *LocalDriver) SyncDirectory(ctx context.Context, sourceContainer, sourceDir, destContainer, destDir string) error {
	return d.WalkDirectory(ctx, sourceContainer, sourceDir, func(path string, entry os.DirEntry) error {
		if entry.IsDir() {
			destPath := filepath.Join(destDir, path)
			return d.CreateDirectory(ctx, destContainer, destPath)
		}
		sourcePath := filepath.Join(sourceDir, path)
		destPath := filepath.Join(destDir, path)
		reader, err := d.Get(ctx, sourceContainer, sourcePath)
		if err != nil {
			return fmt.Errorf("get source file %s: %w", sourcePath, err)
		}
		defer reader.Close()
		err = d.Put(ctx, destContainer, destPath, reader)
		if err != nil {
			return fmt.Errorf("put dest file %s: %w", destPath, err)
		}
		return nil
	})
}

// CompareDirectories compares two directories and returns differences
func (d *LocalDriver) CompareDirectories(ctx context.Context, sourceContainer, sourceDir, destContainer, destDir string) (*DirectoryDiff, error) {
	diff := &DirectoryDiff{
		Added:    []string{},
		Modified: []string{},
		Deleted:  []string{},
	}
	sourceFiles := make(map[string]os.FileInfo)
	err := d.WalkDirectory(ctx, sourceContainer, sourceDir, func(path string, entry os.DirEntry) error {
		if !entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				sourceFiles[path] = info
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	destFiles := make(map[string]os.FileInfo)
	err = d.WalkDirectory(ctx, destContainer, destDir, func(path string, entry os.DirEntry) error {
		if !entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				destFiles[path] = info
				if sourceInfo, exists := sourceFiles[path]; exists {
					if sourceInfo.Size() != info.Size() {
						diff.Modified = append(diff.Modified, path)
					}
				} else {
					diff.Added = append(diff.Added, path)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for path := range sourceFiles {
		if _, exists := destFiles[path]; !exists {
			diff.Deleted = append(diff.Deleted, path)
		}
	}
	return diff, nil
}

// GetDirectoryModTime returns the most recent modification time in a directory
func (d *LocalDriver) GetDirectoryModTime(ctx context.Context, container, dir string) (time.Time, error) {
	var latestTime time.Time
	err := d.WalkDirectory(ctx, container, dir, func(path string, entry os.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("walk directory failed: %w", err)
	}
	return latestTime, nil
}

// HasDirectoryChanged checks if directory has changed since a given time
func (d *LocalDriver) HasDirectoryChanged(ctx context.Context, container, dir string, since time.Time) (bool, error) {
	modTime, err := d.GetDirectoryModTime(ctx, container, dir)
	if err != nil {
		return false, err
	}
	return modTime.After(since), nil
}

// AtomicWrite performs an atomic write operation using temp file + rename
func (d *LocalDriver) AtomicWrite(ctx context.Context, container, artifact string, data io.Reader) error {
	containerPath := filepath.Join(d.basePath, container)
	if err := os.MkdirAll(containerPath, 0750); err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	finalPath := filepath.Join(d.basePath, container, artifact)
	parentDir := filepath.Dir(finalPath)
	if err := os.MkdirAll(parentDir, 0750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	tempFile, err := os.CreateTemp(parentDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() {
		if tempFile != nil {
			_ = tempFile.Close()
		}
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.Copy(tempFile, data); err != nil {
		return fmt.Errorf("write to temp file: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	tempFile = nil
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}
	tempPath = ""
	return nil
}

// AtomicRename performs an atomic rename operation
func (d *LocalDriver) AtomicRename(ctx context.Context, container, oldName, newName string) error {
	oldPath := filepath.Join(d.basePath, container, oldName)
	newPath := filepath.Join(d.basePath, container, newName)
	if _, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}
	newDir := filepath.Dir(newPath)
	if err := os.MkdirAll(newDir, 0750); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("rename failed: %w", err)
	}
	return nil
}

// AtomicDelete performs an atomic delete by moving to trash first
func (d *LocalDriver) AtomicDelete(ctx context.Context, container, artifact string) error {
	sourcePath := filepath.Join(d.basePath, container, artifact)
	if _, err := os.Stat(sourcePath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}
	trashDir := filepath.Join(d.basePath, ".trash", container)
	if err := os.MkdirAll(trashDir, 0750); err != nil {
		return fmt.Errorf("create trash directory: %w", err)
	}
	timestamp := time.Now().Unix()
	trashName := fmt.Sprintf("%s.%d", filepath.Base(artifact), timestamp)
	trashPath := filepath.Join(trashDir, trashName)
	if err := os.Rename(sourcePath, trashPath); err != nil {
		return fmt.Errorf("move to trash failed: %w", err)
	}
	return nil
}

// Transaction represents a batch of operations
type Transaction struct {
	driver     *LocalDriver
	operations []func() error
	committed  bool
}

// BeginTransaction starts a new transaction
func (d *LocalDriver) BeginTransaction(ctx context.Context) (*Transaction, error) {
	return &Transaction{
		driver:     d,
		operations: make([]func() error, 0),
	}, nil
}

// Put adds a put operation to the transaction
func (t *Transaction) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	content, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	t.operations = append(t.operations, func() error {
		return t.driver.Put(ctx, container, artifact, bytes.NewReader(content))
	})
	return nil
}

// Commit executes all operations in the transaction
func (t *Transaction) Commit() error {
	if t.committed {
		return fmt.Errorf("transaction already committed")
	}
	for _, op := range t.operations {
		if err := op(); err != nil {
			return fmt.Errorf("transaction failed: %w", err)
		}
	}
	t.committed = true
	return nil
}

// Rollback cancels the transaction
func (t *Transaction) Rollback() error {
	if t.committed {
		return fmt.Errorf("transaction already committed")
	}
	t.operations = nil
	t.committed = true
	return nil
}
