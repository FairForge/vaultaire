package api

// Post-merge review of #621 / #629 (2026-10-07): what every path that
// removes an object owes AFTER its bytes are gone, in one place — the way
// the write paths share object_write_shared.go. Single DELETE and
// DeleteObjects each had their own copy and drifted: the batch path erased
// no parity shards (they stayed on the leg until the job's stale pass, or
// forever when that pass could not reach the leg) and left the lock row (a
// stale retention refused the next upload to the key); the single DELETE
// handler built its adapter without the Smart promoter, so only the batch
// path settled a demoted object's second copy.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"go.uber.org/zap"
)

// objectDeleteAftermath is the bookkeeping of a removed object: the head
// row (the billing record, releasing exactly its bytes), the lock row, the
// Smart second copy and its ledger, the parity shards and their row, then
// the s3:ObjectRemoved:Delete notification and the object.deleted event —
// one per key, in a batch too (AWS fires ObjectRemoved per key).
type objectDeleteAftermath struct {
	db            *sql.DB
	gci           *crypto.GlobalContentIndex
	quota         QuotaManager
	smartPromoter *SmartPromoter
	vaultParity   *VaultParity
	notify        *NotificationDispatcher
	logger        *zap.Logger
	// fanout: the targets of the notification and the event, resolved ONCE
	// for a batch (forBatch); nil = resolved per key, in the dispatch
	// goroutines (single DELETE).
	fanout *deleteFanout
}

// deleteFanout is where a batch's per-key notification and event go: the
// bucket's enabled bucket_notifications rows and the tenant's enabled
// webhook_endpoints rows. Both empty = no per-key goroutine at all. Before
// this (post-merge review of #631) a 1,000-key DeleteObjects started 2,000
// goroutines, each with its own query, against a 50-connection pool, to
// find 2,000 times that there was nothing to deliver to.
type deleteFanout struct {
	targets   []notifyTarget
	endpoints []webhookEndpoint
	// webhookGen: the tenant's webhook generation the endpoints were read
	// at (a queued job re-reads them when it moved).
	webhookGen uint64

	// pending: one entry per settled key, handed to the process-wide
	// delivery pool once every key is done (deliverInOrder) — never a
	// goroutine per key (Prompt 2a PR 4: a 1,000-key batch with one target
	// and one webhook meant 2,000 concurrent POSTs and 1,000
	// webhook_deliveries inserts against the 50-connection pool), nor a
	// worker set per batch (2a.2: concurrent batches piled up).
	mu      sync.Mutex
	pending []pendingDelivery
}

// pendingDelivery is one key's notification + webhook, its events row
// already written (eventID empty when it was not).
type pendingDelivery struct {
	key      string
	eventID  string
	dataJSON []byte
}

// enqueue remembers a settled key's delivery.
func (f *deleteFanout) enqueue(p pendingDelivery) {
	f.mu.Lock()
	f.pending = append(f.pending, p)
	f.mu.Unlock()
}

// deliverInOrder hands every pending key's notification and webhook to
// the process-wide delivery pool (event_delivery.go) in the order position
// gives (the request's key order) — a notification job then a webhook job
// per key, each with its own deadline. "In key order" is START order: the
// pool's workers take jobs from one FIFO queue, so a key's delivery never
// starts before an earlier key's, and arrivals may still interleave by up
// to the worker count. Detached from the response like before: it does not
// wait for the targets. What the queue cannot take is recorded at once as
// failed `dropped: overloaded` (one statement), never a waiting goroutine.
// Nothing to deliver to = nothing submitted.
func (f *deleteFanout) deliverInOrder(d objectDeleteAftermath, tenantID, bucket string, position func(key string) int) {
	f.mu.Lock()
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()
	if len(pending) == 0 || (len(f.targets) == 0 && len(f.endpoints) == 0) {
		return
	}
	sort.SliceStable(pending, func(i, j int) bool { return position(pending[i].key) < position(pending[j].key) })
	const eventName, eventType = "s3:ObjectRemoved:Delete", "object.deleted"
	jobs := make([]deliveryJob, 0, 2*len(pending))
	for _, p := range pending {
		if len(f.targets) > 0 {
			jobs = append(jobs, d.notify.notificationJob(f.targets, tenantID, bucket, eventName, p.key, 0, ""))
		}
		if p.eventID != "" && len(owedRows(f.endpoints, p.eventID, eventType)) > 0 {
			jobs = append(jobs, webhookJob(d.db, d.logger, f.endpoints, f.webhookGen, p.eventID, eventType, tenantID, p.dataJSON))
		}
	}
	eventDeliveries.submit(jobs...)
}

