package packstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// --- fixtures ---------------------------------------------------------------

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	var reg sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT to_regclass('pack_members')::text`).Scan(&reg))
	if !reg.Valid {
		t.Skip("migration 080 not applied — run `make test-db`")
	}
	return db
}

func uniq(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%x", prefix, b)
}

// testBackend is the local driver under a per-test name, with byte-range
// reads (the local driver has none) and counters. Every test has its own
// backend name, so its packs rows are its own (the DB is shared).
type testBackend struct {
	engine.Driver
	name   string
	base   string
	puts   atomic.Int64
	gets   atomic.Int64
	ranges atomic.Int64
	// failPut, when set, fails every Put.
	failPut atomic.Bool
}

func newTestBackend(t *testing.T) *testBackend {
	t.Helper()
	base := t.TempDir()
	return &testBackend{Driver: drivers.NewLocalDriver(base, zap.NewNop()), name: uniq("packtest"), base: base}
}

func (b *testBackend) Name() string { return b.name }

func (b *testBackend) Put(ctx context.Context, c, a string, r io.Reader, opts ...engine.PutOption) error {
	b.puts.Add(1)
	if b.failPut.Load() {
		return errors.New("injected put failure")
	}
	return b.Driver.Put(ctx, c, a, r, opts...)
}

func (b *testBackend) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	b.gets.Add(1)
	return b.Driver.Get(ctx, c, a)
}

func (b *testBackend) GetRange(_ context.Context, c, a string, off, n int64) (io.ReadCloser, error) {
	b.ranges.Add(1)
	f, err := os.Open(filepath.Join(b.base, c, a))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, engine.ErrNotFound(c, a)
		}
		return nil, err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, n), f}, nil
}

// files lists the pack files on the backend.
func (b *testBackend) files(t *testing.T) []string {
	t.Helper()
	names, err := b.Driver.List(context.Background(), DefaultContainer, "")
	require.NoError(t, err)
	return names
}

func newTestStore(t *testing.T, db *sql.DB, be engine.Driver, mut func(*Options)) *Store {
	t.Helper()
	o := Options{StagingDir: t.TempDir(), Logger: zap.NewNop(), VerifyBackoff: time.Millisecond}
	if mut != nil {
		mut(&o)
	}
	s, err := New(db, be, o)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM packs WHERE backend = $1`, be.Name())
	})
	return s
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func readMember(t *testing.T, s *Store, tenant, key string) ([]byte, error) {
	t.Helper()
	rc, err := s.Get(context.Background(), tenant, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), q, args...).Scan(&n))
	return n
}

// --- round trip ---------------------------------------------------------------

func TestStore_ManySmallMembersRoundTripThroughOnePack(t *testing.T) {
	// Arrange
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	want := map[string][]byte{}
	w, err := s.NewWriter()
	require.NoError(t, err)
	defer func() { _ = w.Close(ctx) }()

	// Act
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("shard/%04d", i)
		b := randBytes(t, (i*397)%20000) // 0 … 20 KB, one empty member
		want[key] = b
		sealed, err := w.Add(ctx, tenant, key, int64(len(b)), bytes.NewReader(b))
		require.NoError(t, err)
		require.Nil(t, sealed, "nothing seals below the target")
	}
	assert.False(t, mustExists(t, s, tenant, "shard/0001"), "a member is not stored before its pack's commit")
	sealed, err := w.Flush(ctx)

	// Assert
	require.NoError(t, err)
	require.NotNil(t, sealed)
	assert.Equal(t, 300, sealed.Members)
	assert.Len(t, sealed.Stored, 300)
	assert.Equal(t, int64(1), be.puts.Load(), "the pack is uploaded ONCE")
	assert.Equal(t, []string{sealed.Name}, be.files(t))
	assert.Equal(t, 300, countRows(t, db, `SELECT COUNT(*) FROM pack_members WHERE tenant_id = $1 AND deleted_at IS NULL`, tenant))

	be.gets.Store(0)
	for key, b := range want {
		got, err := readMember(t, s, tenant, key)
		require.NoError(t, err, key)
		require.True(t, bytes.Equal(b, got), "member %s byte-exact", key)
		assert.True(t, mustExists(t, s, tenant, key))
	}
	assert.Zero(t, be.gets.Load(), "reads are byte ranges, never the whole pack")

	// a range inside a member
	b := want["shard/0100"]
	rc, err := s.GetRange(ctx, tenant, "shard/0100", 10, 100)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.Equal(t, b[10:110], got)

	_, err = s.GetRange(ctx, tenant, "shard/0100", int64(len(b))-1, 2)
	assert.Error(t, err, "a range past the member's end is refused")
}

