package drivers

import (
	"github.com/FairForge/vaultaire/internal/engine"
)

// Driver is an alias for engine.Driver — the canonical storage backend interface.
type Driver = engine.Driver

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
