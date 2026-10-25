package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// keyReencryptJobName is the job's name in job_runs and on the metrics.
const keyReencryptJobName = "key_reencrypt"

const (
	// keyReencryptRetireAfter: how long the old address of a re-encrypted
	// chunk is kept, for a reader that resolved the row before the swap.
	// The bound that matters is not another process's cached row
	// (gciCacheTTL, 5 min) but a stream still on its plan: a chunked GET
	// resolves every chunk when it starts, and 10 GiB at 10 MB/s is 17 min
	// (a tenant paced at the 64 KiB/s egress floor needs 17 min per 64 MiB
	// chunk). 15 min cut those at their next chunk. The GET now re-reads a
	// chunk's row when its address answers "not there" (WP-R8-1 A2), so the
	// grace is the fallback for a reader that cannot re-read (an older
	// build during a switch, a cached plan elsewhere): 1 h.
	keyReencryptRetireAfter = time.Hour
	// keyReencryptScanLimit bounds one run's candidate scan; the byte budget
	// usually stops it first.
	keyReencryptScanLimit    = 10000
	defaultKeyReencryptMaxGB = 50
)

// keyReencryptChunks counts the job's per-chunk outcomes.
var keyReencryptChunks = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_key_reencrypt_chunks_total",
	Help: "Chunks the key_reencrypt job handled, by outcome: reencrypted (sealed under the tenant's newest key, row swapped) | failed (left on its version, a note on the run) | retired_deleted (an old address deleted after its grace) | retired_kept (an old address an index row names again: not deleted).",
}, []string{"outcome"})

func init() {
	for _, o := range []string{"reencrypted", "failed", "retired_deleted", "retired_kept"} {
		keyReencryptChunks.WithLabelValues(o)
	}
}

// KeyReencryptRunner moves a tenant's encrypted chunks onto its newest key
// version after a rotation (WP-R8-1). Every 10 minutes:
//
//  1. old addresses whose grace has passed (retired_chunk_blobs) are deleted
//     under the chunk's lock — unless an index row names the address again
//     (dedup GC swept the row and a PUT re-stored the chunk there);
//
//  2. for tenants with a retired version, the encrypted rows below the
//     newest version, oldest first, up to KEY_REENCRYPT_MAX_GB_PER_RUN:
//     under the chunk's advisory lock (the PUT path's and the GC's) the row
//     is re-read, its blob read and checked against ciphertext_hash, opened
//     with the version it records, sealed with the newest, written to a NEW
//     key (_chunks/<tenant>/<hash>.k<N>), the row swapped only if it still
//     names the old blob, the cache entry dropped, and the old address
//     retired for keyReencryptRetireAfter — never deleted in the same run.
//
//  3. whole SSE-S3 objects — head rows and retained versions — within what
//     is left of the same budget (WP-R2-1 Part C, PR C2,
//     key_reencrypt_whole.go): re-sealed at a NEW address on the same
//     backend, every row naming the old address and version re-pointed in
//     one transaction, the old address retired ('reencrypted') for the
//     retired_blobs collector — never overwritten in place (the R2-05/R2-15
//     class) and never deleted in the run.
type KeyReencryptRunner struct {
	db     *sql.DB
	eng    engine.Engine
	gci    *crypto.GlobalContentIndex
	enc    *crypto.ChunkEncryptionService
	logger *zap.Logger

	// MaxBytesPerRun is the run's budget of chunk bytes (plaintext sizes).
	MaxBytesPerRun int64
	// RetireAfter is the old address's grace.
	RetireAfter time.Duration
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
	now     func() time.Time
	// onlyScope limits a run to one tenant's chunks and objects (tests: other
	// packages rotate their own tenants on the same database).
	onlyScope string
	// sse opens and re-seals whole SSE-S3 objects; nil = the whole-object
	// pass is off (set by the server from its SSE service).
	sse *crypto.SSEService

	// The credential pass (plan 43.7, credential_backfill.go): credKeys nil
	// = the chunk service's key store (tests inject a sabotaged seal);
	// credentialBatch 0 = defaultCredentialBatch; beforeCredentialWrite is a
	// test seam between the check and the write (the race).
	credKeys              credentialStore
	credentialBatch       int
	beforeCredentialWrite func(table, keyID string)
}

