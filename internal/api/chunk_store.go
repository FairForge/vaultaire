package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// chunkStore is the one way a chunk blob is read, written, checked or
// deleted (WP-R8-7). A chunk has ONE address — backend (its index row's
// backend_id) + engine.ChunkContainer + engine.ChunkAddressTenant in the
// driver context — and every call here builds it through engine.ChunkContext,
// whatever request or job is calling. No call site outside this file and
// chunk_move.go names the chunk container in a storage call
// (TestChunkBlobCalls_GoThroughTheChunkStore).
//
// Blobs written before WP-R8-7 sit under the uploading tenant's prefix on a
// fixed-bucket backend. READS fall back to that address when the one address
// answers "not there" — and only then: an error is never read as a miss.
// Writes and deletes never fall back. The chunk move (chunk_move.go) copies
// the old blobs to the one address; when
// vaultaire_chunk_legacy_address_reads_total stays at 0 after it, the
// fallback (legacyTenants, getLegacy, legacyCopyExists and
// engine.LegacyChunkContext) can be deleted.
type chunkStore struct {
	eng    engine.Engine
	db     *sql.DB
	logger *zap.Logger
}

// chunkAddr names one chunk: its index key and where its row says the blob is.
type chunkAddr struct {
	scope   string // dedup scope: crypto.GlobalDedupScope or a tenant id
	hash    string // plaintext hash
	backend string // global_content_index.backend_id
	key     string // global_content_index.storage_key
}

// singleBackendEngine is the slice of *engine.CoreEngine that addresses one
// backend: a chunk lives on the backend its row names and nowhere else.
type singleBackendEngine interface {
	GetOn(ctx context.Context, backend, container, artifact string) (io.ReadCloser, error)
	PutOn(ctx context.Context, backend, container, artifact string, data io.Reader, opts ...engine.PutOption) error
	DeleteOn(ctx context.Context, backend, container, artifact string) error
	ExistsOn(ctx context.Context, backend, container, artifact string) (bool, error)
}

// chunkLegacyReads counts chunk reads served from a legacy address. A plain
// counter: it exists at 0 from the first scrape.
var chunkLegacyReads = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "vaultaire_chunk_legacy_address_reads_total",
	Help: "Chunk reads that missed at the one address and were served from the uploading tenant's prefix (blobs written before WP-R8-7). 0 after the chunk move; then the read fallback can be removed.",
})

// legacyLogged remembers which chunks' legacy reads were logged: one line
// per chunk, not one per read. Bounded — when full it starts over.
var legacyLogged = struct {
	sync.Mutex
	seen map[string]struct{}
}{seen: map[string]struct{}{}}

const legacyLoggedMax = 8192

func firstLegacyRead(scope, hash string) bool {
	k := scope + "/" + hash
	legacyLogged.Lock()
	defer legacyLogged.Unlock()
	if _, ok := legacyLogged.seen[k]; ok {
		return false
	}
	if len(legacyLogged.seen) >= legacyLoggedMax {
		legacyLogged.seen = map[string]struct{}{}
	}
	legacyLogged.seen[k] = struct{}{}
	return true
}

func newChunkStore(eng engine.Engine, db *sql.DB, logger *zap.Logger) *chunkStore {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &chunkStore{eng: eng, db: db, logger: logger}
}

// put stores a NEW chunk's blob at the one address and returns the backend
// it landed on (the caller records it as the row's backend_id). Placement is
// the engine's: the primary, failing over among the general-purpose backends.
func (c *chunkStore) put(ctx context.Context, storageKey string, data []byte) (string, error) {
	return c.eng.Put(engine.ChunkContext(ctx), chunkContainer, storageKey,
		bytes.NewReader(data), engine.WithContentLength(int64(len(data))))
}

// getAt reads the blob at the address ctx names, from the row's backend only.
func (c *chunkStore) getAt(ctx context.Context, a chunkAddr) (io.ReadCloser, error) {
	if sb, ok := c.eng.(singleBackendEngine); ok && a.backend != "" {
		return sb.GetOn(ctx, a.backend, chunkContainer, a.key)
	}
	return c.eng.Get(ctx, chunkContainer, a.key)
}

// get reads a chunk's blob: the one address, then — only when that address
// answers "not there" — where a blob written before WP-R8-7 would be.
func (c *chunkStore) get(ctx context.Context, a chunkAddr) (io.ReadCloser, error) {
	rc, err := c.getAt(engine.ChunkContext(ctx), a)
	if err == nil {
		return rc, nil
	}
	if !isObjectMissingErr(err) {
		// Unreachable, refused, timed out: not a statement about the blob.
		return nil, err
	}
	rc, tenantID, legacyErr := c.getLegacy(ctx, a)
	if legacyErr != nil {
		return nil, legacyErr
	}
	if rc == nil {
		return nil, err
	}
	chunkLegacyReads.Inc()
	if firstLegacyRead(a.scope, a.hash) {
		c.logger.Warn("chunk read from a legacy address — run the chunk move (POST /api/v1/admin/chunk-move)",
			zap.String("backend", a.backend), zap.String("scope", a.scope),
			zap.String("hash", a.hash), zap.String("under_tenant", tenantID))
	}
	return rc, nil
}

