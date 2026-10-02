package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// ChunkMover is the operator job that brings chunk blobs written before
// WP-R8-7 to the one address (chunk_store.go). On a fixed-bucket backend
// such a blob is under the prefix of the tenant that uploaded it first:
// `t-<uploader>/_global/<storage_key>`; its address is
// `t-_global/_global/<storage_key>`.
//
// One run works on ONE backend and does two passes:
//
//  1. every global_content_index row whose blob is on that backend: is the
//     blob at the one address and does it verify? If not, a copy that
//     verifies is looked for under the tenants that could have written it
//     (the manifests that reference the chunk, the writes the engine
//     recorded, the ids the operator names), copied to the one address, read
//     back and verified — and only then is every old copy deleted;
//  2. every tenant's `_global` container on that backend is listed: a blob
//     there whose chunk still has a row is moved as above (it catches a
//     tenant pass 1 did not know to ask), a blob with no row at all is an
//     orphan — no manifest can reference it (tenant_chunk_refs has a foreign
//     key on the row) — and is deleted.
//
// Properties, each of them tested (chunk_move_test.go):
//   - dry run by default; a dry run reads only;
//   - "verified" is the size and the hash the row holds — plaintext hash
//     after decompression, ciphertext hash for an encrypted chunk;
//   - an old copy is deleted only after the one address was read back and
//     verified IN THE SAME CALL, under the chunk's advisory lock — the lock
//     the PUT path and dedup GC take;
//   - never on a backend where the address does not depend on the tenant
//     (local, s3compat): there the "old copy" IS the blob. Decided from the
//     driver's own key builder (engine.KeyAddresser), per delete;
//   - idempotent and resumable with no cursor: a run that stopped anywhere
//     — between a copy and its delete included — is finished by the next
//     run; what is done is found done.
type ChunkMover struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	logger *zap.Logger

	// BatchSize is the page of index rows read at a time.
	BatchSize int
	// tenantsOverride (tests): the tenants pass 2 lists instead of every
	// tenant in the database — the test database is shared.
	tenantsOverride []string
	// afterCopy (tests) runs between the verified copy and the delete of the
	// old copies: where a deploy would stop the run.
	afterCopy func()
}

// maxChunkBlobBytes bounds one blob read: the chunker's maximum is 16 MiB;
// compression or encryption framing never doubles it.
const maxChunkBlobBytes = 64 << 20

// ChunkMoveOptions selects one run.
type ChunkMoveOptions struct {
	Backend string
	// DryRun reads only and reports what a run would do.
	DryRun bool
	// ExtraTenants are prefixes to look under besides the ones the database
	// names — an erased tenant's id from an `account.erased` audit row whose
	// chunk_blobs_left was not 0.
	ExtraTenants []string
}

// ChunkMoveResult is the report of one run. In a dry run Moved, BytesMoved,
// LegacyDeleted and OrphansDeleted are what a run WOULD do.
type ChunkMoveResult struct {
	Object  string `json:"object"`
	Backend string `json:"backend"`
	DryRun  bool   `json:"dry_run"`
	// Rows is the number of index rows on the backend that were examined.
	Rows int `json:"rows"`
	// AtAddress: the blob is at the one address, verified, and no old copy
	// was found. When AtAddress == Rows and Orphans == 0 the move is done.
	AtAddress     int   `json:"at_address"`
	Moved         int   `json:"moved"`
	BytesMoved    int64 `json:"bytes_moved"`
	LegacyDeleted int   `json:"legacy_deleted"`
	// Missing: no copy that verifies was found anywhere — the object that
	// references the chunk cannot be read.
	Missing int `json:"missing"`
	// Failed: could not be decided or done this run; the next run retries.
	Failed int `json:"failed"`
	// Orphans: blobs under a tenant's chunk container with no index row.
	Orphans        int `json:"orphans"`
	OrphansDeleted int `json:"orphans_deleted"`
	// Unrecognized: names under a tenant's chunk container that are not a
	// chunk key, or that belong to a row on another backend. Left alone.
	Unrecognized  int      `json:"unrecognized"`
	TenantsListed int      `json:"tenants_listed"`
	Note          string   `json:"note,omitempty"`
	Errors        []string `json:"errors,omitempty"`
}

