package api

// WP-R2-1 Part C, PR C2: the key_reencrypt job's pass over whole SSE-S3
// objects — head rows AND retained versions (object_versions.storage_key) —
// sealed under an older version of their tenant's key. It runs after the
// chunk pass, on the same job lock and the same byte budget
// (KEY_REENCRYPT_MAX_GB_PER_RUN covers chunks and objects together), and
// resumes the same way: every run rescans the rows still below the newest
// version, oldest key first, so what moved drops out.
//
// One blob at a time, streamed (WP-R8-5: one segment in memory, never the
// object; a 0x01/0x02 blob — ≤ 256 MiB, written before — is opened whole by
// the compatibility opener and re-sealed as 0x03 like any other):
//
//  1. read it from the backend its row RECORDS (never the fan-out; tenant in
//     the context), check the header names the version the row does, open
//     it under that version as it streams;
//  2. seal it under the newest version as it streams and write it to a NEW
//     address (newBlobAddress) on the SAME backend, in the same container —
//     never over the old bytes, which a GET may be reading; the plaintext's
//     MD5 is checked against the row's ETag once written (a mismatch queues
//     the new blob as an orphan, nothing re-pointed);
//  3. one transaction, under the head-row lock then the version lock (the
//     flip's order): every row that still names the old address AND the old
//     key version — the head row, the version rows — is re-pointed to the
//     new address and version; the ETag (the plaintext's) is unchanged;
//  4. the old address is retired ('reencrypted'), never deleted in the run:
//     the retired_blobs collector deletes it after its grace and its re-check.
//
// A write or a delete that changed the rows between 1 and 3 wins: nothing is
// re-pointed and the new blob is queued 'orphan_write' (due now; the
// collector's re-check is what makes that safe).
//
// Skipped, counted as still pending, with a note (KeyReencryptResult.
// ObjectsSkipped):
//   - archive: the row's backend is the tape backend (engine.ArchiveBackends)
//     — reading it would trigger a restore;
//   - demotion: an open Smart-demotion ledger row for the key (its copies are
//     the ledger's to move);
//   - parity: a vault_parity row for the key — the shards are computed from
//     the STORED bytes (vault_parity.go protect reads the blob as stored, i.e.
//     the ciphertext), so a re-encrypted object would need its parity marked
//     stale and recomputed; vault-floor objects live on the archive backend
//     anyway, so the two skips coincide today;
//   - export: the `_exports` system bucket (generated GDPR exports expire in
//     7 days);
//   - unavailable: no recorded backend, an unregistered one, or an open
//     breaker.
// Chunked objects are the chunk pass's (their head rows record no key
// version and are not candidates).

import (
	"bufio"
	"context"
	"crypto/md5" // #nosec G501 -- S3 ETag semantics (the plaintext's MD5), an integrity check, not a MAC
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// retiredReencrypted: the old address of a blob the key_reencrypt job
// re-sealed under the tenant's newest key version.
const retiredReencrypted = "reencrypted"

// reencryptSkipUnavailable is the skip reason the run decides itself; the
// others (archive, export, demotion, parity, and unavailable for a row with
// no backend) come from objectReencryptCandidatesSQL.
const reencryptSkipUnavailable = "unavailable"

// keyReencryptObjects counts the job's per-blob outcomes for whole objects.
var keyReencryptObjects = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_key_reencrypt_objects_total",
	Help: "Whole SSE-S3 blobs (head rows and retained versions) the key_reencrypt job handled, by outcome: reencrypted (re-sealed under the tenant's newest key at a new address, rows re-pointed, old address retired) | changed (a write or delete changed the rows first: the new blob is an orphan, nothing re-pointed) | skipped (archive backend, open demotion, parity row, export, backend unavailable: still pending) | failed (left on its version, a note on the run).",
}, []string{"outcome"})

func init() {
	for _, o := range []string{"reencrypted", "changed", "skipped", "failed"} {
		keyReencryptObjects.WithLabelValues(o)
	}
}

// keyReencryptBeforeSwapHook runs after the new blob is written, before the
// re-point transaction (tests land an overwrite there).
var keyReencryptBeforeSwapHook = func(newAddr string) {}

