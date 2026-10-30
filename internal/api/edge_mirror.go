package api

// WP-EDGE-4 Part A (Stage 1a Prompt 48, plan 40.10 slice 3): the second
// copy at the edge. A tenant with edge_mirror ON gets every blob it writes
// at the edge on two backends — the placement's and a mirror leg (the
// first registered EdgeWriter of MIRROR_LEGS that is not the placement's
// and whose breaker is closed) — at the SAME address on both, recorded in
// blob_copies (migration 113). What decides the leg is the admit, once: it
// is recorded on the admit row (attrs.mirror_leg) and the index and the
// commit read it from there, so a breaker that opens between the admit and
// the commit never makes a row name a leg the bytes are not on.
//
// The copy is never the client's failure: a mirror PUT that failed (or a
// mirror ETag that is not the primary's) commits the object on its one
// backend and leaves the copy `pending` for the blob_copies job, which
// writes it from the primary within ten minutes (blob_copies_job.go).

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// defaultMirrorLegs is MIRROR_LEGS' default: the two fast legs.
var defaultMirrorLegs = []string{"lyve", "idrive"}

var mirrorLegName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// mirrorLegsFromEnv reads MIRROR_LEGS (comma-separated driver names, in
// preference order): trimmed and deduplicated; a list with a name that is
// no driver name is a Warn and the default (unregistered names are skipped
// at use, as VAULT_PARITY_LEGS').
func mirrorLegsFromEnv(getenv func(string) string, logger *zap.Logger) []string {
	raw := strings.TrimSpace(getenv("MIRROR_LEGS"))
	if raw == "" {
		return append([]string(nil), defaultMirrorLegs...)
	}
	var legs []string
	seen := map[string]bool{}
	for _, n := range strings.Split(raw, ",") {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		if !mirrorLegName.MatchString(n) {
			if logger != nil {
				logger.Warn("invalid MIRROR_LEGS (comma-separated driver names), keeping the default",
					zap.String("value", raw), zap.Strings("default", defaultMirrorLegs))
			}
			return append([]string(nil), defaultMirrorLegs...)
		}
		seen[n] = true
		legs = append(legs, n)
	}
	if len(legs) == 0 {
		return append([]string(nil), defaultMirrorLegs...)
	}
	return legs
}

// bucketScope is what of a bucket its mirror leg must satisfy (RP76-1):
// the region it is pinned to ("" or the default region = unpinned) and its
// tier preference — and its tenant's residency (WP-ACCT-1, residency.go:
// "" = anywhere).
type bucketScope struct {
	Region, Tier string
	Residency    string
}

// pinned reports whether the bucket names a region other than the default
// (whose buckets the primary serves).
func (b bucketScope) pinned() bool {
	return b.Region != "" && b.Region != drivers.IDriveDefaultRegion(os.Getenv)
}

