// Package logstore is the transparency log's Postgres store (keystone K4,
// migration 103): translog_entries, translog_hashes (tlog's stored hashes by
// storage index) and translog_checkpoints. The Merkle arithmetic is
// internal/translog's (golang.org/x/mod/sumdb/tlog); this package only reads
// and writes rows.
//
// Append is ONE transaction under pg_advisory_xact_lock('translog'): the next
// index is MAX(idx)+1, the entry and its stored hashes are written together,
// so indexes have no gap and every committed entry has its hashes. The
// tables refuse UPDATE/DELETE/TRUNCATE (migration 103's trigger).
package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"
	"github.com/transparency-dev/formats/log"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
)

const lockLog = `SELECT pg_advisory_xact_lock(hashtextextended('translog', 0))`

// Store is the log on a database.
type Store struct {
	db        *sql.DB
	signer    note.Signer
	now       func() time.Time
	highwater string
}

// New returns the store; signer nil = entries append, nothing is signed.
func New(db *sql.DB, signer note.Signer) *Store {
	return &Store{db: db, signer: signer, now: time.Now}
}

// WithHighwater keeps the newest checkpoint this box signed in the file at
// path, OUTSIDE the database (RP37-11): a database restored from a dump
// holds an older newest checkpoint, and without the file the job would sign
// a different tree at a size the key already signed — the equivocation the
// log exists to make impossible. Before every signature the stored log must
// extend BOTH the newest checkpoint in the database and the file's; the
// file is rewritten atomically (0600) after each signature. Both slots read
// one file (TRANSLOG_HIGHWATER_FILE). "" = no file (tests of other things).
func (s *Store) WithHighwater(path string) *Store {
	s.highwater = path
	return s
}

// Signing reports whether the store has a signing key.
func (s *Store) Signing() bool { return s != nil && s.signer != nil }

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// hashReader reads stored hashes, in the order asked, through q.
func hashReader(ctx context.Context, q querier) translog.HashReader {
	return translog.HashReaderFunc(func(indexes []int64) ([]translog.Hash, error) {
		rows, err := q.QueryContext(ctx, `SELECT idx, hash FROM translog_hashes WHERE idx = ANY($1)`, pq.Array(indexes))
		if err != nil {
			return nil, fmt.Errorf("read stored hashes: %w", err)
		}
		defer func() { _ = rows.Close() }()
		got := make(map[int64]translog.Hash, len(indexes))
		for rows.Next() {
			var idx int64
			var b []byte
			if err := rows.Scan(&idx, &b); err != nil {
				return nil, fmt.Errorf("read stored hashes: %w", err)
			}
			if len(b) != translog.HashSize {
				return nil, fmt.Errorf("stored hash %d: %d bytes", idx, len(b))
			}
			var h translog.Hash
			copy(h[:], b)
			got[idx] = h
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read stored hashes: %w", err)
		}
		out := make([]translog.Hash, len(indexes))
		for i, x := range indexes {
			h, ok := got[x]
			if !ok {
				return nil, fmt.Errorf("stored hash %d is missing", x)
			}
			out[i] = h
		}
		return out, nil
	})
}