func (r *ChunkMoveResult) fail(format string, args ...any) {
	r.Failed++
	if len(r.Errors) < 20 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// NewChunkMover builds the mover (nil without a database or an engine).
func NewChunkMover(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *ChunkMover {
	if db == nil || eng == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ChunkMover{db: db, eng: eng, logger: logger, BatchSize: 500}
}

// errChunkMoveRequest is a refusal of the request itself (unknown backend).
var errChunkMoveRequest = errors.New("chunk move request")

// Run does one run on one backend.
func (m *ChunkMover) Run(ctx context.Context, opts ChunkMoveOptions) (ChunkMoveResult, error) {
	res := ChunkMoveResult{Object: "chunk_move", Backend: opts.Backend, DryRun: opts.DryRun}
	drv, ok := m.eng.GetDriver(opts.Backend)
	if !ok {
		return res, fmt.Errorf("%w: backend %q is not registered", errChunkMoveRequest, opts.Backend)
	}
	for _, tid := range opts.ExtraTenants {
		if engine.IsReservedTenantID(tid) || strings.Contains(tid, "/") {
			return res, fmt.Errorf("%w: %q is not a tenant id", errChunkMoveRequest, tid)
		}
	}
	keyer, ok := drv.(engine.KeyAddresser)
	if !ok {
		res.Note = "this backend's driver does not report its keys (engine.KeyAddresser): the move cannot tell an old copy from the blob itself and does nothing"
		return res, nil
	}
	if !m.legacyAddressDiffers(ctx, keyer, "tenant-probe", "_chunks/probe") {
		res.Note = "a chunk's address does not depend on the tenant on this backend: there is nothing to move"
		return res, nil
	}
	if state := m.eng.GetFailoverStatus()[opts.Backend]; state == engine.StateOpen.String() {
		return res, fmt.Errorf("backend %s: circuit breaker is open", opts.Backend)
	}

	run := &chunkMoveRun{m: m, opts: opts, res: &res, keyer: keyer, drv: drv,
		store: newChunkStore(m.eng, m.db, m.logger), counted: map[string]bool{}, unresolved: map[string]chunkAddr{}}
	err := run.rows(ctx)
	if err == nil {
		err = run.listings(ctx)
	}
	res.Missing = len(run.unresolved)
	if err != nil {
		// Cut short: "no copy found" is not known for a row whose tenants
		// were not all listed.
		res.Missing = 0
		return res, err
	}
	for _, a := range run.unresolved {
		m.logger.Error("chunk move: no copy of a chunk was found anywhere",
			zap.String("backend", a.backend), zap.String("scope", a.scope), zap.String("hash", a.hash))
	}
	return res, nil
}

// legacyAddressDiffers reports whether the tenant's legacy address of a key
// is a different object from the one address.
func (m *ChunkMover) legacyAddressDiffers(ctx context.Context, keyer engine.KeyAddresser, tenantID, key string) bool {
	one := keyer.ObjectKey(engine.ChunkContext(ctx), chunkContainer, key)
	old := keyer.ObjectKey(engine.LegacyChunkContext(ctx, tenantID), chunkContainer, key)
	return one != "" && old != "" && one != old
}

type chunkMoveRun struct {
	m     *ChunkMover
	opts  ChunkMoveOptions
	res   *ChunkMoveResult
	keyer engine.KeyAddresser
	drv   engine.Driver
	store *chunkStore
	// counted: the (tenant, key) addresses the row pass already examined —
	// whatever came of it — so the listing pass neither counts nor retries
	// them: one chunk, one line in the report.
	counted map[string]bool
	// unresolved: rows the row pass found no good copy for. The listing pass
	// may still find one under a tenant the rows did not name; what is left
	// at the end is Missing.
	unresolved map[string]chunkAddr
}

// rows is pass 1: every index row on the backend.
func (r *chunkMoveRun) rows(ctx context.Context) error {
	lastScope, lastHash := "", ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := r.rowBatch(ctx, lastScope, lastHash)
		if err != nil {
			return err
		}
		for _, a := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			lastScope, lastHash = a.scope, a.hash
			tenants, err := r.store.legacyTenants(ctx, a)
			if err != nil {
				r.res.Rows++
				r.res.fail("chunk %s: where it may be: %v", shortHash(a.hash), err)
				continue
			}
			tenants = mergeTenants(tenants, r.opts.ExtraTenants)
			for _, tid := range tenants {
				r.counted[tid+"\x00"+a.key] = true
			}
			r.moveOne(ctx, a, tenants, true)
		}
		if len(batch) < r.m.BatchSize {
			return nil
		}
	}
}

