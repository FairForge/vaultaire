package drivers

import (
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
	"syscall"
	"time"

	"go.uber.org/zap"
)

// LocalDriver implements the Driver interface for local filesystem
type LocalDriver struct {
	basePath string
	logger   *zap.Logger
}

// NewLocalDriver creates a new local filesystem driver
func NewLocalDriver(basePath string, logger *zap.Logger) *LocalDriver {
	return &LocalDriver{
		basePath: basePath,
		logger:   logger,
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

	// Create parent directory if artifact has subdirectories
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
			d.logger.Error("failed to close file", // Changed from l.logger to d.logger
				zap.String("path", fullPath), // Changed from path to fullPath
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
			return nil // Skip errors, continue walking
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
	return nil // Return nil on success!

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

	// Clean up any previous test files
	os.Remove(testLink)
	os.Remove(testFile)
	defer os.Remove(testLink)
	defer os.Remove(testFile)

	// Create test file
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		return false
	}

	// Try to create symlink
	if err := os.Symlink(testFile, testLink); err != nil {
		return false
	}

	return true
}

// GetWithOptions retrieves an artifact with configurable options
func (d *LocalDriver) GetWithOptions(ctx context.Context, container, artifact string, opts GetOptions) (io.ReadCloser, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if it's a symlink
	info, err := os.Lstat(fullPath) // Lstat doesn't follow symlinks
	if err != nil {
		return nil, fmt.Errorf("lstat failed: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if !opts.FollowSymlinks {
			return nil, fmt.Errorf("artifact is a symlink and FollowSymlinks is false")
		}
		// Use os.Open which follows symlinks
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

	// Check if it's a symlink
	if info.Mode()&os.ModeSymlink != 0 {
		fi.IsSymlink = true
		// Read symlink target
		if target, err := os.Readlink(fullPath); err == nil {
			fi.SymlinkTarget = target // Keep full path
		}
	}

	return fi, nil
}

// SetPermissions sets the file permissions for an artifact
func (d *LocalDriver) SetPermissions(ctx context.Context, container, artifact string, mode os.FileMode) error {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}

	// Set permissions
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

	// Return just the permission bits (not file type bits)
	return info.Mode() & os.ModePerm, nil
}

// SetOwnership sets the owner and group for an artifact
func (d *LocalDriver) SetOwnership(ctx context.Context, container, artifact string, uid, gid int) error {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}

	// Set ownership
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

	// Get system-specific file info
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