// Append appends e and returns its index. With a dedup key already in the
// log it appends nothing and returns the first entry's index, appended =
// false. Every failure counts in vaultaire_translog_append_failures_total.
func (s *Store) Append(ctx context.Context, e translog.Entry) (idx int64, appended bool, err error) {
	defer func() {
		if err != nil && e != nil {
			appendFailures.WithLabelValues(e.Kind()).Inc()
		}
	}()
	if s == nil || s.db == nil {
		return 0, false, errors.New("translog: no database")
	}
	body, err := translog.Encode(e, s.now())
	if err != nil {
		return 0, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("translog append: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, lockLog); err != nil {
		return 0, false, fmt.Errorf("translog append: lock: %w", err)
	}
	key := e.DedupKey()
	if key != "" {
		err := tx.QueryRowContext(ctx, `SELECT idx FROM translog_entries WHERE kind = $1 AND dedup_key = $2`, e.Kind(), key).Scan(&idx)
		switch {
		case err == nil:
			return idx, false, nil
		case !errors.Is(err, sql.ErrNoRows):
			return 0, false, fmt.Errorf("translog append: dedup: %w", err)
		}
	}
	var n, stored int64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COALESCE(MAX(idx) + 1, 0) FROM translog_entries),
	                                          (SELECT COALESCE(MAX(idx) + 1, 0) FROM translog_hashes)`).Scan(&n, &stored); err != nil {
		return 0, false, fmt.Errorf("translog append: size: %w", err)
	}
	if stored != translog.StoredHashCount(n) {
		return 0, false, fmt.Errorf("translog append: %d entries but %d stored hashes (want %d) — the tables are out of step", n, stored, translog.StoredHashCount(n))
	}
	first, hashes, err := translog.StoredHashes(n, body, hashReader(ctx, tx))
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO translog_entries (idx, kind, body, dedup_key) VALUES ($1, $2, $3, NULLIF($4, ''))`,
		n, e.Kind(), body, key); err != nil {
		return 0, false, fmt.Errorf("translog append: entry %d: %w", n, err)
	}
	idxs := make([]int64, len(hashes))
	vals := make([][]byte, len(hashes))
	for i := range hashes {
		idxs[i] = first + int64(i)
		vals[i] = hashes[i][:]
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO translog_hashes (idx, hash) SELECT * FROM unnest($1::bigint[], $2::bytea[])`,
		pq.Array(idxs), pq.Array(vals)); err != nil {
		return 0, false, fmt.Errorf("translog append: hashes of %d: %w", n, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("translog append: commit %d: %w", n, err)
	}
	return n, true, nil
}

// Size is the number of entries.
func (s *Store) Size(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(idx) + 1, 0) FROM translog_entries`).Scan(&n); err != nil {
		return 0, fmt.Errorf("translog size: %w", err)
	}
	return n, nil
}

// Root is the root of the first size entries.
func (s *Store) Root(ctx context.Context, size int64) (translog.Hash, error) {
	return translog.Root(size, hashReader(ctx, s.db))
}

// InclusionProof proves entry idx is in the tree of size entries.
func (s *Store) InclusionProof(ctx context.Context, idx, size int64) ([]translog.Hash, error) {
	return translog.InclusionProof(idx, size, hashReader(ctx, s.db))
}

// ConsistencyProof proves the tree of oldSize is a prefix of newSize's.
func (s *Store) ConsistencyProof(ctx context.Context, oldSize, newSize int64) ([]translog.Hash, error) {
	return translog.ConsistencyProof(oldSize, newSize, hashReader(ctx, s.db))
}

// HashReader reads the stored hashes (tiles are served from it).
func (s *Store) HashReader(ctx context.Context) translog.HashReader { return hashReader(ctx, s.db) }