// bucketScopeOf reads a bucket's scope; a bucket that cannot be read is
// unpinned with no tier (the placement read the same row before).
func (s *Server) bucketScopeOf(ctx context.Context, tenantID, bucket string) bucketScope {
	var b bucketScope
	if s.db == nil {
		return b
	}
	var region, tier, residency sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT b.region, b.tier_preference, t.residency
		  FROM buckets b LEFT JOIN tenants t ON t.id = b.tenant_id
		 WHERE b.tenant_id = $1 AND b.name = $2`,
		tenantID, bucket).Scan(&region, &tier, &residency); err == nil {
		b.Region, b.Tier, b.Residency = region.String, tier.String, residency.String
	}
	return b
}

// allows reports whether a leg storing in region may hold the bucket's
// copy: a leg of the bucket's jurisdiction (an unpinned bucket's is the
// default region's — a US bucket is never mirrored to the EU), or, for an
// unpinned bucket only, a leg of no known region. A tenant's residency
// admits only legs of its own jurisdiction: a leg of no known region is
// outside every residency.
func (b bucketScope) allows(region string) bool {
	if b.Residency != "" && (region == "" || jurisdiction(region) != b.Residency) {
		return false
	}
	if region == "" {
		return !b.pinned()
	}
	home := b.Region
	if !b.pinned() {
		home = drivers.IDriveDefaultRegion(os.Getenv)
	}
	return jurisdiction(region) == jurisdiction(home)
}

// jurisdiction is a region's jurisdiction (RP78-4): the region table's
// (eu-west-3 is London, the UK), else the id's first segment (lyve's us).
func jurisdiction(region string) string {
	return drivers.RegionJurisdiction(region)
}

// legRegion is the region a mirror leg stores in: an idrive-<region>
// driver's region, the primary idrive's IDRIVE_REGION, lyve's LYVE_REGION
// (us-west-1 by default); "" when unknown — never a pinned bucket's leg.
func legRegion(leg string) string {
	switch {
	case leg == "idrive":
		return drivers.IDriveDefaultRegion(os.Getenv)
	case strings.HasPrefix(leg, "idrive-"):
		if r := strings.TrimPrefix(leg, "idrive-"); drivers.IsValidRegion(r) {
			return r
		}
	case leg == "lyve":
		if r := strings.TrimSpace(os.Getenv("LYVE_REGION")); r != "" {
			return r
		}
		return "us-west-1"
	}
	return ""
}

// mirrorLegFor is the leg a blob placed on placement in a bucket of scope b
// is mirrored to: the first leg of MIRROR_LEGS that is a registered
// EdgeWriter, is not the placement's backend, and satisfies the bucket — a
// leg of the bucket's jurisdiction (bucketScope.allows; RP76-1: an EU
// bucket with no EU leg is a single copy); "" when none is (no mirror this write;
// never a refusal). A strict-target tier or placement and an rs object
// are never mirrored (RP76-1, RP76-6).
func (s *Server) mirrorLegFor(placement string, b bucketScope) string {
	if placement == rsBackendSentinel || engine.IsStrictTarget(placement) || engine.IsStrictTarget(b.Tier) {
		return ""
	}
	for _, leg := range s.mirrorLegs {
		if leg == placement {
			continue
		}
		if !b.allows(legRegion(leg)) {
			continue
		}
		if _, err := s.edgeWriterFor(leg); err == nil {
			return leg
		}
	}
	return ""
}

// mirrorFor is the leg of a tenant's write into bucket placed on
// placement, "" when the tenant is not mirrored or no leg satisfies the
// bucket. The bucket is read only for a mirrored tenant.
func (s *Server) mirrorFor(ctx context.Context, tenantID, bucket, placement string) string {
	if s.flags == nil || !s.flags.Enabled(flagEdgeMirror, tenantID) {
		return ""
	}
	return s.mirrorLegFor(placement, s.bucketScopeOf(ctx, tenantID, bucket))
}

// blobCopy is one blob_copies row.
type blobCopy struct {
	TenantID, Bucket, Key, StorageKey string
	Source, Leg                       string
	State, ETag                       string
	Size                              int64
	// Kind and Index: a mirror (index 0) or an RS shard (WP-EDGE-4 PR B).
	Kind  string
	Index int
}

// The copy states (113).
const (
	copyPending = "pending"
	copyReady   = "ready"
	copyStale   = "stale"
)

// mirrorCopyState is the state a commit records for a mirror whose leg
// answered mirrorETag for a blob whose primary MD5 is primaryMD5: ready
// only when both are the same MD5 (the same bytes), pending otherwise — the
// job writes it. The row's etag is the primary's MD5 either way: what the
// job's copy is checked against.
func mirrorCopyState(primaryMD5, mirrorETag string) (string, string) {
	p := strings.ToLower(strings.Trim(strings.TrimSpace(primaryMD5), `"`))
	m := strings.ToLower(strings.Trim(strings.TrimSpace(mirrorETag), `"`))
	if m != "" && m == p {
		return copyReady, p
	}
	return copyPending, p
}

// recordBlobCopy writes (or re-asserts) a copy row: a ready row is never
// downgraded by a later pending write of the same blob (a retried commit).
func recordBlobCopy(ctx context.Context, q execer, c blobCopy) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, state, etag, size)
		VALUES ($1, $2, $3, $4, $5, $6, 'mirror', $7, $8, $9)
		ON CONFLICT (tenant_id, storage_key, leg, shard_index) DO UPDATE SET
			state = CASE WHEN blob_copies.state = 'ready' AND EXCLUDED.state = 'pending' THEN 'ready' ELSE EXCLUDED.state END,
			etag = CASE WHEN EXCLUDED.etag <> '' THEN EXCLUDED.etag ELSE blob_copies.etag END,
			size = EXCLUDED.size, source_backend = EXCLUDED.source_backend, updated_at = NOW()`,
		c.TenantID, c.Bucket, c.Key, c.StorageKey, c.Source, c.Leg, c.State, c.ETag, c.Size)
	return err
}
