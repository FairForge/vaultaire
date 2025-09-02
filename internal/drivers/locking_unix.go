//go:build !windows
// +build !windows

package drivers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile acquires a file lock using flock
func (d *LocalDriver) LockFile(ctx context.Context, container, artifact string, lockType LockType) (*FileLock, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	file, err := os.OpenFile(fullPath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}

	// Convert our lock type to syscall flags
	var lockFlag int
	if lockType == LockExclusive {
		lockFlag = syscall.LOCK_EX
	} else {
		lockFlag = syscall.LOCK_SH
	}

	// Always use non-blocking lock
	err = syscall.Flock(int(file.Fd()), lockFlag|syscall.LOCK_NB)
	if err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("lock would block")
		}
		return nil, fmt.Errorf("flock failed: %w", err)
	}

	return &FileLock{
		file: file,
		path: fullPath,
	}, nil
}

func (fl *FileLock) Unlock() error {
	if fl.file != nil {
		// Ignore unlock error
		_ = syscall.Flock(int(fl.file.Fd()), syscall.LOCK_UN)
		return fl.file.Close()
	}
	return nil
}
