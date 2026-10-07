package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

// Writer assembles members into a pack in a local staging file and uploads
// each pack ONCE, then records it. It is for batch writers (jobs), not the
// synchronous S3 PUT path, and is not safe for concurrent use — open one
// Writer per goroutine (any number of Writers may run at once).
//
// A member is stored only when the Add or Flush that sealed its pack
// returned that pack without error: before, nothing names it. A crash
// anywhere before the commit loses nothing the caller recorded — the job
// redoes the batch, and GC deletes the uploaded file.
type Writer struct {
	s       *Store
	f       *os.File
	off     int64
	members []stagedMember
	keys    map[[2]string]struct{}
	// intent is the packs row this writer inserted for a seal that has not
	// committed yet (a retried Flush of the same bytes reuses it).
	intent     int64
	intentName string
	closed     bool
}

type stagedMember struct {
	FooterMember
	// src is the pack_members row a GC rewrite moves (0 for a new member).
	src int64
}

// MemberRef names a stored member.
type MemberRef struct {
	Tenant string
	Key    string
}

// SealedPack is a committed pack.
type SealedPack struct {
	ID      int64
	Name    string
	Size    int64
	SHA256  string
	Members int
	// Stored are the members this commit recorded (a GC move whose source
	// was deleted meanwhile is not among them).
	Stored []MemberRef
}

// NewWriter opens a writer with an empty staging file.
func (s *Store) NewWriter() (*Writer, error) {
	f, err := os.CreateTemp(s.o.StagingDir, "stage-*.pack")
	if err != nil {
		return nil, fmt.Errorf("pack staging file: %w", err)
	}
	w := &Writer{s: s, f: f, keys: map[[2]string]struct{}{}}
	if err := w.reset(); err != nil {
		closeRemove(f)
		return nil, err
	}
	return w, nil
}

func (w *Writer) reset() error {
	if err := w.f.Truncate(0); err != nil {
		return fmt.Errorf("reset staging file: %w", err)
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("reset staging file: %w", err)
	}
	if _, err := w.f.WriteString(headerMagic); err != nil {
		return fmt.Errorf("write pack header: %w", err)
	}
	w.off = HeaderSize
	w.members = w.members[:0]
	w.keys = map[[2]string]struct{}{}
	w.intent, w.intentName = 0, ""
	return nil
}

// Add appends one member of exactly size bytes read from r. When the member
// would take the pack past the target size or member count, the pack built so
// far is sealed first and returned (its members are stored); otherwise the
// returned pack is nil. A failed Add leaves the pack as it was before it.
func (w *Writer) Add(ctx context.Context, tenantID, key string, size int64, r io.Reader) (*SealedPack, error) {
	return w.add(ctx, tenantID, key, size, r, 0, "")
}