// KeyReencryptResult is one run's outcome, recorded as job_runs.result.
type KeyReencryptResult struct {
	// Rewrapped / UnwrapFailures: the master re-key pass, run first while
	// ENCRYPTION_MASTER_KEY_PREVIOUS is set (WP-R8-1 follow-up A3). PREVIOUS
	// may be removed once the old slot is stopped and a run reports both 0.
	Rewrapped      int  `json:"rewrapped"`
	UnwrapFailures int  `json:"unwrap_failures"`
	MasterRekey    bool `json:"master_rekey,omitempty"`

	Candidates     int   `json:"candidates"`
	Reencrypted    int   `json:"reencrypted"`
	Failed         int   `json:"failed"`
	Bytes          int64 `json:"bytes"`
	BudgetReached  bool  `json:"budget_reached,omitempty"`
	RetiredDeleted int   `json:"retired_deleted"`
	RetiredKept    int   `json:"retired_kept,omitempty"`
	RetiredFailed  int   `json:"retired_failed,omitempty"`

	// Whole SSE-S3 objects (WP-R2-1 C2): blobs — a head row's or a retained
	// version's — below the newest version. Changed = a write or a delete
	// changed the rows first (the new blob queued as an orphan). Skipped by
	// reason (archive, demotion, parity, export, unavailable): still pending.
	ObjectCandidates   int            `json:"object_candidates"`
	ObjectsReencrypted int            `json:"objects_reencrypted"`
	ObjectsChanged     int            `json:"objects_changed,omitempty"`
	ObjectsFailed      int            `json:"objects_failed,omitempty"`
	ObjectsSkipped     map[string]int `json:"objects_skipped,omitempty"`

	// S3 secrets (plan 43.7, credential_backfill.go): live rows sealed for
	// the first time, re-sealed under the tenant's newest version, lost to
	// another writer, and not moved (a note; they stay candidates).
	CredentialsWrapped   int `json:"credentials_wrapped"`
	CredentialsRewrapped int `json:"credentials_rewrapped"`
	CredentialsRaced     int `json:"credentials_raced,omitempty"`
	CredentialFailures   int `json:"credential_failures"`
	// Errors are the first few per-chunk/per-object failures (the rest are
	// in the log).
	Errors []string `json:"errors,omitempty"`
}

func (r *KeyReencryptResult) fail(format string, args ...any) {
	if len(r.Errors) < 10 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// NewKeyReencryptRunner builds the runner; nil without a database, an
// engine, the index or the chunk encryption service (no master key = no
// tenant keys = nothing to re-encrypt).
func NewKeyReencryptRunner(db *sql.DB, eng engine.Engine, gci *crypto.GlobalContentIndex, enc *crypto.ChunkEncryptionService, logger *zap.Logger) *KeyReencryptRunner {
	if db == nil || eng == nil || gci == nil || enc == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	maxGB := int64(defaultKeyReencryptMaxGB)
	if v := os.Getenv("KEY_REENCRYPT_MAX_GB_PER_RUN"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			logger.Warn("KEY_REENCRYPT_MAX_GB_PER_RUN rejected, default kept",
				zap.String("value", v), zap.Int64("default", maxGB))
		} else {
			maxGB = n
		}
	}
	return &KeyReencryptRunner{db: db, eng: eng, gci: gci, enc: enc, logger: logger,
		MaxBytesPerRun: maxGB << 30, RetireAfter: keyReencryptRetireAfter,
		JobName: keyReencryptJobName, now: time.Now}
}