// objectReencryptCandidate is one whole blob below its tenant's newest key
// version: the head row's or a retained version's (one candidate per
// address — the latest version names the head row's blob).
type objectReencryptCandidate struct {
	tenantID, bucket, key string
	blob, backend, etag   string
	size                  int64
	keyVersion            int
	skip                  string
}

// objectReencryptCandidatesSQL: $1 = the SSE-S3 algorithm, $2 = the tenant
// scope (” = all), $3 = the archive backends, $4 = the exports bucket,
// $5 = the scan limit. Movable candidates first, then the skipped ones (so a
// backlog of skips never starves the movable), oldest key version first.
const objectReencryptCandidatesSQL = `
	WITH cur AS (
		SELECT tenant_id, MAX(key_version) AS v FROM tenant_key_versions
		 WHERE ($2 = '' OR tenant_id = $2) GROUP BY tenant_id HAVING COUNT(*) > 1
	), blobs AS (
		SELECT h.tenant_id, h.bucket, h.object_key, COALESCE(h.storage_key, h.object_key) AS blob,
		       COALESCE(h.backend_name, '') AS backend, h.etag, h.size_bytes, COALESCE(h.encryption_key_version, 1) AS kv, 0 AS pref
		  FROM object_head_cache h JOIN cur ON cur.tenant_id = h.tenant_id
		 WHERE h.encryption_algorithm = $1 AND NOT h.is_chunked AND COALESCE(h.encryption_key_version, 1) < cur.v
		UNION ALL
		SELECT v.tenant_id, v.bucket, v.object_key, v.storage_key,
		       COALESCE(v.backend_name, ''), v.etag, v.size_bytes, COALESCE(v.encryption_key_version, 1), 1
		  FROM object_versions v JOIN cur ON cur.tenant_id = v.tenant_id
		 WHERE v.storage_key IS NOT NULL AND NOT v.is_delete_marker
		   AND v.encryption_algorithm = $1 AND COALESCE(v.encryption_key_version, 1) < cur.v
	), one AS (
		SELECT DISTINCT ON (tenant_id, bucket, object_key, blob) *
		  FROM blobs ORDER BY tenant_id, bucket, object_key, blob, pref
	)
	SELECT o.tenant_id, o.bucket, o.object_key, o.blob, o.backend, o.etag, o.size_bytes, o.kv,
	       CASE
	         WHEN o.backend = ANY($3) THEN 'archive'
	         WHEN o.bucket = $4 THEN 'export'
	         WHEN EXISTS (SELECT 1 FROM smart_demotions d
	                       WHERE d.tenant_id = o.tenant_id AND d.bucket = o.bucket AND d.object_key = o.object_key
	                         AND d.hot_outcome IN ('', 'delete_failed')) THEN 'demotion'
	         WHEN EXISTS (SELECT 1 FROM vault_parity p
	                       WHERE p.tenant_id = o.tenant_id AND p.bucket = o.bucket AND p.object_key = o.object_key) THEN 'parity'
	         WHEN o.backend = '' THEN 'unavailable'
	         ELSE ''
	       END AS skip
	  FROM one o
	 ORDER BY (CASE WHEN o.backend = ANY($3) OR o.bucket = $4 THEN 1 ELSE 0 END), o.kv, o.tenant_id, o.bucket, o.object_key, o.blob
	 LIMIT $5`