func (w *Writer) add(ctx context.Context, tenantID, key string, size int64, r io.Reader, src int64, wantSHA string) (*SealedPack, error) {
	if w.closed {
		return nil, ErrWriterClosed
	}
	if tenantID == "" || key == "" || size < 0 {
		return nil, fmt.Errorf("%w: tenant, key and a size are required", engine.ErrInvalidInput)
	}
	if size > w.s.o.TargetSize {
		return nil, fmt.Errorf("%s/%s (%d bytes): %w", tenantID, key, size, ErrMemberTooLarge)
	}
	id := [2]string{tenantID, key}
	if _, dup := w.keys[id]; dup {
		return nil, fmt.Errorf("%s/%s: %w", tenantID, key, ErrDuplicateKey)
	}
	var sealed *SealedPack
	if len(w.members) > 0 && (w.off+size > w.s.o.TargetSize || len(w.members) >= w.s.o.MaxMembers) {
		sp, err := w.seal(ctx)
		if err != nil {
			return nil, err
		}
		sealed = sp
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(w.f, h), r, size)
	if err == nil {
		// one byte more than declared is a mismatch too
		var one [1]byte
		if m, _ := io.ReadFull(r, one[:]); m > 0 {
			err = fmt.Errorf("%s/%s: more than %d bytes: %w", tenantID, key, size, ErrSizeMismatch)
		}
	} else if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%s/%s: %d of %d bytes: %w", tenantID, key, n, size, ErrSizeMismatch)
	}
	if err == nil {
		sum := hex.EncodeToString(h.Sum(nil))
		if wantSHA != "" && sum != wantSHA {
			err = fmt.Errorf("%s/%s: %w", tenantID, key, ErrCorrupt)
		} else {
			w.members = append(w.members, stagedMember{FooterMember{Tenant: tenantID, Key: key, Offset: w.off, Length: size, SHA256: sum}, src})
			w.keys[id] = struct{}{}
			w.off += size
			if w.intent != 0 { // the bytes changed: a held intent is for another name
				w.s.abandonIntent(ctx, w.intent, w.intentName)
				w.intent, w.intentName = 0, ""
			}
			return sealed, nil
		}
	}
	// roll the staging file back to before this member
	if terr := w.truncate(w.off); terr != nil {
		return sealed, fmt.Errorf("%w (and the staging file could not be rolled back: %w)", err, terr)
	}
	return sealed, fmt.Errorf("add member: %w", err)
}

func (w *Writer) truncate(at int64) error {
	if err := w.f.Truncate(at); err != nil {
		return fmt.Errorf("truncate staging file: %w", err)
	}
	if _, err := w.f.Seek(at, io.SeekStart); err != nil {
		return fmt.Errorf("seek staging file: %w", err)
	}
	return nil
}

// Flush seals the pack built so far (nil when it is empty). A failed Flush
// keeps the members: it may be called again.
func (w *Writer) Flush(ctx context.Context) (*SealedPack, error) {
	if w.closed {
		return nil, ErrWriterClosed
	}
	if len(w.members) == 0 {
		return nil, nil
	}
	return w.seal(ctx)
}

// Close discards anything not flushed and removes the staging file. An
// intent this writer left (a failed seal) is cleaned up best effort; GC
// finishes what this cannot.
func (w *Writer) Close(ctx context.Context) error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.intent != 0 {
		w.s.abandonIntent(ctx, w.intent, w.intentName)
	}
	closeRemove(w.f)
	return nil
}

// seal writes the footer, uploads the pack once and commits it.
func (w *Writer) seal(ctx context.Context) (*SealedPack, error) {
	footer := make([]FooterMember, len(w.members))
	for i, m := range w.members {
		footer[i] = m.FooterMember
	}
	tail, err := encodeTail(footer)
	if err != nil {
		return nil, err
	}
	if _, err := w.f.Write(tail); err != nil {
		_ = w.truncate(w.off)
		return nil, fmt.Errorf("write pack footer: %w", err)
	}
	sp, err := w.upload(ctx, tail)
	if err != nil {
		if terr := w.truncate(w.off); terr != nil {
			return nil, fmt.Errorf("%w (and the staging file could not be rolled back: %w)", err, terr)
		}
		return nil, err
	}
	if err := w.reset(); err != nil {
		return sp, err
	}
	return sp, nil
}

func (w *Writer) upload(ctx context.Context, tail []byte) (*SealedPack, error) {
	s := w.s
	size := w.off + int64(len(tail))
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind staging file: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, w.f); err != nil {
		return nil, fmt.Errorf("hash staging file: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	name := packName(sum)

	id, upload, err := s.intent(ctx, w, name, sum, size)
	if err != nil {
		return nil, err
	}
	if upload {
		if _, err := w.f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind staging file: %w", err)
		}
		if err := s.backend.Put(bctx(ctx), s.o.Container, name, io.LimitReader(w.f, size), engine.WithContentLength(size)); err != nil {
			return nil, fmt.Errorf("upload pack %s to %s: %w", name, s.name, err)
		}
		if err := s.verify(ctx, name, size, tail[len(tail)-int(TrailerSize):]); err != nil {
			return nil, err
		}
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			return nil, err
		}
	}
	sp, err := s.commit(ctx, id, name, sum, size, w.members)
	if err != nil {
		return nil, err
	}
	w.intent, w.intentName = 0, ""
	packsSealed.WithLabelValues(s.name).Inc()
	packBytes.WithLabelValues(s.name).Add(float64(size))
	packMembers.WithLabelValues(s.name).Add(float64(len(sp.Stored)))
	return sp, nil
}

