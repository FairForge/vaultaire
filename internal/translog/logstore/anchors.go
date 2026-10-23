package logstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/ots"
)

// The checkpoints' anchors (K4c-3a, migration 106, translog_anchors): what
// an anchoring service answered for a signed checkpoint — today an
// OpenTimestamps calendar's proof, pending until its aggregate is in a
// Bitcoin block, then complete. A row is never rewritten: an upgrade is a
// new row (state complete) beside the pending one.

// The kinds and states of migration 106.
const (
	AnchorKindOTS   = "ots"
	AnchorKindRekor = "rekor" // allowed by the table, not built (D-P35-2)

	AnchorPending  = "pending"
	AnchorComplete = "complete"
)

// Anchor is one row of translog_anchors.
type Anchor struct {
	Size    int64
	Kind    string
	Service string
	State   string
	Proof   []byte
	// Digest is SHA-256 of the checkpoint note as the log signed it.
	Digest     []byte
	ReceivedAt time.Time
}

// AddAnchor keeps one answer. The same (checkpoint, kind, service, state)
// twice is kept once.
func (s *Store) AddAnchor(ctx context.Context, a Anchor) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO translog_anchors (checkpoint_size, kind, service, state, proof, digest)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (checkpoint_size, kind, service, state) DO NOTHING`,
		a.Size, a.Kind, a.Service, a.State, a.Proof, a.Digest)
	if err != nil {
		return fmt.Errorf("translog: keep the %s anchor of %d from %s: %w", a.Kind, a.Size, a.Service, err)
	}
	return nil
}

// Unanchored are the signed checkpoints no row of (kind, service) names,
// newest first, at most limit.
func (s *Store) Unanchored(ctx context.Context, kind, service string, limit int) ([]Stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.size, c.root, c.note, c.signed_at FROM translog_checkpoints c
		WHERE NOT EXISTS (SELECT 1 FROM translog_anchors a WHERE a.checkpoint_size = c.size AND a.kind = $1 AND a.service = $2)
		ORDER BY c.size DESC LIMIT $3`, kind, service, limit)
	if err != nil {
		return nil, fmt.Errorf("translog: the checkpoints not anchored at %s: %w", service, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Stored
	for rows.Next() {
		var c Stored
		var root []byte
		if err := rows.Scan(&c.Size, &root, &c.Note, &c.SignedAt); err != nil {
			return nil, fmt.Errorf("translog: the checkpoints not anchored at %s: %w", service, err)
		}
		copy(c.Root[:], root)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("translog: the checkpoints not anchored at %s: %w", service, err)
	}
	return out, nil
}

// PendingAnchors are the pending rows of (kind, service) with no complete
// row beside them, received before olderThan, oldest first, at most limit.
func (s *Store) PendingAnchors(ctx context.Context, kind, service string, olderThan time.Time, limit int) ([]Anchor, error) {
	return s.anchors(ctx, `SELECT checkpoint_size, kind, service, state, proof, digest, received_at FROM translog_anchors p
		WHERE p.kind = $1 AND p.service = $2 AND p.state = 'pending' AND p.received_at < $3
		  AND NOT EXISTS (SELECT 1 FROM translog_anchors c WHERE c.checkpoint_size = p.checkpoint_size AND c.kind = p.kind
		                  AND c.service = p.service AND c.state = 'complete')
		ORDER BY p.received_at, p.checkpoint_size LIMIT $4`, kind, service, olderThan, limit)
}

// Anchors are the rows of one checkpoint and kind the read side serves: per
// service the complete one, else the pending one.
func (s *Store) Anchors(ctx context.Context, size int64, kind string) ([]Anchor, error) {
	return s.anchors(ctx, `SELECT DISTINCT ON (service) checkpoint_size, kind, service, state, proof, digest, received_at
		FROM translog_anchors WHERE checkpoint_size = $1 AND kind = $2
		ORDER BY service, (state = 'complete') DESC, received_at DESC`, size, kind)
}

// AllAnchors are every row of one checkpoint, in the order received.
func (s *Store) AllAnchors(ctx context.Context, size int64) ([]Anchor, error) {
	return s.anchors(ctx, `SELECT checkpoint_size, kind, service, state, proof, digest, received_at
		FROM translog_anchors WHERE checkpoint_size = $1 ORDER BY received_at, kind, service, state`, size)
}

func (s *Store) anchors(ctx context.Context, q string, args ...any) ([]Anchor, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("translog: anchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Anchor
	for rows.Next() {
		var a Anchor
		if err := rows.Scan(&a.Size, &a.Kind, &a.Service, &a.State, &a.Proof, &a.Digest, &a.ReceivedAt); err != nil {
			return nil, fmt.Errorf("translog: anchors: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("translog: anchors: %w", err)
	}
	return out, nil
}

// AnchorOTS is the .ots file the read side serves for a checkpoint: every
// calendar's proof (complete, else pending) of the note's digest, combined.
// translog.ErrNoAnchor when there is none — or when a row's digest is not the
// note's (never served).
func (s *Store) AnchorOTS(ctx context.Context, size int64) ([]byte, error) {
	var note string
	err := s.db.QueryRowContext(ctx, `SELECT note FROM translog_checkpoints WHERE size = $1`, size).Scan(&note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, translog.ErrNoAnchor
	}
	if err != nil {
		return nil, fmt.Errorf("translog: the checkpoint of %d: %w", size, err)
	}
	rows, err := s.Anchors(ctx, size, AnchorKindOTS)
	if err != nil {
		return nil, err
	}
	want := NoteDigest(note)
	var proofs [][]byte
	for _, r := range rows {
		if bytes.Equal(r.Digest, want[:]) {
			proofs = append(proofs, r.Proof)
		}
	}
	if len(proofs) == 0 {
		return nil, translog.ErrNoAnchor
	}
	out, err := ots.Combine(proofs...)
	if err != nil {
		return nil, fmt.Errorf("translog: the anchors of %d: %w", size, err)
	}
	return out, nil
}

// NoteDigest is what is anchored: SHA-256 of the checkpoint note exactly as
// the log signed it (the body and the log's signature line, no cosignature
// lines — translog_checkpoints.note).
func NoteDigest(note string) [32]byte { return sha256.Sum256([]byte(note)) }
