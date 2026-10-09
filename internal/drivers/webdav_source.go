package drivers

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

// A request body that trickles (Prompt 2b.2 C1 P2-b): the PUT deadline is
// paused while the transport waits for the source (Prompt 2b A2 — a slow
// client is not a slow bridge), the idle watchdog never runs on the
// source's time, and the API server has no body timeout. So an upload at a
// byte every few seconds held its bridge's request slot for as long as the
// client liked: 8 of them (SYNC_WEBDAV_MAX_CONCURRENCY) blocked a bridge,
// 3 its large uploads.
//
// The bound is on the SOURCE: averaged over a window of time spent waiting
// for the source's bytes (WebDAVDefaultSourceWindow, 60 s — the server's
// own pace is not counted), the body must arrive at SYNC_WEBDAV_SOURCE_MIN_RATE
// (default 32 KiB/s; "off" disables it) or the upload fails with
// engine.ErrSourceTooSlow — the caller's error (a 400 RequestTimeout to a
// client still there), never a bridge charge or an engine breaker charge.
// A read the source never returns from is cut when the window runs out
// while the transport waits in it (the request is cancelled, its slot
// freed); the reads that do return are checked as they return.
//
// Only a non-seekable body is guarded: a seekable one (a staged piece, an
// in-memory chunk) is local and never trickles.

const (
	// WebDAVDefaultSourceMinRate is the slowest body an upload may stream.
	WebDAVDefaultSourceMinRate int64 = 32 << 10
	// WebDAVDefaultSourceWindow is the source-waiting time it is averaged over.
	WebDAVDefaultSourceWindow = 60 * time.Second
)

// WithWebDAVSourceMinRate sets the minimum body rate in bytes/s (0 disables
// the check; negative keeps the default) and the window it is averaged
// over (≤ 0 keeps the default).
func WithWebDAVSourceMinRate(rate int64, window time.Duration) WebDAVOption {
	return func(s *webdavSettings) {
		if rate >= 0 {
			s.sourceMinRate = rate
		}
		if window > 0 {
			s.sourceWindow = window
		}
	}
}

// sourceGuard reads a request body and fails it when it arrives below the
// minimum rate. onSlow (set per attempt by send) cancels the request that
// is waiting in the source.
type sourceGuard struct {
	r      io.Reader
	need   int64 // bytes per window
	window time.Duration

	mu      sync.Mutex
	waited  time.Duration // source-waiting time since the checkpoint
	got     int64         // bytes since the checkpoint
	reading uint64        // token of the read in progress (0 = none)
	next    uint64
	slow    bool
	onSlow  func()
}

// guardSource wraps a non-seekable body in a sourceGuard (rate 0 = no
// guard; a body already guarded is returned as is).
func guardSource(data io.Reader, s webdavSettings) io.Reader {
	if s.sourceMinRate <= 0 || s.sourceWindow <= 0 {
		return data
	}
	if _, ok := data.(*sourceGuard); ok {
		return data
	}
	if _, ok := data.(io.Seeker); ok {
		return data
	}
	need := int64(float64(s.sourceMinRate) * s.sourceWindow.Seconds())
	return &sourceGuard{r: data, need: max(need, 1), window: s.sourceWindow}
}

func (g *sourceGuard) tooSlowErr() error {
	return fmt.Errorf("%w: under %d bytes in %s of waiting for the body", engine.ErrSourceTooSlow, g.need, g.window)
}

// setOnSlow installs (or, with nil, removes) the cancel of the attempt in
// flight.
func (g *sourceGuard) setOnSlow(f func()) {
	g.mu.Lock()
	g.onSlow = f
	g.mu.Unlock()
}

// tooSlow reports whether the guard has failed the body.
func (g *sourceGuard) tooSlow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.slow
}

func (g *sourceGuard) Read(p []byte) (int, error) {
	g.mu.Lock()
	if g.slow {
		g.mu.Unlock()
		return 0, g.tooSlowErr()
	}
	g.next++
	tok := g.next
	g.reading = tok
	var timer *time.Timer
	if g.got < g.need {
		// Short so far: if this read waits out the rest of the window, the
		// body is too slow — whether or not it ever returns.
		timer = time.AfterFunc(max(g.window-g.waited, 0), func() {
			g.mu.Lock()
			if g.reading != tok || g.slow {
				g.mu.Unlock()
				return
			}
			g.slow = true
			f := g.onSlow
			g.mu.Unlock()
			if f != nil {
				f()
			}
		})
	}
	g.mu.Unlock()

	start := time.Now()
	n, err := g.r.Read(p)
	took := time.Since(start)
	if timer != nil {
		timer.Stop()
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.reading = 0
	g.waited += took
	g.got += int64(n)
	if g.waited >= g.window && !g.slow {
		if g.got < g.need {
			g.slow = true
		} else {
			g.waited, g.got = 0, 0
		}
	}
	if g.slow && (err == nil || err == io.EOF) {
		// The bytes of this read are dropped with the upload.
		return 0, g.tooSlowErr()
	}
	return n, err
}
