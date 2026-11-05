package api

// RP98 — the review of WP-XFER-1 (docs/reviews/WP-STRIPE-11-REVIEW.md): the
// transfer job's findings fixed by the review. Each was proven on origin/main
// first by a fork's probe asserting the wrong behaviour (the probes are not
// committed); these are the same scenarios asserting the right one.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

func transferItemState(f *transferFixture, id, key string) (state string, attempts int, lastError string) {
	f.t.Helper()
	require.NoError(f.t, f.db.QueryRow(`SELECT state, attempts, last_error FROM transfer_items WHERE job_id = $1 AND source_key = $2`,
		id, key).Scan(&state, &attempts, &lastError))
	return
}

func (f *transferFixture) setRate(id string, bps int64) {
	f.t.Helper()
	_, err := f.db.Exec(`UPDATE transfer_jobs SET rate_bytes_per_sec = $2 WHERE id = $1`, id, bps)
	require.NoError(f.t, err)
}

// RP98-1: an object the job's rate cannot move inside one run was read again
// from its first byte every run, for ever — its attempts never counted, every
// item behind it waiting. It fails at once, before a byte of it is read, and
// the job goes on.
func TestTransfer_AnObjectOneRunCannotCopyFailsAtOnceUnread(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("a-big.bin", testBytes(6<<20), "", nil)
	f.src.put("b-small.bin", testBytes(1000), "", nil)
	v := f.create("")
	f.setRate(v.ID, 1<<20) // 6 MiB at 1 MiB/s: six seconds
	f.job.maxRun = 2 * time.Second

	done := f.runUntilDone(v.ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(1), done.Failed)
	assert.Equal(t, int64(1), done.Copied, "the item behind it was copied")
	st, _, why := transferItemState(f, v.ID, "a-big.bin")
	assert.Equal(t, itemFailed, st)
	assert.Contains(t, why, "cannot be copied inside one run")
	assert.Zero(t, f.src.getsOf("a-big.bin"), "not a byte of it was read")
	_, ok := f.head("a-big.bin")
	assert.False(t, ok)
}

// RP98-1: a copy the run's end cuts (the source serves it too slowly for one
// run) is a try when it had the whole run; the third fails the item and the
// job goes on. It used to stay pending with attempts 0 for ever.
func TestTransfer_ACopyCutAtTheRunsEndIsATryThenAFailure(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("a-slow.bin", testBytes(4000), "", nil)
	f.src.put("b-fine.bin", testBytes(1000), "", nil)
	inner := f.src.srv.Config.Handler
	f.src.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "a-slow.bin") {
			select { // a source that does not answer inside the run
			case <-time.After(20 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		inner.ServeHTTP(w, r)
	})
	v := f.create("")
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond) // the scheduler's MaxRunTime
		defer cancel()
		_, err := f.job.RunOnce(ctx)
		require.NoError(t, err)
	}
	run()
	st, attempts, why := transferItemState(f, v.ID, "a-slow.bin")
	assert.Equal(t, itemPending, st)
	assert.Equal(t, 1, attempts, "the cut is a try")
	assert.Contains(t, why, "did not finish inside one run")
	run()
	run()
	st, _, why = transferItemState(f, v.ID, "a-slow.bin")
	assert.Equal(t, itemFailed, st, "the third cut fails the item")
	assert.Contains(t, why, "were not copied inside one run in 3 tries")

	done := f.runUntilDone(v.ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(1), done.Copied, "the job went on to the item behind it")
	assert.Equal(t, int64(1), done.Failed)
}

// RP98-1 (the other shape): the token bucket refuses a wait that would cross
// the run's deadline BEFORE the deadline; the reader's error was not the
// source's, so the PUT's 400 IncompleteBody read as the destination's answer
// and the item failed for good on its first cut.
func TestTransfer_ACopyTheRateCutsIsNotTheDestinationsRefusal(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("a.bin", testBytes(3<<20), "", nil)
	v := f.create("")
	f.setRate(v.ID, 1<<20) // three seconds of copy
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	_, err := f.job.RunOnce(ctx)
	cancel()
	require.NoError(t, err)
	st, attempts, why := transferItemState(f, v.ID, "a.bin")
	assert.Equal(t, itemPending, st, "cut, not failed: %s", why)
	assert.Equal(t, 1, attempts)
	assert.NotContains(t, f.view(v.ID).LastError, "IncompleteBody")
	// A run with the time copies it.
	done := f.runUntilDone(v.ID)
	assert.Equal(t, int64(1), done.Copied)
	assert.Zero(t, done.Failed)
}

// transferFailingDriver fails every Put while on.
type transferFailingDriver struct {
	*edgeFakeDriver
	on atomic.Bool
}

func (d *transferFailingDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	if d.on.Load() {
		_, _ = io.Copy(io.Discard, data)
		return errors.New("backend down")
	}
	return d.edgeFakeDriver.Put(ctx, container, artifact, data, opts...)
}

