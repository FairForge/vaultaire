package dataplane

// The mirror (WP-EDGE-4, Stage 1a Prompt 48): a mirrored write's body goes
// to two links at once — the placement's backend and the mirror leg — read
// ONCE from the client and teed into two request bodies (two io.Pipes), so
// nothing is buffered beyond one copy buffer. The primary decides the
// write: a mirror that fails or stops reading is cut loose (its pipe
// closed) and the primary goes on alone; the commit then reports no mirror
// ETag and the copy is the blob_copies job's. A primary that fails ends
// both.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// mirrorStall is how long one write to the mirror's body may wait before
// the mirror is cut loose (the primary never waits longer for it).
var mirrorStall = 30 * time.Second

// errMirrorAnswered: the mirror answered; its body is not read any more.
var errMirrorAnswered = errors.New("data plane: the mirror leg answered")

// errMirrorStalled: the mirror stopped reading its body.
var errMirrorStalled = errors.New("data plane: the mirror leg stopped reading the body")

// mirrorTee writes every buffer to the primary's pipe and — while it keeps
// up — the mirror's.
type mirrorTee struct {
	a, b  *io.PipeWriter
	bDead bool
	// cancelB aborts the mirror's request: a transport stuck writing to a
	// backend that stopped reading never reads its pipe again, so closing
	// the pipe alone would not end it.
	cancelB context.CancelFunc
}

func (t *mirrorTee) Write(p []byte) (int, error) {
	if t.bDead {
		return t.a.Write(p)
	}
	done := make(chan error, 1)
	go func() {
		_, err := t.b.Write(p)
		done <- err
	}()
	n, errA := t.a.Write(p)
	timer := time.NewTimer(mirrorStall)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.bDead = true
		}
	case <-timer.C:
		t.cancelB()
		_ = t.b.CloseWithError(errMirrorStalled)
		<-done
		t.bDead = true
	}
	return n, errA
}

// putPair is one link's PUT of a mirrored body.
type putPair struct {
	URL     string
	Headers map[string]string
}

// putTwo PUTs body (n bytes) to both links at once: the primary's response
// (the caller closes it), the mirror's ETag ("" when the mirror did not
// store the body), and the primary's error.
func putTwo(ctx context.Context, client *http.Client, method string, links [2]putPair, n int64, body io.Reader) (*http.Response, string, error) {
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
		return nil, "", err
	}
	rb, err := newReq(mctx, links[1], pb)
	if err != nil {
		// No mirror request: the primary alone.
		_ = pa.Close()
		ra, _ = newReq(ctx, links[0], body)
		resp, err := client.Do(ra)
		return resp, "", err
	}
	tee := &mirrorTee{a: wa, b: wb, cancelB: cancelB}
	go func() {
		buf := make([]byte, 1<<20)
		_, err := io.CopyBuffer(tee, body, buf)
		_ = wa.CloseWithError(err)
		_ = wb.CloseWithError(err)
	}()
	mc := make(chan string, 1)
	go func() {
		resp, err := client.Do(rb)
		// Answered (or not): the mirror reads no more of the body — a
		// backend that answered before reading it must not hold the tee.
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
	}
	return resp, <-mc, err
}
