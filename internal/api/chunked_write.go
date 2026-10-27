package api

// WP-R3-1: the chunked write as a core with no HTTP in it. A PUT
// (handleChunkedPut) and a CompleteMultipartUpload both hand it a stream and
// what the head row records; it runs chunker → dedup lookups → store pool →
// manifest + head row in ONE transaction (the flip, commitBlobWrite). The
// manifest goes to its own address (WP-VM-1): the flip keeps or retires the
// one the displaced row named. The handlers keep what
// is theirs: the status line and headers, Object Lock, x-amz-version-id, the
// event they fire. TestChunkBlobCalls_GoThroughTheChunkStore keeps every
// chunk store and manifest install inside this file.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// chunkedBackendSentinel is the backend_name of every chunked head row
// (WP-R3-1, the rule WP-R7-5 left here): a chunked object's bytes are the
// content index's — each chunk row carries its own backend_id — so the head
// row names no route, and every reader skips it for a chunked row (routing
// truth, the boot check, GET/CDN hints, demotion, parity, re-encrypt). Rows
// written before carry the first stored chunk's backend or ” (a manifest
// copy); they read exactly the same.
const chunkedBackendSentinel = "chunked"

// chunkedWrite is one chunked object write.
type chunkedWrite struct {
	TenantID, Bucket, Key string
	// Body is the object's bytes, read once to EOF (never buffered whole:
	// the chunker holds at most one chunk per store worker).
	Body io.Reader
	// DeclaredSize is the size the writer was told (a PUT's declared
	// length, the sum of a Complete's parts); < 0 = none. A body of another
	// length is refused before the install.
	DeclaredSize int64
	// Attrs is what the head row records.
	Attrs objectAttrs
	// ETag is called once the body is consumed, before the install: a PUT's
	// whole-stream MD5 (checked against Content-MD5), a Complete's -N.
	ETag func() (string, error)
	// DetachInstall runs the install on a context detached from ctx (a
	// Complete inside the long-op detach: once every chunk is stored, the
	// commit runs to the end whatever happens to the operation).
	DetachInstall bool
	// InTx, when set, runs inside the install's transaction after the head
	// row (a Complete marks its upload completed there).
	InTx func(ctx context.Context, tx *sql.Tx) error
	// Bill is settled inside the install's transaction (WP-R10-8).
	Bill *writeBill
}

// chunkedResult is what a chunked write stored.
type chunkedResult struct {
	ETag         string
	Size         int64 // logical bytes measured from the body
	PhysicalSize int64 // bytes this write added to the backend (0 = fully deduplicated)
	Chunks       int
	DedupRatio   float32
	Flip         flipResult
	// Encrypted: the chunks are sealed per chunk (AES256-CE) — the answer
	// says x-amz-server-side-encryption as HEAD and GET do (R8-15).
	Encrypted bool
}

