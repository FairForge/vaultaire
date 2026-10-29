package dataplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mirror leg that stops reading the body is cut loose once its buffer is
// full: the primary gets every byte and answers; the mirror reports no ETag.
func TestPutTwo_AStalledMirrorIsCutLooseAndThePrimaryFinishes(t *testing.T) {
	old := mirrorMaxLag
	mirrorMaxLag = 2 << 20
	defer func() { mirrorMaxLag = old }()

	var got []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1)
		_, _ = r.Body.Read(b)
		<-release // never reads the rest
	}))
	defer mirror.Close()
	defer close(release)

	body := bytes.Repeat([]byte("x"), 8<<20)
	start := time.Now()
	resp, mr, err := putTwoInline(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body), nil)
	mirrorETag, cut := mr.ETag, mr.Cut
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, `"p"`, resp.Header.Get("ETag"))
	assert.Equal(t, body, got, "every byte to the primary")
	assert.Empty(t, mirrorETag)
	assert.True(t, cut)
	assert.Less(t, time.Since(start), 10*time.Second)
	mirrorMaxLag = old // the healthy half runs with the real buffer

	// Both healthy: both answer, the same bytes.
	var gotB []byte
	mirror2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"m"`)
	}))
	defer mirror2.Close()
	resp, mr, err = putTwoInline(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror2.URL}}, int64(len(body)), bytes.NewReader(body), nil)
	mirrorETag, cut = mr.ETag, mr.Cut
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, `"m"`, mirrorETag)
	assert.False(t, cut)
	assert.Equal(t, body, gotB)
}

// RP76-2: a mirror slower than the primary trails it by at most its
// buffer (mirrorMaxLag) and is cut there; the primary's writes never wait
// for it, so the primary receives every byte at its own pace.
func TestPutTwo_TheMirrorNeverHoldsThePrimaryPastItsBuffer(t *testing.T) {
	var got []byte
	var primaryDone time.Time
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		primaryDone = time.Now()
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 4 MiB/s: 16 s for the 64 MiB body.
		b := make([]byte, 64<<10)
		for {
			_, err := r.Body.Read(b)
			if err != nil {
				return
			}
			time.Sleep(16 * time.Millisecond)
		}
	}))
	defer mirror.Close()

	body := bytes.Repeat([]byte("y"), 64<<20)
	start := time.Now()
	resp, mr, err := putTwoInline(context.Background(), NewBackendClient(30*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body), nil)
	mirrorETag, cut := mr.ETag, mr.Cut
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, body, got, "every byte to the primary")
	assert.Less(t, primaryDone.Sub(start), 8*time.Second, "the primary kept its own pace (the mirror's is 16 s)")
	assert.True(t, cut, "the mirror is cut once it trails by its buffer")
	assert.Empty(t, mirrorETag)
}

// RP76-2: a mirror that took every byte but does not answer is given
// mirrorAfter past the primary's answer, then cut.
func TestPutTwo_AMirrorThatDoesNotAnswerIsCutAfterThePrimary(t *testing.T) {
	old := mirrorAfter
	mirrorAfter = 200 * time.Millisecond
	defer func() { mirrorAfter = old }()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer mirror.Close()
	defer close(release)

	body := bytes.Repeat([]byte("z"), 1<<20)
	start := time.Now()
	resp, mr, err := putTwoInline(context.Background(), NewBackendClient(30*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body), nil)
	mirrorETag, cut := mr.ETag, mr.Cut
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.True(t, cut)
	assert.Empty(t, mirrorETag)
}

// RP78-1: a mirrored PUT takes its mirror's buffer from the host's budget
// before the tee; a budget with nothing left sends the PUT unmirrored — one
// PUT, no mirror request, no error (the copy is the job's).
func TestPutTwo_NoBudgetNoMirror(t *testing.T) {
	var got []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	var mirrorCalls atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer mirror.Close()

	budget := NewByteBudget(int64(mirrorMaxLag) + mirrorBudgetSlack)
	hold, err := budget.Acquire(context.Background(), budget.Limit()) // nothing left
	require.NoError(t, err)
	defer hold()

	body := bytes.Repeat([]byte("n"), 2<<20)
	start := time.Now()
	resp, run, err := putTwo(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body), budget)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, body, got, "the primary's PUT, whole")
	require.NotNil(t, run)
	assert.True(t, run.unbudgeted, "no budget: unmirrored")
	assert.Zero(t, mirrorCalls.Load(), "no mirror request")
	assert.Less(t, time.Since(start), 2*time.Second, "the wait for the budget is short")

	// With the budget free, the mirror takes it and gives it back at its end.
	hold()
	resp, run, err = putTwo(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body), budget)
	require.NoError(t, err)
	_ = resp.Body.Close()
	<-run.done
	assert.False(t, run.unbudgeted)
	assert.EqualValues(t, 1, mirrorCalls.Load())
	assert.Zero(t, budget.InUse(), "released when the mirror ended")
}

// RP78-2: the primary's answer is the write's; the mirror settles after.
// A mirror at 2 MB/s still running when the primary answered: putTwo
// returns at the primary's pace, the run finishes later with its ETag.
func TestPutTwo_ThePrimarysAnswerDoesNotWaitForTheMirror(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64<<10)
		for {
			if _, err := r.Body.Read(b); err != nil {
				break
			}
			time.Sleep(30 * time.Millisecond) // ~2 MB/s
		}
		w.Header().Set("ETag", `"m"`)
	}))
	defer mirror.Close()
	budget := NewByteBudget(64 << 20)
	body := bytes.Repeat([]byte("s"), 4<<20) // under the lag bound: never cut
	start := time.Now()
	resp, run, err := putTwo(context.Background(), NewBackendClient(30*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body), budget)
	require.NoError(t, err)
	_ = resp.Body.Close()
	answered := time.Since(start)
	assert.Less(t, answered, time.Second, "the primary's pace (the mirror's is ~2 s)")
	assert.False(t, run.finished(), "the mirror is still running")
	assert.Positive(t, budget.InUse(), "its buffer stays charged until it ends")
	<-run.done
	etag, cut := run.result()
	assert.Equal(t, `"m"`, etag)
	assert.False(t, cut)
	assert.Greater(t, time.Since(start), answered+time.Second/2, "the mirror settled after")
	assert.Zero(t, budget.InUse())
}
