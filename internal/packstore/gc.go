package packstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"go.uber.org/zap"
)

// GCResult is one GC run's outcome (job_runs.result of pack_gc).
type GCResult struct {
	Backend string `json:"backend"`
	// RetiredFinished: packs a previous run retired whose file or row was left.
	RetiredFinished int `json:"retired_finished"`
	// IntentsExpired: uploads whose commit never happened within IntentGrace.
	IntentsExpired int `json:"intents_expired"`
	// OrphansDeleted: intents expired plus pack files no row names.
	OrphansDeleted int `json:"orphans_deleted"`
	// PacksRewritten: packs whose live members were moved into a new pack.
	PacksRewritten int `json:"packs_rewritten"`
	MembersMoved   int `json:"members_moved"`
	// PacksDeleted: committed packs deleted once nothing live was left in them.
	PacksDeleted int      `json:"packs_deleted"`
	BytesFreed   int64    `json:"bytes_freed"`
	Errors       []string `json:"errors,omitempty"`
}

func (r *GCResult) fail(format string, args ...any) {
	if len(r.Errors) < 20 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// GC is one idempotent pass over the store's backend:
//
//  1. retired packs left by an earlier run: delete the file, then the row;
//  2. expired intents (sealed_at NULL, created_at older than IntentGrace):
//     retire, delete the file, delete the row;
//  3. orphan files: a valid pack name on the backend that no row names;
//  4. rewrite: a sealed pack with nothing live, live bytes under
//     LiveThreshold of its size, fewer member rows than its member_count (the
//     account erasure deleted some), or a tombstone older than
//     TombstoneMaxAge — its live members go into new packs, then it is retired
//     and deleted.
//
// Every age is a Postgres timestamp (packs.created_at, pack_members.deleted_at
// against NOW()). The backend's modification time is never read: Sync's
// bridge reports 1970 for every file (2026-10-07).
//
// A run returns an error only when the index cannot be read; one pack that
// could not be handled is in Errors and is the next run's work.
func (s *Store) GC(ctx context.Context) (GCResult, error) {
	res := GCResult{Backend: s.name}
	if err := s.finishRetired(ctx, &res); err != nil {
		return res, err
	}
	if err := s.expireIntents(ctx, &res); err != nil {
		return res, err
	}
	if err := s.deleteOrphans(ctx, &res); err != nil {
		return res, err
	}
	if err := s.compact(ctx, &res); err != nil {
		return res, err
	}
	return res, nil
}

type packRef struct {
	id   int64
	name string
	size int64
}

func (s *Store) packRefs(ctx context.Context, q string, args ...any) ([]packRef, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list packs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []packRef
	for rows.Next() {
		var p packRef
		if err := rows.Scan(&p.id, &p.name, &p.size); err != nil {
			return nil, fmt.Errorf("list packs: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list packs: %w", err)
	}
	return out, nil
}

// removeRetired deletes a retired pack's file, then its row (members cascade).
func (s *Store) removeRetired(ctx context.Context, p packRef) error {
	if err := s.backend.Delete(bctx(ctx), s.o.Container, p.name); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete pack %s on %s: %w", p.name, s.name, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM packs WHERE id = $1 AND retired_at IS NOT NULL`, p.id); err != nil {
		return fmt.Errorf("delete pack row %s: %w", p.name, err)
	}
	return nil
}

func (s *Store) finishRetired(ctx context.Context, res *GCResult) error {
	refs, err := s.packRefs(ctx, `SELECT id, name, size FROM packs WHERE backend = $1 AND retired_at IS NOT NULL ORDER BY id`, s.name)
	if err != nil {
		return err
	}
	for _, p := range refs {
		if err := s.removeRetired(ctx, p); err != nil {
			res.fail("%v", err)
			continue
		}
		res.RetiredFinished++
	}
	return nil
}

func (s *Store) expireIntents(ctx context.Context, res *GCResult) error {
	refs, err := s.packRefs(ctx, `
		SELECT id, name, size FROM packs
		 WHERE backend = $1 AND sealed_at IS NULL AND retired_at IS NULL
		   AND created_at < NOW() - make_interval(secs => $2::float8)
		 ORDER BY id`, s.name, s.o.IntentGrace.Seconds())
	if err != nil {
		return err
	}
	for _, p := range refs {
		r, err := s.db.ExecContext(ctx, `UPDATE packs SET retired_at = NOW() WHERE id = $1 AND sealed_at IS NULL AND retired_at IS NULL`, p.id)
		if err != nil {
			res.fail("retire intent %s: %v", p.name, err)
			continue
		}
		if n, _ := r.RowsAffected(); n != 1 {
			continue // committed meanwhile
		}
		if err := s.removeRetired(ctx, p); err != nil {
			res.fail("%v", err)
			continue
		}
		res.IntentsExpired++
		orphansDeleted.WithLabelValues(s.name).Inc()
	}
	return nil
}

// deleteOrphans deletes pack files no row names. A writer inserts its row
// BEFORE it uploads, and the listing is taken before the rows are read, so a
// listed file of an upload in flight always has its row: no age is needed.
func (s *Store) deleteOrphans(ctx context.Context, res *GCResult) error {
	names, err := s.backend.List(bctx(ctx), s.o.Container, "")
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		res.fail("list %s on %s: %v", s.o.Container, s.name, err)
		return nil
	}
	known := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM packs WHERE backend = $1`, s.name)
	if err != nil {
		return fmt.Errorf("list pack names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("list pack names: %w", err)
		}
		known[n] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list pack names: %w", err)
	}
	for _, n := range names {
		if known[n] || !validPackName(n) {
			continue
		}
		if err := s.backend.Delete(bctx(ctx), s.o.Container, n); err != nil && !isNotFound(err) {
			res.fail("delete orphan %s: %v", n, err)
			continue
		}
		res.OrphansDeleted++
		orphansDeleted.WithLabelValues(s.name).Inc()
	}
	return nil
}

// liveMember is a member a rewrite moves.
type liveMember struct {
	id               int64
	tenant, key, sha string
	offset, length   int64
}

func (s *Store) compact(ctx context.Context, res *GCResult) error {
	cands, err := s.packRefs(ctx, `
		SELECT p.id, p.name, p.size
		  FROM packs p LEFT JOIN pack_members m ON m.pack_id = p.id
		 WHERE p.backend = $1 AND p.sealed_at IS NOT NULL AND p.retired_at IS NULL
		 GROUP BY p.id
		HAVING COUNT(m.id) FILTER (WHERE m.deleted_at IS NULL) = 0
		    OR COALESCE(SUM(m.byte_length) FILTER (WHERE m.deleted_at IS NULL), 0) < $2::float8 * p.size
		    OR COUNT(DISTINCT m.byte_offset) < p.member_count
		    OR MIN(m.deleted_at) < NOW() - make_interval(secs => $3::float8)
		 ORDER BY p.id`, s.name, s.o.LiveThreshold, s.o.TombstoneMaxAge.Seconds())
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return nil
	}

	var w *Writer
	defer func() {
		if w != nil {
			_ = w.Close(ctx)
		}
	}()
	var done []packRef
	var budget int64
	for _, p := range cands {
		if budget >= s.o.MaxRewriteBytes {
			break
		}
		members, err := s.liveMembers(ctx, p.id)
		if err != nil {
			return err
		}
		if len(members) > 0 {
			if w == nil {
				if w, err = s.NewWriter(); err != nil {
					return err
				}
			}
			moved, err := s.rewriteInto(ctx, w, p, members)
			res.MembersMoved += moved
			if err != nil {
				res.fail("rewrite pack %s: %v", p.name, err)
				continue
			}
			res.PacksRewritten++
			gcRewrites.WithLabelValues(s.name).Inc()
		}
		budget += p.size
		done = append(done, p)
	}
	if w != nil {
		if _, err := w.Flush(ctx); err != nil {
			res.fail("seal rewritten pack: %v", err)
			return nil // nothing moved: the old packs keep their live rows
		}
	}
	for _, p := range done {
		retired, err := s.retireIfEmpty(ctx, p.id)
		if err != nil {
			res.fail("retire pack %s: %v", p.name, err)
			continue
		}
		if !retired {
			continue
		}
		if err := s.removeRetired(ctx, p); err != nil {
			res.fail("%v", err)
			continue
		}
		res.PacksDeleted++
		res.BytesFreed += p.size
		gcPacksDeleted.WithLabelValues(s.name).Inc()
	}
	return nil
}

func (s *Store) liveMembers(ctx context.Context, packID int64) ([]liveMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, member_key, sha256, byte_offset, byte_length
		  FROM pack_members WHERE pack_id = $1 AND deleted_at IS NULL ORDER BY byte_offset`, packID)
	if err != nil {
		return nil, fmt.Errorf("live members of pack %d: %w", packID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []liveMember
	for rows.Next() {
		var m liveMember
		if err := rows.Scan(&m.id, &m.tenant, &m.key, &m.sha, &m.offset, &m.length); err != nil {
			return nil, fmt.Errorf("live members of pack %d: %w", packID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("live members of pack %d: %w", packID, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].offset < out[j].offset })
	return out, nil
}

// rewriteInto streams the old pack once and adds its live members to w (the
// stream is read in offset order; dead bytes are skipped). Each member's
// sha256 is checked against its row as it is copied.
func (s *Store) rewriteInto(ctx context.Context, w *Writer, p packRef, members []liveMember) (int, error) {
	rc, err := s.backend.Get(bctx(ctx), s.o.Container, p.name)
	if err != nil {
		return 0, fmt.Errorf("get pack %s on %s: %w", p.name, s.name, err)
	}
	defer func() { _ = rc.Close() }()
	pos := int64(0)
	moved := 0
	for _, m := range members {
		if _, err := io.CopyN(io.Discard, rc, m.offset-pos); err != nil {
			return moved, fmt.Errorf("skip to %d: %w", m.offset, err)
		}
		pos = m.offset
		if _, err := w.add(ctx, m.tenant, m.key, m.length, io.LimitReader(rc, m.length), m.id, m.sha); err != nil {
			if errors.Is(err, ErrCorrupt) {
				memberCorrupt.WithLabelValues(s.name).Inc()
				s.logger.Error("packstore: corrupt member left in place", zap.String("pack", p.name),
					zap.String("tenant", m.tenant), zap.String("key", m.key))
			}
			return moved, err
		}
		pos += m.length
		moved++
	}
	return moved, nil
}

// retireIfEmpty retires a pack with no live member left.
func (s *Store) retireIfEmpty(ctx context.Context, packID int64) (bool, error) {
	r, err := s.db.ExecContext(ctx, `
		UPDATE packs p SET retired_at = NOW()
		 WHERE p.id = $1 AND p.retired_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM pack_members m WHERE m.pack_id = p.id AND m.deleted_at IS NULL)`, packID)
	if err != nil {
		return false, fmt.Errorf("retire pack %d: %w", packID, err)
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}
