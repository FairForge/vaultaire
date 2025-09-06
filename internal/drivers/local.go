package drivers

import (
	"bufio"
	"bytes"
	"context"
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

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/fsnotify/fsnotify"
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
				return bufio.NewReaderSize(nil, 1024*1024)
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
	cleanPath := filepath.Clean(fullPath)
	if !strings.HasPrefix(cleanPath, filepath.Clean(d.basePath)) {
		return nil, fmt.Errorf("path traversal detected: %s", artifact)
	}

	d.logger.Debug("LocalDriver.Get",
		zap.String("container", container),
		zap.String("artifact", artifact),
		zap.String("fullPath", fullPath))

	return os.Open(fullPath)
}

// Put stores an artifact in a container
func (d *LocalDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	// Apply options (but don't use them yet)
	var options engine.PutOptions
	for _, opt := range opts {
		opt(&options)
	}

	containerPath := filepath.Join(d.basePath, container)
	if err := os.MkdirAll(containerPath, 0750); err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	fullPath := filepath.Join(d.basePath, container, artifact)
	cleanPath := filepath.Clean(fullPath)
	if !strings.HasPrefix(cleanPath, filepath.Clean(d.basePath)) {
		return fmt.Errorf("path traversal detected: %s", artifact)
	}

	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Copy data
	_, err = io.Copy(file, data)
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return nil
}

// Delete removes an artifact from a container
func (d *LocalDriver) Delete(ctx context.Context, container, artifact string) error {
	fullPath := filepath.Join(d.basePath, container, artifact)
	return os.Remove(fullPath)
}

// List lists artifacts in a container
func (d *LocalDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	containerPath := filepath.Join(d.basePath, container)
	var artifacts []string

	err := filepath.Walk(containerPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			if rel, err := filepath.Rel(containerPath, path); err == nil {
				if !strings.HasSuffix(rel, ".meta") {
					artifacts = append(artifacts, rel)
				}
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
	defer func() {
		if err := file.Close(); err != nil {
			d.logger.Error("failed to close file", zap.Error(err))
		}
	}()

	var h hash.Hash
	switch algorithm {
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
		defer func() { _ = reader.Close() }()
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

// BufferedWriter wraps a file with write buffering
type BufferedWriter struct {
	file   *os.File
	buffer *bufio.Writer
	driver *LocalDriver
	mu     sync.Mutex
}

// Write implements io.Writer
func (bw *BufferedWriter) Write(p []byte) (n int, err error) {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	return bw.buffer.Write(p)
}

// Flush forces buffer to disk
func (bw *BufferedWriter) Flush() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	return bw.buffer.Flush()
}

// Close flushes and closes the file
func (bw *BufferedWriter) Close() error {
	bw.mu.Lock()
	defer bw.mu.Unlock()

	// Flush buffer first
	if err := bw.buffer.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}

	// Sync to disk
	if err := bw.file.Sync(); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	// Close file
	if err := bw.file.Close(); err != nil {
		return fmt.Errorf("close failed: %w", err)
	}

	// Update stats
	bw.driver.mu.Lock()
	bw.driver.stats.Writes++
	bw.driver.mu.Unlock()

	return nil
}

// PutBuffered creates a buffered writer for efficient small writes
func (d *LocalDriver) PutBuffered(ctx context.Context, container, artifact string) (io.WriteCloser, error) {
	containerPath := filepath.Join(d.basePath, container)
	if err := os.MkdirAll(containerPath, 0750); err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	fullPath := filepath.Join(d.basePath, container, artifact)

	// Security check
	if !strings.HasPrefix(fullPath, d.basePath) {
		return nil, fmt.Errorf("path traversal detected")
	}

	// Create parent directory
	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0750); err != nil {
		return nil, fmt.Errorf("create parent directory: %w", err)
	}

	// Create file
	file, err := os.Create(fullPath)
	if err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}

	// Create buffered writer (64KB buffer)
	buffer := bufio.NewWriterSize(file, 1024*1024)

	return &BufferedWriter{
		file:   file,
		buffer: buffer,
		driver: d,
	}, nil
}