// reencryptObjects is the whole-object pass (file comment). Called after the
// chunk pass with what is left of the run's budget.
func (k *KeyReencryptRunner) reencryptObjects(ctx context.Context, res *KeyReencryptResult) error {
	if k.sse == nil || res.BudgetReached {
		return nil
	}
	rows, err := k.db.QueryContext(ctx, objectReencryptCandidatesSQL, crypto.SSEAlgorithm, k.onlyScope,
		pq.Array(engine.ArchiveBackends()), tenant.ExportsBucket, keyReencryptScanLimit)
	if err != nil {
		return fmt.Errorf("scan whole-object re-encrypt candidates: %w", err)
	}
	var cands []objectReencryptCandidate
	for rows.Next() {
		var c objectReencryptCandidate
		if err := rows.Scan(&c.tenantID, &c.bucket, &c.key, &c.blob, &c.backend, &c.etag, &c.size, &c.keyVersion, &c.skip); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan whole-object re-encrypt candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan whole-object re-encrypt candidates: %w", err)
	}
	_ = rows.Close()
	res.ObjectCandidates = len(cands)

	ce, _ := k.eng.(*engine.CoreEngine)
	for _, c := range cands {
		if ctx.Err() != nil {
			return nil // the rest waits for the next run
		}
		if c.skip == "" && ce != nil {
			_, registered := ce.GetDriver(c.backend)
			if !registered || ce.GetFailoverStatus()[c.backend] == engine.StateOpen.String() {
				c.skip = reencryptSkipUnavailable
			}
		}
		if c.skip != "" {
			res.skipObject(c.skip)
			keyReencryptObjects.WithLabelValues("skipped").Inc()
			continue
		}
		if res.Bytes >= k.MaxBytesPerRun {
			res.BudgetReached = true
			return nil
		}
		outcome, err := k.reencryptObject(ctx, ce, c)
		keyReencryptObjects.WithLabelValues(outcome).Inc()
		switch outcome {
		case "reencrypted":
			res.ObjectsReencrypted++
			res.Bytes += c.size
		case "changed":
			res.ObjectsChanged++
		default:
			res.ObjectsFailed++
			res.fail("object %s/%s (%s) of %s: %v", c.bucket, c.key, c.blob, c.tenantID, err)
			k.logger.Warn("key re-encrypt: object left on its version (next run retries)",
				zap.String("tenant_id", c.tenantID), zap.String("bucket", c.bucket), zap.String("key", c.key),
				zap.String("storage_key", c.blob), zap.String("backend", c.backend), zap.Error(err))
		}
	}
	return nil
}

// reencryptObject moves one blob (steps 1–4 of the file comment).
func (k *KeyReencryptRunner) reencryptObject(ctx context.Context, ce *engine.CoreEngine, c objectReencryptCandidate) (string, error) {
	if ce == nil {
		return "failed", errors.New("the engine cannot address one backend")
	}
	drv, ok := ce.GetDriver(c.backend)
	if !ok {
		return "failed", fmt.Errorf("backend %s is not registered", c.backend)
	}
	tctx := common.WithTenantID(ctx, c.tenantID)
	container, _ := objectBlobAddress(c.tenantID, c.bucket, c.key, c.blob)

	// 1. Read under the version the row records.
	rc, err := drv.Get(tctx, container, c.blob)
	if err != nil {
		return "failed", fmt.Errorf("read the blob: %w", err)
	}
	defer func() { _ = rc.Close() }()
	// The version check reads the header only: the longest a 0x01/0x02/0x03
	// header check needs (a whole 0x02 blob's overhead, 1,121 bytes).
	br := bufio.NewReaderSize(rc, crypto.SSEOverheadBytes)
	head, _ := br.Peek(crypto.SSEOverheadBytes)
	if v, err := crypto.SSEBlobKeyVersion(head); err != nil {
		return "failed", fmt.Errorf("the blob's header: %w", err)
	} else if v != c.keyVersion {
		return "failed", fmt.Errorf("the blob is sealed under v%d, its row says v%d", v, c.keyVersion)
	}
	plaintext, err := k.sse.Open(tctx, c.tenantID, br)
	if err != nil {
		return "failed", fmt.Errorf("open under v%d: %w", c.keyVersion, err)
	}

	// 2. Seal under the newest version as it streams, at a new address on
	// the same backend; the plaintext's MD5 is taken on the way.
	hasher := md5.New() // #nosec G401 -- S3 ETag semantics
	sealer, err := newSSEWriteSealer(tctx, k.sse, c.tenantID,
		io.TeeReader(&plaintextSized{r: plaintext, want: c.size}, hasher), c.size)
	if err != nil {
		return "failed", fmt.Errorf("seal under the newest version: %w", err)
	}
	newVersion := sealer.version
	newAddr, err := newBlobAddress(c.key)
	if err != nil {
		return "failed", err
	}
	if err := drv.Put(tctx, container, newAddr, sealer, engine.WithContentLength(k.sse.SealedLength(c.size))); err != nil {
		// A partial file on a local/WebDAV backend is the erasure sweep's
		// (as for any failed write); S3-class backends store nothing.
		return "failed", fmt.Errorf("write the re-sealed blob: %w", err)
	}
	if len(c.etag) == 32 && !strings.Contains(c.etag, "-") && hex.EncodeToString(hasher.Sum(nil)) != c.etag {
		retireOrphanWrite(ctx, k.db, k.logger, c.tenantID, c.bucket, newAddr, c.backend)
		return "failed", errors.New("the opened bytes do not match the row's ETag")
	}
	keyReencryptBeforeSwapHook(newAddr)

	// 3. + 4. Re-point, retire.
	moved, err := k.repointReencrypted(ctx, c, newAddr, newVersion)
	if err != nil {
		// Whether or not the commit landed, the collector's re-check decides
		// the new blob's fate: queued as an orphan, kept if a row names it.
		retireOrphanWrite(ctx, k.db, k.logger, c.tenantID, c.bucket, newAddr, c.backend)
		return "failed", err
	}
	if !moved {
		return "changed", nil
	}
	return "reencrypted", nil
}