// forBatch resolves the fanout once for every key of a batch on bucket. A
// load that fails is logged and treated as no target: the per-key loads
// would fail the same way and deliver nothing either, at 1,000 times the
// cost; the event rows are still written per key and `GET /api/v1/events`
// serves them.
func (d objectDeleteAftermath) forBatch(ctx context.Context, tenantID, bucket string) objectDeleteAftermath {
	f := &deleteFanout{}
	if d.db == nil {
		d.fanout = f
		return d
	}
	var err error
	notifyGen := notifyGeneration(tenantID, bucket)
	f.webhookGen = webhookGeneration(tenantID)
	if f.targets, err = d.notify.Targets(ctx, tenantID, bucket); err != nil {
		d.logger.Error("batch delete: notification targets not loaded; no notification for this batch",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket))
	} else if d.notify != nil {
		rememberNotifyTargets(tenantID, bucket, notifyGen, f.targets, nil)
	}
	if f.endpoints, err = loadWebhookEndpoints(ctx, d.db, d.logger, tenantID); err != nil {
		d.logger.Error("batch delete: webhook endpoints not loaded; no webhook for this batch",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket))
	} else {
		rememberWebhookEndpoints(tenantID, f.webhookGen, f.endpoints, nil)
	}
	d.fanout = f
	return d
}

func (s *Server) objectDeleteAftermath() objectDeleteAftermath {
	return objectDeleteAftermath{db: s.db, gci: s.gci, quota: s.quotaManager, smartPromoter: s.smartPromoter,
		vaultParity: s.vaultParity, notify: NewNotificationDispatcher(s.db, s.logger), logger: s.log()}
}

func (a *S3ToEngine) objectDeleteAftermath() objectDeleteAftermath {
	return objectDeleteAftermath{db: a.db, gci: a.gci, quota: a.quota, smartPromoter: a.smartPromoter,
		vaultParity: a.vaultParity, notify: a.notifySvc, logger: a.logger}
}

// headRead is what a DELETE read of the key's head row before it removed
// the bytes: whether there was a row, and the address it named.
type headRead struct {
	found      bool
	storageKey string
}

// deleteBeforeRowHook runs in settle before the head row is deleted (tests
// commit a write of the key there: PM-6).
var deleteBeforeRowHook = func(tenantID, bucket, key string) {}