// intent records the pack row before the upload (sealed_at NULL — the mark
// GC finds an orphan by). It returns the row and whether to upload: an
// identical pack already committed is not uploaded again (a pack is never
// overwritten); one in flight or being deleted is ErrPackBusy.
func (s *Store) intent(ctx context.Context, w *Writer, name, sum string, size int64) (int64, bool, error) {
	if w.intent != 0 && w.intentName == name {
		return w.intent, true, nil // this writer's own retry
	}
	var full int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM packs WHERE backend = $1 AND folder = $2 AND retired_at IS NULL`,
		s.name, packFolder(sum)).Scan(&full); err != nil {
		return 0, false, fmt.Errorf("count packs in folder %s: %w", packFolder(sum), err)
	}
	if full >= s.o.MaxPerFolder {
		return 0, false, fmt.Errorf("%s/%s (%d packs): %w", s.o.Container, packFolder(sum), full, ErrFolderFull)
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO packs (backend, name, folder, size, sha256, member_count)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (backend, name) DO NOTHING
		RETURNING id`, s.name, name, packFolder(sum), size, sum, len(w.members)).Scan(&id)
	if err == nil {
		w.intent, w.intentName = id, name
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("record pack intent %s: %w", name, err)
	}
	var sealed, retired sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT id, sealed_at, retired_at FROM packs WHERE backend = $1 AND name = $2`,
		s.name, name).Scan(&id, &sealed, &retired); err != nil {
		return 0, false, fmt.Errorf("%s: %w", name, ErrPackBusy)
	}
	if sealed.Valid && !retired.Valid {
		return id, false, nil
	}
	return 0, false, fmt.Errorf("%s: %w", name, ErrPackBusy)
}

// verify reads the trailer back at exactly size-48: a short, long or foreign
// file has no matching trailer there. Retried (bridges see a new file late).
func (s *Store) verify(ctx context.Context, name string, size int64, trailer []byte) error {
	var last error
	for attempt := 0; attempt < s.o.VerifyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("verify pack %s: %w", name, ctx.Err())
			case <-time.After(s.o.VerifyBackoff):
			}
		}
		rc, err := s.openRange(ctx, name, size-TrailerSize, TrailerSize)
		if err != nil {
			last = err
			continue
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err == nil && bytes.Equal(got, trailer) {
			return nil
		}
		if err != nil {
			last = fmt.Errorf("read trailer at %d: %w", size-TrailerSize, err)
		} else {
			last = fmt.Errorf("trailer at %d does not match (%d bytes read)", size-TrailerSize, len(got))
		}
	}
	return fmt.Errorf("verify pack %s on %s: %w", name, s.name, last)
}

// commit records every member and seals the pack in ONE transaction. A
// member replaces the live row of its (tenant, key); a GC move (src) happens
// only if its source row is still live — a member deleted while its pack was
// being rewritten stays deleted. Retried on a unique-index race or deadlock
// with another writer.
func (s *Store) commit(ctx context.Context, packID int64, name, sum string, size int64, members []stagedMember) (*SealedPack, error) {
	var sp *SealedPack
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		sp, err = s.commitOnce(ctx, packID, name, sum, size, members)
		var pqErr *pq.Error
		if err == nil || !errors.As(err, &pqErr) || (pqErr.Code != "23505" && pqErr.Code != "40P01") {
			return sp, err
		}
		s.logger.Debug("pack commit retried", zap.String("pack", name), zap.Error(err))
	}
	return sp, err
}

func (s *Store) commitOnce(ctx context.Context, packID int64, name, sum string, size int64, members []stagedMember) (*SealedPack, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("commit pack %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var retired sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT retired_at FROM packs WHERE id = $1 FOR UPDATE`, packID).Scan(&retired)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && retired.Valid) {
		return nil, fmt.Errorf("commit pack %s: %w", name, ErrIntentLost)
	}
	if err != nil {
		return nil, fmt.Errorf("lock pack %s: %w", name, err)
	}

	dec := map[int64]int64{}
	sp := &SealedPack{ID: packID, Name: name, Size: size, SHA256: sum, Members: len(members)}
	var live int64
	for _, m := range members {
		var rows *sql.Rows
		if m.src != 0 {
			rows, err = tx.QueryContext(ctx, `UPDATE pack_members SET deleted_at = NOW()
				WHERE id = $1 AND deleted_at IS NULL RETURNING pack_id, byte_length`, m.src)
		} else {
			rows, err = tx.QueryContext(ctx, `UPDATE pack_members SET deleted_at = NOW()
				WHERE tenant_id = $1 AND member_key = $2 AND deleted_at IS NULL RETURNING pack_id, byte_length`, m.Tenant, m.Key)
		}
		if err != nil {
			return nil, fmt.Errorf("replace member %s/%s: %w", m.Tenant, m.Key, err)
		}
		replaced := 0
		for rows.Next() {
			var pid, l int64
			if err := rows.Scan(&pid, &l); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("replace member %s/%s: %w", m.Tenant, m.Key, err)
			}
			dec[pid] += l
			replaced++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("replace member %s/%s: %w", m.Tenant, m.Key, err)
		}
		_ = rows.Close()
		if m.src != 0 && replaced == 0 {
			continue // deleted (or replaced) while its pack was being rewritten
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pack_members (pack_id, tenant_id, member_key, byte_offset, byte_length, sha256)
			VALUES ($1, $2, $3, $4, $5, $6)`, packID, m.Tenant, m.Key, m.Offset, m.Length, m.SHA256); err != nil {
			return nil, fmt.Errorf("record member %s/%s: %w", m.Tenant, m.Key, err)
		}
		live += m.Length
		sp.Stored = append(sp.Stored, MemberRef{Tenant: m.Tenant, Key: m.Key})
	}
	pids := make([]int64, 0, len(dec))
	for pid := range dec {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	for _, pid := range pids {
		if _, err := tx.ExecContext(ctx, `UPDATE packs SET live_bytes = GREATEST(live_bytes - $2, 0) WHERE id = $1`, pid, dec[pid]); err != nil {
			return nil, fmt.Errorf("decrement live bytes of pack %d: %w", pid, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE packs SET sealed_at = COALESCE(sealed_at, NOW()), live_bytes = live_bytes + $2 WHERE id = $1`,
		packID, live); err != nil {
		return nil, fmt.Errorf("seal pack %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit pack %s: %w", name, err)
	}
	return sp, nil
}

// abandonIntent removes an uncommitted pack: retire the row, delete the file,
// delete the row. Best effort — GC expires what is left.
func (s *Store) abandonIntent(ctx context.Context, id int64, name string) {
	res, err := s.db.ExecContext(ctx, `UPDATE packs SET retired_at = NOW() WHERE id = $1 AND sealed_at IS NULL AND retired_at IS NULL`, id)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	if err := s.backend.Delete(bctx(ctx), s.o.Container, name); err != nil && !isNotFound(err) {
		s.logger.Warn("packstore: abandoned pack not deleted; GC retries", zap.String("pack", name), zap.Error(err))
		return
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM packs WHERE id = $1 AND retired_at IS NOT NULL`, id)
}
