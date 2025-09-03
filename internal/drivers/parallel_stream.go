// internal/drivers/parallel_stream.go
package drivers

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

type StreamManager struct {
	semaphore chan struct{}
	logger    *zap.Logger
	mu        sync.Mutex
	active    int
}

type Stream struct {
	manager *StreamManager
	id      int
}

func NewStreamManager(maxConcurrent int, logger *zap.Logger) *StreamManager {
	return &StreamManager{
		semaphore: make(chan struct{}, maxConcurrent),
		logger:    logger,
	}
}

func (m *StreamManager) AcquireStream(ctx context.Context) (*Stream, error) {
	select {
	case m.semaphore <- struct{}{}:
		m.mu.Lock()
		m.active++
		streamID := m.active
		m.mu.Unlock()

		m.logger.Debug("stream acquired",
			zap.Int("id", streamID),
			zap.Int("active", m.active))

		return &Stream{
			manager: m,
			id:      streamID,
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Stream) Release() {
	<-s.manager.semaphore

	s.manager.mu.Lock()
	s.manager.active--
	s.manager.mu.Unlock()

	s.manager.logger.Debug("stream released",
		zap.Int("id", s.id),
		zap.Int("active", s.manager.active))
}
