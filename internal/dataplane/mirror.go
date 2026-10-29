package dataplane

// The mirror (WP-EDGE-4, Stage 1a Prompt 48): a mirrored write's body goes
// to two links at once — the placement's backend and the mirror leg — read
// ONCE from the client and teed into two request bodies (two io.Pipes). The
// primary decides the write: a mirror that fails or trails the primary by
// more than its buffer (mirrorMaxLag, RP76-2) is cut loose (its request
// cancelled, its pipe closed) and the primary goes on alone; the commit
// then reports no mirror ETag (and mirror_cut) and the copy is the
// blob_copies job's. A primary that fails ends both. The primary's writes
// never wait for the mirror: the mirror is fed from its own buffer.
//
// The buffer is the host's bytes (RP78-1): putTwo takes mirrorMaxLag +
// mirrorBudgetSlack from the host's Budget before the tee, within
// mirrorBudgetWait, and gives it back when the mirror ends; a budget that
// cannot spare it sends the PUT unmirrored (never a refusal — the mirror
// is an extra). The primary's answer is the write's (RP78-2): putTwo
// returns at it, the mirror running on (mirrorRun) within mirrorDeadline; a
// whole PUT settles it after the commit (POST /api/v1/edge/mirror), a codec
// chunk waits for it within mirrorAfter (putTwoInline).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// mirrorMaxLag is how far the mirror may trail the primary (the bytes
// buffered for it) before it is cut loose (RP76-2).
var mirrorMaxLag = 16 << 20

// mirrorAfter is how long a codec chunk's mirror may answer after the
// primary did (putTwoInline); a whole PUT's settles after its commit.
var mirrorAfter = 5 * time.Second

// mirrorBudgetSlack is the buffer beyond the queue: the copy in flight.
const mirrorBudgetSlack = 2 << 20

// mirrorBudgetWait is how long a mirrored PUT waits for its buffer before
// it is sent unmirrored.
const mirrorBudgetWait = 200 * time.Millisecond

// The mirror's outer bound (the Worker's mirrorDeadlineMs, D-EDGE-46): 30 s
// plus the body at 2 MiB/s.
const (
	mirrorGrace   = 30 * time.Second
	mirrorMinRate = 2 << 20
)

// mirrorDeadline is how long a mirror PUT of n bytes may take.
func mirrorDeadline(n int64) time.Duration {
	return mirrorGrace + time.Duration(float64(n)/mirrorMinRate*float64(time.Second))
}

// errMirrorAnswered: the mirror answered; its body is not read any more.
var errMirrorAnswered = errors.New("data plane: the mirror leg answered")

// errMirrorCut: the mirror fell behind its buffer, or did not answer in time.
var errMirrorCut = errors.New("data plane: the mirror leg fell behind the primary and was cut")

// mirrorTee writes every buffer to the primary's pipe and queues a copy for
// the mirror, whose own goroutine (feed) drains the queue into the
// mirror's pipe — so the primary is never paced by the mirror.
type mirrorTee struct {
	a, b *io.PipeWriter
	// cancelB aborts the mirror's request: a transport stuck writing to a
	// backend that stopped reading never reads its pipe again, so closing
	// the pipe alone would not end it.
	cancelB context.CancelFunc

	mu     sync.Mutex
	cond   *sync.Cond
	queue  [][]byte
	queued int
	dead   bool  // the mirror is cut or failed: nothing more is queued
	cut    bool  // ... because it fell behind (or answered too late)
	closed bool  // the source ended
	srcErr error // ... with this (nil = EOF)
}

