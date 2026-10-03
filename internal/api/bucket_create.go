package api

import (
	"context"
	"fmt"
	"os"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/drivers"
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
)

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
	out := bucketCreateOutcome{}

	if s.db != nil && tenantID != "default" {
		if err := s.db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)",
			tenantID, bucket).Scan(&out.alreadyOwned); err != nil {
			return out, fmt.Errorf("check bucket ownership %s: %w", bucket, err)
		}
		if !out.alreadyOwned {
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

	if region == "" {
		region = drivers.IDriveDefaultRegion(os.Getenv)
	}
	if !drivers.IsValidRegion(region) {
		out.state = bucketCreateInvalidRegion
		return out, nil
	}
	// A region the account has but this deployment has no driver for must be
	// refused: accepting the bucket would silently store its objects on the
	// primary — a data-residency breach with a truthful-looking label
	// (Review R7-01 / WP-R7-1).
	if !drivers.IDriveRegionAvailable(region) {
		out.state = bucketCreateRegionUnavailable
		return out, nil
	}
	out.region = region

	if out.alreadyOwned {
		// Idempotent re-create: nothing to write; report the stored region.
		var stored string
		if err := s.db.QueryRowContext(ctx, `SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2`,
			tenantID, bucket).Scan(&stored); err == nil && stored != "" {
			out.region = stored
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
	residency := "us"
	if drivers.IsEURegion(region) {
		residency = "eu"
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO buckets (tenant_id, name, visibility, sse_enabled, region, data_residency)
		VALUES ($1, $2, 'private', $3, $4, $5)
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, bucket, sseDefault, region, residency); err != nil {
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
		zap.String("bucket", bucket), zap.String("tenant", tenantID), zap.String("region", out.region))
	return out, nil
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
		default:
			res.State = handlers.BucketCreateOK
		}
		return res, nil
	}
}