func mustExists(t *testing.T, s *Store, tenant, key string) bool {
	t.Helper()
	ok, err := s.Exists(context.Background(), tenant, key)
	require.NoError(t, err)
	return ok
}

func TestStore_ReadsFallBackToAWholeGetWithoutRanges(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	plain := noRange{be} // hides GetRange
	s := newTestStore(t, db, plain, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	w, err := s.NewWriter()
	require.NoError(t, err)
	a, b := randBytes(t, 5000), randBytes(t, 7000)
	_, err = w.Add(ctx, tenant, "a", int64(len(a)), bytes.NewReader(a))
	require.NoError(t, err)
	_, err = w.Add(ctx, tenant, "b", int64(len(b)), bytes.NewReader(b))
	require.NoError(t, err)
	_, err = w.Flush(ctx)
	require.NoError(t, err)

	got, err := readMember(t, s, tenant, "b")
	require.NoError(t, err)
	assert.Equal(t, b, got)
}

type noRange struct{ d *testBackend }

func (n noRange) Name() string { return n.d.Name() }
func (n noRange) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	return n.d.Get(ctx, c, a)
}
func (n noRange) Put(ctx context.Context, c, a string, r io.Reader, o ...engine.PutOption) error {
	return n.d.Put(ctx, c, a, r, o...)
}
func (n noRange) Delete(ctx context.Context, c, a string) error { return n.d.Delete(ctx, c, a) }
func (n noRange) List(ctx context.Context, c, p string) ([]string, error) {
	return n.d.List(ctx, c, p)
}
func (n noRange) Exists(ctx context.Context, c, a string) (bool, error) { return n.d.Exists(ctx, c, a) }
func (n noRange) HealthCheck(ctx context.Context) error                 { return n.d.HealthCheck(ctx) }

func TestWriter_SealsAtTheTargetSizeAndTheMemberCount(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	ctx := context.Background()
	tenant := uniq("tenant")

	t.Run("size", func(t *testing.T) {
		s := newTestStore(t, db, be, func(o *Options) { o.TargetSize = 64 << 10 })
		w, err := s.NewWriter()
		require.NoError(t, err)
		var sealed []*SealedPack
		for i := 0; i < 20; i++ {
			b := randBytes(t, 10<<10)
			sp, err := w.Add(ctx, tenant, fmt.Sprintf("size/%d", i), int64(len(b)), bytes.NewReader(b))
			require.NoError(t, err)
			if sp != nil {
				sealed = append(sealed, sp)
			}
		}
		sp, err := w.Flush(ctx)
		require.NoError(t, err)
		sealed = append(sealed, sp)
		total := 0
		for _, p := range sealed {
			total += p.Members
			assert.LessOrEqual(t, p.Members, 6, "64 KiB / 10 KiB")
		}
		assert.Equal(t, 20, total)
		assert.GreaterOrEqual(t, len(sealed), 4)

		// a member above the target is refused (it goes whole, not packed)
		big := randBytes(t, 65<<10)
		_, err = w.Add(ctx, tenant, "big", int64(len(big)), bytes.NewReader(big))
		assert.ErrorIs(t, err, ErrMemberTooLarge)
	})

	t.Run("count", func(t *testing.T) {
		s := newTestStore(t, db, be, func(o *Options) { o.MaxMembers = 3 })
		w, err := s.NewWriter()
		require.NoError(t, err)
		seals := 0
		for i := 0; i < 7; i++ {
			sp, err := w.Add(ctx, tenant, fmt.Sprintf("count/%d", i), 1, bytes.NewReader([]byte{byte(i)}))
			require.NoError(t, err)
			if sp != nil {
				assert.Equal(t, 3, sp.Members)
				seals++
			}
		}
		sp, err := w.Flush(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, sp.Members)
		assert.Equal(t, 2, seals)
	})
}

