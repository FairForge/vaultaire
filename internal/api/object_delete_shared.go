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
	if f.targets, err = d.notify.Targets(ctx, tenantID, bucket); err != nil {
		d.logger.Error("batch delete: notification targets not loaded; no notification for this batch",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket))
	}
	if f.endpoints, err = loadWebhookEndpoints(ctx, d.db, d.logger, tenantID); err != nil {
		d.logger.Error("batch delete: webhook endpoints not loaded; no webhook for this batch",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket))
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
// there — S3 DELETE is idempotent). A chunked object releases its manifest
// in the head row's transaction (R8-08): a failure there leaves the object
// intact and is returned, so the caller answers an error and the client
// retries; a failed row delete of a whole object is only logged (the blob
// is already gone; the drifted row is the only loss). Everything after the
// row is best effort and never fails the delete.
func (d objectDeleteAftermath) settle(ctx context.Context, tenantID, bucket, key string, isChunked bool) error {
	if d.db != nil {
		deleted, found, delErr := deleteHeadRowReleasing(ctx, d.db, manifestReleaser(d.gci), tenantID, bucket, key)
		switch {
		case delErr != nil && isChunked:
			return fmt.Errorf("chunked delete: %w", delErr)
		case delErr != nil:
			d.logger.Error("head cache delete failed", zap.Error(delErr),
				zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", key))
		case found && deleted.Size > 0 && d.quota != nil:
			// Detached from the request: the delete has committed, so the
			// bookkeeping completes even if the client has disconnected.
			qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := releaseQuotaOn(qctx, d.quota, tenantID, deleted.Floor, deleted.Size); err != nil {
				d.logger.Error("quota release after delete failed",
					zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", deleted.Size))
			}
			cancel()
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
		d.notify.FireTo(d.fanout.targets, tenantID, bucket, "s3:ObjectRemoved:Delete", key, 0, "")
		emitEventTo(ctx, d.db, d.logger, d.fanout.endpoints, "object.deleted", tenantID, data)
		return nil
	}
	d.notify.Fire(tenantID, bucket, "s3:ObjectRemoved:Delete", key, 0, "")
	emitEvent(ctx, d.db, d.logger, "object.deleted", tenantID, data)
	return nil
}