// GetWriteBufferStats returns buffer statistics
func (d *LocalDriver) GetWriteBufferStats() map[string]interface{} {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return map[string]interface{}{
		"total_writes": d.stats.Writes,
		"buffer_size":  64 * 1024,
	}
}

// MultipartUpload represents an in-progress multipart upload
// MultipartUpload represents an in-progress multipart upload
type MultipartUpload struct {
	ID        string
	Container string
	Artifact  string
}

// CreateMultipartUpload initiates a multipart upload
// CreateMultipartUpload initiates a multipart upload
func (d *LocalDriver) CreateMultipartUpload(ctx context.Context, container, artifact string) (*MultipartUpload, error) {
	return &MultipartUpload{
		ID:        fmt.Sprintf("mpu_%d", time.Now().UnixNano()),
		Container: container,
		Artifact:  artifact,
	}, nil
}

// CompletedPart represents a completed upload part
type CompletedPart struct {
	PartNumber int
	ETag       string
	Size       int64
}

// UploadPart uploads a single part of a multipart upload
func (d *LocalDriver) UploadPart(ctx context.Context, upload *MultipartUpload, partNumber int, data io.Reader) (CompletedPart, error) {
	// Create temp directory for this upload
	tempDir := filepath.Join(d.basePath, ".uploads", upload.ID)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return CompletedPart{}, fmt.Errorf("create temp dir: %w", err)
	}

	// Save part to temp file
	partPath := filepath.Join(tempDir, fmt.Sprintf("part_%d", partNumber))
	file, err := os.Create(partPath)
	if err != nil {
		return CompletedPart{}, fmt.Errorf("create part file: %w", err)
	}
	defer func() { _ = file.Close() }()

	size, err := io.Copy(file, data)
	if err != nil {
		return CompletedPart{}, fmt.Errorf("write part: %w", err)
	}

	return CompletedPart{
		PartNumber: partNumber,
		ETag:       fmt.Sprintf("etag-%d", partNumber),
		Size:       size,
	}, nil
}

// CompleteMultipartUpload assembles all parts into the final file
func (d *LocalDriver) CompleteMultipartUpload(ctx context.Context, upload *MultipartUpload, parts []CompletedPart) error {
	// Create container directory
	containerPath := filepath.Join(d.basePath, upload.Container)
	if err := os.MkdirAll(containerPath, 0755); err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	// Create final file
	finalPath := filepath.Join(containerPath, upload.Artifact)
	finalFile, err := os.Create(finalPath)
	if err != nil {
		return fmt.Errorf("create final file: %w", err)
	}
	defer func() { _ = finalFile.Close() }()

	// Assemble parts in order
	tempDir := filepath.Join(d.basePath, ".uploads", upload.ID)
	for _, part := range parts {
		partPath := filepath.Join(tempDir, fmt.Sprintf("part_%d", part.PartNumber))
		partFile, err := os.Open(partPath)
		if err != nil {
			return fmt.Errorf("open part %d: %w", part.PartNumber, err)
		}

		if _, err := io.Copy(finalFile, partFile); err != nil {
			_ = partFile.Close()
			return fmt.Errorf("copy part %d: %w", part.PartNumber, err)
		}
		_ = partFile.Close()
	}

	// Clean up temp files
	_ = os.RemoveAll(tempDir)

	return nil
}

// Exists checks if an artifact exists
func (d *LocalDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)
	_, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return true, nil
}

// Watch implements the Watcher interface
func (d *LocalDriver) Watch(ctx context.Context, prefix string) (<-chan WatchEvent, <-chan error, error) {
	events := make(chan WatchEvent, 10)
	errors := make(chan error, 1)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, fmt.Errorf("create watcher: %w", err)
	}

	watchPath := d.basePath
	if prefix != "" {
		watchPath = filepath.Join(d.basePath, prefix)
	}

	if err = filepath.Walk(watchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := watcher.Add(path); err != nil {
				return fmt.Errorf("add watch path %s: %w", path, err)
			}
			d.logger.Debug("watching directory", zap.String("path", path))
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("walk directory: %w", err)
	}

	go d.processWatchEvents(ctx, watcher, events, errors, prefix)
	return events, errors, nil
}

