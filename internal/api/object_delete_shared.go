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
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/usage"
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
	siaPromoter   *SiaPromoter
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
	key     string
	eventID string
	event   recordedEvent
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
	var intents []droppedDelivery
	for _, p := range pending {
		if len(f.targets) > 0 {
			jobs = append(jobs, d.notify.notificationJob(f.targets, tenantID, bucket, eventName, p.key, 0, ""))
		}
		if p.eventID == "" {
			continue
		}
		if owed := owedRows(f.endpoints, p.eventID, eventType); len(owed) > 0 {
			intents = append(intents, owed...)
			jobs = append(jobs, webhookJob(d.db, d.logger, f.endpoints, f.webhookGen, p.event))
		}
	}
	// The batch's outbox rows in one statement before its jobs (WP-R11-3).
	recordWebhookIntents(context.Background(), d.db, d.logger, tenantID, intents)
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
	return objectDeleteAftermath{db: s.db, gci: s.gci, quota: s.quotaManager, smartPromoter: s.smartPromoter, siaPromoter: s.siaPromoter,
		vaultParity: s.vaultParity, notify: NewNotificationDispatcher(s.db, s.logger), logger: s.log()}
}

func (a *S3ToEngine) objectDeleteAftermath() objectDeleteAftermath {
	return objectDeleteAftermath{db: a.db, gci: a.gci, quota: a.quota, smartPromoter: a.smartPromoter, siaPromoter: a.siaPromoter,
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

// deleteMarkerWrittenHook runs after a delete marker's transaction (tests
// commit a write of the key there: PM-6 under versioning).
var deleteMarkerWrittenHook = func(tenantID, bucket, key string) {}

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
		var superseded bool
		var delErr error
		if read.found {
			// The row and its bytes' release in one transaction (WP-R10-8):
			// a version that keeps the blob keeps the bill (WP-R2-1 C2).
			_, _, superseded, delErr = deleteHeadRowIfAt(ctx, d.db, d.quota != nil, tenantID, bucket, key, read.storageKey)
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
			d.afterObjectGone(ctx, tenantID, bucket, key)
		} else {
			// A Smart-demoted object has a second copy the delete did not
			// reach: settled whatever superseded it (WP-R13-2).
			d.smartPromoter.OnDelete(ctx, tenantID, bucket, key)
			// A promoted copy of the deleted object (sia_promotion.go): the
			// write that superseded the delete retired it in its flip.
			d.siaPromoter.OnObjectChanged(ctx, tenantID, bucket, key)
		}
	}
	d.announce(ctx, tenantID, bucket, key)
	return nil
}