// Entries returns the bodies of entries from … from+n-1 that exist.
func (s *Store) Entries(ctx context.Context, from, n int64) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, body FROM translog_entries WHERE idx >= $1 AND idx < $2 ORDER BY idx`, from, from+n)
	if err != nil {
		return nil, fmt.Errorf("translog entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][]byte
	want := from
	for rows.Next() {
		var idx int64
		var body []byte
		if err := rows.Scan(&idx, &body); err != nil {
			return nil, fmt.Errorf("translog entries: %w", err)
		}
		if idx != want {
			return nil, fmt.Errorf("translog entries: %d missing", want)
		}
		want++
		out = append(out, body)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("translog entries: %w", err)
	}
	return out, nil
}

// Stored is a signed checkpoint as stored.
type Stored struct {
	Size     int64
	Root     translog.Hash
	Note     string
	SignedAt time.Time
}

// LatestCheckpoint is the checkpoint of the largest size; ok = false when
// none was ever signed.
func (s *Store) LatestCheckpoint(ctx context.Context) (Stored, bool, error) {
	var c Stored
	var root []byte
	err := s.db.QueryRowContext(ctx, `SELECT size, root, note, signed_at FROM translog_checkpoints ORDER BY size DESC LIMIT 1`).
		Scan(&c.Size, &root, &c.Note, &c.SignedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Stored{}, false, nil
	}
	if err != nil {
		return Stored{}, false, fmt.Errorf("translog latest checkpoint: %w", err)
	}
	copy(c.Root[:], root)
	return c, true, nil
}

// CheckpointResult is one checkpoint pass (the translog_checkpoint job's
// job_runs.result).
type CheckpointResult struct {
	Size           int64  `json:"size"`
	CheckpointSize int64  `json:"checkpoint_size"`
	HighwaterSize  int64  `json:"highwater_size,omitempty"`
	Signed         bool   `json:"signed"`
	Root           string `json:"root,omitempty"`
	Reason         string `json:"reason,omitempty"`
	// Refused names the refusal (RefusedRewritten, RefusedBehindHighwater,
	// RefusedHighwaterUnreadable): the run failed and nothing was signed.
	Refused string `json:"refused,omitempty"`
	// Note: "first run" when there was no high-water file yet.
	Note string `json:"note,omitempty"`
}

// Why a checkpoint pass refused to sign (vaultaire_translog_checkpoint_refusals_total{reason}).
const (
	// RefusedRewritten: the stored log does not extend a tree the key
	// signed (the database's newest checkpoint or the high-water file's).
	RefusedRewritten = "rewritten"
	// RefusedBehindHighwater: the database holds fewer entries than the
	// newest tree this box signed — it was restored behind it.
	RefusedBehindHighwater = "behind_highwater"
	// RefusedHighwaterUnreadable: the high-water file exists but cannot be
	// read or parsed — never taken as "first run".
	RefusedHighwaterUnreadable = "highwater_unreadable"
)

// RefusalReasons lists every refusal reason.
func RefusalReasons() []string {
	return []string{RefusedRewritten, RefusedBehindHighwater, RefusedHighwaterUnreadable}
}

// refuse records a refusal on res and the counter, and returns err.
func refuse(res *CheckpointResult, reason string, err error) error {
	res.Refused = reason
	checkpointRefusals.WithLabelValues(reason).Inc()
	return err
}

// Checkpoint signs a checkpoint of the whole log when it grew past the
// newest one. Before signing it proves — from the stored hashes — that the
// log extends the newest checkpoint in the database AND the high-water
// file's (WithHighwater); a log that does not, or a database holding fewer
// entries than the file, is a refusal: an error, a count, nothing signed.
// Without a signing key it does the same checks and signs nothing ("no
// signing key").
func (s *Store) Checkpoint(ctx context.Context) (CheckpointResult, error) {
	var res CheckpointResult
	size, err := s.Size(ctx)
	if err != nil {
		return res, err
	}
	res.Size = size
	latest, ok, err := s.LatestCheckpoint(ctx)
	if err != nil {
		return res, err
	}
	hw, hwOK, err := s.readHighwater()
	if err != nil {
		return res, refuse(&res, RefusedHighwaterUnreadable, err)
	}
	if s.highwater != "" && !hwOK {
		res.Note = "first run: no high-water file yet, written after the next signature"
	}
	if hwOK {
		res.HighwaterSize = hw.Size
		if hw.Size > size {
			return res, refuse(&res, RefusedBehindHighwater, fmt.Errorf(
				"translog: the database holds %d entries but this box signed a tree of %d (%s) — restored behind the high-water: refusing to sign (docs/TRANSPARENCY.md § After a database restore)",
				size, hw.Size, s.highwater))
		}
	}
	if ok {
		res.CheckpointSize = latest.Size
		if latest.Size > size {
			return res, refuse(&res, RefusedRewritten, fmt.Errorf("translog: the log has %d entries but a checkpoint was signed at %d", size, latest.Size))
		}
	}
	if size == 0 {
		res.Reason = "empty"
		return res, nil
	}
	root, err := s.Root(ctx, size)
	if err != nil {
		return res, err
	}
	res.Root = fmt.Sprintf("%x", root[:])
	prove := func(o, n int64) ([]translog.Hash, error) { return s.ConsistencyProof(ctx, o, n) }
	if ok {
		if err := checkExtends(translog.Checkpoint{Size: latest.Size, Root: latest.Root}, size, root, prove); err != nil {
			return res, refuse(&res, RefusedRewritten, err)
		}
	}
	if hwOK {
		if err := checkExtends(hw, size, root, prove); err != nil {
			return res, refuse(&res, RefusedRewritten, fmt.Errorf("%w (the high-water file %s)", err, s.highwater))
		}
	}
	if ok && latest.Size == size {
		// Both signed trees proven above (every pass re-proves them, so a
		// restore is refused even before the log grows): nothing new to sign.
		res.Reason = "unchanged"
		return res, nil
	}
	if s.signer == nil {
		res.Reason = "no signing key"
		return res, nil
	}
	msg, err := translog.SignCheckpoint(s.signer, size, root)
	if err != nil {
		return res, err
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO translog_checkpoints (size, root, note) VALUES ($1, $2, $3) ON CONFLICT (size) DO NOTHING`,
		size, root[:], string(msg))
	if err != nil {
		return res, fmt.Errorf("translog: store checkpoint %d: %w", size, err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		res.Reason = "already signed"
		return res, nil
	}
	res.Signed, res.CheckpointSize = true, size
	// After the row, never before: a file ahead of a checkpoint that was
	// never stored would refuse every later run.
	if err := s.writeHighwater(translog.Checkpoint{Origin: translog.Origin, Size: size, Root: root}); err != nil {
		return res, err
	}
	res.HighwaterSize = size
	return res, nil
}