// repointReencrypted is the swap: false when no row named the old address at
// the old key version any more (the new blob is queued as an orphan in the
// same transaction).
func (k *KeyReencryptRunner) repointReencrypted(ctx context.Context, c objectReencryptCandidate, newAddr string, newVersion int) (bool, error) {
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin re-point: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, c.tenantID, c.bucket, c.key); err != nil {
		return false, err
	}
	if err := lockVersionKey(ctx, tx, c.tenantID, c.bucket, c.key); err != nil {
		return false, err
	}
	head, err := tx.ExecContext(ctx, `
		UPDATE object_head_cache SET storage_key = $5, encryption_key_version = $6
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND NOT is_chunked
		  AND COALESCE(storage_key, object_key) = $4 AND COALESCE(backend_name, '') = $7
		  AND encryption_algorithm = $8 AND COALESCE(encryption_key_version, 1) = $9`,
		c.tenantID, c.bucket, c.key, c.blob, newAddr, newVersion, c.backend, crypto.SSEAlgorithm, c.keyVersion)
	if err != nil {
		return false, fmt.Errorf("re-point the head row: %w", err)
	}
	vers, err := tx.ExecContext(ctx, `
		UPDATE object_versions SET storage_key = $5, encryption_key_version = $6, backend_name = $7
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND storage_key = $4 AND NOT is_delete_marker
		  AND encryption_algorithm = $8 AND COALESCE(encryption_key_version, 1) = $9`,
		c.tenantID, c.bucket, c.key, c.blob, newAddr, newVersion, c.backend, crypto.SSEAlgorithm, c.keyVersion)
	if err != nil {
		return false, fmt.Errorf("re-point the version rows: %w", err)
	}
	nh, _ := head.RowsAffected()
	nv, _ := vers.RowsAffected()
	if nh+nv == 0 {
		// A write or a delete got there first: its rows stand.
		if err := queueOrphanTx(ctx, tx, c.tenantID, c.bucket, newAddr, c.backend); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	// Nothing names the old address now (unless a row this pass does not
	// touch does — a demotion ledger row: then it is kept, not queued).
	if err := retireBlob(ctx, tx, c.tenantID, c.bucket, c.key, c.blob, c.backend, retiredReencrypted); err != nil &&
		!errors.Is(err, errUnaddressableBlob) {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit re-point: %w", err)
	}
	return true, nil
}

// queueOrphanTx is retireOrphanWrite inside the caller's transaction: the
// address is due at once; the collector's re-check keeps it if a row names it.
func queueOrphanTx(ctx context.Context, tx *sql.Tx, tenantID, bucket, addr, backend string) error {
	container, _ := objectBlobAddress(tenantID, bucket, "", addr)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (backend_id, container, storage_key) DO UPDATE SET reason = EXCLUDED.reason, retire_after = EXCLUDED.retire_after`,
		backend, container, addr, tenantID, retiredOrphanWrite); err != nil {
		return fmt.Errorf("queue the orphan %s: %w", addr, err)
	}
	return nil
}

func (r *KeyReencryptResult) skipObject(reason string) {
	if r.ObjectsSkipped == nil {
		r.ObjectsSkipped = map[string]int{}
	}
	r.ObjectsSkipped[reason]++
}