// storeChunkedObject is the chunked write (file comment). On any error
// nothing is committed: the references the write took are given back
// (DecrementRef, on a cancellation-immune context) and chunk blobs it stored
// that no row references any more are dedup GC's after its 7-day grace.
func (a *S3ToEngine) storeChunkedObject(ctx context.Context, cw chunkedWrite) (chunkedResult, error) {
	if a.db == nil || a.gci == nil {
		return chunkedResult{}, errors.New("the chunked path requires a database and the content index")
	}
	contentType := cw.Attrs.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// The write's first act (WP-R2-6, chunk_put_budget.go): one worker's
	// buffers from the process budget, before a byte of the body is read
	// or a chunk claimed — refused as errChunkPutBudget (503 SlowDown) when
	// the budget cannot spare them within the wait.
	hold, err := a.chunkBudget().reserve(ctx, chunkPutReserveBytes, chunkPutBudgetWait)
	if err != nil {
		return chunkedResult{}, err
	}
	defer hold.close()

	// pctx cancels the chunker, the store workers, and their DB work as one
	// unit: the first error anywhere stops the whole upload promptly.
	pctx, cancelStores := context.WithCancel(ctx)
	defer cancelStores()

	chunker, err := crypto.DefaultChunker()
	if err != nil {
		return chunkedResult{}, fmt.Errorf("create chunker: %w", err)
	}
	chunkCh, err := chunker.ChunkContextDepth(pctx, cw.Body, chunkerLookahead)
	if err != nil {
		return chunkedResult{}, fmt.Errorf("start streaming chunker: %w", err)
	}

	// Dedup scope: encrypted chunks are namespaced to the tenant (their
	// convergent key is per-tenant, so identical plaintext yields distinct
	// ciphertext — cross-tenant dedup would hand one tenant another's
	// undecryptable bytes). Unencrypted chunks stay globally shared.
	encrypting := a.chunkEncSvc != nil
	dedupScope := crypto.GlobalDedupScope
	if encrypting {
		// The tenant ID becomes the dedup-scope partition, so it must not
		// be the shared scope's name. The S3 front door refuses reserved
		// ids (handleS3Request); this is the same rule where it is
		// load-bearing.
		if engine.IsReservedTenantID(cw.TenantID) {
			return chunkedResult{}, fmt.Errorf("tenant ID is reserved: it collides with the shared dedup scope")
		}
		dedupScope = cw.TenantID
	}

	// The manifest's own address (WP-VM-1): never the bare key, so this
	// write neither replaces the manifest a row names (a retained version, a
	// GET in flight) nor is replaced by a later write of the key.
	addr, err := newBlobAddress(cw.Key)
	if err != nil {
		return chunkedResult{}, err
	}

	// New-chunk stores fan out to a bounded worker pool — the store step is a
	// full HTTPS round-trip to the backend, and running stores one at a time
	// capped engine-path uploads at chunk_size ÷ round-trip (~19 MB/s to
	// iDrive). This dispatcher loop stays sequential for every dedup
	// decision; only compress → encrypt → storeChunkLocked runs on workers.
	pool := newChunkStorePool(pctx, a, cancelStores,
		cw.TenantID, cw.TenantID, cw.Bucket, addr, dedupScope, contentType, encrypting)
	pool.hold = hold

	// Every chunk processed takes one GCI reference (fresh insert at
	// ref_count 1, ON CONFLICT increment, or IncrementRef on a dedup hit). If
	// the write aborts before the manifest install commits, those references
	// are orphaned — nothing ever releases them, so shared chunks become
	// unsweepable forever (F10). Compensate on every error path; after a
	// successful install the manifest owns the references. Decrements run on a
	// cancellation-immune context: the abort may BE a cancellation (a client
	// gone, a long operation cut at its bound). This defer only reads pool
	// state after join() — every return below joins first.
	manifestInstalled := false
	defer func() {
		if manifestInstalled {
			return
		}
		compCtx := context.WithoutCancel(ctx)
		for _, ref := range pool.compensationRefs() {
			if _, decErr := a.gci.DecrementRef(compCtx, ref.scope, ref.hash); decErr != nil {
				a.logger.Warn("compensating ref decrement failed after aborted chunked write (reconcile will heal)",
					zap.String("hash", ref.hash), zap.Error(decErr))
			}
		}
	}()

	var measuredSize int64
	var chunkCount int
	for result := range chunkCh {
		if result.Err != nil {
			pool.fail(fmt.Errorf("chunking stream: %w", result.Err))
			break
		}

		chunk := result.Chunk
		measuredSize += int64(chunk.Size)
		chunkCount++

		if pool.failed() {
			break
		}

		// A duplicate of a hash that is being stored right now waits for that
		// store instead of racing it into a second store of the same blob.
		if pool.noteDuplicate(&chunk) {
			continue
		}

		lookup, lookupErr := a.gci.LookupChunk(pctx, dedupScope, chunk.Hash)
		if lookupErr != nil {
			pool.fail(fmt.Errorf("lookup chunk %s: %w", chunk.Hash[:16], lookupErr))
			break
		}

		mustStore := lookup.IsNewChunk
		if !mustStore {
			rows, incErr := a.gci.IncrementRef(pctx, dedupScope, chunk.Hash)
			if incErr != nil {
				pool.fail(fmt.Errorf("increment ref %s: %w", chunk.Hash[:16], incErr))
				break
			}
			if rows == 0 {
				// The chunk vanished between lookup and increment — GC swept
				// it (the lookup was likely served from a stale cache entry).
				// Treat it as new: re-store the data and re-insert the row.
				// Proceeding without this installs a manifest pointing at
				// deleted data (WP-6).
				a.logger.Warn("dedup hit on vanished chunk — re-storing",
					zap.String("hash", chunk.Hash[:16]),
					zap.String("scope", dedupScope))
				mustStore = true
				lookup.Entry = nil
			}
		}

		if !mustStore {
			// The ciphertext hash describes the blob that is actually stored,
			// so on a dedup hit it is copied from the index row — never
			// recomputed. The stored blob's compression was decided by the
			// FIRST upload's Content-Type; recomputing under the current
			// request's Content-Type (or a different zstd version) can hash a
			// blob that was never stored, making the object fail its
			// integrity check on every GET.
			// The key version likewise (WP-R8-1): a dedup hit after a
			// rotation points at a blob sealed under the OLD version, and
			// the ref says so until the re-encrypt job moves the blob.
			var refCiphertextHash *string
			var refKeyVersion *int
			if lookup.Entry != nil {
				refCiphertextHash = lookup.Entry.CiphertextHash
				refKeyVersion = lookup.Entry.EncryptionKeyVersion
			}
			pool.addRef(chunk.Index, chunk.Offset, chunk.Hash, refCiphertextHash, refKeyVersion)
			continue
		}

		// The chunk's buffers (itself, compressed, sealed — the worst case;
		// shrunk once known) from what the write holds, else the budget:
		// the dispatcher waits here when neither has them, until a chunk of
		// this write finishes — fewer in flight, never a refusal.
		held := chunkPutHoldBytes(chunk.Size)
		if err := hold.hold(pctx, held); err != nil {
			pool.fail(fmt.Errorf("chunk %s buffers: %w", chunk.Hash[:16], err))
			break
		}
		pool.submit(chunk, held)
	}

	if joinErr := pool.join(); joinErr != nil {
		return chunkedResult{}, joinErr
	}
	// The chunker stops without an error when the request's context ends
	// (a client that went away after its last byte): what it measured is
	// not the body, and the size check below would blame the client's
	// declaration for it (WP-R2-3).
	if err := ctx.Err(); err != nil {
		return chunkedResult{}, fmt.Errorf("chunked write stopped: %w", err)
	}
	physicalSize := pool.physicalSize
	newRefs := pool.sortedRefs()

	// Reject declared-vs-measured mismatch before installing the manifest:
	// stored chunks without refs are swept by dedup GC, but a manifest with
	// a client-invented logical size would poison billing permanently.
	if cw.DeclaredSize >= 0 && measuredSize != cw.DeclaredSize {
		return chunkedResult{}, fmt.Errorf("declared %d, measured %d: %w",
			cw.DeclaredSize, measuredSize, errDecodedLengthMismatch)
	}

	// physicalSize stays truthful: 0 means fully deduplicated — this write
	// added no new physical bytes. dedup_ratio 0 is the fully-deduped
	// sentinel.
	dedupRatio := float32(0)
	if physicalSize > 0 {
		dedupRatio = float32(measuredSize) / float32(physicalSize)
	}

	// Before the manifest install: a refused ETag (a PUT's Content-MD5, a
	// Complete's staged-part check) leaves the stored chunks to the deferred
	// compensator and dedup GC.
	etag, err := cw.ETag()
	if err != nil {
		return chunkedResult{}, err
	}

	chunkEncAlgo := ""
	if encrypting {
		chunkEncAlgo = "AES256-CE"
	}

	ictx := ctx
	if cw.DetachInstall {
		var cancel context.CancelFunc
		ictx, cancel = postCommit(ctx)
		defer cancel()
	}

	// The manifest at its address and the head row naming it commit in ONE
	// transaction (the flip). The flip locks the head row first, so
	// concurrent overwrites of the same key serialize; the manifest a
	// displaced row names is kept by a version or retired (WP-VM-1) — never
	// released here, so a GET that resolved it a moment ago finishes. The
	// version row names this manifest too (backend 'chunked'): Enabled keeps
	// it as a version once a later write displaces it.
	attrs := cw.Attrs
	attrs.ContentType = contentType
	flip, dbErr := commitBlobWrite(ictx, a.db, blobWrite{TenantID: cw.TenantID, Bucket: cw.Bucket, Key: cw.Key, Bill: cw.Bill,
		Version: &versionWrite{Size: measuredSize, ETag: etag, ContentType: contentType, Backend: chunkedBackendSentinel,
			Blob: versionBlobRef{StorageKey: addr, Enc: objectEncryption{Algorithm: chunkEncAlgo}}},
		Upsert: func(tx *sql.Tx) error {
			// At a fresh address: nothing to replace, only the install.
			if metaErr := a.gci.ReplaceObjectManifestTx(ictx, tx, cw.TenantID, cw.Bucket, addr, newRefs, &crypto.ObjectMeta{
				TenantID:       cw.TenantID,
				BucketName:     cw.Bucket,
				ObjectKey:      addr,
				TotalSize:      measuredSize,
				ChunkCount:     chunkCount,
				ContentType:    &contentType,
				LogicalSize:    measuredSize,
				PhysicalSize:   &physicalSize,
				DedupRatio:     &dedupRatio,
				PipelineConfig: chunkedPipeline(chunker, encrypting),
			}); metaErr != nil {
				return fmt.Errorf("replace object manifest: %w", metaErr)
			}
			if err := upsertChunkedObjectHeadRow(ictx, tx, cw.TenantID, cw.Bucket, cw.Key, addr, measuredSize, etag, chunkEncAlgo, attrs); err != nil {
				return err
			}
			if cw.InTx != nil {
				return cw.InTx(ictx, tx)
			}
			return nil
		}})
	if dbErr != nil {
		// Manifest swap and head upsert rolled back together: the previous
		// object is fully intact (old manifest, old head row) and the retry
		// re-runs the whole swap. This write's ref increments are released
		// by the deferred compensator (F10); chunk blobs whose refs drop to
		// zero are reclaimed by dedup GC.
		return chunkedResult{}, fmt.Errorf("install chunked object %s/%s: %w", cw.Bucket, cw.Key, dbErr)
	}
	manifestInstalled = true
	// A whole-object blob this key held before (at <key> or at its address)
	// was retired in the install's transaction (the flip, WP-R2-1): nothing
	// can serve it, and the collector removes it after its grace.

	a.logger.Info("chunked object stored",
		zap.String("tenant_id", cw.TenantID),
		zap.String("s3.bucket", cw.Bucket),
		zap.String("s3.object", cw.Key),
		zap.String("first_store_backend", pool.backendName),
		zap.String("etag", etag),
		zap.Int("chunks", chunkCount),
		zap.Float32("dedup_ratio", dedupRatio),
		zap.Int64("size", measuredSize))

	return chunkedResult{ETag: etag, Size: measuredSize, PhysicalSize: physicalSize, Chunks: chunkCount,
		DedupRatio: dedupRatio, Flip: flip, Encrypted: encrypting}, nil
}