func (r *chunkMoveRun) rowBatch(ctx context.Context, afterScope, afterHash string) ([]chunkAddr, error) {
	rows, err := r.m.db.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash, storage_key
		  FROM global_content_index
		 WHERE backend_id = $1 AND (dedup_scope, plaintext_hash) > ($2, $3)
		 ORDER BY dedup_scope, plaintext_hash
		 LIMIT $4`, r.opts.Backend, afterScope, afterHash, r.m.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("read index rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []chunkAddr
	for rows.Next() {
		a := chunkAddr{backend: r.opts.Backend}
		if err := rows.Scan(&a.scope, &a.hash, &a.key); err != nil {
			return nil, fmt.Errorf("scan index row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listings is pass 2: what every tenant's chunk container holds.
func (r *chunkMoveRun) listings(ctx context.Context) error {
	tenants, err := r.tenants(ctx)
	if err != nil {
		return err
	}
	for _, tid := range tenants {
		if err := ctx.Err(); err != nil {
			return err
		}
		if engine.IsReservedTenantID(tid) || strings.Contains(tid, "/") {
			continue
		}
		names, err := r.drv.List(engine.LegacyChunkContext(ctx, tid), chunkContainer, "")
		r.res.TenantsListed++
		if err != nil {
			// Never read as "nothing there".
			r.res.fail("list the chunk container under tenant %s: %v", tid, err)
			continue
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if name == "" || r.counted[tid+"\x00"+name] {
				continue
			}
			r.listed(ctx, tid, name)
		}
	}
	return nil
}

// tenants are the prefixes pass 2 lists: every tenant, every tenant the
// engine recorded a chunk write for, and the ids the operator named.
func (r *chunkMoveRun) tenants(ctx context.Context) ([]string, error) {
	if r.m.tenantsOverride != nil {
		return mergeTenants(r.m.tenantsOverride, r.opts.ExtraTenants), nil
	}
	ids, err := queryStrings(ctx, r.m.db, `
		SELECT id FROM tenants
		UNION
		SELECT tenant_id FROM object_locations WHERE bucket = $1`, chunkContainer)
	if err != nil {
		return nil, fmt.Errorf("read tenants: %w", err)
	}
	out := mergeTenants(ids, r.opts.ExtraTenants)
	sort.Strings(out)
	return out, nil
}

func mergeTenants(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, tid := range append(append([]string(nil), a...), b...) {
		if tid == "" || seen[tid] {
			continue
		}
		seen[tid] = true
		out = append(out, tid)
	}
	return out
}

// chunkKeyIdentity is the (scope, hash) a storage key names: `_chunks/<hash>`
// is a shared chunk, `_chunks/<tenant>/<hash>` a tenant-scoped one.
func chunkKeyIdentity(key string) (scope, hash string, ok bool) {
	rest, found := strings.CutPrefix(key, "_chunks/")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	switch len(parts) {
	case 1:
		scope, hash = crypto.GlobalDedupScope, parts[0]
	case 2:
		scope, hash = parts[0], parts[1]
	default:
		return "", "", false
	}
	if len(hash) != 64 || scope == "" {
		return "", "", false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", "", false
	}
	return scope, hash, true
}

// listed handles one name found under a tenant's chunk container.
func (r *chunkMoveRun) listed(ctx context.Context, tenantID, name string) {
	scope, hash, ok := chunkKeyIdentity(name)
	if !ok {
		r.res.Unrecognized++
		return
	}
	var backend, key string
	err := r.m.db.QueryRowContext(ctx, `
		SELECT backend_id, storage_key FROM global_content_index
		 WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash).Scan(&backend, &key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.orphan(ctx, tenantID, chunkAddr{scope: scope, hash: hash, backend: r.opts.Backend, key: name})
	case err != nil:
		r.res.fail("chunk %s: read its row: %v", shortHash(hash), err)
	case backend != r.opts.Backend || key != name:
		// The row's blob is somewhere else; this is not the copy it names.
		r.res.Unrecognized++
	default:
		// A row pass 1 examined, with a copy under a tenant it did not ask.
		r.moveOne(ctx, chunkAddr{scope: scope, hash: hash, backend: backend, key: key}, []string{tenantID}, false)
	}
}