func TestWriter_RefusesADuplicateKeyAndASizeMismatch(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	w, err := s.NewWriter()
	require.NoError(t, err)
	defer func() { _ = w.Close(ctx) }()

	_, err = w.Add(ctx, tenant, "k", 3, bytes.NewReader([]byte("abc")))
	require.NoError(t, err)
	_, err = w.Add(ctx, tenant, "k", 3, bytes.NewReader([]byte("abc")))
	assert.ErrorIs(t, err, ErrDuplicateKey)

	_, err = w.Add(ctx, tenant, "short", 10, bytes.NewReader([]byte("abc")))
	assert.ErrorIs(t, err, ErrSizeMismatch)
	_, err = w.Add(ctx, tenant, "long", 2, bytes.NewReader([]byte("abc")))
	assert.ErrorIs(t, err, ErrSizeMismatch)

	// the failed members left nothing behind: the pack holds "k" only
	sp, err := w.Flush(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, sp.Members)
	got, err := readMember(t, s, tenant, "k")
	require.NoError(t, err)
	assert.Equal(t, []byte("abc"), got)
	_, err = readMember(t, s, tenant, "short")
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- crash safety and GC ------------------------------------------------------

func TestStore_CrashBeforeCommitLeavesNoIndexRowsAndGCRemovesTheOrphan(t *testing.T) {
	// Arrange: the upload lands, the process "dies" before the commit.
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, func(o *Options) { o.IntentGrace = time.Nanosecond })
	s.beforeCommit = func() error { return errors.New("simulated crash") }
	ctx := context.Background()
	tenant := uniq("tenant")
	w, err := s.NewWriter()
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err := w.Add(ctx, tenant, fmt.Sprintf("k%d", i), 100, bytes.NewReader(randBytes(t, 100)))
		require.NoError(t, err)
	}

	// Act
	_, err = w.Flush(ctx)

	// Assert: no member is stored, the uploaded pack is an orphan
	require.Error(t, err)
	assert.Zero(t, countRows(t, db, `SELECT COUNT(*) FROM pack_members WHERE tenant_id = $1`, tenant))
	assert.False(t, mustExists(t, s, tenant, "k0"))
	_, err = readMember(t, s, tenant, "k0")
	assert.ErrorIs(t, err, ErrNotFound)
	require.Len(t, be.files(t), 1)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM packs WHERE backend = $1 AND sealed_at IS NULL`, be.Name()))

	// A pack file no row names at all (a wiped index) is an orphan too.
	require.NoError(t, be.Driver.Put(ctx, DefaultContainer, packName(fmt.Sprintf("%064x", 42)), bytes.NewReader([]byte("stray"))))
	// …and a file that is not a pack name is left alone.
	require.NoError(t, be.Driver.Put(ctx, DefaultContainer, "README.txt", bytes.NewReader([]byte("hi"))))

	s.beforeCommit = nil
	time.Sleep(5 * time.Millisecond)
	res, err := s.GC(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.IntentsExpired)
	assert.Equal(t, 1, res.OrphansDeleted)
	assert.Equal(t, []string{"README.txt"}, be.files(t))
	assert.Zero(t, countRows(t, db, `SELECT COUNT(*) FROM packs WHERE backend = $1`, be.Name()))

	// idempotent
	res, err = s.GC(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.IntentsExpired+res.OrphansDeleted+res.PacksRewritten+res.PacksDeleted)
}

func TestStore_AFailedUploadCanBeRetriedByTheSameWriter(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	w, err := s.NewWriter()
	require.NoError(t, err)
	b := randBytes(t, 4000)
	_, err = w.Add(ctx, tenant, "k", int64(len(b)), bytes.NewReader(b))
	require.NoError(t, err)

	be.failPut.Store(true)
	_, err = w.Flush(ctx)
	require.Error(t, err)
	be.failPut.Store(false)
	sp, err := w.Flush(ctx)
	require.NoError(t, err)
	require.NotNil(t, sp)
	got, err := readMember(t, s, tenant, "k")
	require.NoError(t, err)
	assert.Equal(t, b, got)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM packs WHERE backend = $1`, be.Name()))
}

