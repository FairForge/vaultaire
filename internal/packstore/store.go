// Package packstore batches small immutable objects into large,
// content-addressed pack files on a slow per-file backend, with the index in
// Postgres (migration 080). See CLAUDE.md in this directory.
package packstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// Defaults of Options.
const (
	// DefaultContainer is the container every pack lives in.
	DefaultContainer = "_packs"
	// DefaultTargetSize: a pack is sealed before it would grow past this.
	// 256 MiB is the piece size Sync's bridges stream fastest (156 MB/s up,
	// 450 MB/s down across 5 bridges, 2026-10-07).
	DefaultTargetSize = 256 << 20
	// DefaultMaxMembers bounds the members (and the footer) of one pack.
	DefaultMaxMembers = 4096
	// DefaultMaxPerFolder is Sync's hard limit of files per folder.
	DefaultMaxPerFolder = 50000
	// DefaultIntentGrace: an upload whose commit has not happened this long
	// after its intent row was written is an orphan.
	DefaultIntentGrace = 6 * time.Hour
	// DefaultLiveThreshold: a pack whose live bytes fall below this fraction
	// of its size is rewritten.
	DefaultLiveThreshold = 0.5
	// DefaultTombstoneMaxAge: a deleted member's bytes leave the backend at
	// most this long after the delete (its pack is rewritten).
	DefaultTombstoneMaxAge = 30 * 24 * time.Hour
	// DefaultMaxRewriteBytes bounds the bytes one GC run rewrites.
	DefaultMaxRewriteBytes = 4 << 30
)

// addressTenant is the tenant in the context of every driver call: packs mix
// tenants, so they live under the system's reserved id (engine.ChunkAddressTenant,
// `t-_global/` on a fixed-bucket backend) — never under a customer's prefix,
// which the account-erasure sweep would walk.
const addressTenant = engine.ChunkAddressTenant

// Errors.
var (
	ErrNotFound       = errors.New("packstore: member not found")
	ErrCorrupt        = errors.New("packstore: member bytes do not match their sha256")
	ErrMemberTooLarge = errors.New("packstore: member larger than the pack target size (store it whole)")
	ErrDuplicateKey   = errors.New("packstore: key already in this pack")
	ErrSizeMismatch   = errors.New("packstore: member reader does not hold the declared size")
	ErrFolderFull     = errors.New("packstore: pack folder holds the maximum number of packs")
	ErrPackBusy       = errors.New("packstore: a pack of the same name is being written or deleted; redo the batch later")
	ErrIntentLost     = errors.New("packstore: the pack's intent row is gone or retired (GC expired it); redo the batch")
	ErrWriterClosed   = errors.New("packstore: writer is closed")
)

// Options configures a Store. Zero values take the defaults.
type Options struct {
	// BackendName overrides backend.Name() (the name the engine registered
	// the driver under; the packs rows are keyed by it).
	BackendName string
	// Container is where packs live on the backend (default "_packs").
	Container string
	// StagingDir holds the pack being assembled (default
	// <os.TempDir()>/vaultaire-packs). One file per open Writer, removed on
	// Close; never larger than TargetSize plus the footer.
	StagingDir      string
	TargetSize      int64
	MaxMembers      int
	MaxPerFolder    int
	IntentGrace     time.Duration
	LiveThreshold   float64
	TombstoneMaxAge time.Duration
	MaxRewriteBytes int64
	// VerifyAttempts / VerifyBackoff: the post-upload read of the trailer is
	// retried (a bridge sees a new file after a delay; default 4 × 2 s).
	VerifyAttempts int
	VerifyBackoff  time.Duration
	Logger         *zap.Logger
}

// Store is a pack store over one backend.
type Store struct {
	db      *sql.DB
	backend engine.Driver
	name    string
	o       Options
	logger  *zap.Logger

	// beforeCommit is a test hook between the verified upload and the
	// commit (a "crash" there).
	beforeCommit func() error
}

