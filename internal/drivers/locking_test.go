package drivers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalDriver_FileLocking_Flock(t *testing.T) {
	driver := setupTestDriver(t)
	ctx := context.Background()

	err := driver.Put(ctx, "test", "locked.txt", strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ExclusiveLock", func(t *testing.T) {
		lock, err := driver.LockFile(ctx, "test", "locked.txt", LockExclusive)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Unlock() }()

		// Try to acquire another lock (should timeout)
		ctx2, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()

		_, err = driver.LockFile(ctx2, "test", "locked.txt", LockExclusive)
		if err == nil {
			t.Error("Should not be able to acquire second exclusive lock")
		}
	})

	t.Run("SharedLocks", func(t *testing.T) {
		var wg sync.WaitGroup
		errors := make(chan error, 3)

		// Multiple readers should work
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lock, err := driver.LockFile(ctx, "test", "shared.txt", LockShared)
				if err != nil {
					errors <- err
					return
				}
				time.Sleep(50 * time.Millisecond)
				_ = lock.Unlock()
			}()
		}

		wg.Wait()
		close(errors)

		for err := range errors {
			if err != nil {
				t.Errorf("Shared lock failed: %v", err)
			}
		}
	})
}