func newMirrorTee(a, b *io.PipeWriter, cancelB context.CancelFunc) *mirrorTee {
	t := &mirrorTee{a: a, b: b, cancelB: cancelB}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// Write queues p for the mirror (or cuts it when the queue would pass
// mirrorMaxLag), then writes p to the primary.
func (t *mirrorTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	if !t.dead {
		if t.queued+len(p) > mirrorMaxLag {
			t.cutLocked()
		} else {
			t.queue = append(t.queue, append([]byte(nil), p...))
			t.queued += len(p)
			t.cond.Signal()
		}
	}
	t.mu.Unlock()
	return t.a.Write(p)
}

// cutLocked cuts the mirror loose; t.mu is held.
func (t *mirrorTee) cutLocked() {
	if t.dead {
		return
	}
	t.dead, t.cut = true, true
	t.queue, t.queued = nil, 0
	t.cond.Broadcast()
	t.cancelB()
	_ = t.b.CloseWithError(errMirrorCut)
}

// cutIfRunning cuts the mirror when it has not ended (answered or failed).
func (t *mirrorTee) cutIfRunning() {
	t.mu.Lock()
	t.cutLocked()
	t.mu.Unlock()
}

// fail marks the mirror failed (it answered, or its pipe broke): not a cut.
func (t *mirrorTee) fail() {
	t.mu.Lock()
	if !t.dead {
		t.dead = true
		t.queue, t.queued = nil, 0
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

// end records the source's end; feed closes the mirror's pipe after the
// queue drains.
func (t *mirrorTee) end(err error) {
	t.mu.Lock()
	t.closed, t.srcErr = true, err
	t.cond.Broadcast()
	t.mu.Unlock()
}

// feed drains the queue into the mirror's pipe until the source ends or
// the mirror dies.
func (t *mirrorTee) feed() {
	for {
		t.mu.Lock()
		for len(t.queue) == 0 && !t.closed && !t.dead {
			t.cond.Wait()
		}
		if t.dead {
			t.mu.Unlock()
			return
		}
		if len(t.queue) == 0 { // closed and drained
			err := t.srcErr
			t.mu.Unlock()
			_ = t.b.CloseWithError(err)
			return
		}
		p := t.queue[0]
		t.queue = t.queue[1:]
		t.mu.Unlock()
		_, err := t.b.Write(p)
		t.mu.Lock()
		t.queued -= len(p)
		t.mu.Unlock()
		if err != nil {
			t.fail()
			return
		}
	}
}

func (t *mirrorTee) wasCut() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cut
}

// putPair is one link's PUT of a mirrored body.
type putPair struct {
	URL     string
	Headers map[string]string
}

// mirrorRun is a mirror PUT that may outlive the primary's answer. done is
// closed when it has ended (answered, failed or cut) and its buffer is
// given back; result is read after.
type mirrorRun struct {
	tee  *mirrorTee
	done chan struct{}
	etag string
	// unbudgeted: the budget could not spare the buffer, the PUT went
	// unmirrored (RP78-1); done is closed.
	unbudgeted bool
}

func (m *mirrorRun) finished() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// cut cuts the mirror loose when it is still running.
func (m *mirrorRun) cut() {
	if m.tee != nil {
		m.tee.cutIfRunning()
	}
}

// result is the mirror's ETag ("" when it did not store the body) and
// whether it was cut; read after done.
func (m *mirrorRun) result() (string, bool) {
	if m.tee == nil {
		return "", false
	}
	if m.tee.wasCut() {
		return "", true
	}
	return m.etag, false
}

// putTwo PUTs body (n bytes) to both links at once and returns at the
// primary's answer (the caller closes it): the mirror goes on as the
// mirrorRun, bounded by mirrorDeadline and its lag. A primary that fails
// cuts the mirror and returns once it has ended. budget nil = no bound.
func putTwo(ctx context.Context, client *http.Client, method string, links [2]putPair, n int64, body io.Reader, budget Budget) (*http.Response, *mirrorRun, error) {
	newReq := func(rctx context.Context, l putPair, r io.Reader) (*http.Request, error) {
		req, err := http.NewRequestWithContext(rctx, method, l.URL, r)
		if err != nil {
			return nil, err
		}
		for k, v := range l.Headers {
			if k != "content-length" && k != "host" {
				req.Header.Set(k, v)
			}
		}
		req.ContentLength = n
		if n == 0 {
			req.Body = http.NoBody
		}
		return req, nil
	}
	alone := func(run *mirrorRun) (*http.Response, *mirrorRun, error) {
		close(run.done)
		ra, err := newReq(ctx, links[0], body)
		if err != nil {
			return nil, run, err
		}
		resp, err := client.Do(ra)
		return resp, run, err
	}
	release := func() {}
	if budget != nil {
		bctx, cancel := context.WithTimeout(ctx, mirrorBudgetWait)
		rel, err := budget.Acquire(bctx, int64(mirrorMaxLag)+mirrorBudgetSlack)
		cancel()
		if err != nil {
			return alone(&mirrorRun{done: make(chan struct{}), unbudgeted: true})
		}
		release = rel
	}
	pa, wa := io.Pipe()
	pb, wb := io.Pipe()
	// The mirror outlives the request that started it (RP78-2): its own
	// clock, never the client's.
	mctx, cancelB := context.WithTimeout(context.WithoutCancel(ctx), mirrorDeadline(n))
	ra, err := newReq(ctx, links[0], pa)
	if err != nil {
		cancelB()
		release()
		return nil, nil, err
	}
	rb, err := newReq(mctx, links[1], pb)
	if err != nil {
		// No mirror request: the primary alone.
		cancelB()
		release()
		_ = pa.Close()
		return alone(&mirrorRun{done: make(chan struct{})})
	}
	tee := newMirrorTee(wa, wb, cancelB)
	run := &mirrorRun{tee: tee, done: make(chan struct{})}
	go tee.feed()
	go func() {
		buf := make([]byte, 1<<20)
		_, err := io.CopyBuffer(tee, body, buf)
		_ = wa.CloseWithError(err)
		tee.end(err)
	}()
	go func() {
		defer close(run.done)
		defer release()
		defer cancelB()
		resp, err := client.Do(rb)
		// Answered (or not): the mirror reads no more of the body — a
		// backend that answered before reading it must not hold the tee.
		tee.fail()
		_ = pb.CloseWithError(errMirrorAnswered)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			run.etag = resp.Header.Get("ETag")
		}
	}()
	resp, err := client.Do(ra)
	if err != nil {
		// The primary failed: the body stops for both.
		_ = pa.CloseWithError(err)
		tee.cutIfRunning()
		<-run.done
		return resp, run, err
	}
	return resp, run, nil
}

// putTwoInline is putTwo for a codec chunk: the mirror is waited for within
// mirrorAfter of the primary's answer, then cut (the chunk's copy row is
// written by the write's commit, which carries its ETag).
func putTwoInline(ctx context.Context, client *http.Client, method string, links [2]putPair, n int64, body io.Reader, budget Budget) (*http.Response, MirrorResult, error) {
	resp, run, err := putTwo(ctx, client, method, links, n, body, budget)
	if err != nil || run == nil {
		return resp, MirrorResult{}, err
	}
	mr := MirrorResult{Unbudgeted: run.unbudgeted}
	if !run.unbudgeted {
		timer := time.NewTimer(mirrorAfter)
		select {
		case <-run.done:
		case <-timer.C:
			run.cut()
			<-run.done
		}
		timer.Stop()
		// A mirror that answered as the timer fired is not cut (RP78-7):
		// its ETag is kept.
		mr.ETag, mr.Cut = run.result()
	}
	return resp, mr, nil
}
