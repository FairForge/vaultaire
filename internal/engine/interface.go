package engine

import (
	"context"
	"io"
)

// Engine is the universal interface for storage, compute, and ML
type Engine interface {
	// Storage operations (visible to users)
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)

	// Put stores an artifact and returns the name of the backend it was
	// written to. Callers must persist this value alongside object metadata
	// so that Get can route the read to the same backend, regardless of
	// what the intelligence / cost-optimizer would otherwise select.
	Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) (string, error)

	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container, prefix string) ([]Artifact, error)

	// Hidden capabilities (implement as no-ops for now)
	Execute(ctx context.Context, container string, wasm []byte, input io.Reader) (io.Reader, error)
	Query(ctx context.Context, sql string) (ResultSet, error)
	Train(ctx context.Context, model string, data []byte) error
	Predict(ctx context.Context, model string, input []byte) ([]byte, error)

	// Metadata operations
	GetContainerMetadata(ctx context.Context, container string) (*Container, error)
	GetArtifactMetadata(ctx context.Context, container, artifact string) (*Artifact, error)

	// Health and metrics
	HealthCheck(ctx context.Context) error
	GetMetrics(ctx context.Context) (map[string]interface{}, error)
}

// Driver interface for storage backends
type Driver interface {
	Name() string
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
	Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error
	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container, prefix string) ([]string, error)
	Exists(ctx context.Context, container, artifact string) (bool, error)
	HealthCheck(ctx context.Context) error
}

// TenantObject is one object a TenantWalker found.
type TenantObject struct {
	// Container and Artifact are the pair Get/Delete address the object by
	// under the walked tenant. Container is "" when the stored key has no
	// container segment at all (nothing this code writes).
	Container string
	Artifact  string
	// Remove deletes exactly this object — the key the listing returned, not
	// one rebuilt from the pair. A miss is not an error.
	Remove func(ctx context.Context) error
}

// TenantWalker is an optional interface for drivers that can enumerate
// everything ONE tenant holds on the backend, in whatever container it is —
// including containers no table remembers (WP-R10-3c, the account-erasure
// sweep). Drivers that key every object under the tenant implement it: the
// fixed-bucket ones (`t-<tenant>/<container>/<artifact>`) and the
// container-only ones (`<tenant>_<bucket>/…`).
//
// The contract, because a walk that lists the wrong prefix hands the caller
// another customer's data to delete:
//   - an empty tenant id, or one containing '/', is refused (never the
//     "default" fallback Get/Put/Delete use);
//   - the listing is bounded by a separator after the tenant id, so tenant
//     `ab` never matches tenant `abc`;
//   - every page is read; a listing that cannot be completed is an error,
//     never a short success;
//   - a key the backend returns from outside the tenant's prefix aborts the
//     walk with an error and is not handed to fn;
//   - an error from fn stops the walk and is returned.
//
// The shared chunk container is NOT filtered here: on a fixed-bucket backend
// chunk blobs written during a tenant's request sit under that tenant's
// prefix. The caller decides what it may touch.
type TenantWalker interface {
	WalkTenant(ctx context.Context, tenantID string, fn func(TenantObject) error) error
}

// RangeGetter is an optional interface for drivers that support byte-range
// reads directly (avoiding full-object download + discard). Drivers that
// implement this will be used for range GETs, dramatically improving download
// speed for large objects accessed via multipart/range clients.
type RangeGetter interface {
	GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error)
}

// Restorer is an optional interface for archive-class drivers (Geyser tape)
// whose objects can be evicted to cold storage and need an explicit recall
// before Get succeeds (V18.2 minimum recall slice). Wire semantics mirror
// AWS Glacier so rclone/aws-cli restore workflows work unmodified.
type Restorer interface {
	// RestoreObject requests a recall of an archived object; the restored
	// copy stays readable for the given number of days. Returns
	// ErrRestoreAlreadyInProgress when a recall is already running.
	RestoreObject(ctx context.Context, container, artifact string, days int32) error
	// RestoreStatus reports the backend's view of the object's restore state
	// (the raw x-amz-restore header value, empty when no restore was
	// requested) and its storage class.
	RestoreStatus(ctx context.Context, container, artifact string) (*RestoreStatus, error)
}

// RestoreStatus is a Restorer's per-object recall state.
type RestoreStatus struct {
	// Restore is the raw x-amz-restore value from the backend, e.g.
	// `ongoing-request="true"` or `ongoing-request="false", expiry-date="..."`.
	// Empty when the object is archived with no restore requested (matching
	// AWS, where the header is absent in that state).
	Restore string
	// StorageClass is the backend-reported class (e.g. "GLACIER"; empty for
	// objects still on the staging disk).
	StorageClass string
}

// ResultSet for query operations (future use)
type ResultSet interface {
	Next() bool
	Scan(dest ...interface{}) error
	Close() error
}

// ComputeEngine for WASM execution (future use)
type ComputeEngine interface {
	LoadModule(wasm []byte) error
	Execute(input []byte) ([]byte, error)
}

// MLEngine for machine learning operations (future use)
type MLEngine interface {
	LoadModel(path string) error
	Train(data [][]float64, labels []float64) error
	Predict(input []float64) (float64, error)
}

// PutOption is a function that configures Put operations
type PutOption func(*PutOptions)

// PutOptions holds options for Put operations
type PutOptions struct {
	ContentType     string
	CacheControl    string
	ContentEncoding string
	ContentLanguage string
	ContentLength   int64
	StorageClass    string
	UserMetadata    map[string]string
}

// ApplyPutOptions applies functional options and returns the result.
func ApplyPutOptions(opts ...PutOption) PutOptions {
	var o PutOptions
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithContentType sets the content type
func WithContentType(ct string) PutOption {
	return func(o *PutOptions) {
		o.ContentType = ct
	}
}

// WithUserMetadata sets user metadata
func WithUserMetadata(meta map[string]string) PutOption {
	return func(o *PutOptions) {
		o.UserMetadata = meta
	}
}

// WithContentLength passes the known body size to drivers that require
// Content-Length (iDrive, Geyser). When set, drivers can stream directly
// without buffering the entire body to determine size.
func WithContentLength(n int64) PutOption {
	return func(o *PutOptions) {
		o.ContentLength = n
	}
}

// WithStorageClass sets the S3 storage class hint for backend routing.
func WithStorageClass(class string) PutOption {
	return func(o *PutOptions) {
		o.StorageClass = class
	}
}
