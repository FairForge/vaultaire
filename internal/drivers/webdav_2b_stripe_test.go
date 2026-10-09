package drivers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b Part B — striping. Each scenario is driven through the fake
// bridges' handlers, so the same test runs on 3fd261c/0b376cf for the
// "before" numbers.

// clocked is a striped driver on a settable clock.
type clocked struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clocked) get() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clocked) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// reapDuringLastPiece: when the upload's last piece has been stored and checked, the
// clock jumps past the 6 h grace (a long upload) and the reaper runs —
// between the last piece and the commit.
func reapDuringLastPiece(t *testing.T, heartbeat time.Duration) (m *MultiWebDAVDriver, body []byte, putErr error) {
	var mref atomic.Pointer[MultiWebDAVDriver]
	clock := &clocked{now: time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)}
	var once sync.Once
	bs := newBridges(t, 2, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r)
			// After the last piece's own size check (its PROPFIND), before
			// anything the commit does.
			if r.Method == "PROPFIND" && strings.HasSuffix(r.URL.Path, "/p00002%o") {
				once.Do(func() {
					clock.add(7 * time.Hour)
					time.Sleep(60 * time.Millisecond) // heartbeats (if any) at the new time
					_, _ = mref.Load().ReapOrphanStripes(context.Background(), WebDAVDefaultStripeGrace)
				})
			}
		})
	})
	m = newStriped(t, bs, func(c *WebDAVConfig) { c.LargeConcurrency = 1 })
	m.now = clock.get
	setStripeHeartbeat(m, heartbeat)
	mref.Store(m)
	ctx := davCtx("tenant-a")
	body = randBody(t, 3*testPiece)
	putErr = m.Put(ctx, "bucket", "long", onlyReader{strings.NewReader(string(body))}, withLen(len(body)))
	return m, body, putErr
}

func TestStripe_AReapBeforeTheCommitNeverReturnsSuccessForAnUnreadableObject(t *testing.T) {
	// Arrange + Act: the reaper runs between the last piece and the commit,
	// the upload's heartbeat silent (as if every beat failed). Before: the
	// generation was reaped, the manifest written, Put returned nil and the
	// GET failed.
	m, body, err := reapDuringLastPiece(t, 0)

	// Assert: never nil-and-unreadable.
	if err == nil {
		got, gerr := m.Get(davCtx("tenant-a"), "bucket", "long")
		require.NoError(t, gerr, "Put said yes, so the object must read")
		assert.Equal(t, body, readAllClose(t, got))
		return
	}
	_, gerr := m.Get(davCtx("tenant-a"), "bucket", "long")
	assert.Error(t, gerr, "no manifest was committed")
}

func TestStripe_AHeartbeatKeepsALongUploadFromTheReaper(t *testing.T) {
	// Before (no heartbeat): the upload's pieces were reaped; Put nil, GET failed.
	m, body, err := reapDuringLastPiece(t, 5*time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, body, readAllClose(t, mustGetM(davCtx("tenant-a"), t, m, "bucket", "long")))
}

