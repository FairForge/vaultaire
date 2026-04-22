package auth

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)

var reservedSlugs = map[string]bool{
	"admin": true, "cdn": true, "api": true, "dashboard": true,
	"static": true, "health": true, "login": true, "register": true,
	"status": true, "docs": true, "webhook": true, "auth": true,
	"metrics": true, "version": true, "s3": true, "console": true,
	"www": true, "logout": true, "reset": true, "verify": true,
}

// GenerateSlug creates a URL-safe slug from a company name.
// Deterministic: same input always produces same base slug.
// Does NOT check uniqueness — call EnsureSlugUnique for that.
func GenerateSlug(company string) string {
	s := strings.ToLower(company)

	// Replace non-alphanumeric with hyphens.
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s = b.String()

	// Collapse consecutive hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}

	// Trim leading/trailing hyphens.
	s = strings.Trim(s, "-")

	if s == "" {
		return "tenant"
	}

	if len(s) < 2 {
		s += "-storage"
	}

	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}

	if IsReservedSlug(s) {
		s += "-1"
	}

	if !slugRe.MatchString(s) {
		s = "x" + s
		if len(s) > 63 {
			s = s[:63]
			s = strings.TrimRight(s, "-")
		}
	}

	return s
}

// IsReservedSlug returns true if the slug conflicts with a route path.
func IsReservedSlug(slug string) bool {
	return reservedSlugs[slug]
}

// ValidateSlug returns true if slug matches the required pattern.
func ValidateSlug(slug string) bool {
	return slugRe.MatchString(slug)
}

// EnsureSlugUnique checks the tenants table and appends -N suffix
// if the base slug is taken or reserved. Returns the unique slug.
func EnsureSlugUnique(ctx context.Context, db *sql.DB, baseSlug string) (string, error) {
	if IsReservedSlug(baseSlug) {
		baseSlug = baseSlug + "-1"
	}

	var exists string
	err := db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE slug = $1`, baseSlug).Scan(&exists)
	if err == sql.ErrNoRows {
		return baseSlug, nil
	}
	if err != nil {
		return "", fmt.Errorf("check slug uniqueness: %w", err)
	}

	for i := 1; i <= 100; i++ {
		candidate := fmt.Sprintf("%s-%d", baseSlug, i)
		err := db.QueryRowContext(ctx, `SELECT slug FROM tenants WHERE slug = $1`, candidate).Scan(&exists)
		if err == sql.ErrNoRows {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("check slug uniqueness: %w", err)
		}
	}

	return "", fmt.Errorf("could not find unique slug after 100 attempts for base %q", baseSlug)
}

// EnsureTenantSlug generates and sets a slug for a tenant if one
// doesn't exist yet. No-op if slug is already set.
func EnsureTenantSlug(ctx context.Context, db *sql.DB, tenantID string, logger *zap.Logger) {
	var slug sql.NullString
	var name string
	err := db.QueryRowContext(ctx, `SELECT slug, name FROM tenants WHERE id = $1`, tenantID).Scan(&slug, &name)
	if err != nil {
		logger.Error("ensure tenant slug: query tenant", zap.Error(err), zap.String("tenant_id", tenantID))
		return
	}

	if slug.Valid {
		return
	}

	base := GenerateSlug(name)
	unique, err := EnsureSlugUnique(ctx, db, base)
	if err != nil {
		logger.Error("ensure tenant slug: uniqueness check", zap.Error(err))
		return
	}

	_, err = db.ExecContext(ctx, `UPDATE tenants SET slug = $1 WHERE id = $2 AND slug IS NULL`, unique, tenantID)
	if err != nil {
		logger.Error("ensure tenant slug: update", zap.Error(err))
	}
}

// CanEnablePublicRead checks whether a tenant's plan tier allows
// public-read bucket visibility. Archive/tape tiers are too slow
// for public users.
func CanEnablePublicRead(tier string) (allowed bool, reason string) {
	archiveTiers := map[string]bool{
		"archive":      true,
		"vault18":      true,
		"geyser":       true,
		"deep-archive": true,
	}

	if tier == "" {
		return true, ""
	}

	if archiveTiers[tier] {
		return false, "Public access isn't available on archive-tier storage. Upgrade to a standard or performance tier to enable public buckets."
	}

	return true, ""
}
