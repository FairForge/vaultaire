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
	"regexp"
	"strings"

	"go.uber.org/zap"
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

// mirrorLegFor is the leg a blob placed on placement is mirrored to: the
// first leg of MIRROR_LEGS that is a registered EdgeWriter, is not the
// placement's backend, and whose breaker is closed — "" when none is (no
// mirror this write; never a refusal).
func (s *Server) mirrorLegFor(placement string) string {
	for _, leg := range s.mirrorLegs {
		if leg == placement {
			continue
		}
		if _, err := s.edgeWriterFor(leg); err == nil {
			return leg
		}
	}
	return ""
}

// mirrorFor is the leg of a tenant's write placed on placement, "" when
// the tenant is not mirrored or no leg is available.
func (s *Server) mirrorFor(tenantID, placement string) string {
	if s.flags == nil || !s.flags.Enabled(flagEdgeMirror, tenantID) {
		return ""
	}
	return s.mirrorLegFor(placement)
}

// blobCopy is one blob_copies row.
type blobCopy struct {
	TenantID, Bucket, Key, StorageKey string
	Source, Leg                       string
	State, ETag                       string
	Size                              int64
}

// The copy states (113).
const (
	copyPending = "pending"
	copyReady   = "ready"
	copyStale   = "stale"
)

// mirrorCopyState is the state a commit records for a mirror whose leg
// answered mirrorETag for a blob whose primary MD5 is primaryMD5: ready
// only when both are the same MD5 (the same bytes), pending otherwise —
// the job writes it.
func mirrorCopyState(primaryMD5, mirrorETag string) (string, string) {
	m := strings.ToLower(strings.Trim(strings.TrimSpace(mirrorETag), `"`))
	if m != "" && m == strings.ToLower(primaryMD5) {
		return copyReady, m
	}
	return copyPending, ""
}

// recordBlobCopy writes (or re-asserts) a copy row: a ready row is never
// downgraded by a later pending write of the same blob (a retried commit).
func recordBlobCopy(ctx context.Context, q execer, c blobCopy) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, state, etag, size)
		VALUES ($1, $2, $3, $4, $5, $6, 'mirror', $7, $8, $9)
		ON CONFLICT (tenant_id, storage_key, leg, shard_index) DO UPDATE SET
			state = CASE WHEN blob_copies.state = 'ready' AND EXCLUDED.state = 'pending' THEN 'ready' ELSE EXCLUDED.state END,
			etag = CASE WHEN EXCLUDED.state = 'ready' THEN EXCLUDED.etag ELSE blob_copies.etag END,
			size = EXCLUDED.size, source_backend = EXCLUDED.source_backend, updated_at = NOW()`,
		c.TenantID, c.Bucket, c.Key, c.StorageKey, c.Source, c.Leg, c.State, c.ETag, c.Size)
	return err
}
