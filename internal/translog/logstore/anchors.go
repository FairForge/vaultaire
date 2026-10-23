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
	// AnchorResubmitted (migration 107, RP48-2): the fresh pending proof of a
	// re-submission — a pending row past the horizon whose calendar may have
	// lost the commitment is re-submitted ONCE (the unique index admits one
	// such row per checkpoint and calendar). Asked to complete like a pending
	// row; also the outcome counted for the re-submission call.
	AnchorResubmitted = "resubmitted"
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

// AnchorCursor is where the upgrade pass stopped: the key of the last row
// asked (RP48-2, the vault_parity reconcile pattern). The zero value is the
// start.
type AnchorCursor struct {
	ReceivedAt time.Time `json:"received_at"`
	Size       int64     `json:"size"`
	State      string    `json:"state"`
}

const pendingAnchorsQuery = `SELECT checkpoint_size, kind, service, state, proof, digest, received_at FROM translog_anchors p
		WHERE p.kind = $1 AND p.service = $2 AND p.state IN ('pending', 'resubmitted') AND p.received_at < $3
		  AND NOT EXISTS (SELECT 1 FROM translog_anchors c WHERE c.checkpoint_size = p.checkpoint_size AND c.kind = p.kind
		                  AND c.service = p.service AND c.state = 'complete')`

// PendingAnchorsFrom are the pending and resubmitted rows of (kind, service)
// with no complete row beside them, received before olderThan — starting
// after the cursor in (received_at, checkpoint_size, state) order and
// wrapping to the start when fewer than limit remain, so every waiting row
// is asked within a bounded number of runs whatever the others do (RP48-2:
// sixteen rows a calendar lost used to starve every newer one for ever).
// Each row is returned at most once; the second value is the next cursor.
func (s *Store) PendingAnchorsFrom(ctx context.Context, kind, service string, olderThan time.Time, after AnchorCursor, limit int) ([]Anchor, AnchorCursor, error) {
	out, err := s.anchors(ctx, pendingAnchorsQuery+`
		  AND (p.received_at, p.checkpoint_size, p.state) > ($4, $5, $6)
		ORDER BY p.received_at, p.checkpoint_size, p.state LIMIT $7`,
		kind, service, olderThan, after.ReceivedAt, after.Size, after.State, limit)
	if err != nil {
		return nil, after, err
	}
	if len(out) < limit {
		key := func(a Anchor) string {
			return fmt.Sprintf("%d|%d|%s", a.ReceivedAt.UnixNano(), a.Size, a.State)
		}
		seen := map[string]bool{}
		for _, a := range out {
			seen[key(a)] = true
		}
		head, err := s.anchors(ctx, pendingAnchorsQuery+`
		ORDER BY p.received_at, p.checkpoint_size, p.state LIMIT $4`, kind, service, olderThan, limit-len(out))
		if err != nil {
			return nil, after, err
		}
		for _, a := range head {
			if !seen[key(a)] {
				out = append(out, a)
			}
		}
	}
	next := after
	if len(out) > 0 {
		last := out[len(out)-1]
		next = AnchorCursor{ReceivedAt: last.ReceivedAt, Size: last.Size, State: last.State}
	}
	return out, next, nil
}

// ResubmittableAnchors are the pending rows of (kind, service) received
// before the horizon with no complete row and no re-submission beside them
// (RP48-2): candidates for the one re-submission, oldest first.
func (s *Store) ResubmittableAnchors(ctx context.Context, kind, service string, olderThan time.Time, limit int) ([]Anchor, error) {
	return s.anchors(ctx, `SELECT checkpoint_size, kind, service, state, proof, digest, received_at FROM translog_anchors p
		WHERE p.kind = $1 AND p.service = $2 AND p.state = 'pending' AND p.received_at < $3
		  AND NOT EXISTS (SELECT 1 FROM translog_anchors c WHERE c.checkpoint_size = p.checkpoint_size AND c.kind = p.kind
		                  AND c.service = p.service AND c.state IN ('complete', 'resubmitted'))
		ORDER BY p.received_at, p.checkpoint_size LIMIT $4`, kind, service, olderThan, limit)
}

// Anchors are every row of one checkpoint and kind, in the order received —
// the read side serves them all combined (RP48-3: a wrong complete answer
// must not replace the same calendar's good pending path; `ots verify`
// judges each path).
func (s *Store) Anchors(ctx context.Context, size int64, kind string) ([]Anchor, error) {
	return s.anchors(ctx, `SELECT checkpoint_size, kind, service, state, proof, digest, received_at
		FROM translog_anchors WHERE checkpoint_size = $1 AND kind = $2
		ORDER BY received_at, service, state`, size, kind)
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
// row's proof of the note's digest — pending, resubmitted and complete —
// combined (RP48-3).
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