// RP98-2: the tries of a transient failure followed each other in the same
// second — one run with OUR backend down failed every item for good and the
// job said completed. A try is now a run apart, and a destination's 5xx has
// thirty of them.
func TestTransfer_AnOutageOfOursFailsNoItem(t *testing.T) {
	f := setupTransferFixture(t)
	boom := &transferFailingDriver{edgeFakeDriver: f.drv}
	boom.on.Store(true)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	require.NoError(t, eng.AddDriver("edgefake", boom))
	eng.SetPrimary("edgefake")
	f.srv.engine = eng
	for i := 0; i < 5; i++ {
		f.src.put(fmt.Sprintf("o-%d.bin", i), testBytes(1000), "", nil)
	}
	v := f.create("")
	for i := 0; i < 4; i++ { // four runs of the outage
		_, err := f.job.RunOnce(context.Background())
		require.NoError(t, err)
	}
	got := f.view(v.ID)
	assert.Equal(t, transferRunning, got.State)
	assert.Zero(t, got.Failed, "an outage of ours fails no item")
	assert.Contains(t, got.LastError, "tried again next run")
	st, attempts, _ := transferItemState(f, v.ID, "o-0.bin")
	assert.Equal(t, itemPending, st)
	assert.Equal(t, 4, attempts, "one try a run")
	assert.Equal(t, 4, f.src.getsOf("o-0.bin"), "one read of the source a run, not three a second")
	assert.Zero(t, f.src.getsOf("o-1.bin"), "the items behind it wait their turn")

	boom.on.Store(false) // the backend is back
	done := f.runUntilDone(v.ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(5), done.Copied)
	assert.Zero(t, done.Failed)
}

// RP98-2: a source's transient failure keeps its three tries (the prompt's
// rule), a run apart.
func TestTransfer_ASourcesTransientFailureIsTriedARunApart(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("flaky.bin", testBytes(1000), "", nil)
	f.src.put("z.bin", testBytes(1000), "", nil)
	f.src.mu.Lock()
	f.src.fail5xx["flaky.bin"] = 1000
	f.src.mu.Unlock()
	v := f.create("")
	_, err := f.job.RunOnce(context.Background())
	require.NoError(t, err)
	st, attempts, _ := transferItemState(f, v.ID, "flaky.bin")
	assert.Equal(t, itemPending, st, "one run is one try")
	assert.Equal(t, 1, attempts)
	done := f.runUntilDone(v.ID)
	assert.Equal(t, int64(1), done.Failed, "the third try fails it")
	assert.Equal(t, int64(1), done.Copied, "and the job goes on")
}

// turnOf is the turn that makes first go first of the two tenants (the due
// list is in tenant order).
func turnOf(first, other string) int {
	if first < other {
		return 0
	}
	return 1
}

// RP98-3: the oldest job took every run whole — no other tenant's job began
// until it had finished. Every due tenant gets a slice of a run.
func TestTransfer_EveryTenantsJobGetsASliceOfARun(t *testing.T) {
	a := setupTransferFixture(t)
	b := setupTransferFixture(t)
	for i := 0; i < 12; i++ {
		a.src.put(fmt.Sprintf("o-%02d.bin", i), testBytes(1<<20), "", nil)
	}
	b.src.put("tiny.bin", testBytes(10), "", nil)
	va := a.create("")
	_, err := a.db.Exec(`UPDATE transfer_jobs SET rate_bytes_per_sec = $2, created_at = NOW() - interval '1 hour' WHERE id = $1`, va.ID, 1<<20)
	require.NoError(t, err)
	vb := b.create("")
	a.job.onlyTenant, a.job.onlyTenants = "", []string{a.tenantID, b.tenantID}
	a.job.runTime, a.job.minSlice = 2*time.Second, 100*time.Millisecond
	a.job.turn = func(int) int { return turnOf(a.tenantID, b.tenantID) } // A's job first, as before the fix
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = a.job.RunOnce(ctx)
	require.NoError(t, err)

	ga, gb := a.view(va.ID), b.view(vb.ID)
	assert.Equal(t, transferRunning, ga.State, "A's twelve seconds of copying are not done")
	assert.Equal(t, transferCompleted, gb.State, "B's job ran in the same run")
	assert.Equal(t, int64(1), gb.Copied)
}

// RP98-3: the first slice rotates, so no tenant is always behind another.
func TestTransfer_TheFirstSliceRotates(t *testing.T) {
	a := setupTransferFixture(t)
	b := setupTransferFixture(t)
	for i := 0; i < 6; i++ {
		a.src.put(fmt.Sprintf("o-%02d.bin", i), testBytes(1<<20), "", nil)
		b.src.put(fmt.Sprintf("o-%02d.bin", i), testBytes(1<<20), "", nil)
	}
	va := a.create("")
	_, err := a.db.Exec(`UPDATE transfer_jobs SET rate_bytes_per_sec = $2, created_at = NOW() - interval '1 hour' WHERE id = $1`, va.ID, 1<<20)
	require.NoError(t, err)
	vb := b.create("")
	b.setRate(vb.ID, 1<<20)
	a.job.onlyTenant, a.job.onlyTenants = "", []string{a.tenantID, b.tenantID}
	a.job.runTime = time.Second                                          // one slice's worth: only the first tenant of the run works
	a.job.turn = func(int) int { return turnOf(b.tenantID, a.tenantID) } // B's job first
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = a.job.RunOnce(ctx)
	require.NoError(t, err)
	assert.NotNil(t, b.view(vb.ID).StartedAt, "the turn made the younger job first")
	assert.Nil(t, a.view(va.ID).StartedAt)

	j := newTransferJob(a.srv)
	for n := 1; n <= 5; n++ {
		got := j.turn(n)
		assert.True(t, got >= 0 && got < n, "the clock's turn is inside the list")
	}
}

