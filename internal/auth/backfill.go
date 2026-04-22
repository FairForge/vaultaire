package auth

import (
	"context"
	"database/sql"
	"fmt"

	"go.uber.org/zap"
)

// BackfillBuckets creates rows in the buckets table for any
// (tenant_id, bucket) pair found in object_head_cache that doesn't
// already exist in buckets. All backfilled buckets are private.
func BackfillBuckets(ctx context.Context, db *sql.DB, logger *zap.Logger) error {
	result, err := db.ExecContext(ctx, `
		INSERT INTO buckets (tenant_id, name, visibility)
		SELECT DISTINCT tenant_id, bucket, 'private'
		FROM object_head_cache
		WHERE NOT EXISTS (
			SELECT 1 FROM buckets b
			WHERE b.tenant_id = object_head_cache.tenant_id
			AND b.name = object_head_cache.bucket
		)
		ON CONFLICT (tenant_id, name) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("backfill buckets: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows > 0 {
		logger.Info("backfilled buckets from object_head_cache", zap.Int64("count", rows))
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
