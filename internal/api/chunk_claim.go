package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"go.uber.org/zap"
)

// A new chunk is stored in three short steps (WP-R8-2 B.1, migration 093).
// It used to be one transaction holding a pooled connection and the chunk's
// advisory lock across the backend PUT — up to the PUT deadline and its
// retry: 8 workers × 6 chunked PUTs pinned 48 of the pool's 50 connections
// to backend latency (R8-09a).
//
//  1. claim — one short transaction under
//     pg_advisory_xact_lock(hashtext(scope), hashtext(hash)) (the lock dedup
//     GC's sweepOne takes as a session lock): the row is re-read; a READY row
//     is referenced (the R8-02 rule: two writers of one new chunk may encode
//     it differently — compression follows each request's Content-Type — so
//     the loser takes a reference on the winner's row and never writes over
//     its blob); a PENDING row younger than the takeover window is waited on
//     (the lock released, polled with backoff, bounded by the request
//     context); an ABSENT row, or a pending one older than the window (its
//     writer died), is claimed: pending, with THIS writer's encoding, at
//     ref_count 0 — a pending row holds no reference.
//  2. the backend PUT, with no lock and no connection held, bounded by
//     chunkPutBound.
//  3. ready — under the lock again: the row flips to ready, takes this
//     writer's reference and the backend the blob landed on, only while the
//     claim is still this writer's (pending_since = the claim's). Zero rows =
//     taken over meanwhile: this writer's blob is the loser's; it is deleted
//     only when the row now names another address (the same address holds
//     the winner's bytes), and the loop references the winner.
//
// A PUT that fails keeps or drops its claim by what the backends said
// (RP13-1, engine.PutOutcome):
//   - an attempt that ended WITHOUT AN ANSWER — a deadline, a reset — may
//     still commit: a backend that took the whole body can store it after
//     the caller gave up (RP8-11). The claim stays, so the next writer waits
//     the window out and takes it over, and the late bytes land inside the
//     window, never over the taker's (decision 8: the PUT ran under
//     chunkPutBound from after the claim, a late commit lands within a
//     second bound, the window is two).
//   - every attempt ANSWERED with an error (a 500, a 403), never connected
//     (a refused dial) or never asked (an open breaker): nothing was
//     committed. The claim is dropped (when no reference was taken on it —
//     the previous build, in the switch overlap, may take one), so the next
//     writer stores the chunk at once.
//
// The request ending is NOT a cancel of the PUT (RP14-1, Prompt 14; it was
// until then, and a cancel after the body went out was the unanswered case
// above — every chunk in flight when an upload was cut kept its claim, and
// the client's immediate retry waited the window on all of them). A PUT
// that has started runs to its own end on a context detached from the
// request (chunk_put_detached.go) and its outcome is known: OK → the row is
// ready at ref 0 and a retry dedups onto it within seconds; an answered
// error → dropped; no answer → kept. A PUT not started when the request
// ended is never attempted and its claim is dropped at once. A writer that
// dies (the process) leaves its claim: the next writer takes it over after
// the window, or the chunk orphan sweep (B.2) erases it with any blob it
// left.

// errChunkWaitGaveUp: the request ended while another writer's claim on the
// chunk was being waited on.
var errChunkWaitGaveUp = errors.New("chunk: gave up waiting for another writer's store")

type chunkClaimKind int

const (
	chunkReferenced chunkClaimKind = iota // a ready row was referenced: done
	chunkWait                             // another writer's claim is young: wait
	chunkClaimed                          // this writer holds the claim: PUT, then ready
)

