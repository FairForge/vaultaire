package api

import (
	"context"
	"fmt"
	"os"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// maxBucketsPerTenant is the hard cap for paid tiers.
const maxBucketsPerTenant = 1000

type bucketCreateState int

const (
	bucketCreateOK                bucketCreateState = iota // row inserted (or re-created: owned already)
	bucketCreateCapMax                                     // 1000-bucket hard cap
	bucketCreateCapFree                                    // FreeTierLimits.MaxBuckets
	bucketCreateInvalidRegion                              // not a region id we know
	bucketCreateRegionUnavailable                          // known region, no driver on this deployment (WP-R7-1)
	bucketCreateRegionDown                                 // enabled region, down by its probe or breaker (Prompt 10 Part A)
	bucketCreateLockMismatch                               // re-create of an owned bucket without Object Lock, asking for it (RP16-4)
	bucketCreateReservedName                               // the name is a web path's first segment (bucket_reserved.go, Prompt 41 item 5); refused for a NEW bucket only
	bucketCreateOutsideResidency                           // the region is outside the tenant's residency, or none inside it is enabled (WP-ACCT-1)
)

// bucketCreateOptions are what a CreateBucket asks for beyond the name and
// region. objectLock = `x-amz-bucket-object-lock-enabled: true` (RP16-4):
// the bucket is created with Object Lock on and versioning Enabled, as AWS
// creates it.
type bucketCreateOptions struct {
	objectLock bool
	// private: the bucket is created Private (the management API's
	// "mode": "private", RP33-1 c — plan line 1699, "set at creation"): the
	// row is born Standard and made Private through the door in the same
	// transaction, so no write can land between the two.
	private bool
}

// bucketCreateOutcome is what createBucketRegistry decided. Region is the
// STORED region (an idempotent re-create reports the one the bucket has).
type bucketCreateOutcome struct {
	state        bucketCreateState
	region       string
	alreadyOwned bool
}

// createBucketRegistry is the one bucket-creation rule (Review R11-05,
// the R4-22 lesson): the S3 CreateBucket handler and the management API's
// POST /buckets used to diverge — the management path had no free-tier cap,
// no 1000 cap, no region validation, no sse default and no event. Order:
// idempotent re-create short-circuit (BucketAlreadyOwnedByYou must not 403
// at the limit), hard cap, free-tier cap, region rules, marker dir, row
// (`ON CONFLICT DO NOTHING`), slug, stored-region read-back, event.
//
// region == "" means the deployment default. The caller has already run
// validateBucketName. With no database (dev) only the marker dir is made.
func (s *Server) createBucketRegistry(ctx context.Context, tenantID, bucket, region string) (bucketCreateOutcome, error) {
	return s.createBucketRegistryWith(ctx, tenantID, bucket, region, bucketCreateOptions{})
}

// createBucketRegistryWith is createBucketRegistry with the S3 request's
// options.
func (s *Server) createBucketRegistryWith(ctx context.Context, tenantID, bucket, region string, opts bucketCreateOptions) (bucketCreateOutcome, error) {
	out := bucketCreateOutcome{}

	if s.db != nil && tenantID != "default" {
		if err := s.db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)",
			tenantID, bucket).Scan(&out.alreadyOwned); err != nil {
			return out, fmt.Errorf("check bucket ownership %s: %w", bucket, err)
		}
		if !out.alreadyOwned {
			// A web path's name is refused for a NEW bucket only: one that
			// exists under it keeps working and re-creates idempotently.
			if s.reservedBucketName(bucket) {
				out.state = bucketCreateReservedName
				return out, nil
			}
			count, err := countCustomerBuckets(ctx, s.db, tenantID)
			if err != nil {
				return out, fmt.Errorf("count buckets for %s: %w", tenantID, err)
			}
			if count >= maxBucketsPerTenant {
				out.state = bucketCreateCapMax
				return out, nil
			}
			if s.quotaManager != nil {
				tier, _ := s.quotaManager.GetTier(ctx, tenantID)
				if usage.IsFreeTier(tier) && count >= usage.FreeTierLimits.MaxBuckets {
					out.state = bucketCreateCapFree
					return out, nil
				}
			}
		}
	}

	// The tenant's residency (WP-ACCT-1, residency.go): a new bucket only in
	// a region of its jurisdiction; one that names no region gets the first
	// enabled region inside it. An owned bucket's idempotent re-create
	// creates nothing and is not refused.
	if !out.alreadyOwned {
		residency, err := s.tenantResidency(ctx, tenantID)
		if err != nil {
			return out, err
		}
		if residency != "" {
			if region == "" {
				region = firstRegionInside(residency)
			}
			if region == "" || !regionInside(region, residency) {
				s.log().Info("bucket create refused: outside the tenant's residency",
					zap.String("tenant", tenantID), zap.String("region", region), zap.String("residency", residency))
				out.state = bucketCreateOutsideResidency
				return out, nil
			}
		}
	}
	if region == "" {
		region = drivers.IDriveDefaultRegion(os.Getenv)
	}
	// The region gate (region_gate.go): a region we do not know, one this
	// deployment has no driver for — accepting it would silently store the
	// objects on the primary, a data-residency breach with a truthful-looking
	// label (Review R7-01 / WP-R7-1) — and one whose driver is down by its
	// probe or breaker (Prompt 10 Part A: a bucket nothing can be stored in).
	// A bucket the tenant already owns is not refused for its region being
	// down: re-creating it creates nothing.
	switch st := s.regionCreateState(region); {
	case st == regionUnknown:
		s.refuseRegion(tenantID, region, st)
		out.state = bucketCreateInvalidRegion
		return out, nil
	case st == regionNotEnabled:
		s.refuseRegion(tenantID, region, st)
		out.state = bucketCreateRegionUnavailable
		return out, nil
	case st == regionDown && !out.alreadyOwned:
		s.refuseRegion(tenantID, region, st)
		out.state = bucketCreateRegionDown
		return out, nil
	}
	out.region = region

	if out.alreadyOwned {
		// Idempotent re-create: nothing to write; report the stored region.
		// Asking for Object Lock on an owned bucket that has none is
		// refused, never a 200 that leaves it unlocked (RP16-4).
		var stored string
		var locked bool
		if err := s.db.QueryRowContext(ctx, `SELECT region, object_lock_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`,
			tenantID, bucket).Scan(&stored, &locked); err == nil {
			if stored != "" {
				out.region = stored
			}
			if opts.objectLock && !locked {
				out.state = bucketCreateLockMismatch
			}
		}
		return out, nil
	}

	// Marker directory (best-effort; the registry row is the truth — R4-04).
	dirPath, safe := safeBucketPath("/tmp/vaultaire", tenantID, bucket)
	if !safe {
		return out, fmt.Errorf("bucket path for %s is not safe", bucket)
	}
	if err := os.MkdirAll(dirPath, 0755); err != nil { // #nosec G301 -- bucket dirs need read access
		return out, fmt.Errorf("create bucket dir %s: %w", bucket, err)
	}

	if s.db == nil {
		return out, nil
	}

	sseDefault := s.sseService != nil
	// data_residency is the coarse label the dashboard settings page shows
	// (050); the dashboard used to derive it and the S3/management paths left
	// it NULL — one rule now (Review R15, WP-R12-10).
	residency := drivers.DataResidency(region) // RP78-4: London is the UK
	// An Object Lock bucket is created with versioning Enabled (AWS: lock
	// implies versioning, and it cannot be suspended by the create).
	versioning := "disabled"
	if opts.objectLock {
		versioning = "Enabled"
	}
	if opts.private {
		// Born Standard and made Private through the door in ONE
		// transaction (RP33-1 c): no write can land between the two.
		inserted, err := s.createPrivateBucketRow(ctx, tenantID, bucket, sseDefault, region, residency, opts.objectLock, versioning)
		if err != nil {
			return out, err
		}
		if !inserted {
			// A concurrent create of the same name won: nothing of ours to set.
			out.alreadyOwned = true
			return out, nil
		}
	} else if _, err := s.db.ExecContext(ctx, `
		INSERT INTO buckets (tenant_id, name, visibility, sse_enabled, region, data_residency,
			object_lock_enabled, versioning_status)
		VALUES ($1, $2, 'private', $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, bucket, sseDefault, region, residency, opts.objectLock, versioning); err != nil {
		return out, fmt.Errorf("persist bucket %s: %w", bucket, err)
	}
	auth.EnsureTenantSlug(ctx, s.db, tenantID, s.logger)

	// Re-creating an owned bucket is a no-op: report the region the bucket
	// HAS, not the one requested.
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&stored); err == nil && stored != "" {
		out.region = stored
	}

	emitEvent(ctx, s.db, s.logger, "bucket.created", tenantID, map[string]interface{}{
		"bucket": bucket,
		"region": out.region,
	})
	s.logger.Info("bucket created",
		zap.String("bucket", bucket), zap.String("tenant", tenantID), zap.String("region", out.region),
		zap.Bool("object_lock", opts.objectLock))
	return out, nil
}

// createPrivateBucketRow inserts the bucket's row (born Standard, as every
// create) and makes it Private through the door in the same transaction.
// inserted is false when the name already existed (nothing changed).
func (s *Server) createPrivateBucketRow(ctx context.Context, tenantID, bucket string, sseDefault bool, region, residency string,
	objectLock bool, versioning string) (inserted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("persist bucket %s: %w", bucket, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO buckets (tenant_id, name, visibility, sse_enabled, region, data_residency,
				object_lock_enabled, versioning_status)
			VALUES ($1, $2, 'private', $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, name) DO NOTHING
			RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM ins)
	`, tenantID, bucket, sseDefault, region, residency, objectLock, versioning).Scan(&inserted); err != nil {
		return false, fmt.Errorf("persist bucket %s: %w", bucket, err)
	}
	if !inserted {
		return false, nil
	}
	private := true
	if _, err := modes.SetBucket(ctx, tx, tenantID, bucket, modes.Change{SSECRequired: &private}); err != nil {
		return false, fmt.Errorf("make bucket %s private: %w", bucket, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("persist bucket %s: %w", bucket, err)
	}
	return true, nil
}