func (d *LocalDriver) processWatchEvents(
	ctx context.Context,
	watcher *fsnotify.Watcher,
	events chan<- WatchEvent,
	errors chan<- error,
	prefix string,
) {
	defer close(events)
	defer close(errors)
	defer func() {
		if err := watcher.Close(); err != nil {
			d.logger.Error("failed to close watcher", zap.Error(err))
		}
	}()

	for {
		select {
		case <-ctx.Done():
			d.logger.Debug("stopping watcher", zap.String("reason", "context cancelled"))
			return

		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			if we := d.convertEvent(event, prefix); we != nil {
				select {
				case events <- *we:
					d.logger.Debug("sent watch event",
						zap.Int("type", int(we.Type)),
						zap.String("path", we.Path))
				case <-ctx.Done():
					return
				}
			}

			if err := watcher.Add(event.Name); err != nil {
				d.logger.Error("failed to add directory", zap.String("path", event.Name), zap.Error(err))
			}

			if event.Op&fsnotify.Create == fsnotify.Create {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := watcher.Add(event.Name); err != nil {
						d.logger.Error("failed to add new dir", zap.String("path", event.Name), zap.Error(err))
					}
					d.logger.Debug("added new directory to watcher",
						zap.String("path", event.Name))
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			select {
			case errors <- fmt.Errorf("watcher error: %w", err):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (d *LocalDriver) convertEvent(event fsnotify.Event, prefix string) *WatchEvent {
	relPath, err := filepath.Rel(d.basePath, event.Name)
	if err != nil {
		return nil
	}

	if prefix != "" && !strings.HasPrefix(relPath, prefix) {
		return nil
	}

	we := &WatchEvent{Path: relPath}

	switch {
	case event.Op&fsnotify.Create == fsnotify.Create:
		we.Type = WatchEventCreate
	case event.Op&fsnotify.Write == fsnotify.Write:
		we.Type = WatchEventModify
	case event.Op&fsnotify.Remove == fsnotify.Remove:
		we.Type = WatchEventDelete
	case event.Op&fsnotify.Rename == fsnotify.Rename:
		we.Type = WatchEventRename
	default:
		return nil
	}

	return we
}

// Copy copies a file preserving permissions
func (d *LocalDriver) Copy(ctx context.Context, srcContainer, srcArtifact, dstContainer, dstArtifact string) error {
	perm, err := d.GetPermissions(ctx, srcContainer, srcArtifact)
	if err != nil {
		return fmt.Errorf("get source permissions: %w", err)
	}

	reader, err := d.Get(ctx, srcContainer, srcArtifact)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	defer func() { _ = reader.Close() }()

	err = d.Put(ctx, dstContainer, dstArtifact, reader)
	if err != nil {
		return fmt.Errorf("write destination: %w", err)
	}

	return d.SetPermissions(ctx, dstContainer, dstArtifact, perm)
}

// FileLock represents a file lock
type FileLock struct {
	file *os.File
	path string
}

// LockType for file locking
type LockType int

const (
	LockShared LockType = iota
	LockExclusive
)

// WriteAt writes data at a specific offset
func (d *LocalDriver) WriteAt(ctx context.Context, container, artifact string, data []byte, offset int64) error {
	fullPath := filepath.Join(d.basePath, container, artifact)

	file, err := os.OpenFile(fullPath, os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	_, err = file.WriteAt(data, offset)
	return err
}

// HoleInfo represents a hole in a sparse file
type HoleInfo struct {
	Offset int64
	Length int64
}

// FileInfoExtended includes block allocation info
type FileInfoExtended struct {
	Size            int64
	BlocksAllocated int64
}