func TestStripe_AManifestStoredButAnswered500IsCommitted(t *testing.T) {
	// Arrange: v1 stored; then v2's manifest PUT is stored by the bridge,
	// which answers 500 (every attempt). Before: the upload dropped v2's
	// pieces while v2's manifest stayed — and v1's manifest was gone: both
	// versions lost, the GET failed.
	var fail500 atomic.Bool
	bs := newBridges(t, 2, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && fail500.Load() && strings.HasSuffix(r.URL.Path, "/k%s") {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r) // stored
				http.Error(w, "upstream timeout", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	v1, v2 := randBody(t, 3*testPiece), randBody(t, 2*testPiece+5)
	putBytes(ctx, t, m, "bucket", "k", v1)
	fail500.Store(true)

	// Act
	err := m.Put(ctx, "bucket", "k", onlyReader{strings.NewReader(string(v2))}, withLen(len(v2)))
	fail500.Store(false)

	// Assert: the stored manifest is the commit — v2 reads back.
	require.NoError(t, err)
	reader := newStriped(t, bs, nil) // no cache
	assert.Equal(t, v2, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))
}

func TestStripe_PlainAndStripedWritesOfOneKeyNeverLoseTheObject(t *testing.T) {
	// Arrange: the striped commit is held between its manifest write and
	// its removal of the plain file, while a plain write of the same key
	// runs (up to 500 ms). Before: the plain write stored its file and
	// removed the manifest; the striped commit then removed the plain file
	// — both 200, the object 404.
	var hold atomic.Bool
	plainDone := make(chan struct{})
	var m *MultiWebDAVDriver
	var plainErr error
	var started sync.WaitGroup
	small := randBody(t, 100)
	bs := newBridges(t, 2, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/k%o") && hold.CompareAndSwap(true, false) {
				started.Add(1)
				go func() {
					defer started.Done()
					plainErr = m.Put(davCtx("tenant-a"), "bucket", "k", onlyReader{strings.NewReader(string(small))}, withLen(len(small)))
					close(plainDone)
				}()
				select {
				case <-plainDone:
				case <-time.After(500 * time.Millisecond):
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	m = newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	big := randBody(t, 3*testPiece)
	hold.Store(true)

	// Act
	stripedErr := m.Put(ctx, "bucket", "k", onlyReader{strings.NewReader(string(big))}, withLen(len(big)))
	started.Wait()

	// Assert: both succeeded, and the key reads as one of them (the last).
	require.NoError(t, stripedErr)
	require.NoError(t, plainErr)
	reader := newStriped(t, bs, nil)
	got := readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k"))
	assert.True(t, string(got) == string(small) || string(got) == string(big), "one whole version, %d bytes", len(got))
}

func TestStripe_BothFilesAfterAnInterruptedCommitServeTheRecordedSize(t *testing.T) {
	// Arrange: a striped commit stopped after its manifest (the plain file
	// of the previous version still there). The caller knows the committed
	// size (the head row). Before: GET served the plain file — the OLD
	// bytes under the new ETag.
	bs := newBridges(t, 2, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	old, cur := randBody(t, 100), randBody(t, 3*testPiece)
	putBytes(ctx, t, m, "bucket", "k", cur)
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/bucket/k%o", string(old)) // the leftover
	reader := newStriped(t, bs, nil)

	got := readAllClose(t, mustGetM(expectSize(ctx, int64(len(cur))), t, reader, "bucket", "k"))

	assert.Equal(t, len(cur), len(got))
	assert.Equal(t, cur, got)
}

func TestStripe_AnOverwriteLetsARunningReadOfTheOldVersionFinish(t *testing.T) {
	// Arrange: a GET of v1 has read its first bytes; v2 overwrites it.
	// Before: v1's pieces were deleted at the commit and the running read
	// failed at its next piece.
	bs := newBridges(t, 2, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	v1, v2 := randBody(t, 4*testPiece), randBody(t, 3*testPiece)
	putBytes(ctx, t, m, "bucket", "k", v1)
	rc := mustGetM(ctx, t, m, "bucket", "k")
	first := make([]byte, 100)
	_, err := io.ReadFull(rc, first)
	require.NoError(t, err)

	// Act
	putBytes(ctx, t, m, "bucket", "k", v2)
	rest, err := io.ReadAll(rc)
	_ = rc.Close()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, v1, append(first, rest...))
	assert.Equal(t, v2, readAllClose(t, mustGetM(ctx, t, m, "bucket", "k")))
}

func TestStripe_BootSweepsOnlyDeadProcessesStaging(t *testing.T) {
	// Before: one shared folder; nothing was ever cleaned after a kill.
	dir := t.TempDir()
	dead := filepath.Join(dir, "p999999") // no such process
	live := filepath.Join(dir, fmt.Sprintf("p%d", os.Getppid()))
	for _, d := range []string{dead, live} {
		require.NoError(t, os.MkdirAll(d, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(d, "piece-1"), []byte("x"), 0o600))
	}
	staleRoot := filepath.Join(dir, "piece-old")
	require.NoError(t, os.WriteFile(staleRoot, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(staleRoot, time.Now().Add(-8*time.Hour), time.Now().Add(-8*time.Hour)))
	freshRoot := filepath.Join(dir, "piece-new")
	require.NoError(t, os.WriteFile(freshRoot, []byte("x"), 0o600))

	newStriped(t, newBridges(t, 1, true, nil), func(c *WebDAVConfig) { c.StagingDir = dir })

	assert.NoDirExists(t, dead, "a dead process's staging")
	assert.FileExists(t, filepath.Join(live, "piece-1"), "another live process's (the other slot's) is kept")
	assert.NoFileExists(t, staleRoot)
	assert.FileExists(t, freshRoot, "a recent file of the old shared layout may be in flight")
}