// dashboardBucketCreator adapts createBucketRegistry to the dashboard's
// BucketCreator so all three entry points (S3 CreateBucket, the management
// API and the dashboard form) share one rule (WP-R12-10).
func (s *Server) dashboardBucketCreator() handlers.BucketCreator {
	return func(ctx context.Context, tenantID, name, region string) (handlers.BucketCreateResult, error) {
		out, err := s.createBucketRegistry(ctx, tenantID, name, region)
		if err != nil {
			return handlers.BucketCreateResult{}, err
		}
		res := handlers.BucketCreateResult{Region: out.region}
		switch out.state {
		case bucketCreateCapMax:
			res.State = handlers.BucketCreateCapMax
		case bucketCreateCapFree:
			res.State = handlers.BucketCreateCapFree
		case bucketCreateInvalidRegion:
			res.State = handlers.BucketCreateInvalidRegion
		case bucketCreateRegionUnavailable:
			res.State = handlers.BucketCreateRegionUnavailable
		case bucketCreateRegionDown:
			res.State = handlers.BucketCreateRegionDown
		case bucketCreateReservedName:
			res.State = handlers.BucketCreateReservedName
		case bucketCreateOutsideResidency:
			res.State = handlers.BucketCreateOutsideResidency
		default:
			res.State = handlers.BucketCreateOK
		}
		return res, nil
	}
}
