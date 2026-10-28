package api

// The public catalogue at the index (WP-EDGE-4 PR D, Stage 1a Prompt 48;
// plan 10.3). A host marks a chunk `public` when its copy of the catalogue
// filter holds the chunk's plain hash; here the claim is checked against
// public_chunks (the operator's seeded artefacts) and the ready `_public`
// copy. Verified, the chunk is a hit on that ONE copy (never claimed, never
// uploaded, never sealed — public content); not verified (the filter's
// false positive), it is an ordinary chunk of the tenant's scope. The
// commit may reference `_public` only for a hash this admit's index
// answered so (edgeAdmitAttrs.CodecPublic): a host cannot point a
// tenant's manifest at a public chunk the index never confirmed.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// verifyPublicChunks clears the public mark of every item the catalogue
// does not hold (one query per call).
func (s *Server) verifyPublicChunks(ctx context.Context, items []edgeIndexItem) error {
	var asked []string
	for _, it := range items {
		if it.public {
			asked = append(asked, it.hash)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT c.plain_hash FROM public_chunks c
		JOIN global_content_index g ON g.dedup_scope = $1 AND g.plaintext_hash = c.plain_hash AND g.state = 'ready' AND NOT g.encrypted
		WHERE c.plain_hash = ANY($2)`, crypto.PublicDedupScope, pq.Array(asked))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	held := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return err
		}
		held[h] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		if items[i].public && !held[items[i].hash] {
			items[i].public = false
			edgeCodecChunks.WithLabelValues("public_miss").Inc()
		}
	}
	return nil
}

// referencePublicChunk takes one reference on the `_public` copy of a
// plain hash and answers its size; a copy that is gone is the host's
// retry (errEdgeChunkVanished: its next index call finds no public copy
// and the chunk uploads).
func (s *Server) referencePublicChunk(ctx context.Context, plainHash string) (int64, error) {
	var size int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE global_content_index
		   SET ref_count = ref_count + 1, last_accessed_at = NOW(), marked_for_deletion = FALSE, marked_at = NULL
		 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'ready' AND NOT encrypted
		RETURNING size_bytes`, crypto.PublicDedupScope, plainHash).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errEdgeChunkVanished
	}
	if err != nil {
		return 0, fmt.Errorf("reference public chunk: %w", err)
	}
	s.gci.InvalidateCache(crypto.PublicDedupScope, plainHash)
	return size, nil
}

// scope is the dedup scope a settled chunk's reference lives in.
func (cr edgeCodecRef) scope(tenantID string) string {
	if cr.Public {
		return crypto.PublicDedupScope
	}
	return tenantID
}