// settle runs the aftermath for one key whose bytes are gone (or were never
// there — S3 DELETE is idempotent). It runs detached from the request
// (postCommit): the backend delete has happened, so neither a client that
// hangs up nor a shutdown's cut may stop it halfway (Prompt 2a.2 G1: a cut
// batch reported its keys Deleted and kept every head row — HEAD 200, GET
// 404, still billed). A head row that cannot be deleted is returned as an
// error, so the caller answers one and the client's retry (a backend miss,
// idempotent) settles the key; a chunked object releases its manifest in
// that same transaction (R8-08) and stays intact on failure. Everything
// after the row is best effort and never fails the delete.
//
// The row goes only while it is the one the DELETE read (read: the address
// whose bytes it removed; PM-6 of WP-R2-1): a write that committed in
// between is current — the DELETE is ordered before it, answers 204, and
// leaves the write's row, bytes and retention alone. A DELETE that read no
// row deletes none.
func (d objectDeleteAftermath) settle(ctx context.Context, tenantID, bucket, key string, isChunked bool, read headRead) error {
	ctx, cancel := postCommit(ctx)
	defer cancel()
	deleteBeforeRowHook(tenantID, bucket, key)
	if d.db != nil {
		var deleted displacedRow
		var found, superseded bool
		var delErr error
		if read.found {
			deleted, found, superseded, delErr = deleteHeadRowIfAt(ctx, d.db, manifestReleaser(d.gci), tenantID, bucket, key, read.storageKey)
		} else if exErr := d.db.QueryRowContext(ctx, headRowExistsSQL, tenantID, bucket, key).Scan(&superseded); exErr != nil {
			// Nothing was read, so no row is deleted; a row there now is a
			// write's (superseded), and its retention stays.
			delErr = fmt.Errorf("head row after a delete of nothing: %w", exErr)
		}
		switch {
		case delErr != nil && isChunked:
			return fmt.Errorf("chunked delete: %w", delErr)
		case delErr != nil:
			return fmt.Errorf("head row delete: %w", delErr)
		case found && d.quota != nil:
			// A version that keeps the blob keeps the bill (WP-R2-1 C2).
			if err := releaseDisplacedOn(ctx, d.quota, tenantID, deleted); err != nil {
				d.logger.Error("quota release after delete failed",
					zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", deleted.Size))
			}
		}
		// The retention (expired, governance-bypassed, or none) goes with the
		// object: the PUT-side lock check no longer needs a head row, so a
		// stale lock row would refuse the next upload to this key. A write
		// that superseded the delete keeps its own.
		if superseded && read.found {
			d.logger.Info("delete: a write of the key committed after its bytes were removed — the write is current, its row kept (the delete is ordered before it)",
				zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", key))
		}
		if !superseded {
			if _, err := d.db.ExecContext(ctx, `
				DELETE FROM object_locks WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
				tenantID, bucket, key); err != nil {
				d.logger.Error("object lock row delete failed", zap.Error(err),
					zap.String("bucket", bucket), zap.String("key", key))
			}
		}
		// A Smart-demoted object has a second copy (the hot one, until it is
		// reclaimed) that the delete did not reach: remove it now and settle
		// the ledger, so no later job owes this key a delete (WP-R13-2).
		d.smartPromoter.OnDelete(ctx, tenantID, bucket, key)
		// The parity copy of a vault object goes with it (WP-VAULT-1); a leg
		// that cannot be reached now is the job's stale pass to finish. A
		// superseding write's object is not this delete's to unprotect.
		if !superseded {
			d.vaultParity.OnObjectDeleted(ctx, tenantID, bucket, key)
		}
	}
	d.announce(ctx, tenantID, bucket, key)
	return nil
}

// announce fires a removed key's s3:ObjectRemoved:Delete notification and
// object.deleted event: per key, in a batch too (AWS fires ObjectRemoved per
// key) — from the batch's one load of the targets when there is one.
func (d objectDeleteAftermath) announce(ctx context.Context, tenantID, bucket, key string) {
	data := map[string]interface{}{"bucket": bucket, "key": key}
	if d.fanout != nil {
		// The events row now (the tenant's log, as before); the deliveries
		// from the batch's worker set once every key is settled.
		p := pendingDelivery{key: key}
		if d.db != nil {
			if eventID, dataJSON, ok := recordEvent(ctx, d.db, d.logger, "object.deleted", tenantID, data); ok {
				p.eventID, p.dataJSON = eventID, dataJSON
			}
		}
		d.fanout.enqueue(p)
		return
	}
	d.notify.Fire(tenantID, bucket, "s3:ObjectRemoved:Delete", key, 0, "")
	emitEvent(ctx, d.db, d.logger, "object.deleted", tenantID, data)
}

// The two versioned flavours of a delete, shared by DELETE and DeleteObjects
// (Stage 1a Prompt 3, Part 0: the batch ignored <VersionId> and wrote no
// marker — R3-09, WP-R3-4 — which since WP-R2-1 C2 left the latest version
// billed and unreachable). Object Lock is the caller's (checked before
// either); the notification is the caller's (announce).

// deleteVersionReleasing is DELETE ?versionId: the version row goes (the head
// row re-pointed or dropped when it was the current object's, deleteVersion)
// and the bill follows — the current object's bytes when the deleted version
// was its blob, the retained version's own bytes otherwise. found is false
// for a version id the key does not have.
func (d objectDeleteAftermath) deleteVersionReleasing(ctx context.Context, tenantID, bucket, key, versionID string) (bool, error) {
	found, rp, err := deleteVersion(ctx, d.db, tenantID, bucket, key, versionID)
	if err != nil || !found {
		return found, err
	}
	rel := displacedRow{ReplacedVersion: rp.VersionReleased}
	if rp.Changed {
		rel.Size, rel.Floor = rp.OldSize, rp.Floor
	}
	d.releaseDetached(ctx, tenantID, rel)
	return true, nil
}

// deleteBehindMarker is a DELETE with no version id on an Enabled bucket: a
// delete marker becomes the latest version (a pre-versioning object is
// anchored as the null version first, writeDeleteMarker) and the head row
// goes; a version that keeps the head row's blob keeps it and its bill, a
// blob no version names is retired. Returns the marker's version id.
func (d objectDeleteAftermath) deleteBehindMarker(ctx context.Context, tenantID, bucket, key string) (string, error) {
	markerID := generateVersionID()
	if err := writeDeleteMarker(ctx, d.db, tenantID, bucket, key, markerID); err != nil {
		return "", fmt.Errorf("write the delete marker: %w", err)
	}
	// The head-cache row is the billing record (WP-1): DELETE...RETURNING
	// captures the removed size atomically. An object chunked before
	// versioning was enabled releases its manifest in the same transaction
	// (R8-07).
	marked, found, err := deleteHeadRowReleasing(ctx, d.db, manifestReleaser(d.gci), tenantID, bucket, key)
	if err != nil {
		return "", fmt.Errorf("delete-marker head row: %w", err)
	}
	if found {
		d.releaseDetached(ctx, tenantID, marked)
		// The blob the head row named stays while a version keeps it (the
		// undelete path); one no version names is displaced.
		if marked.StorageKey != "" {
			if err := retireBlobNow(ctx, d.db, tenantID, bucket, key, marked.StorageKey, marked.Backend, retiredOverwritten); err != nil {
				d.logger.Warn("delete marker: the displaced blob is not queued for the collector — the erasure sweep is its only remover",
					zap.Error(err), zap.String("bucket", bucket), zap.String("object", key), zap.String("storage_key", marked.StorageKey))
			}
		}
	}
	return markerID, nil
}

// releaseDetached releases what removing d frees, on a context detached
// from the request: the row change has committed.
func (d objectDeleteAftermath) releaseDetached(ctx context.Context, tenantID string, rel displacedRow) {
	if d.quota == nil || len(rel.releases()) == 0 {
		return
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := releaseDisplacedOn(qctx, d.quota, tenantID, rel); err != nil {
		d.logger.Error("quota release after delete failed",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", rel.Size))
	}
}

// knownCopiesSQL: the backends other than the recorded one ($6) that hold
// the blob at this address as far as the tables know — an unsettled
// Smart-demotion ledger row of the key at the address (its hot copy until
// reclaimed, its cold copy), and a retired_blobs row naming the address on
// another backend (a CompleteMultipartUpload retry that reused its address
// after the first attempt's blob was queued as an orphan elsewhere; 090).
const knownCopiesSQL = `
	SELECT hot_backend FROM smart_demotions
	 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND COALESCE(storage_key, '') = $4
	   AND hot_deleted_at IS NULL AND hot_outcome NOT IN ('kept_changed', 'object_gone') AND hot_backend <> $6
	UNION
	SELECT cold_backend FROM smart_demotions
	 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND COALESCE(storage_key, '') = $4
	   AND hot_outcome NOT IN ('kept_changed', 'object_gone') AND cold_backend <> $6
	UNION
	SELECT backend_id FROM retired_blobs
	 WHERE container = $5 AND storage_key = $4 AND $4 <> '' AND tenant_id = $1 AND backend_id <> $6`

// knownBlobCopies is every backend other than recorded the blob at
// storageKey ("" = the key) is known to be on. A DELETE removes them with
// the recorded copy (engine.WithKnownCopies): every one attempted, any
// failure the delete's (R6-24). The Smart ledger's settle still runs after
// the row (and the daily reclaim after a failure): this is the attempt
// made before the DELETE answers.
func knownBlobCopies(ctx context.Context, db *sql.DB, tenantID, bucket, key, container, storageKey, recorded string) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, knownCopiesSQL, tenantID, bucket, key, storageKey, container, recorded)
	if err != nil {
		return nil, fmt.Errorf("known copies of %s/%s: %w", bucket, key, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("known copies of %s/%s: %w", bucket, key, err)
		}
		if b != "" {
			out = append(out, b)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("known copies of %s/%s: %w", bucket, key, err)
	}
	return out, nil
}
