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
func (d objectDeleteAftermath) settle(ctx context.Context, tenantID, bucket, key string, isChunked bool) error {
	ctx, cancel := postCommit(ctx)
	defer cancel()
	if d.db != nil {
		deleted, found, delErr := deleteHeadRowReleasing(ctx, d.db, manifestReleaser(d.gci), tenantID, bucket, key)
		switch {
		case delErr != nil && isChunked:
			return fmt.Errorf("chunked delete: %w", delErr)
		case delErr != nil:
			return fmt.Errorf("head row delete: %w", delErr)
		case found && deleted.Size > 0 && d.quota != nil:
			if err := releaseQuotaOn(ctx, d.quota, tenantID, deleted.Floor, deleted.Size); err != nil {
				d.logger.Error("quota release after delete failed",
					zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", deleted.Size))
			}
		}
		// The retention (expired, governance-bypassed, or none) goes with the
		// object: the PUT-side lock check no longer needs a head row, so a
		// stale lock row would refuse the next upload to this key.
		if _, err := d.db.ExecContext(ctx, `
			DELETE FROM object_locks WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			tenantID, bucket, key); err != nil {
			d.logger.Error("object lock row delete failed", zap.Error(err),
				zap.String("bucket", bucket), zap.String("key", key))
		}
		// A Smart-demoted object has a second copy (the hot one, until it is
		// reclaimed) that the delete did not reach: remove it now and settle
		// the ledger, so no later job owes this key a delete (WP-R13-2).
		d.smartPromoter.OnDelete(ctx, tenantID, bucket, key)
		// The parity copy of a vault object goes with it (WP-VAULT-1); a leg
		// that cannot be reached now is the job's stale pass to finish.
		d.vaultParity.OnObjectDeleted(ctx, tenantID, bucket, key)
	}
	// Per key, in a batch too (AWS fires ObjectRemoved per key) — from the
	// batch's one load of the targets when there is one.
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
		return nil
	}
	d.notify.Fire(tenantID, bucket, "s3:ObjectRemoved:Delete", key, 0, "")
	emitEvent(ctx, d.db, d.logger, "object.deleted", tenantID, data)
	return nil
}