// RP98-4: a source — or a proxy before it — that answers a ranged GET with
// the whole object was spliced: the first n bytes of every answer taken as
// [off, off+n), stored as `copied` wherever no MD5 could catch it. The
// answer's Content-Range must be the span asked.
func TestTransfer_ASourceThatIgnoresRangeFailsTheItemNeverSplices(t *testing.T) {
	f := setupTransferFixture(t)
	prior := transferRangeBytes
	transferRangeBytes = 1 << 20
	t.Cleanup(func() { transferRangeBytes = prior })
	body := make([]byte, 3<<20)
	for i := range body {
		body[i] = byte(i >> 20) // each range its own byte
	}
	f.src.putMultipartETag("big.bin", body) // not an MD5: no Content-MD5 on the PUT
	inner := f.src.srv.Config.Handler
	f.src.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" && !strings.HasPrefix(rg, "bytes=0-") && strings.HasSuffix(r.URL.Path, "big.bin") {
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef-3"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		inner.ServeHTTP(w, r)
	})
	v := f.create("")
	done := f.runUntilDone(v.ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Zero(t, done.Copied)
	assert.Equal(t, int64(1), done.Failed)
	st, _, why := transferItemState(f, v.ID, "big.bin")
	assert.Equal(t, itemFailed, st)
	assert.Contains(t, why, "not the byte range asked")
	_, ok := f.head("big.bin")
	assert.False(t, ok, "nothing of it is at the destination")
}

func TestTransfer_RangeAnswered(t *testing.T) {
	for _, c := range []struct {
		name, header  string
		off, n, total int64
		wantErr       bool
	}{
		{"the span asked", "bytes 16-31/100", 16, 16, 100, false},
		{"the last, short span", "bytes 96-99/100", 96, 4, 100, false},
		{"no header: a 200 with the whole object", "", 16, 16, 100, true},
		{"another span", "bytes 0-15/100", 16, 16, 100, true},
		{"a shorter span", "bytes 16-20/100", 16, 16, 100, true},
		{"an object of another size than listed", "bytes 16-31/200", 16, 16, 100, true},
		{"an unknown total", "bytes 16-31/*", 16, 16, 100, true},
		{"not a range", "items 1-2/3", 16, 16, 100, true},
		{"an empty object's probe: the total is not asked", "bytes 0-0/1", 0, 1, -1, false},
	} {
		err := rangeAnswered(c.header, c.off, c.n, c.total)
		assert.Equal(t, c.wantErr, err != nil, c.name)
		if err != nil {
			assert.ErrorIs(t, err, errTransferWrongRange, c.name)
		}
	}
}

// RP98-6: a listing page of nothing but folder markers read as "listed all":
// the job completed with every object behind the markers never listed.
func TestTransfer_APageOfFolderMarkersDoesNotEndTheListing(t *testing.T) {
	f := setupTransferFixture(t)
	for i := 0; i < transferListPage; i++ {
		f.src.put(fmt.Sprintf("d/%04d/", i), nil, "", nil)
	}
	f.src.put("z-real.bin", testBytes(100), "", nil)
	done := f.runUntilDone(f.create("").ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(1), done.Copied)
	_, ok := f.head("z-real.bin")
	assert.True(t, ok, "the object behind the markers was listed and copied")
}

// RP98-11: the API took the system bucket `_exports` as a destination (the
// dashboard's select leaves it out): objects in a bucket no route lists.
func TestTransfer_TheSystemBucketIsNoDestination(t *testing.T) {
	f := setupTransferFixture(t)
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, '_exports') ON CONFLICT DO NOTHING`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = '_exports'`, f.tenantID) })
	f.src.put("a.bin", testBytes(10), "", nil)
	var in transferCreate
	in.Source.Endpoint, in.Source.Bucket = f.src.srv.URL, "src-bucket"
	in.Source.AccessKey, in.Source.SecretKey = f.src.keyID, "secret"
	in.Bucket = "_exports"
	_, status, _, code, _, _ := f.srv.createTransfer(context.Background(), f.tenantID, f.userID, in)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "bucket_not_found", code)
	rec := f.srv.transferS3(context.Background(), f.tenantID, http.MethodPut, "_exports", "a.bin", nil, http.Header{}, strings.NewReader("x"), 1)
	assert.Equal(t, http.StatusNotFound, rec.status, "and the job's own door refuses it")
}
