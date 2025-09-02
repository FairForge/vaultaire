//go:build linux
// +build linux

package drivers

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
)

const (
	SEEK_DATA = 3 // Seek to next data
	SEEK_HOLE = 4 // Seek to next hole
)

// CreateSparse creates a true sparse file using fallocate
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
	defer file.Close()

	// Use fallocate to create sparse file efficiently
	err = syscall.Fallocate(int(file.Fd()), 0, 0, size)
	if err != nil {
		// Fallback to truncate if fallocate not supported
		return file.Truncate(size)
	}

	return nil
}

// GetHoles returns actual holes in a sparse file using SEEK_HOLE/SEEK_DATA
func (d *LocalDriver) GetHoles(ctx context.Context, container, artifact string) ([]HoleInfo, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	file, err := os.Open(fullPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var holes []HoleInfo
	var offset int64 = 0

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := info.Size()

	for offset < fileSize {
		// Find next hole
		holeStart, err := syscall.Seek(int(file.Fd()), offset, SEEK_HOLE)
		if err != nil || holeStart >= fileSize {
			break
		}

		// Find next data after hole
		dataStart, err := syscall.Seek(int(file.Fd()), holeStart, SEEK_DATA)
		if err != nil {
			// Rest of file is a hole
			holes = append(holes, HoleInfo{
				Offset: holeStart,
				Length: fileSize - holeStart,
			})
			break
		}

		holes = append(holes, HoleInfo{
			Offset: holeStart,
			Length: dataStart - holeStart,
		})

		offset = dataStart
	}

	return holes, nil
}

// GetFileInfo returns detailed file information including actual block allocation
func (d *LocalDriver) GetFileInfo(ctx context.Context, container, artifact string) (*FileInfoExtended, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	var stat syscall.Stat_t
	err := syscall.Stat(fullPath, &stat)
	if err != nil {
		return nil, err
	}

	return &FileInfoExtended{
		Size:            stat.Size,
		BlocksAllocated: stat.Blocks, // 512-byte blocks actually allocated
	}, nil
}
