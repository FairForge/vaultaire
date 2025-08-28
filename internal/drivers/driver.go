package drivers

import (
	"context"
	"io"
)

// Driver is the common interface all storage drivers must implement
type Driver interface {
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
	Put(ctx context.Context, container, artifact string, data io.Reader) error
	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container string, prefix string) ([]string, error)
	Exists(ctx context.Context, container, artifact string) (bool, error)
}