// New returns a Store writing packs to backend with the index in db.
func New(db *sql.DB, backend engine.Driver, o Options) (*Store, error) {
	if db == nil || backend == nil {
		return nil, errors.New("packstore: a database and a backend are required")
	}
	if o.Container == "" {
		o.Container = DefaultContainer
	}
	if o.StagingDir == "" {
		o.StagingDir = filepath.Join(os.TempDir(), "vaultaire-packs")
	}
	if o.TargetSize <= 0 {
		o.TargetSize = DefaultTargetSize
	}
	if o.MaxMembers <= 0 {
		o.MaxMembers = DefaultMaxMembers
	}
	if o.MaxPerFolder <= 0 {
		o.MaxPerFolder = DefaultMaxPerFolder
	}
	if o.IntentGrace <= 0 {
		o.IntentGrace = DefaultIntentGrace
	}
	if o.LiveThreshold <= 0 || o.LiveThreshold > 1 {
		o.LiveThreshold = DefaultLiveThreshold
	}
	if o.TombstoneMaxAge <= 0 {
		o.TombstoneMaxAge = DefaultTombstoneMaxAge
	}
	if o.MaxRewriteBytes <= 0 {
		o.MaxRewriteBytes = DefaultMaxRewriteBytes
	}
	if o.VerifyAttempts <= 0 {
		o.VerifyAttempts = 4
	}
	if o.VerifyBackoff <= 0 {
		o.VerifyBackoff = 2 * time.Second
	}
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if err := os.MkdirAll(o.StagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("packstore staging dir %s: %w", o.StagingDir, err)
	}
	name := o.BackendName
	if name == "" {
		name = backend.Name()
	}
	initSeries(name)
	return &Store{db: db, backend: backend, name: name, o: o, logger: o.Logger}, nil
}

// Backend is the name of the backend the store writes to.
func (s *Store) Backend() string { return s.name }

// bctx is the context of every backend call.
func bctx(ctx context.Context) context.Context { return common.WithTenantID(ctx, addressTenant) }

func isNotFound(err error) bool {
	var nf engine.NotFoundError
	return errors.As(err, &nf) || errors.Is(err, fs.ErrNotExist)
}

// --- reads ----------------------------------------------------------------------

// memberLoc is where a live member is.
type memberLoc struct {
	packID int64
	pack   string
	offset int64
	length int64
	sha    string
}

