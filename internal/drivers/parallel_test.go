package drivers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestLocalDriver_ParallelOperations(t *testing.T) {
	testDir := t.TempDir()
	logger := zap.NewNop()
	driver := NewLocalDriver(testDir, logger)
	ctx := context.Background()

	t.Run("ConcurrentWrites", func(t *testing.T) {
		count := 100
		var wg sync.WaitGroup
		errors := make([]error, count)

		start := time.Now()
		for i := 0; i < count; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				data := fmt.Sprintf("file %d content", n)
				errors[n] = driver.Put(ctx, "container", fmt.Sprintf("file-%03d.txt", n), strings.NewReader(data))
			}(i)
		}
		wg.Wait()
		elapsed := time.Since(start)

		for i, err := range errors {
			assert.NoError(t, err, "file %d failed", i)
		}

		t.Logf("Parallel write of %d files took %v", count, elapsed)
	})
}
