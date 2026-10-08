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
	d.notify.Fire(tenantID, bucket, "s3:ObjectRemoved:Delete", key, 0, "")
	emitEvent(ctx, d.db, d.logger, "object.deleted", tenantID, map[string]interface{}{
		"bucket": bucket, "key": key,
	})
	return nil
}