// storeChunkLocked stores a new chunk's blob and makes its index row ready,
// or references the row a concurrent writer made ready (see above).
func (a *S3ToEngine) storeChunkLocked(ctx context.Context, scope, storageKey string, storeData []byte, entry *crypto.GCIEntry) (chunkStoreResult, error) {
	const waitMin, waitMax = 25 * time.Millisecond, time.Second
	wait := waitMin
	for {
		res, kind, since, err := a.claimChunk(ctx, scope, storageKey, entry)
		if err != nil {
			return chunkStoreResult{}, err
		}
		switch kind {
		case chunkReferenced:
			return res, nil
		case chunkWait:
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return chunkStoreResult{}, fmt.Errorf("chunk %s: %w: %w", shortHash(entry.PlaintextHash), errChunkWaitGaveUp, ctx.Err())
			case <-t.C:
			}
			wait = min(wait*2, waitMax)
			continue
		}

		// A chunk whose PUT has not started when the request ended is
		// never attempted: its claim is dropped at once (RP14-1).
		if err := ctx.Err(); err != nil {
			a.dropChunkClaim(ctx, scope, entry.PlaintextHash, since)
			return chunkStoreResult{}, fmt.Errorf("chunk %s: not attempted, the request ended: %w", shortHash(entry.PlaintextHash), err)
		}
		// The PUT runs detached from the request under chunkPutBound
		// (chunk_put_detached.go): if the request ends first, the PUT's own
		// goroutine settles the claim and this writer takes no reference.
		r, detached := a.putChunk(ctx, scope, storageKey, storeData, since, entry)
		if detached {
			return chunkStoreResult{}, fmt.Errorf("chunk %s: the request ended while its PUT ran (it finishes detached): %w", shortHash(entry.PlaintextHash), ctx.Err())
		}
		if r.err != nil {
			if r.outcome.MayHaveLanded() {
				// No answer: the backend may hold the whole body and commit
				// it late (RP8-11). The claim stays, so the next writer
				// waits the window out instead of storing over it (RP13-1).
				a.logger.Warn("chunk store: a PUT ended with no answer and may still land — its claim is kept for the takeover window",
					zap.String("scope", scope), zap.String("hash", shortHash(entry.PlaintextHash)),
					zap.Strings("unanswered", r.outcome.Unanswered()), zap.Duration("takeover_after", a.chunkPendingTakeover),
					zap.Error(r.err))
			} else {
				a.dropChunkClaim(ctx, scope, entry.PlaintextHash, since)
			}
			return chunkStoreResult{}, r.err
		}
		// The flip too runs on its own clock: a request that ends between
		// the PUT's answer and the flip must not leave a landed blob behind
		// a pending claim (the pool gives this writer's reference back).
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), chunkDetachedFinishBound)
		res, done, err := a.readyChunk(rctx, scope, storageKey, r.backend, since, entry, true)
		rcancel()
		if err != nil {
			return chunkStoreResult{}, err
		}
		if done {
			return res, nil
		}
		wait = waitMin // taken over meanwhile: reference the winner's row
	}
}

// primaryName is the backend a new chunk's PUT is expected to land on — what
// a pending row records until the ready flip writes where it did land.
func (a *S3ToEngine) primaryName() string {
	if p, ok := a.engine.(interface{ GetPrimary() string }); ok {
		return p.GetPrimary()
	}
	return ""
}