// spec: every 10 min, first run 4 min after boot, 30 min ceiling. A run is
// `error` only when the candidate scan (or the retired-blob scan) failed; a
// chunk that could not be moved is a note and stays a candidate.
func (k *KeyReencryptRunner) spec() jobSpec {
	return jobSpec{Name: k.JobName, Every: 10 * time.Minute, BootDelay: 4 * time.Minute, MaxRunTime: 30 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := k.RunOnce(ctx)
			k.logger.Info("key re-encrypt run", zap.Bool("master_rekey", res.MasterRekey), zap.Int("rewrapped", res.Rewrapped),
				zap.Int("unwrap_failures", res.UnwrapFailures), zap.Int("candidates", res.Candidates), zap.Int("reencrypted", res.Reencrypted),
				zap.Int("failed", res.Failed), zap.Int64("bytes", res.Bytes), zap.Bool("budget_reached", res.BudgetReached),
				zap.Int("retired_deleted", res.RetiredDeleted), zap.Int("retired_kept", res.RetiredKept),
				zap.Int("retired_failed", res.RetiredFailed), zap.Int("object_candidates", res.ObjectCandidates),
				zap.Int("objects_reencrypted", res.ObjectsReencrypted), zap.Int("objects_changed", res.ObjectsChanged),
				zap.Int("objects_failed", res.ObjectsFailed), zap.Any("objects_skipped", res.ObjectsSkipped),
				zap.Int("credentials_wrapped", res.CredentialsWrapped), zap.Int("credentials_rewrapped", res.CredentialsRewrapped),
				zap.Int("credentials_raced", res.CredentialsRaced), zap.Int("credential_failures", res.CredentialFailures), zap.Error(err))
			rep := jobReport{Rows: int64(res.Reencrypted + res.ObjectsReencrypted + res.CredentialsWrapped + res.CredentialsRewrapped), Result: res}
			var notes []string
			if res.Failed > 0 || res.RetiredFailed > 0 {
				notes = append(notes, fmt.Sprintf("%d chunk(s) not re-encrypted, %d old address(es) not deleted (see the log); they stay for the next run",
					res.Failed, res.RetiredFailed))
			}
			if res.CredentialFailures > 0 {
				notes = append(notes, fmt.Sprintf("%d S3 secret(s) not wrapped (see the log); they stay for the next run", res.CredentialFailures))
			}
			if res.ObjectsFailed > 0 {
				notes = append(notes, fmt.Sprintf("%d whole object(s) not re-encrypted (see the log); they stay for the next run", res.ObjectsFailed))
			}
			if n := skippedTotal(res.ObjectsSkipped); n > 0 {
				notes = append(notes, fmt.Sprintf("%d whole object(s) skipped and still pending %v (archive = never restored from tape; demotion/parity = moved by their own jobs later)",
					n, res.ObjectsSkipped))
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		}}
}

// RunOnce does one pass: the master re-wrap while a master re-key is in
// progress, the S3 secrets' wrap and re-wrap (plan 43.7), retired chunk
// blobs, the chunks' re-encryption, then whole objects' (one byte budget for
// both).
func (k *KeyReencryptRunner) RunOnce(ctx context.Context) (KeyReencryptResult, error) {
	var res KeyReencryptResult
	if err := k.rewrapMaster(ctx, &res); err != nil {
		return res, err
	}
	if err := k.wrapCredentials(ctx, &res); err != nil {
		return res, err
	}
	if err := k.deleteRetired(ctx, &res); err != nil {
		return res, err
	}
	if err := k.reencrypt(ctx, &res); err != nil {
		return res, err
	}
	err := k.reencryptObjects(ctx, &res)
	return res, err
}