// withChunkLock runs fn holding the chunk's advisory lock — the lock a first
// store (storeChunkLocked) and dedup GC (sweepOne) take — on a dedicated
// connection, so a crash releases it.
func (r *chunkMoveRun) withChunkLock(ctx context.Context, a chunkAddr, fn func(conn *sql.Conn)) error {
	conn, err := r.m.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, a.scope, a.hash); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, a.scope, a.hash)
	}()
	fn(conn)
	return nil
}

// chunkRow is what verification needs from the index row.
type chunkRow struct {
	backend, key   string
	size           int64
	compressed     bool
	encrypted      bool
	ciphertextHash sql.NullString
}

// verify checks a blob against the row: for an encrypted chunk the SHA-256 of
// the blob is the row's ciphertext hash; otherwise the blob, decompressed
// when the row says it is compressed, has the row's size and plaintext hash.
func (row chunkRow) verify(hash string, blob []byte) error {
	if row.encrypted {
		if !row.ciphertextHash.Valid || row.ciphertextHash.String == "" {
			return errors.New("encrypted chunk with no ciphertext hash on its row: cannot be verified")
		}
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != row.ciphertextHash.String {
			return errors.New("ciphertext hash mismatch")
		}
		return nil
	}
	plain := blob
	if row.compressed {
		var err error
		if plain, err = crypto.DecompressBuffer(blob); err != nil {
			return fmt.Errorf("decompress: %w", err)
		}
	}
	if int64(len(plain)) != row.size {
		return fmt.Errorf("size %d, the row says %d", len(plain), row.size)
	}
	sum := sha256.Sum256(plain)
	if hex.EncodeToString(sum[:]) != hash {
		return errors.New("plaintext hash mismatch")
	}
	return nil
}

type blobState int

const (
	blobOK      blobState = iota // read and verified
	blobMissing                  // the backend says it is not there
	blobBad                      // read, does not verify
	blobUnknown                  // the backend could not be asked
)

// readVerify reads the blob at the address ctx names and checks it.
func (r *chunkMoveRun) readVerify(ctx context.Context, a chunkAddr, row chunkRow) ([]byte, blobState, error) {
	rc, err := r.store.getAt(ctx, a)
	if err != nil {
		if isObjectMissingErr(err) {
			return nil, blobMissing, nil
		}
		return nil, blobUnknown, err
	}
	blob, err := io.ReadAll(io.LimitReader(rc, maxChunkBlobBytes+1))
	_ = rc.Close()
	if err != nil {
		return nil, blobUnknown, err
	}
	if len(blob) > maxChunkBlobBytes {
		return nil, blobBad, fmt.Errorf("larger than %d bytes", maxChunkBlobBytes)
	}
	if err := row.verify(a.hash, blob); err != nil {
		return nil, blobBad, err
	}
	return blob, blobOK, nil
}