// claimChunk is step 1: under the chunk's lock, reference a ready row, wait
// on a young claim, or claim the chunk (since = the claim's pending_since).
func (a *S3ToEngine) claimChunk(ctx context.Context, scope, storageKey string, entry *crypto.GCIEntry) (chunkStoreResult, chunkClaimKind, time.Time, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("begin chunk claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, scope, entry.PlaintextHash); err != nil {
		return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("advisory lock: %w", err)
	}

	var state, rowBackend string
	var rowCiphertextHash sql.NullString
	var rowKeyVersion sql.NullInt32
	var abandoned bool
	err = tx.QueryRowContext(ctx, `
		SELECT state, backend_id, ciphertext_hash, encryption_key_version,
		       state = 'pending' AND pending_since < clock_timestamp() - make_interval(secs => $3)
		  FROM global_content_index
		 WHERE dedup_scope = $1 AND plaintext_hash = $2
		   FOR UPDATE`, scope, entry.PlaintextHash, a.chunkPendingTakeover.Seconds()).
		Scan(&state, &rowBackend, &rowCiphertextHash, &rowKeyVersion, &abandoned)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("check chunk index under lock: %w", err)
	}

	if found && state == "ready" {
		rows, incErr := a.gci.IncrementRefTx(ctx, tx, scope, entry.PlaintextHash)
		if incErr != nil {
			return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("reference existing chunk: %w", incErr)
		}
		if rows != 1 {
			return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("reference existing chunk: %d rows updated under lock", rows)
		}
		if err := tx.Commit(); err != nil {
			return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("commit chunk reference: %w", err)
		}
		res := chunkStoreResult{backend: rowBackend, reused: true}
		if rowCiphertextHash.Valid {
			h := rowCiphertextHash.String
			res.ciphertextHash = &h
		}
		if rowKeyVersion.Valid {
			v := int(rowKeyVersion.Int32)
			res.keyVersion = &v
		}
		return res, chunkReferenced, time.Time{}, nil
	}
	if found && !abandoned {
		return chunkStoreResult{}, chunkWait, time.Time{}, nil
	}

	// Absent, or a claim whose writer died: this writer's, with its encoding.
	var since time.Time
	if found {
		err = tx.QueryRowContext(ctx, `
			UPDATE global_content_index
			   SET backend_id = $3, storage_key = $4, size_bytes = $5, compressed_size = $6, compression_algo = $7,
			       encrypted = $8, encryption_algo = $9, ciphertext_hash = $10, encryption_key_version = $11,
			       pending_since = clock_timestamp(), marked_for_deletion = FALSE, marked_at = NULL
			 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending'
			RETURNING pending_since`,
			scope, entry.PlaintextHash, a.primaryName(), storageKey, entry.SizeBytes, entry.CompressedSize, entry.CompressionAlgo,
			entry.Encrypted, entry.EncryptionAlgo, entry.CiphertextHash, entry.EncryptionKeyVersion).Scan(&since)
		if err == nil {
			a.logger.Warn("chunk store: a pending claim older than the takeover window was taken over (its writer died)",
				zap.String("scope", scope), zap.String("hash", shortHash(entry.PlaintextHash)),
				zap.Duration("takeover_after", a.chunkPendingTakeover))
		}
	} else {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO global_content_index
			  (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, compressed_size, compression_algo,
			   encrypted, encryption_algo, ciphertext_hash, encryption_key_version, ref_count, state, pending_since)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0, 'pending', clock_timestamp())
			RETURNING pending_since`,
			scope, entry.PlaintextHash, a.primaryName(), storageKey, entry.SizeBytes, entry.CompressedSize, entry.CompressionAlgo,
			entry.Encrypted, entry.EncryptionAlgo, entry.CiphertextHash, entry.EncryptionKeyVersion).Scan(&since)
	}
	if err != nil {
		return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("claim chunk: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return chunkStoreResult{}, 0, time.Time{}, fmt.Errorf("commit chunk claim: %w", err)
	}
	a.gci.InvalidateCache(scope, entry.PlaintextHash)
	return chunkStoreResult{}, chunkClaimed, since, nil
}

// readyChunk is step 3: the claim becomes the ready row of the blob this
// writer stored on bn — with this writer's reference when takeRef (a write
// that goes on to install its manifest), at ref 0 when not (a PUT whose
// request has gone, RP14-1: the next writer of the chunk references it).
// done is false when the claim was taken over meanwhile — the caller then
// references the winner.
func (a *S3ToEngine) readyChunk(ctx context.Context, scope, storageKey, bn string, since time.Time, entry *crypto.GCIEntry, takeRef bool) (chunkStoreResult, bool, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return chunkStoreResult{}, false, fmt.Errorf("begin chunk ready: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, scope, entry.PlaintextHash); err != nil {
		return chunkStoreResult{}, false, fmt.Errorf("advisory lock: %w", err)
	}
	ref := 0
	if takeRef {
		ref = 1
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE global_content_index
		   SET state = 'ready', pending_since = NULL, backend_id = $4,
		       ref_count = ref_count + $5, last_accessed_at = NOW()
		 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3`,
		scope, entry.PlaintextHash, since, bn, ref)
	if err != nil {
		return chunkStoreResult{}, false, fmt.Errorf("make chunk ready: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := tx.Commit(); err != nil {
			return chunkStoreResult{}, false, fmt.Errorf("commit chunk ready: %w", err)
		}
		a.gci.InvalidateCache(scope, entry.PlaintextHash)
		entry.BackendID = bn
		return chunkStoreResult{backend: bn, ciphertextHash: entry.CiphertextHash, keyVersion: entry.EncryptionKeyVersion}, true, nil
	}

	// Taken over: another writer claimed (and maybe stored) the chunk after
	// this claim passed its window. Its row decides whether this writer's
	// blob is in the way.
	var state, rowBackend, rowKey string
	err = tx.QueryRowContext(ctx, `
		SELECT state, backend_id, storage_key FROM global_content_index
		 WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, entry.PlaintextHash).Scan(&state, &rowBackend, &rowKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return chunkStoreResult{}, false, fmt.Errorf("re-read a taken-over chunk claim: %w", err)
	case state == "ready" && (rowBackend != bn || rowKey != storageKey):
		// The winner's blob is elsewhere: this one is nobody's. Deleted under
		// the chunk's lock, as dedup GC deletes; a failure leaves it to the
		// chunk orphan sweep.
		if delErr := a.chunks().delete(ctx, chunkAddr{scope: scope, hash: entry.PlaintextHash, backend: bn, key: storageKey}); delErr != nil {
			a.logger.Warn("chunk store: a taken-over writer's blob could not be deleted — the orphan sweep will",
				zap.String("backend", bn), zap.String("key", storageKey), zap.Error(delErr))
		}
	}
	if err := tx.Commit(); err != nil {
		return chunkStoreResult{}, false, fmt.Errorf("commit taken-over chunk claim: %w", err)
	}
	a.logger.Info("chunk store: claim taken over while its PUT ran — referencing the winner",
		zap.String("scope", scope), zap.String("hash", shortHash(entry.PlaintextHash)))
	return chunkStoreResult{}, false, nil
}

// dropChunkClaim removes this writer's claim after its PUT failed with an
// answer (nothing committed — see the header), so the
// next writer of the chunk stores it at once. A claim someone took a
// reference on (the previous build, during the switch overlap) is kept: the
// next writer takes it over after the window. Best effort on its own short
// deadline (the request may be gone): a claim left behind is taken over or
// erased by the orphan sweep.
func (a *S3ToEngine) dropChunkClaim(ctx context.Context, scope, hash string, since time.Time) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.db.ExecContext(dctx, `
		DELETE FROM global_content_index
		 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3 AND ref_count = 0`,
		scope, hash, since); err != nil {
		a.logger.Warn("chunk store: a failed store's claim could not be dropped — it is taken over after its window",
			zap.String("scope", scope), zap.String("hash", shortHash(hash)), zap.Error(err))
	}
	a.gci.InvalidateCache(scope, hash)
}
