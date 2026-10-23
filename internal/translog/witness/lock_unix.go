//go:build unix

package witness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockDir takes the witness directory's lock (<dir>/lock, flock): one
// holder at a time across every process and goroutine — a cron pass, a
// -once=false loop and every add-checkpoint request of `serve` (RP42-5; the
// spec's "checking the old size … and persisting the new checkpoint must be
// performed atomically"). flock is held by the open file, so two opens in
// one process exclude each other too. It waits while ctx allows.
func lockDir(ctx context.Context, dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("witness: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- the witness's own directory
	if err != nil {
		return nil, fmt.Errorf("witness: open the lock: %w", err)
	}
	fd := int(f.Fd()) // #nosec G115 -- a file descriptor fits an int
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("witness: lock %s: %w", dir, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("witness: %s is locked by another pass: %w", dir, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