// Sync's bridge reports a wrong modification time for every file (a file
// uploaded now lists as 1970-01-21 — 2026-10-07 live finding). Every age the
// store reasons about is a Postgres timestamp; a backend modtime is never
// read. A young in-flight pack and a live sealed pack survive GC.
func TestGC_ABackendReportingAn1970ModtimeDeletesNoYoungPack(t *testing.T) {
	// Arrange
	db := openTestDB(t)
	be := &epochBackend{testBackend: newTestBackend(t)}
	s := newTestStore(t, db, be, nil) // default IntentGrace (hours)
	ctx := context.Background()
	tenant := uniq("tenant")

	w, err := s.NewWriter()
	require.NoError(t, err)
	live := randBytes(t, 3000)
	_, err = w.Add(ctx, tenant, "live", int64(len(live)), bytes.NewReader(live))
	require.NoError(t, err)
	_, err = w.Flush(ctx)
	require.NoError(t, err)

	// an upload that has landed but whose commit has not happened yet
	s.beforeCommit = func() error { return errors.New("still committing") }
	w2, err := s.NewWriter()
	require.NoError(t, err)
	_, err = w2.Add(ctx, tenant, "inflight", 10, bytes.NewReader(randBytes(t, 10)))
	require.NoError(t, err)
	_, err = w2.Flush(ctx)
	require.Error(t, err)
	s.beforeCommit = nil
	require.Len(t, be.files(t), 2)

	// Act
	res, err := s.GC(ctx)

	// Assert
	require.NoError(t, err)
	assert.Zero(t, res.OrphansDeleted+res.IntentsExpired+res.PacksDeleted+res.PacksRewritten)
	assert.Len(t, be.files(t), 2, "both young packs are still on the backend")
	got, err := readMember(t, s, tenant, "live")
	require.NoError(t, err)
	assert.Equal(t, live, got)
	assert.Zero(t, be.modtimeCalls.Load(), "the backend's modtime is never consulted")
}

// epochBackend reports every file as modified in January 1970, as Sync's
// bridge does.
type epochBackend struct {
	*testBackend
	modtimeCalls atomic.Int64
}

func (e *epochBackend) ModTime(context.Context, string, string) (time.Time, error) {
	e.modtimeCalls.Add(1)
	return time.Date(1970, 1, 21, 17, 35, 39, 0, time.UTC), nil
}

func TestStore_TombstonesAndCompactionKeepLiveMembersAndFreeTheOldPack(t *testing.T) {
	// Arrange: ten members in one pack, six deleted (live < 50 %).
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	want := map[string][]byte{}
	w, err := s.NewWriter()
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("m%d", i)
		want[k] = randBytes(t, 2000+i)
		_, err := w.Add(ctx, tenant, k, int64(len(want[k])), bytes.NewReader(want[k]))
		require.NoError(t, err)
	}
	old, err := w.Flush(ctx)
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		require.NoError(t, s.Delete(ctx, tenant, fmt.Sprintf("m%d", i)))
	}
	require.NoError(t, s.Delete(ctx, tenant, "m0"), "a second delete is a no-op")
	require.NoError(t, s.Delete(ctx, tenant, "never-there"))
	assert.False(t, mustExists(t, s, tenant, "m0"))
	_, err = readMember(t, s, tenant, "m0")
	assert.ErrorIs(t, err, ErrNotFound)

	// Act
	res, err := s.GC(ctx)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 1, res.PacksRewritten)
	assert.Equal(t, 4, res.MembersMoved)
	assert.Equal(t, 1, res.PacksDeleted)
	files := be.files(t)
	require.Len(t, files, 1)
	assert.NotEqual(t, old.Name, files[0], "the old pack is gone, a new one holds the live members")
	assert.Zero(t, countRows(t, db, `SELECT COUNT(*) FROM packs WHERE backend = $1 AND name = $2`, be.Name(), old.Name))
	for i := 6; i < 10; i++ {
		k := fmt.Sprintf("m%d", i)
		got, err := readMember(t, s, tenant, k)
		require.NoError(t, err, k)
		assert.Equal(t, want[k], got, k)
	}
	for i := 0; i < 6; i++ {
		_, err := readMember(t, s, tenant, fmt.Sprintf("m%d", i))
		assert.ErrorIs(t, err, ErrNotFound)
	}

	// idempotent: the new pack is fully live
	res, err = s.GC(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.PacksRewritten+res.PacksDeleted)
}

