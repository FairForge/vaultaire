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

// PutOption is a function that configures Put operations
type PutOption func(*putOptions)

// putOptions holds options for Put operations
type putOptions struct {
	ContentType     string
	CacheControl    string
	ContentEncoding string
	ContentLanguage string
	UserMetadata    map[string]string
}

// WithContentType sets the content type
func WithContentType(ct string) PutOption {
	return func(o *putOptions) {
		o.ContentType = ct
	}
}

// WithUserMetadata sets user metadata
func WithUserMetadata(meta map[string]string) PutOption {
	return func(o *putOptions) {
		o.UserMetadata = meta
	}
}

// Capability represents what a driver can do
