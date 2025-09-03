// internal/drivers/chunked_transfer.go
package drivers

import (
	"fmt"
	"io"

	"go.uber.org/zap"
)

type ChunkedTransfer struct {
	chunkSize int
	logger    *zap.Logger
}

func NewChunkedTransfer(chunkSize int, logger *zap.Logger) *ChunkedTransfer {
	if chunkSize <= 0 {
		chunkSize = 5 * 1024 * 1024 // Default 5MB chunks
	}
	return &ChunkedTransfer{
		chunkSize: chunkSize,
		logger:    logger,
	}
}

// ChunkedWrite writes data in chunks
func (c *ChunkedTransfer) ChunkedWrite(w io.Writer, r io.Reader) (int64, error) {
	buffer := make([]byte, c.chunkSize)
	var written int64

	for {
		n, err := r.Read(buffer)
		if n > 0 {
			nw, werr := w.Write(buffer[:n])
			written += int64(nw)

			if werr != nil {
				return written, fmt.Errorf("write chunk: %w", werr)
			}

			c.logger.Debug("wrote chunk",
				zap.Int("size", nw),
				zap.Int64("total", written))
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return written, fmt.Errorf("read chunk: %w", err)
		}
	}

	return written, nil
}
