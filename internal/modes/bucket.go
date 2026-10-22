package modes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The buckets columns a mode follows from (migration 102 derives
// buckets.mode from them). NAME TRAP: visibility 'private' means "not
// public-read" — Standard mode, not Private.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public-read"
)

var (
	// ErrPublicSSEC: a public bucket cannot require a customer-provided key
	// (096, WP-SSEC-1 B.5) — 409 bucket_public.
	ErrPublicSSEC = errors.New("bucket_public")
	// ErrNoBucket: the tenant has no such bucket.
	ErrNoBucket = errors.New("no_such_bucket")
	// ErrBadVisibility: a visibility other than private / public-read.
	ErrBadVisibility = errors.New("invalid_visibility")
)

// FromColumns is migration 102's CASE in Go: public-read → public,
// ssec_required → private, else standard.
func FromColumns(visibility string, ssecRequired bool) string {
	switch {
	case visibility == VisibilityPublic:
		return Public
	case ssecRequired:
		return Private
	}
	return Standard
}

// Change is a mode change: the columns to set (nil = keep).
type Change struct {
	Visibility   *string
	SSECRequired *bool
}

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SetBucket is THE door through which a bucket's mode changes (K3): the
// only writer of buckets.visibility and buckets.ssec_required after the
// bucket's creation (which always writes Standard). One statement writes
// the columns and enforces the 096 rule against the row's values after the
// change — a public bucket never requires a key — and returns the mode the
// row now has (the generated column). ErrNoBucket, ErrPublicSSEC,
// ErrBadVisibility; nothing written on a refusal.
func SetBucket(ctx context.Context, q Querier, tenantID, bucket string, c Change) (string, error) {
	if c.Visibility != nil && *c.Visibility != VisibilityPrivate && *c.Visibility != VisibilityPublic {
		return "", fmt.Errorf("bucket %s: visibility %q: %w", bucket, *c.Visibility, ErrBadVisibility)
	}
	var vis sql.NullString
	if c.Visibility != nil {
		vis = sql.NullString{String: *c.Visibility, Valid: true}
	}
	var ssec sql.NullBool
	if c.SSECRequired != nil {
		ssec = sql.NullBool{Bool: *c.SSECRequired, Valid: true}
	}
	var mode string
	err := q.QueryRowContext(ctx, `
		UPDATE buckets
		   SET visibility = COALESCE($3::text, visibility), ssec_required = COALESCE($4::boolean, ssec_required), updated_at = NOW()
		 WHERE tenant_id = $1 AND name = $2
		   AND NOT (COALESCE($3::text, visibility) = 'public-read' AND COALESCE($4::boolean, ssec_required))
		RETURNING mode`, tenantID, bucket, vis, ssec).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
			tenantID, bucket).Scan(&exists); err != nil {
			return "", fmt.Errorf("bucket %s: %w", bucket, err)
		}
		if exists {
			return "", fmt.Errorf("bucket %s: %w", bucket, ErrPublicSSEC)
		}
		return "", fmt.Errorf("bucket %s: %w", bucket, ErrNoBucket)
	}
	if err != nil {
		return "", fmt.Errorf("set the mode of bucket %s: %w", bucket, err)
	}
	return mode, nil
}
