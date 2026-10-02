package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/FairForge/vaultaire/internal/common"
)

// The one address of a chunk blob (WP-R8-7).
//
// A deduplicated chunk is shared by every object — of any tenant — whose
// manifest names its hash, so it must be found at ONE place whatever request
// or job reads, writes or deletes it. The place is three things:
//
//	backend    the global_content_index row's backend_id
//	container  ChunkContainer
//	tenant     ChunkAddressTenant, in the context of the driver call
//
// The third is the one that was missing. Container-keyed backends (local,
// s3compat) build a key from the container alone, so "_global" was one place
// there. The fixed-bucket backends — iDrive (prod's primary), Lyve, Geyser,
// R2, permafrost — key every object `t-<tenant>/<container>/<artifact>` with
// the tenant taken from the CONTEXT of the call. A chunk was therefore stored
// at `t-<uploader>/_global/_chunks/<hash>`: a second tenant whose upload
// deduplicated against it got 200 on PUT and 500 on GET, and dedup GC, whose
// job context carries no tenant, deleted the index row and then "deleted" a
// key under `t-default/` that never existed.
//
// Why this tenant id and not "default" (what GC's context resolved to):
//   - "default" is where every driver call WITHOUT a tenant used to land. It
//     is the address of a bug, and the drivers now refuse such a call;
//   - an id starting with '_' can be nobody's: registration mints
//     `tenant-<hex>`, the S3 front door refuses a reserved id
//     (IsReservedTenantID), and the account-erasure sweep refuses to list
//     under any id that does not start with a letter or a digit;
//   - even a tenant that had this id could not address the chunk container:
//     its objects live in containers named `<id>_<bucket>`, never `_global`.
const (
	// ChunkContainer is the container every chunk blob is stored in.
	ChunkContainer = "_global"
	// ChunkAddressTenant is the tenant id in the context of every chunk blob
	// call. On a fixed-bucket backend a chunk is the object
	// `t-_global/_global/<storage_key>`.
	ChunkAddressTenant = "_global"
)

// ChunkContext is the context of a chunk blob call: THE ONE HELPER. Every
// Put, Get, Exists and Delete of a chunk — the upload pool, the chunked GET,
// dedup GC, the chunk move, any tool — runs on the context this returns, so
// the address never depends on who is asking.
func ChunkContext(ctx context.Context) context.Context {
	return common.WithTenantID(ctx, ChunkAddressTenant)
}

// LegacyChunkContext is the context that addresses a chunk blob where it was
// written BEFORE the one address existed: under the uploading tenant's
// prefix. Only two callers may use it — the read fallback and the chunk move
// (internal/api/chunk_store.go, chunk_move.go) — and neither ever writes
// through it. It goes away with the fallback.
func LegacyChunkContext(ctx context.Context, tenantID string) context.Context {
	return common.WithTenantID(ctx, tenantID)
}

// IsReservedTenantID reports an id no tenant may have: empty, or starting
// with '_' (the system's own prefix, ChunkAddressTenant among them).
func IsReservedTenantID(id string) bool {
	return id == "" || strings.HasPrefix(id, "_")
}

// ErrChunkAddress is returned for a chunk blob write that is not addressed
// through ChunkContext.
var ErrChunkAddress = fmt.Errorf("%w: a chunk blob is written through engine.ChunkContext only", ErrInvalidInput)

// ErrBackendNotRegistered is returned by the *On calls for a backend name no
// driver is registered under. It is never a miss: the blob may well exist.
var ErrBackendNotRegistered = errors.New("backend is not registered")

// KeyAddresser is an optional driver interface: the key a call would address
// on the backend. The chunk move uses it to tell whether two contexts name
// two different objects before it deletes one of them — on a container-keyed
// backend the "legacy" address and the one address are the same object, and
// deleting the first would delete the only copy.
type KeyAddresser interface {
	ObjectKey(ctx context.Context, container, artifact string) string
}

// on runs fn against exactly one backend, through its circuit breaker. A
// chunk lives on the backend its index row names and nowhere else, so asking
// the other backends (what Get's candidate walk does after a miss) can only
// cost round trips. The verdict is the backend's own: a miss is a miss, an
// unavailable backend is ErrAllBackendsUnavailable.
func (e *CoreEngine) on(ctx context.Context, backend string, fn func(d Driver) error) error {
	e.mu.RLock()
	d, ok := e.drivers[backend]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrBackendNotRegistered, backend)
	}
	_, err := e.failover.Execute(ctx, []string{backend}, func(string) error { return fn(d) })
	return err
}

// GetOn reads an artifact from the named backend only.
func (e *CoreEngine) GetOn(ctx context.Context, backend, container, artifact string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := e.on(ctx, backend, func(d Driver) error {
		var getErr error
		rc, getErr = d.Get(ctx, container, artifact)
		return getErr
	})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s on %s: %w", container, artifact, backend, err)
	}
	return rc, nil
}

// PutOn writes an artifact to the named backend only — no failover: the
// caller records (or already recorded) that backend as the artifact's home.
func (e *CoreEngine) PutOn(ctx context.Context, backend, container, artifact string, data io.Reader, opts ...PutOption) error {
	if container == ChunkContainer && common.GetTenantID(ctx) != ChunkAddressTenant {
		return ErrChunkAddress
	}
	err := e.on(ctx, backend, func(d Driver) error { return d.Put(ctx, container, artifact, data, opts...) })
	if err != nil {
		return fmt.Errorf("put %s/%s on %s: %w", container, artifact, backend, err)
	}
	return nil
}

// DeleteOn deletes an artifact on the named backend only.
func (e *CoreEngine) DeleteOn(ctx context.Context, backend, container, artifact string) error {
	err := e.on(ctx, backend, func(d Driver) error { return d.Delete(ctx, container, artifact) })
	if err != nil {
		return fmt.Errorf("delete %s/%s on %s: %w", container, artifact, backend, err)
	}
	return nil
}

// ExistsOn asks the named backend only whether it holds an artifact.
func (e *CoreEngine) ExistsOn(ctx context.Context, backend, container, artifact string) (bool, error) {
	var exists bool
	err := e.on(ctx, backend, func(d Driver) error {
		var exErr error
		exists, exErr = d.Exists(ctx, container, artifact)
		return exErr
	})
	if err != nil {
		return false, fmt.Errorf("exists %s/%s on %s: %w", container, artifact, backend, err)
	}
	return exists, nil
}