// getLegacy looks for the blob under the tenants that could have written it.
// (nil, "", nil) = not found anywhere. A candidate that ERRORS does not end
// the search — found elsewhere is found — but if nothing is found the error
// is returned: "one place could not be asked" is not "not there".
func (c *chunkStore) getLegacy(ctx context.Context, a chunkAddr) (io.ReadCloser, string, error) {
	var hardErr error
	tried := map[string]bool{}
	for _, source := range []func(context.Context, chunkAddr) ([]string, error){c.referencingTenants, c.recordedWriters} {
		tenants, err := source(ctx, a)
		if err != nil {
			return nil, "", fmt.Errorf("chunk %s: legacy address lookup: %w", shortHash(a.hash), err)
		}
		for _, tid := range tenants {
			if tried[tid] {
				continue
			}
			tried[tid] = true
			rc, err := c.getAt(engine.LegacyChunkContext(ctx, tid), a)
			if err == nil {
				return rc, tid, nil
			}
			if !isObjectMissingErr(err) && hardErr == nil {
				hardErr = err
			}
		}
	}
	return nil, "", hardErr
}

// legacyTenantLimit bounds how many prefixes one read tries. A legacy blob is
// under its FIRST uploader's prefix; both sources return the earliest first.
const legacyTenantLimit = 16

// referencingTenants are the tenants whose manifests name the chunk, the
// earliest reference first (indexed: idx_tcr_hash).
func (c *chunkStore) referencingTenants(ctx context.Context, a chunkAddr) ([]string, error) {
	if c.db == nil {
		return nil, nil
	}
	return queryStrings(ctx, c.db, `
		SELECT tenant_id FROM tenant_chunk_refs
		 WHERE plaintext_hash = $1 AND dedup_scope = $2
		 GROUP BY tenant_id ORDER BY MIN(created_at) LIMIT $3`,
		a.hash, a.scope, legacyTenantLimit)
}

// recordedWriters are the tenants the engine recorded a write of this blob
// for (object_locations: one row per Put, keyed by the tenant in the context
// of the write — exactly the legacy prefix). It reaches a blob whose uploader
// has since deleted its object while another tenant still references the
// chunk. Not indexed by key: asked only after the referencing tenants missed.
func (c *chunkStore) recordedWriters(ctx context.Context, a chunkAddr) ([]string, error) {
	if c.db == nil {
		return nil, nil
	}
	return queryStrings(ctx, c.db, `
		SELECT tenant_id FROM object_locations
		 WHERE bucket = $1 AND object_key = $2 AND tenant_id <> $3
		 ORDER BY stored_at LIMIT $4`,
		chunkContainer, a.key, engine.ChunkAddressTenant, legacyTenantLimit)
}

// legacyTenants is every tenant a legacy copy of the chunk may be under.
func (c *chunkStore) legacyTenants(ctx context.Context, a chunkAddr) ([]string, error) {
	refs, err := c.referencingTenants(ctx, a)
	if err != nil {
		return nil, err
	}
	writers, err := c.recordedWriters(ctx, a)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, tid := range append(refs, writers...) {
		if tid == "" || tid == engine.ChunkAddressTenant || seen[tid] {
			continue
		}
		seen[tid] = true
		out = append(out, tid)
	}
	return out, nil
}

// exists asks the row's backend whether the blob is at the one address.
func (c *chunkStore) exists(ctx context.Context, a chunkAddr) (bool, error) {
	return c.existsAt(engine.ChunkContext(ctx), a)
}

func (c *chunkStore) existsAt(ctx context.Context, a chunkAddr) (bool, error) {
	sb, ok := c.eng.(singleBackendEngine)
	if !ok || a.backend == "" {
		return false, errors.New("chunk exists: the engine cannot address a single backend")
	}
	return sb.ExistsOn(ctx, a.backend, chunkContainer, a.key)
}

// legacyCopyExists reports whether a copy of the blob is still at a legacy
// address (dedup GC asks before it deletes the row of a chunk that is not at
// the one address: the row is what the chunk move works from).
func (c *chunkStore) legacyCopyExists(ctx context.Context, a chunkAddr) (bool, error) {
	tenants, err := c.legacyTenants(ctx, a)
	if err != nil {
		return false, err
	}
	for _, tid := range tenants {
		ok, err := c.existsAt(engine.LegacyChunkContext(ctx, tid), a)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// delete removes the blob at the one address, on the row's backend. A miss is
// success (the blob is gone, which is what was asked). Never a legacy address.
func (c *chunkStore) delete(ctx context.Context, a chunkAddr) error {
	ctx = engine.ChunkContext(ctx)
	var err error
	if sb, ok := c.eng.(singleBackendEngine); ok && a.backend != "" {
		err = sb.DeleteOn(ctx, a.backend, chunkContainer, a.key)
	} else {
		err = c.eng.Delete(ctx, chunkContainer, a.key)
	}
	if err != nil && isObjectMissingErr(err) {
		return nil
	}
	return err
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
