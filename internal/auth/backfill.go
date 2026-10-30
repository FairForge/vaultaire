package auth

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// BackfillBuckets recreates the bucket row of every bucket that has head
// rows and no row (a DeleteBucket racing a PUT's commit). The row takes the
// region of the backend that holds the bytes (RP86-3): idrive-<region> is
// that region, anything else the default region — it used to take no region,
// i.e. the default, under any residency, and the next write landed outside.
// A bucket whose region is outside its tenant's residency is skipped with an
// Error line (an operator's to place), never recreated outside.
func BackfillBuckets(ctx context.Context, db *sql.DB, logger *zap.Logger) error {
	rows, err := db.QueryContext(ctx, `
		SELECT h.tenant_id, h.bucket, MIN(COALESCE(h.backend_name, '')), COALESCE(t.residency, '')
		  FROM object_head_cache h
		  LEFT JOIN tenants t ON t.id = h.tenant_id
		 WHERE NOT EXISTS (SELECT 1 FROM buckets b WHERE b.tenant_id = h.tenant_id AND b.name = h.bucket)
		 GROUP BY h.tenant_id, h.bucket, t.residency`)
	if err != nil {
		return fmt.Errorf("backfill buckets: %w", err)
	}
	type missing struct{ tenant, bucket, backend, residency string }
	var todo []missing
	for rows.Next() {
		var m missing
		if err := rows.Scan(&m.tenant, &m.bucket, &m.backend, &m.residency); err != nil {
			_ = rows.Close()
			return fmt.Errorf("backfill buckets: %w", err)
		}
		todo = append(todo, m)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("backfill buckets: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill buckets: %w", err)
	}

	def := drivers.IDriveDefaultRegion(os.Getenv)
	var made int64
	for _, m := range todo {
		region := def
		if r, ok := strings.CutPrefix(m.backend, "idrive-"); ok && drivers.IsValidRegion(r) {
			region = r
		}
		if m.residency != "" && drivers.RegionJurisdiction(region) != m.residency {
			logger.Error("backfill buckets: a bucket with objects and no row is outside its tenant's residency — not recreated",
				zap.String("tenant_id", m.tenant), zap.String("bucket", m.bucket), zap.String("backend", m.backend),
				zap.String("region", region), zap.String("residency", m.residency))
			continue
		}
		res, err := db.ExecContext(ctx, `
			INSERT INTO buckets (tenant_id, name, visibility, region, data_residency)
			VALUES ($1, $2, 'private', $3, $4)
			ON CONFLICT (tenant_id, name) DO NOTHING`, m.tenant, m.bucket, region, drivers.DataResidency(region))
		if err != nil {
			return fmt.Errorf("backfill bucket %s of %s: %w", m.bucket, m.tenant, err)
		}
		n, _ := res.RowsAffected()
		made += n
	}
	if made > 0 {
		logger.Info("backfilled buckets from object_head_cache", zap.Int64("count", made))
	}
	return nil
}

// BackfillSlugs generates slugs for any tenant that has slug=NULL.
// Uses the tenant name (company) to generate the slug, with collision
// resolution via -N suffix.
func BackfillSlugs(ctx context.Context, db *sql.DB, logger *zap.Logger) error {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM tenants WHERE slug IS NULL`)
	if err != nil {
		return fmt.Errorf("backfill slugs: query tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type tenantRow struct {
		id   string
		name string
	}

	var tenants []tenantRow
	for rows.Next() {
		var t tenantRow
		if err := rows.Scan(&t.id, &t.name); err != nil {
			return fmt.Errorf("backfill slugs: scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill slugs: iterate tenants: %w", err)
	}

	for _, t := range tenants {
		base := GenerateSlug(t.name)
		slug, err := EnsureSlugUnique(ctx, db, base)
		if err != nil {
			logger.Error("backfill slug: uniqueness check failed",
				zap.String("tenant_id", t.id), zap.Error(err))
			continue
		}

		_, err = db.ExecContext(ctx,
			`UPDATE tenants SET slug = $1 WHERE id = $2 AND slug IS NULL`,
			slug, t.id)
		if err != nil {
			logger.Error("backfill slug: update failed",
				zap.String("tenant_id", t.id), zap.Error(err))
			continue
		}

		logger.Info("backfilled tenant slug",
			zap.String("tenant_id", t.id), zap.String("slug", slug))
	}

	return nil
}