// moveOne brings one chunk to the one address and removes its old copies
// under the given tenants. countRow: this call is the row's examination
// (pass 1); pass 2 only adds the copies it found.
func (r *chunkMoveRun) moveOne(ctx context.Context, a chunkAddr, tenants []string, countRow bool) {
	if countRow {
		r.res.Rows++
	}
	err := r.withChunkLock(ctx, a, func(conn *sql.Conn) {
		// The row, re-read under the lock: dedup GC may have swept it, a
		// re-store may have put it on another backend.
		var row chunkRow
		var compression sql.NullString
		err := conn.QueryRowContext(ctx, `
			SELECT backend_id, storage_key, size_bytes, compression_algo, encrypted, ciphertext_hash
			  FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			a.scope, a.hash).Scan(&row.backend, &row.key, &row.size, &compression, &row.encrypted, &row.ciphertextHash)
		if errors.Is(err, sql.ErrNoRows) {
			if countRow {
				r.res.Rows--
			}
			return // swept since the scan; a copy left behind is pass 2's orphan
		}
		if err != nil {
			r.res.fail("chunk %s: read its row: %v", shortHash(a.hash), err)
			return
		}
		if row.backend != a.backend || row.key != a.key {
			if countRow {
				r.res.Rows--
			}
			return
		}
		row.compressed = compression.Valid && compression.String != ""

		one := engine.ChunkContext(ctx)
		_, state, err := r.readVerify(one, a, row)
		if state == blobUnknown {
			r.res.fail("chunk %s: read at the one address: %v", shortHash(a.hash), err)
			return
		}
		atAddress := state == blobOK
		if !atAddress {
			blob, found, hardErr := r.goodLegacyCopy(ctx, a, row, tenants)
			switch {
			case !found && hardErr != nil:
				r.res.fail("chunk %s: an old copy could not be read: %v", shortHash(a.hash), hardErr)
				return
			case !found && state == blobBad:
				r.res.fail("chunk %s: the blob at the one address does not verify (%v) and no good copy was found", shortHash(a.hash), err)
				return
			case !found:
				// Not a verdict yet: the listing pass may find a copy under
				// a tenant these rows did not name.
				r.unresolved[a.scope+"\x00"+a.hash] = a
				return
			}
			delete(r.unresolved, a.scope+"\x00"+a.hash)
			if r.opts.DryRun {
				r.res.Moved++
				r.res.BytesMoved += int64(len(blob))
			} else {
				if err := r.m.eng.PutOn(one, a.backend, chunkContainer, a.key, bytes.NewReader(blob),
					engine.WithContentLength(int64(len(blob)))); err != nil {
					r.res.fail("chunk %s: copy to the one address: %v", shortHash(a.hash), err)
					return
				}
				// Read back: what is deleted next must be replaceable by
				// what is there now, not by what was sent.
				if _, state, err := r.readVerify(one, a, row); state != blobOK {
					r.res.fail("chunk %s: the copy at the one address does not read back (%v) — old copy kept", shortHash(a.hash), err)
					return
				}
				r.res.Moved++
				r.res.BytesMoved += int64(len(blob))
				if r.m.afterCopy != nil {
					r.m.afterCopy()
				}
				if ctx.Err() != nil {
					return // the next run finds the copy and deletes the old ones
				}
			}
		}

		// The one address holds a verified blob (or would, in a dry run):
		// the old copies go.
		removed := 0
		for _, tid := range tenants {
			n, err := r.removeLegacy(ctx, a, tid)
			if err != nil {
				r.res.fail("chunk %s: old copy under tenant %s: %v", shortHash(a.hash), tid, err)
				continue
			}
			removed += n
		}
		r.res.LegacyDeleted += removed
		if atAddress && removed == 0 && countRow {
			r.res.AtAddress++
		}
	})
	if err != nil {
		r.res.fail("chunk %s: %v", shortHash(a.hash), err)
	}
}

// goodLegacyCopy returns the first old copy that verifies. An address that
// could not be asked is reported only when no good copy was found.
func (r *chunkMoveRun) goodLegacyCopy(ctx context.Context, a chunkAddr, row chunkRow, tenants []string) ([]byte, bool, error) {
	var hardErr error
	for _, tid := range tenants {
		if !r.m.legacyAddressDiffers(ctx, r.keyer, tid, a.key) {
			continue
		}
		blob, state, err := r.readVerify(engine.LegacyChunkContext(ctx, tid), a, row)
		switch state {
		case blobOK:
			return blob, true, nil
		case blobUnknown:
			if hardErr == nil {
				hardErr = fmt.Errorf("under tenant %s: %w", tid, err)
			}
		case blobBad:
			r.m.logger.Warn("chunk move: an old copy does not verify — left in place",
				zap.String("backend", a.backend), zap.String("hash", a.hash), zap.String("under_tenant", tid), zap.Error(err))
		}
	}
	return nil, false, hardErr
}

// removeLegacy deletes the copy of the chunk under one tenant's prefix, if
// there is one. Returns how many copies it removed (or would remove).
func (r *chunkMoveRun) removeLegacy(ctx context.Context, a chunkAddr, tenantID string) (int, error) {
	// The address must be a different object from the one address — asked
	// of the driver's own key builder, for this key, right before the delete.
	if !r.m.legacyAddressDiffers(ctx, r.keyer, tenantID, a.key) {
		return 0, nil
	}
	old := engine.LegacyChunkContext(ctx, tenantID)
	exists, err := r.store.existsAt(old, a)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	if r.opts.DryRun {
		return 1, nil
	}
	if err := r.m.eng.DeleteOn(old, a.backend, chunkContainer, a.key); err != nil && !isObjectMissingErr(err) {
		return 0, err
	}
	return 1, nil
}

// orphan handles a blob under a tenant's chunk container whose chunk has no
// index row: nothing references it and nothing ever will at this address (a
// new first store writes at the one address).
func (r *chunkMoveRun) orphan(ctx context.Context, tenantID string, a chunkAddr) {
	if !r.m.legacyAddressDiffers(ctx, r.keyer, tenantID, a.key) {
		return
	}
	err := r.withChunkLock(ctx, a, func(conn *sql.Conn) {
		var one int
		err := conn.QueryRowContext(ctx, `
			SELECT 1 FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			a.scope, a.hash).Scan(&one)
		if err == nil {
			return // a row appeared: the next run moves it
		}
		if !errors.Is(err, sql.ErrNoRows) {
			r.res.fail("orphan %s: read its row: %v", shortHash(a.hash), err)
			return
		}
		r.res.Orphans++
		if r.opts.DryRun {
			return
		}
		if err := r.m.eng.DeleteOn(engine.LegacyChunkContext(ctx, tenantID), a.backend, chunkContainer, a.key); err != nil && !isObjectMissingErr(err) {
			r.res.fail("orphan %s under tenant %s: %v", shortHash(a.hash), tenantID, err)
			return
		}
		r.res.OrphansDeleted++
	})
	if err != nil {
		r.res.fail("orphan %s: %v", shortHash(a.hash), err)
	}
}