func TestStore_ADeletedMemberIsPurgedAfterTheTombstoneAge(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, func(o *Options) { o.TombstoneMaxAge = time.Nanosecond })
	ctx := context.Background()
	tenant := uniq("tenant")
	w, err := s.NewWriter()
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		_, err := w.Add(ctx, tenant, fmt.Sprintf("m%d", i), 1000, bytes.NewReader(randBytes(t, 1000)))
		require.NoError(t, err)
	}
	_, err = w.Flush(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, tenant, "m0")) // 90 % still live
	time.Sleep(5 * time.Millisecond)

	res, err := s.GC(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.PacksRewritten, "a tombstone past its age forces the rewrite")
	assert.Equal(t, 9, res.MembersMoved)
}

// pack_members rows are tenant data: the account erasure deletes them
// (account.EraseRows). The pack's bytes still hold the member, so a pack whose
// rows no longer cover its member_count is rewritten at once, whatever its
// live ratio — the erased tenant's bytes leave the backend at the next GC.
func TestStore_ErasedMemberRowsForceARewriteOfTheirPack(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	erased, kept := uniq("erased"), uniq("kept")
	w, err := s.NewWriter()
	require.NoError(t, err)
	_, err = w.Add(ctx, erased, "x", 100, bytes.NewReader(randBytes(t, 100)))
	require.NoError(t, err)
	keep := randBytes(t, 9000)
	_, err = w.Add(ctx, kept, "y", int64(len(keep)), bytes.NewReader(keep))
	require.NoError(t, err)
	old, err := w.Flush(ctx)
	require.NoError(t, err)

	// the erasure's statement (internal/account/erase.go)
	_, err = db.ExecContext(ctx, `DELETE FROM pack_members WHERE tenant_id = $1`, erased)
	require.NoError(t, err)

	res, err := s.GC(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.PacksRewritten)
	files := be.files(t)
	require.Len(t, files, 1)
	assert.NotEqual(t, old.Name, files[0])
	idx, err := s.ReadPackIndex(ctx, files[0])
	require.NoError(t, err)
	require.Len(t, idx.Members, 1)
	assert.Equal(t, kept, idx.Members[0].Tenant, "the new pack's footer no longer names the erased tenant")
	got, err := readMember(t, s, kept, "y")
	require.NoError(t, err)
	assert.Equal(t, keep, got)
}

// --- integrity and recovery ---------------------------------------------------

func TestStore_ACorruptedMemberFailsItsSHA256(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	w, err := s.NewWriter()
	require.NoError(t, err)
	a, b := randBytes(t, 4096), randBytes(t, 4096)
	_, err = w.Add(ctx, tenant, "a", int64(len(a)), bytes.NewReader(a))
	require.NoError(t, err)
	_, err = w.Add(ctx, tenant, "b", int64(len(b)), bytes.NewReader(b))
	require.NoError(t, err)
	sp, err := w.Flush(ctx)
	require.NoError(t, err)

	// flip one byte in the middle of member "a" on the backend
	path := filepath.Join(be.base, DefaultContainer, sp.Name)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw[HeaderSize+2000] ^= 0x01
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	_, err = readMember(t, s, tenant, "a")
	assert.ErrorIs(t, err, ErrCorrupt)
	got, err := readMember(t, s, tenant, "b")
	require.NoError(t, err, "the neighbour is intact")
	assert.Equal(t, b, got)
}

