package dataplane

// The mirror (WP-EDGE-4, Stage 1a Prompt 48): a mirrored write's body goes
// to two links at once — the placement's backend and the mirror leg — read
// ONCE from the client and teed into two request bodies (two io.Pipes). The
// primary decides the write: a mirror that fails, trails the primary by
// more than its buffer (mirrorMaxLag, RP76-2) or has not answered
// mirrorAfter past the primary's answer is cut loose (its request
// cancelled, its pipe closed) and the primary goes on alone; the commit
// then reports no mirror ETag (and mirror_cut) and the copy is the
// blob_copies job's. A primary that fails ends both. The primary's writes
// never wait for the mirror: the mirror is fed from its own buffer.

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

// mirrorAfter is how long the mirror may answer after the primary did.
var mirrorAfter = 5 * time.Second

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

// putTwo PUTs body (n bytes) to both links at once: the primary's response
// (the caller closes it), the mirror's ETag ("" when the mirror did not
// store the body), whether the mirror was cut for falling behind (RP76-2),
// and the primary's error.
func putTwo(ctx context.Context, client *http.Client, method string, links [2]putPair, n int64, body io.Reader) (*http.Response, string, bool, error) {
	pa, wa := io.Pipe()
	pb, wb := io.Pipe()
	mctx, cancelB := context.WithCancel(ctx)
	defer cancelB()
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
	ra, err := newReq(ctx, links[0], pa)
	if err != nil {
		return nil, "", false, err
	}
	rb, err := newReq(mctx, links[1], pb)
	if err != nil {
		// No mirror request: the primary alone.
		_ = pa.Close()
		ra, _ = newReq(ctx, links[0], body)
		resp, err := client.Do(ra)
		return resp, "", false, err
	}
	tee := newMirrorTee(wa, wb, cancelB)
	go tee.feed()
	go func() {
		buf := make([]byte, 1<<20)
		_, err := io.CopyBuffer(tee, body, buf)
		_ = wa.CloseWithError(err)
		tee.end(err)
	}()
	mc := make(chan string, 1)
	go func() {
		resp, err := client.Do(rb)
		// Answered (or not): the mirror reads no more of the body — a
		// backend that answered before reading it must not hold the tee.
		tee.fail()
		_ = pb.CloseWithError(errMirrorAnswered)
		if err != nil {
			mc <- ""
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			mc <- ""
			return
		}
		mc <- resp.Header.Get("ETag")
	}()
	resp, err := client.Do(ra)
	if err != nil {
		// The primary failed: the body stops for both.
		_ = pa.CloseWithError(err)
		tee.cutIfRunning()
		<-mc
		return resp, "", false, err
	}
	// The primary answered: the mirror gets mirrorAfter more to answer.
	timer := time.NewTimer(mirrorAfter)
	defer timer.Stop()
	var mirrorETag string
	select {
	case mirrorETag = <-mc:
	case <-timer.C:
		tee.cutIfRunning()
		<-mc
	}
	if tee.wasCut() {
		return resp, "", true, nil
	}
	return resp, mirrorETag, false, nil
}