// handleChunkMove serves POST /api/v1/admin/chunk-move.
//
//	backend   required: the registered backend to work on
//	dry_run   default true; only dry_run=false changes anything
//	tenant    repeatable: an extra tenant id to look under
//
// Synchronous, on a context detached from the request: a proxy timeout or a
// closed tab does not stop a run half way (and a run stopped half way is
// finished by the next one). One run at a time (409).
func (s *Server) handleChunkMove(w http.ResponseWriter, r *http.Request) {
	if s.chunkMover == nil {
		http.Error(w, "chunk move not available (no database or engine)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	opts := ChunkMoveOptions{Backend: q.Get("backend"), DryRun: true, ExtraTenants: q["tenant"]}
	if opts.Backend == "" {
		http.Error(w, "backend is required (a registered backend name, e.g. idrive)", http.StatusBadRequest)
		return
	}
	if v := q.Get("dry_run"); v != "" {
		dry, err := strconv.ParseBool(v)
		if err != nil {
			http.Error(w, "dry_run must be true or false", http.StatusBadRequest)
			return
		}
		opts.DryRun = dry
	}
	if !s.chunkMoveGate.tryAcquire() {
		writeJobAlreadyRunning(w, "chunk_move")
		return
	}
	defer s.chunkMoveGate.release()
	ctx, cancel := adminTriggerContext(r)
	defer cancel()

	res, err := s.chunkMover.Run(ctx, opts)
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.chunk_move", Error: err,
		Metadata: map[string]any{"backend": res.Backend, "dry_run": res.DryRun, "rows": res.Rows, "moved": res.Moved,
			"legacy_deleted": res.LegacyDeleted, "orphans": res.Orphans, "missing": res.Missing, "failed": res.Failed}})
	s.logger.Info("chunk move", zap.String("backend", res.Backend), zap.Bool("dry_run", res.DryRun),
		zap.Int("rows", res.Rows), zap.Int("at_address", res.AtAddress), zap.Int("moved", res.Moved),
		zap.Int64("bytes_moved", res.BytesMoved), zap.Int("legacy_deleted", res.LegacyDeleted),
		zap.Int("missing", res.Missing), zap.Int("failed", res.Failed), zap.Int("orphans", res.Orphans),
		zap.Int("orphans_deleted", res.OrphansDeleted), zap.Int("unrecognized", res.Unrecognized),
		zap.Strings("errors", res.Errors), zap.Error(err))
	if errors.Is(err, errChunkMoveRequest) {
		http.Error(w, "chunk move: the backend is not registered on this server, or a tenant parameter is not a tenant id", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "chunk move failed (see the log); what was done is kept and the next run continues", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