func TestStore_TheIndexIsRecoverableFromThePackAlone(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	want := map[string][]byte{}
	w, err := s.NewWriter()
	require.NoError(t, err)
	for i := 0; i < 25; i++ {
		k := fmt.Sprintf("r/%d", i)
		want[k] = randBytes(t, 100*i)
		_, err := w.Add(ctx, tenant, k, int64(len(want[k])), bytes.NewReader(want[k]))
		require.NoError(t, err)
	}
	sp, err := w.Flush(ctx)
	require.NoError(t, err)

	// the index is lost
	_, err = db.ExecContext(ctx, `DELETE FROM packs WHERE backend = $1`, be.Name())
	require.NoError(t, err)
	_, err = readMember(t, s, tenant, "r/3")
	require.ErrorIs(t, err, ErrNotFound)

	// Act
	n, err := s.Recover(ctx, sp.Name)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 25, n)
	for k, b := range want {
		got, err := readMember(t, s, tenant, k)
		require.NoError(t, err, k)
		assert.Equal(t, b, got, k)
	}
	n, err = s.Recover(ctx, sp.Name)
	require.NoError(t, err)
	assert.Zero(t, n, "recovery is idempotent")

	// a name whose bytes do not hash to it is refused
	require.NoError(t, be.Driver.Put(ctx, DefaultContainer, packName(fmt.Sprintf("%064x", 7)), bytes.NewReader([]byte("not a pack"))))
	_, err = s.Recover(ctx, packName(fmt.Sprintf("%064x", 7)))
	assert.ErrorIs(t, err, ErrBadPack)
}

// --- concurrency --------------------------------------------------------------

func TestStore_TwoWritersAtOnce(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, func(o *Options) { o.MaxMembers = 17 })
	ctx := context.Background()
	tenant := uniq("tenant")

	var mu sync.Mutex
	versions := map[string][][]byte{}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for wi := 0; wi < 2; wi++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			w, err := s.NewWriter()
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = w.Close(ctx) }()
			for i := 0; i < 60; i++ {
				key := fmt.Sprintf("own/%d/%d", wi, i)
				if i%6 == 0 {
					key = fmt.Sprintf("shared/%d", i) // both writers write these
				}
				b := make([]byte, 500+i)
				_, _ = rand.Read(b)
				mu.Lock()
				versions[key] = append(versions[key], b)
				mu.Unlock()
				if _, err := w.Add(ctx, tenant, key, int64(len(b)), bytes.NewReader(b)); err != nil {
					errs <- err
					return
				}
			}
			if _, err := w.Flush(ctx); err != nil {
				errs <- err
			}
		}(wi)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// exactly one live row per key, and it reads back as one of the versions
	assert.Equal(t, len(versions), countRows(t, db,
		`SELECT COUNT(*) FROM pack_members WHERE tenant_id = $1 AND deleted_at IS NULL`, tenant))
	for key, vs := range versions {
		got, err := readMember(t, s, tenant, key)
		require.NoError(t, err, key)
		match := false
		for _, v := range vs {
			match = match || bytes.Equal(v, got)
		}
		assert.True(t, match, key)
	}
	// live_bytes agrees with the rows
	var drift int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM packs p
		 WHERE p.backend = $1 AND p.live_bytes <> COALESCE((SELECT SUM(byte_length) FROM pack_members m
		                                                    WHERE m.pack_id = p.id AND m.deleted_at IS NULL), 0)`,
		be.Name()).Scan(&drift))
	assert.Zero(t, drift)
}

func TestWriter_AFullFolderIsRefused(t *testing.T) {
	db := openTestDB(t)
	be := newTestBackend(t)
	s := newTestStore(t, db, be, func(o *Options) { o.MaxPerFolder = 1 })
	ctx := context.Background()
	// one pack row in each of the 256 folders
	for i := 0; i < 256; i++ {
		sum := fmt.Sprintf("%02x%062x", i, i)
		_, err := db.ExecContext(ctx, `INSERT INTO packs (backend, name, folder, size, sha256, member_count, sealed_at)
			VALUES ($1, $2, $3, 1, $4, 0, NOW())`, be.Name(), packName(sum), packFolder(sum), sum)
		require.NoError(t, err)
	}
	w, err := s.NewWriter()
	require.NoError(t, err)
	_, err = w.Add(ctx, uniq("tenant"), "k", 1, bytes.NewReader([]byte{1}))
	require.NoError(t, err)
	_, err = w.Flush(ctx)
	assert.ErrorIs(t, err, ErrFolderFull)
	assert.Equal(t, int64(0), be.puts.Load(), "nothing is uploaded into a full folder")
}