// upsertChunkedObjectHeadRow writes the head row of a CHUNKED object inside
// the install's transaction: every attribute column set explicitly (as
// upsertWholeObjectHeadRow does), is_chunked TRUE, the manifest's address
// (WP-VM-1; a chunked object has no whole blob), no key version (versions
// live on the chunks), backend_name the sentinel, the floor every chunked
// object is billed on.
func upsertChunkedObjectHeadRow(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, manifest string,
	size int64, etag, encAlgo string, attrs objectAttrs) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm,
			 content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location,
			 floor, is_chunked, tags, storage_key, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $17, TRUE, $16, NULLIF($18, ''), NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes                = EXCLUDED.size_bytes,
			etag                      = EXCLUDED.etag,
			content_type              = EXCLUDED.content_type,
			backend_name              = EXCLUDED.backend_name,
			metadata                  = EXCLUDED.metadata,
			encryption_algorithm      = EXCLUDED.encryption_algorithm,
			encryption_key_version    = NULL, -- versions live on the chunks
			content_disposition       = EXCLUDED.content_disposition,
			content_encoding          = EXCLUDED.content_encoding,
			content_language          = EXCLUDED.content_language,
			cache_control             = EXCLUDED.cache_control,
			http_expires              = EXCLUDED.http_expires,
			website_redirect_location = EXCLUDED.website_redirect_location,
			floor                     = EXCLUDED.floor,
			is_chunked                = TRUE,
			tags                      = EXCLUDED.tags,
			storage_key               = EXCLUDED.storage_key, -- the manifest's address (WP-VM-1)
			backend_etag              = NULL, -- a manifest has no one backend answer (RP72-1)
			updated_at                = NOW()`,
		tenantID, bucket, key, size, etag, attrs.ContentType, chunkedBackendSentinel, attrs.metadataJSON(), encAlgo,
		attrs.ContentDisposition, attrs.ContentEncoding, attrs.ContentLanguage,
		attrs.CacheControl, attrs.Expires, attrs.WebsiteRedirect, attrs.tagsJSON(), chunkedObjectFloor, manifest)
	if err != nil {
		return fmt.Errorf("upsert chunked head row %s/%s: %w", bucket, key, err)
	}
	return nil
}