func (s *Store) locate(ctx context.Context, tenantID, key string) (*memberLoc, error) {
	var l memberLoc
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.name, m.byte_offset, m.byte_length, m.sha256
		  FROM pack_members m JOIN packs p ON p.id = m.pack_id
		 WHERE m.tenant_id = $1 AND m.member_key = $2 AND m.deleted_at IS NULL
		   AND p.backend = $3 AND p.sealed_at IS NOT NULL AND p.retired_at IS NULL`,
		tenantID, key, s.name).Scan(&l.packID, &l.pack, &l.offset, &l.length, &l.sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s/%s: %w", tenantID, key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("locate member %s/%s: %w", tenantID, key, err)
	}
	return &l, nil
}

// openRange opens bytes [off, off+n) of a pack: one ranged request when the
// backend can, else a whole GET with the prefix discarded.
func (s *Store) openRange(ctx context.Context, pack string, off, n int64) (io.ReadCloser, error) {
	if rg, ok := s.backend.(engine.RangeGetter); ok {
		rc, err := rg.GetRange(bctx(ctx), s.o.Container, pack, off, n)
		if err != nil {
			return nil, fmt.Errorf("range %d+%d of pack %s on %s: %w", off, n, pack, s.name, err)
		}
		return readCloser{io.LimitReader(rc, n), rc}, nil
	}
	rc, err := s.backend.Get(bctx(ctx), s.o.Container, pack)
	if err != nil {
		return nil, fmt.Errorf("get pack %s on %s: %w", pack, s.name, err)
	}
	if _, err := io.CopyN(io.Discard, rc, off); err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("skip to %d in pack %s: %w", off, pack, err)
	}
	return readCloser{io.LimitReader(rc, n), rc}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// open locates a member and opens a window of it; a pack that vanished under
// the lookup (GC moved the member and deleted the pack) is looked up once more.
func (s *Store) open(ctx context.Context, tenantID, key string, off, n int64, whole bool) (io.ReadCloser, *memberLoc, error) {
	start := time.Now()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		l, err := s.locate(ctx, tenantID, key)
		if err != nil {
			return nil, nil, err
		}
		if whole {
			off, n = 0, l.length
		}
		if off < 0 || n < 0 || off+n > l.length {
			return nil, nil, fmt.Errorf("%w: range %d+%d of a %d-byte member", engine.ErrInvalidInput, off, n, l.length)
		}
		rc, err := s.openRange(ctx, l.pack, l.offset+off, n)
		if err == nil {
			memberReadSeconds.WithLabelValues(s.name).Observe(time.Since(start).Seconds())
			return rc, l, nil
		}
		lastErr = err
		if !isNotFound(err) {
			break
		}
	}
	return nil, nil, lastErr
}

// Get returns a member's bytes. The sha256 and the length are verified as the
// stream is read: a mismatch is ErrCorrupt from the Read that reaches the end
// (the bytes before it have been delivered — a caller that must not act on
// unverified bytes reads to EOF first).
func (s *Store) Get(ctx context.Context, tenantID, key string) (io.ReadCloser, error) {
	rc, l, err := s.open(ctx, tenantID, key, 0, 0, true)
	if err != nil {
		return nil, err
	}
	return &verifyingReader{rc: rc, h: sha256.New(), want: l.sha, left: l.length, backend: s.name}, nil
}

// GetRange returns n bytes of a member from off. A window is not verified
// (the sha256 covers the whole member); a range outside the member is refused.
func (s *Store) GetRange(ctx context.Context, tenantID, key string, off, n int64) (io.ReadCloser, error) {
	rc, _, err := s.open(ctx, tenantID, key, off, n, false)
	return rc, err
}

// Exists reports whether a live member is recorded.
func (s *Store) Exists(ctx context.Context, tenantID, key string) (bool, error) {
	_, err := s.locate(ctx, tenantID, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Delete tombstones a member. A key with no live member is a no-op. The bytes
// stay in the pack until GC rewrites it (live ratio, or TombstoneMaxAge).
func (s *Store) Delete(ctx context.Context, tenantID, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete member %s/%s: %w", tenantID, key, err)
	}
	defer func() { _ = tx.Rollback() }()
	var packID, length int64
	err = tx.QueryRowContext(ctx, `
		UPDATE pack_members m SET deleted_at = NOW()
		  FROM packs p
		 WHERE m.pack_id = p.id AND p.backend = $3
		   AND m.tenant_id = $1 AND m.member_key = $2 AND m.deleted_at IS NULL
		RETURNING m.pack_id, m.byte_length`, tenantID, key, s.name).Scan(&packID, &length)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tombstone member %s/%s: %w", tenantID, key, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE packs SET live_bytes = GREATEST(live_bytes - $2, 0) WHERE id = $1`, packID, length); err != nil {
		return fmt.Errorf("decrement live bytes of pack %d: %w", packID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete member %s/%s: %w", tenantID, key, err)
	}
	return nil
}