// afterObjectGone is what a key owes once its current object is gone (its
// head row deleted, its bytes no longer named): the retention (expired,
// governance-bypassed, or none) goes with it — the PUT-side lock check
// needs no head row, so a stale lock row would refuse the next upload; a
// Smart-demoted object's second copy (the hot one, until it is reclaimed)
// is removed and the ledger settled, so no later job owes the key a delete
// (WP-R13-2); the parity copy of a vault object goes (WP-VAULT-1; a leg
// that cannot be reached now is the job's stale pass to finish).
func (d objectDeleteAftermath) afterObjectGone(ctx context.Context, tenantID, bucket, key string) {
	if _, err := d.db.ExecContext(ctx, `
		DELETE FROM object_locks WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key); err != nil {
		d.logger.Error("object lock row delete failed", zap.Error(err),
			zap.String("bucket", bucket), zap.String("key", key))
	}
	d.smartPromoter.OnDelete(ctx, tenantID, bucket, key)
	d.siaPromoter.OnObjectChanged(ctx, tenantID, bucket, key)
	d.vaultParity.OnObjectDeleted(ctx, tenantID, bucket, key)
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
			if ev, ok := recordEvent(ctx, d.db, d.logger, "object.deleted", tenantID, data); ok {
				p.eventID, p.event = ev.id, ev
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
	// The version row, the head row's re-point and the release they make
	// commit together (WP-R10-8).
	found, _, err := deleteVersion(ctx, d.db, d.quota != nil, tenantID, bucket, key, versionID)
	return found, err
}

// deleteBehindMarker is a DELETE with no version id on an Enabled bucket: a
// delete marker becomes the latest version (a pre-versioning object is
// anchored as the null version first, writeDeleteMarker) and the head row
// goes; a version that keeps the head row's blob keeps it and its bill, a
// blob no version names is retired. Returns the marker's version id.
func (d objectDeleteAftermath) deleteBehindMarker(ctx context.Context, tenantID, bucket, key string) (string, error) {
	markerID := generateVersionID()
	// The marker, the head row's removal (the billing record, DELETE ...
	// RETURNING; an object chunked before versioning was enabled is anchored
	// as the null version, which keeps its manifest — PM-3, WP-VM-1) and the
	// displaced blob's fate commit in ONE
	// transaction under the key's locks: a write that commits around the
	// DELETE is wholly before or wholly after it (PM-6 under versioning,
	// Stage 1a Prompt 6 Part 0 — split in two, a PUT committed between the
	// marker and the row delete lost its head row: GET 404 with its version
	// the latest).
	err := writeDeleteMarker(ctx, d.db, tenantID, bucket, key, markerID, func(tx *sql.Tx) error {
		// The blob the head row named stays while a version keeps it (the
		// undelete path); one no version names is displaced — queued before
		// the release, which is the transaction's last statement (WP-R10-8).
		_, _, err := deleteHeadRowTx(ctx, tx, d.quota != nil, tenantID, bucket, key, nil,
			func(row displacedRow) error {
				if row.Chunked || row.StorageKey == "" {
					return nil // a manifest is kept or retired by deleteHeadRowTx itself (WP-VM-1)
				}
				if err := retireBlob(ctx, tx, tenantID, bucket, key, row.StorageKey, row.Backend, retiredOverwritten); err != nil {
					if !errors.Is(err, errUnaddressableBlob) {
						return err
					}
					d.logger.Warn("delete marker: the displaced blob is not queued for the collector — the erasure sweep is its only remover",
						zap.Error(err), zap.String("bucket", bucket), zap.String("object", key), zap.String("storage_key", row.StorageKey))
				}
				return nil
			})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("write the delete marker: %w", err)
	}
	deleteMarkerWrittenHook(tenantID, bucket, key)
	return markerID, nil
}

// deleteSuspended is a DELETE with no version id on a Suspended bucket
// (R61-5, as AWS): the key's "null" version — the current object when a
// Suspended write put it, or a retained one — is replaced by a "null"
// delete marker, which becomes the latest version. In ONE transaction under
// the key's locks: the null version's row is replaced; the head row goes;
// bytes no row names any more (the null version's, and the current
// object's when it WAS the null version) are retired — a GET in flight
// finishes, the collector deletes them — and released; a current object an
// Enabled-era version names keeps its bytes and its bill (that version
// stays, non-current). Before, the head row went and the null version kept
// the bytes billed and listed as live. headGone reports whether the key
// had a current object whose bytes left with it (its lock row, parity and
// Smart copy go too).
func (d objectDeleteAftermath) deleteSuspended(ctx context.Context, tenantID, bucket, key string) (headGone bool, err error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, tenantID, bucket, key); err != nil {
		return false, err
	}
	if err := lockVersionKey(ctx, tx, tenantID, bucket, key); err != nil {
		return false, err
	}
	// The null version as it is now, and whether it is retained bytes of
	// its own (not the head row's: those are the head row's bill).
	var nullKey, nullBackend, nullFloor string
	var nullSize int64
	var nullRetained, nullFound bool
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(v.storage_key, ''), COALESCE(v.backend_name, ''), v.size_bytes, `+usage.RetainedVersionFloor+`,
		       `+usage.VersionRetained+`
		FROM object_versions v
		LEFT JOIN object_head_cache h ON h.tenant_id = v.tenant_id AND h.bucket = v.bucket AND h.object_key = v.object_key
		WHERE v.tenant_id = $1 AND v.bucket = $2 AND v.object_key = $3 AND v.version_id = 'null' AND NOT v.is_delete_marker`,
		tenantID, bucket, key).Scan(&nullKey, &nullBackend, &nullSize, &nullFloor, &nullRetained)
	switch {
	case err == nil:
		nullFound = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, fmt.Errorf("read the null version: %w", err)
	}
	// The null row becomes the marker (the latest version); every other row
	// is non-current.
	if _, err := tx.ExecContext(ctx, `UPDATE object_versions SET is_latest = FALSE
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest`, tenantID, bucket, key); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO object_versions
			(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker)
		VALUES ($1, $2, $3, 'null', 0, '', 'application/octet-stream', TRUE, TRUE)
		ON CONFLICT (tenant_id, bucket, object_key, version_id) DO UPDATE SET
			size_bytes = 0, etag = '', content_type = 'application/octet-stream', is_latest = TRUE, is_delete_marker = TRUE,
			backend_name = NULL, storage_key = NULL, encryption_algorithm = NULL, encryption_key_version = NULL, floor = NULL,
			metadata = NULL, content_disposition = NULL, content_encoding = NULL, content_language = NULL,
			cache_control = NULL, http_expires = NULL, website_redirect_location = NULL, created_at = NOW()`,
		tenantID, bucket, key); err != nil {
		return false, fmt.Errorf("write the null delete marker: %w", err)
	}
	var rels []quotaRelease
	// A retained null version (bytes of its own) is removed: retired and
	// released. One that was the head row's bytes is the head row's below.
	if nullFound && nullRetained && nullKey != "" {
		if err := retireRowBytes(ctx, tx, tenantID, bucket, key, nullKey, nullBackend, nullBackend == chunkedBackendSentinel, retiredVersionDeleted); err != nil &&
			!errors.Is(err, errUnaddressableBlob) {
			return false, err
		}
		rels = append(rels, quotaRelease{Floor: nullFloor, Bytes: nullSize})
	}
	// The head row: its bytes are kept only by a version that still names
	// them (an Enabled-era current version); otherwise retired with it.
	row, found, err := deleteHeadRowTx(ctx, tx, false, tenantID, bucket, key, nil,
		func(row displacedRow) error {
			if row.Chunked || row.KeptByVersion {
				return nil // a manifest is retired by deleteHeadRowTx itself
			}
			if err := retireBlob(ctx, tx, tenantID, bucket, key, blobArtifact(key, row.StorageKey), row.Backend, retiredDeleted); err != nil &&
				!errors.Is(err, errUnaddressableBlob) {
				return err
			}
			return nil
		})
	if err != nil {
		return false, err
	}
	if found {
		rels = append(rels, row.releases()...)
	}
	// The bill, last (WP-R10-8).
	if d.quota != nil {
		if err := releasesTx(ctx, tx, tenantID, rels); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return found && !row.KeptByVersion, nil
}

// knownCopiesSQL: the backends other than the recorded one ($6) that hold
// the blob at this address as far as the tables know — an unsettled
// Smart-demotion ledger row of the key at the address (its hot copy until
// reclaimed, its cold copy), and a retired_blobs row naming the address on
// another backend (a CompleteMultipartUpload retry that reused its address
// after the first attempt's blob was queued as an orphan elsewhere; 090),
// and a mirror copy of the blob on its leg (blob_copies, WP-EDGE-4: the
// row is left for the blob_copies job, which finds it orphaned).
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
	 WHERE container = $5 AND storage_key = $4 AND $4 <> '' AND tenant_id = $1 AND backend_id <> $6
	   AND backend_id <> '` + chunkedBackendSentinel + `'
	UNION
	SELECT leg FROM blob_copies
	 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND storage_key = $4 AND kind = 'mirror' AND leg <> $6` // a retired manifest is no copy of a blob (WP-VM-1); a mirror copy is (WP-EDGE-4)

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
