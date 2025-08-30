package drivers

import (
	"context"
	"io"
	"sync"
)

type ReaderPool struct {
	pool    chan io.ReadCloser
	factory func() (io.ReadCloser, error)
	maxSize int
	mu      sync.Mutex
	active  int
}

func NewReaderPool(size int, factory func() (io.ReadCloser, error)) *ReaderPool {
	return &ReaderPool{
		pool:    make(chan io.ReadCloser, size),
		factory: factory,
		maxSize: size,
	}
}

func (p *ReaderPool) Get(ctx context.Context) (io.ReadCloser, error) {
	select {
	case reader := <-p.pool:
		return reader, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		p.mu.Lock()
		if p.active < p.maxSize {
			p.active++
			p.mu.Unlock()
			return p.factory()
		}
		p.mu.Unlock()

		select {
		case reader := <-p.pool:
			return reader, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (p *ReaderPool) Put(reader io.ReadCloser) {
	select {
	case p.pool <- reader:
	default:
		_ = reader.Close()
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}
}