// verifyingReader hashes a member as it is read and checks it at the end.
type verifyingReader struct {
	rc      io.ReadCloser
	h       hash.Hash
	want    string
	left    int64
	backend string
	err     error
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if int64(len(p)) > v.left {
		p = p[:v.left]
	}
	var n int
	var err error
	if len(p) > 0 {
		n, err = v.rc.Read(p)
		v.h.Write(p[:n])
		v.left -= int64(n)
	}
	if v.left == 0 {
		if hex.EncodeToString(v.h.Sum(nil)) != v.want {
			memberCorrupt.WithLabelValues(v.backend).Inc()
			v.err = ErrCorrupt
			return n, v.err
		}
		v.err = io.EOF
		return n, v.err
	}
	if errors.Is(err, io.EOF) {
		memberCorrupt.WithLabelValues(v.backend).Inc()
		v.err = fmt.Errorf("%w: %d bytes short", ErrCorrupt, v.left)
		return n, v.err
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.rc.Close() }

// --- the index from the pack alone --------------------------------------------

// download streams a pack to a staging file and checks it hashes to its name.
func (s *Store) download(ctx context.Context, name string) (*os.File, int64, error) {
	if !validPackName(name) {
		return nil, 0, fmt.Errorf("%w: %q is not a pack name", ErrBadPack, name)
	}
	rc, err := s.backend.Get(bctx(ctx), s.o.Container, name)
	if err != nil {
		return nil, 0, fmt.Errorf("get pack %s on %s: %w", name, s.name, err)
	}
	defer func() { _ = rc.Close() }()
	f, err := os.CreateTemp(s.o.StagingDir, "read-*.pack")
	if err != nil {
		return nil, 0, fmt.Errorf("staging file: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), rc)
	if err != nil {
		closeRemove(f)
		return nil, 0, fmt.Errorf("download pack %s: %w", name, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != sumOfName(name) {
		closeRemove(f)
		return nil, 0, fmt.Errorf("%w: %s does not hash to its name", ErrBadPack, name)
	}
	return f, n, nil
}

func closeRemove(f *os.File) {
	_ = f.Close()
	_ = os.Remove(f.Name())
}

// ReadPackIndex downloads a pack, checks it hashes to its name and returns
// its footer.
func (s *Store) ReadPackIndex(ctx context.Context, name string) (*Footer, error) {
	f, n, err := s.download(ctx, name)
	if err != nil {
		return nil, err
	}
	defer closeRemove(f)
	return ReadIndex(f, n)
}

// Recover rebuilds the index of one pack from its footer (a lost or damaged
// database): the pack row is recorded sealed if absent, and every footer
// member with no live row of its (tenant, key) is recorded live. Returns the
// members recorded; a second run records none. Tombstones lost with the
// database are not known — a member deleted before the loss comes back.
func (s *Store) Recover(ctx context.Context, name string) (int, error) {
	f, size, err := s.download(ctx, name)
	if err != nil {
		return 0, err
	}
	defer closeRemove(f)
	footer, err := ReadIndex(f, size)
	if err != nil {
		return 0, err
	}
	sum := sumOfName(name)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("recover pack %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()
	var packID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO packs (backend, name, folder, size, sha256, member_count, sealed_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (backend, name) DO UPDATE SET sealed_at = COALESCE(packs.sealed_at, NOW())
		RETURNING id`, s.name, name, packFolder(sum), size, sum, len(footer.Members)).Scan(&packID)
	if err != nil {
		return 0, fmt.Errorf("record recovered pack %s: %w", name, err)
	}
	recorded := 0
	var live int64
	for _, m := range footer.Members {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO pack_members (pack_id, tenant_id, member_key, byte_offset, byte_length, sha256)
			SELECT $1, $2, $3, $4, $5, $6
			 WHERE NOT EXISTS (SELECT 1 FROM pack_members
			                    WHERE tenant_id = $2 AND member_key = $3 AND deleted_at IS NULL)`,
			packID, m.Tenant, m.Key, m.Offset, m.Length, m.SHA256)
		if err != nil {
			return 0, fmt.Errorf("record recovered member %s/%s: %w", m.Tenant, m.Key, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			recorded++
			live += m.Length
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE packs SET live_bytes = live_bytes + $2 WHERE id = $1`, packID, live); err != nil {
		return 0, fmt.Errorf("live bytes of recovered pack %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("recover pack %s: %w", name, err)
	}
	return recorded, nil
}