// readHighwater reads the high-water file: ok = false when there is no file
// (or none configured). Anything else that is not a checkpoint body of this
// log is an error — never "first run".
func (s *Store) readHighwater() (translog.Checkpoint, bool, error) {
	if s.highwater == "" {
		return translog.Checkpoint{}, false, nil
	}
	b, err := os.ReadFile(s.highwater) // #nosec G304 -- the operator's TRANSLOG_HIGHWATER_FILE
	if errors.Is(err, fs.ErrNotExist) {
		return translog.Checkpoint{}, false, nil
	}
	if err != nil {
		return translog.Checkpoint{}, false, fmt.Errorf("translog: read the high-water file %s: %w", s.highwater, err)
	}
	var cp log.Checkpoint
	rest, err := cp.Unmarshal(b)
	if err != nil || len(rest) != 0 || cp.Origin != translog.Origin || len(cp.Hash) != translog.HashSize || cp.Size == 0 || cp.Size > 1<<62 {
		return translog.Checkpoint{}, false, fmt.Errorf("translog: the high-water file %s is not a checkpoint of %s", s.highwater, translog.Origin)
	}
	var root translog.Hash
	copy(root[:], cp.Hash)
	return translog.Checkpoint{Origin: cp.Origin, Size: int64(cp.Size), Root: root}, true, nil
}

// writeHighwater replaces the high-water file with cp's checkpoint body,
// atomically: a temporary file in the same directory, synced, renamed over
// it, the directory synced.
func (s *Store) writeHighwater(cp translog.Checkpoint) error {
	if s.highwater == "" {
		return nil
	}
	dir := filepath.Dir(s.highwater)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("translog: high-water directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".highwater-*")
	if err != nil {
		return fmt.Errorf("translog: write the high-water file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	body := log.Checkpoint{Origin: cp.Origin, Size: uint64(cp.Size), Hash: cp.Root[:]}.Marshal()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("translog: write the high-water file: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("translog: write the high-water file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("translog: write the high-water file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("translog: write the high-water file: %w", err)
	}
	if err := os.Rename(tmp, s.highwater); err != nil {
		return fmt.Errorf("translog: write the high-water file: %w", err)
	}
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- the high-water file's own directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// checkExtends proves the tree (size, root) extends old.
func checkExtends(old translog.Checkpoint, size int64, root translog.Hash, prove func(oldSize, newSize int64) ([]translog.Hash, error)) error {
	if old.Size == size {
		if old.Root != root {
			return fmt.Errorf("translog: the stored log does not extend checkpoint %d — refusing to sign: a different root at the same size", old.Size)
		}
		return nil
	}
	p, err := prove(old.Size, size)
	if err != nil {
		return fmt.Errorf("translog: consistency proof %d→%d: %w", old.Size, size, err)
	}
	if err := translog.VerifyConsistency(p, old.Size, size, old.Root, root); err != nil {
		return fmt.Errorf("translog: the stored log does not extend checkpoint %d — refusing to sign: %w", old.Size, err)
	}
	return nil
}

// CheckpointNote is the newest signed checkpoint as the read side serves
// it (translog.Source).
func (s *Store) CheckpointNote(ctx context.Context) ([]byte, int64, bool, error) {
	c, ok, err := s.LatestCheckpoint(ctx)
	if err != nil || !ok {
		return nil, 0, false, err
	}
	return []byte(c.Note), c.Size, true, nil
}

// ByDedupKey is the entry a dedup key names: its index and body; found =
// false when none.
func (s *Store) ByDedupKey(ctx context.Context, kind, key string) (idx int64, body []byte, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT idx, body FROM translog_entries WHERE kind = $1 AND dedup_key = $2`, kind, key).Scan(&idx, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("translog: by dedup key: %w", err)
	}
	return idx, body, true, nil
}