func skippedTotal(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// rewrapMaster re-wraps, while ENCRYPTION_MASTER_KEY_PREVIOUS is set, every
// tenant key row that opens only under it. The boot pass sees the rows that
// existed at boot; the old slot of a two-slot switch keeps creating rows
// under the old master for up to LONG_OP_TIMEOUT after it, and a row nobody
// reads before PREVIOUS is removed would then open under no key (WP-R8-1
// follow-up A3). The run's result says when PREVIOUS can go.
func (k *KeyReencryptRunner) rewrapMaster(ctx context.Context, res *KeyReencryptResult) error {
	keys := k.enc.Keys()
	if keys == nil || !keys.HasPrevious() {
		return nil
	}
	res.MasterRekey = true
	var rr crypto.RewrapResult
	var err error
	if k.onlyScope != "" {
		rr, err = keys.RewrapTenant(ctx, k.onlyScope)
	} else {
		rr, err = keys.RewrapAll(ctx)
	}
	if err != nil {
		return fmt.Errorf("master re-wrap pass: %w", err)
	}
	res.Rewrapped = rr.Rewrapped
	res.UnwrapFailures = len(rr.Failed)
	if k.onlyScope == "" {
		tenantKeysUnwrapFailures.Set(float64(len(rr.Failed)))
	}
	if len(rr.Failed) > 0 {
		shown := rr.Failed
		if len(shown) > 50 {
			shown = shown[:50]
		}
		k.logger.Error("tenant keys that open under no configured master key: these tenants' encrypted objects answer 500 until the right ENCRYPTION_MASTER_KEY (or _PREVIOUS) is set",
			zap.Int("count", len(rr.Failed)), zap.Strings("first", shown))
	}
	return nil
}

// deleteRetired deletes old addresses past their grace.
func (k *KeyReencryptRunner) deleteRetired(ctx context.Context, res *KeyReencryptResult) error {
	rows, err := k.db.QueryContext(ctx, `
		SELECT backend_id, storage_key, dedup_scope, plaintext_hash
		  FROM retired_chunk_blobs WHERE retire_after <= $1 AND ($3 = '' OR dedup_scope = $3)
		 ORDER BY retire_after LIMIT $2`, k.now(), keyReencryptScanLimit, k.onlyScope)
	if err != nil {
		return fmt.Errorf("read retired chunk blobs: %w", err)
	}
	var due []chunkAddr
	for rows.Next() {
		var a chunkAddr
		if err := rows.Scan(&a.backend, &a.key, &a.scope, &a.hash); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan retired chunk blob: %w", err)
		}
		due = append(due, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read retired chunk blobs: %w", err)
	}
	_ = rows.Close()

	store := newChunkStore(k.eng, k.db, k.logger)
	for _, a := range due {
		if ctx.Err() != nil {
			return nil // the rest waits for the next run
		}
		err := withChunkLock(ctx, k.db, a.scope, a.hash, func(conn *sql.Conn) error {
			var live bool
			if err := conn.QueryRowContext(ctx, `
				SELECT EXISTS (SELECT 1 FROM global_content_index WHERE backend_id = $1 AND storage_key = $2)`,
				a.backend, a.key).Scan(&live); err != nil {
				return fmt.Errorf("is the address live: %w", err)
			}
			if live {
				res.RetiredKept++
				keyReencryptChunks.WithLabelValues("retired_kept").Inc()
			} else {
				if err := store.delete(ctx, a); err != nil {
					return fmt.Errorf("delete: %w", err)
				}
				res.RetiredDeleted++
				keyReencryptChunks.WithLabelValues("retired_deleted").Inc()
			}
			_, err := conn.ExecContext(ctx, `DELETE FROM retired_chunk_blobs WHERE backend_id = $1 AND storage_key = $2`, a.backend, a.key)
			return err
		})
		if err != nil {
			// A failed delete keeps the row: the next run tries again.
			res.RetiredFailed++
			res.fail("old address %s on %s: %v", a.key, a.backend, err)
			k.logger.Warn("key re-encrypt: old chunk address not deleted (kept for the next run)",
				zap.String("backend", a.backend), zap.String("key", a.key), zap.Error(err))
		}
	}
	return nil
}

// reencryptCandidate is one row below its tenant's newest version.
type reencryptCandidate struct {
	scope, hash string
	size        int64
}

// candidates are the ready rows below their tenant's newest version, oldest
// first, one scan's worth. A pending row (WP-R8-2: its blob is still being
// written) is never one.
func (k *KeyReencryptRunner) candidates(ctx context.Context) ([]reencryptCandidate, error) {
	rows, err := k.db.QueryContext(ctx, `
		WITH cur AS (
			SELECT tenant_id, MAX(key_version) AS v FROM tenant_key_versions
			 GROUP BY tenant_id HAVING COUNT(*) > 1
		)
		SELECT g.dedup_scope, g.plaintext_hash, g.size_bytes
		  FROM global_content_index g JOIN cur ON cur.tenant_id = g.dedup_scope
		 WHERE g.encrypted AND g.state = 'ready' AND g.encryption_key_version < cur.v AND ($2 = '' OR g.dedup_scope = $2)
		 ORDER BY g.first_seen_at, g.dedup_scope, g.plaintext_hash
		 LIMIT $1`, keyReencryptScanLimit, k.onlyScope)
	if err != nil {
		return nil, fmt.Errorf("scan re-encrypt candidates: %w", err)
	}
	var cands []reencryptCandidate
	for rows.Next() {
		var c reencryptCandidate
		if err := rows.Scan(&c.scope, &c.hash, &c.size); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan re-encrypt candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("scan re-encrypt candidates: %w", err)
	}
	_ = rows.Close()
	return cands, nil
}

func (k *KeyReencryptRunner) reencrypt(ctx context.Context, res *KeyReencryptResult) error {
	cands, err := k.candidates(ctx)
	if err != nil {
		return err
	}
	res.Candidates = len(cands)

	current := map[string]int{} // tenant → newest version, read once per run past every cache
	for _, c := range cands {
		if ctx.Err() != nil {
			return nil
		}
		if res.Bytes >= k.MaxBytesPerRun {
			res.BudgetReached = true
			return nil
		}
		cur, ok := current[c.scope]
		if !ok {
			v, err := k.enc.Keys().CurrentVersion(ctx, c.scope)
			if err != nil {
				res.Failed++
				res.fail("tenant %s: current version: %v", c.scope, err)
				continue
			}
			current[c.scope] = v
			cur = v
		}
		moved, err := k.reencryptOne(ctx, c, cur)
		switch {
		case err != nil:
			res.Failed++
			keyReencryptChunks.WithLabelValues("failed").Inc()
			res.fail("chunk %s of %s: %v", shortHash(c.hash), c.scope, err)
			k.logger.Warn("key re-encrypt: chunk left on its version (next run retries)",
				zap.String("scope", c.scope), zap.String("hash", c.hash), zap.Error(err))
		case moved:
			res.Reencrypted++
			res.Bytes += c.size
			keyReencryptChunks.WithLabelValues("reencrypted").Inc()
		}
	}
	return nil
}

// reencryptOne moves one chunk to version cur. false, nil = the row changed
// under us (swept, already moved) — nothing to do.
func (k *KeyReencryptRunner) reencryptOne(ctx context.Context, c reencryptCandidate, cur int) (bool, error) {
	moved := false
	err := withChunkLock(ctx, k.db, c.scope, c.hash, func(conn *sql.Conn) error {
		var old chunkAddr
		var ctHash sql.NullString
		var version sql.NullInt32
		var encrypted bool
		err := conn.QueryRowContext(ctx, `
			SELECT backend_id, storage_key, ciphertext_hash, encryption_key_version, encrypted
			  FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'ready'`,
			c.scope, c.hash).Scan(&old.backend, &old.key, &ctHash, &version, &encrypted)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // swept since the scan
		}
		if err != nil {
			return fmt.Errorf("re-read the row: %w", err)
		}
		if !encrypted || !version.Valid || int(version.Int32) >= cur || !ctHash.Valid {
			return nil // moved since the scan, or nothing to move
		}
		old.scope, old.hash = c.scope, c.hash

		store := newChunkStore(k.eng, k.db, k.logger)
		rc, err := store.get(ctx, old)
		if err != nil {
			return fmt.Errorf("read the blob: %w", err)
		}
		var buf bytes.Buffer
		_, err = buf.ReadFrom(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("read the blob: %w", err)
		}
		// Checked against the row's hash, then opened with the version the
		// row records: the sealed bytes (raw or zstd) come out unchanged.
		sealed, err := k.enc.DecryptChunkData(ctx, c.scope, int(version.Int32), c.hash, buf.Bytes(), ctHash.String)
		if err != nil {
			return fmt.Errorf("open under v%d: %w", version.Int32, err)
		}
		ct, newHash, err := k.enc.EncryptChunkDataVersion(ctx, c.scope, cur, c.hash, sealed)
		if err != nil {
			return fmt.Errorf("seal under v%d: %w", cur, err)
		}
		// A new key under the tenant's chunk prefix (the sweep, the chunk
		// move and legacyCopyExists keep working): the old blob stays where
		// it is for readers that resolved it.
		newKey := fmt.Sprintf("_chunks/%s/%s.k%d", c.scope, c.hash, cur)
		backend, _, err := store.put(ctx, newKey, ct)
		if err != nil {
			return fmt.Errorf("write the re-encrypted blob: %w", err)
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin swap: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		upd, err := tx.ExecContext(ctx, `
			UPDATE global_content_index
			   SET backend_id = $3, storage_key = $4, ciphertext_hash = $5, encryption_key_version = $6
			 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND storage_key = $7 AND ciphertext_hash = $8 AND state = 'ready'`,
			c.scope, c.hash, backend, newKey, newHash, cur, old.key, ctHash.String)
		if err != nil {
			return fmt.Errorf("swap the row: %w", err)
		}
		if n, _ := upd.RowsAffected(); n != 1 {
			return fmt.Errorf("swap the row: %d rows matched the old blob", n)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO retired_chunk_blobs (backend_id, storage_key, dedup_scope, plaintext_hash, retire_after)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (backend_id, storage_key) DO UPDATE SET retire_after = EXCLUDED.retire_after`,
			old.backend, old.key, c.scope, c.hash, k.now().Add(k.RetireAfter)); err != nil {
			return fmt.Errorf("retire the old address: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit swap: %w", err)
		}
		k.gci.InvalidateCache(c.scope, c.hash)
		moved = true
		return nil
	})
	return moved, err
}

// withChunkLock runs fn holding the chunk's session-level advisory lock on a
// dedicated connection — the lock the chunked PUT (as an xact lock), the
// dedup GC sweep and the chunk move take on (scope, hash).
func withChunkLock(ctx context.Context, db *sql.DB, scope, hash string, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, scope, hash); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, scope, hash)
	}()
	return fn(conn)
}
