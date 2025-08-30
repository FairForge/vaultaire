package drivers

import (
	"context"
	"fmt"
	"io"
	"sync"
)

const (
	DefaultChunkSize = 4 * 1024 * 1024 // 4MB
	MaxConcurrency   = 8
)

type ChunkReader struct {
	source io.ReaderAt
	size   int64
}

func NewChunkReader(source io.ReaderAt, size int64) *ChunkReader {
	return &ChunkReader{source: source, size: size}
}

func (c *ChunkReader) ReadParallel(ctx context.Context) ([]byte, error) {
	numChunks := (c.size + DefaultChunkSize - 1) / DefaultChunkSize
	chunks := make([][]byte, numChunks)

	var wg sync.WaitGroup
	errChan := make(chan error, numChunks)
	sem := make(chan struct{}, MaxConcurrency)

	for i := int64(0); i < numChunks; i++ {
		wg.Add(1)
		go func(idx int64) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			offset := idx * DefaultChunkSize
			size := DefaultChunkSize
			if offset+int64(size) > c.size {
				size = int(c.size - offset)
			}

			chunk := make([]byte, size)
			_, err := c.source.ReadAt(chunk, offset)
			if err != nil && err != io.EOF {
				errChan <- fmt.Errorf("read chunk %d: %w", idx, err)
				return
			}
			chunks[idx] = chunk
		}(i)
	}

	wg.Wait()
	close(errChan)

	if err := <-errChan; err != nil {
		return nil, err
	}

	// Combine chunks
	result := make([]byte, 0, c.size)
	for _, chunk := range chunks {
		result = append(result, chunk...)
	}
	return result, nil
}
