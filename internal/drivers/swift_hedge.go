package drivers

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// The Swift driver's hedge (hedge.go is the S3-class one; same metrics, same
// DRIVER_HEDGE_AFTER). Blomp's proxies sometimes give a request no first
// byte for ~5 s — any proxy IP, fresh or kept-alive connections, reads and
// HEADs alike — where the usual answer comes in 60–650 ms (prod 2026-10-21;
// the 5 s tails of the stored.ge → Blomp path were all this). A request
// with no answer after DRIVER_HEDGE_AFTER is sent once more on a connection
// opened for it; the first ANSWER (any status: a 404 is the object's
// verdict, a 5xx goes to do's retry) wins and the other is cancelled.
//
// Hedged: a HEAD (the probe's container HEAD, an object HEAD), a listing
// page, and — unlike the S3-class drivers — the GET of a body, on its
// HEADERS: the hedge fires before the first byte, so no byte of a body has
// been read, and the winner's body is the whole answer; the loser's body
// is closed unread. The S3-class rule ("never a GetObject") exists because
// their GETs stall rarely and a hedged GET there would go through the
// SDK; Swift's GET is our own request, and a 5 s first byte on a 64 MiB
// range is most of that range's time.
//
// Never hedged: a PUT (the bytes twice), the bulk delete (a POST with a
// body), and a DELETE — deleteName's verdict comes from the DELETE's
// status, and a hedge's 404 arriving while the first attempt deleted the
// object would report a miss (and, for a large object, skip its pieces);
// the HEAD before it is hedged, and Blomp rate-limits writes per container
// (~3/s), so a second DELETE would cost a write slot too. Never a request
// whose caller has gone.

// swiftHedgeOp is the hedge's op label for req, "" = never hedged.
func swiftHedgeOp(req swiftRequest) string {
	if req.body != nil || req.bodyBytes != nil || req.account {
		return ""
	}
	switch req.method {
	case http.MethodHead:
		if req.name == "" {
			return "head_bucket"
		}
		return "head_object"
	case http.MethodGet:
		if req.name == "" {
			return "list"
		}
		return "get"
	}
	return ""
}

// swiftHedgeOps are the op labels the Swift driver counts under.
var swiftHedgeOps = []string{"head_bucket", "head_object", "list", "get"}

func initSwiftHedgeSeries(driver string) {
	for _, op := range swiftHedgeOps {
		driverHedges.WithLabelValues(driver, op)
		driverHedgesWon.WithLabelValues(driver, op)
	}
}

// hedgeable reports whether the driver hedges req.
func (d *SwiftDriver) hedgeable(req swiftRequest) bool {
	return d.hedgeAfter > 0 && d.fresh != nil && swiftHedgeOp(req) != ""
}

// roundTrip sends one attempt of hreq, hedged when req may be.
func (d *SwiftDriver) roundTrip(ctx context.Context, req swiftRequest, hreq *http.Request) (*http.Response, error) {
	if !d.hedgeable(req) {
		return d.client.Do(hreq)
	}
	return hedgedRoundTrip(ctx, d.hedgeAfter, d.name, swiftHedgeOp(req), func(actx context.Context, fresh bool) (*http.Response, error) {
		c := d.client
		if fresh {
			c = d.fresh
		}
		return c.Do(hreq.WithContext(actx))
	})
}

type swiftAttempt struct {
	resp *http.Response
	err  error
	i    int // 0 = the first attempt, 1 = the hedge
}

// hedgedRoundTrip runs send(ctx, false) and, when it has not answered after
// `after`, send(ctx, true) on a fresh connection; the first response wins.
// A transport error is no answer: the other attempt, while it runs, may
// still bring one; both without one = the first attempt's error. Each
// attempt has its own context: the loser's is cancelled at once (a late
// response's body closed unread), the winner's when its body is closed.
func hedgedRoundTrip(ctx context.Context, after time.Duration, driver, op string,
	send func(ctx context.Context, fresh bool) (*http.Response, error)) (*http.Response, error) {
	if after <= 0 || ctx.Err() != nil {
		return send(ctx, false)
	}
	results := make(chan swiftAttempt, 2)
	var cancels [2]context.CancelFunc
	launch := func(i int) {
		actx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		go func() {
			resp, err := send(actx, i == 1) //nolint:bodyclose // handed on: the winner's caller closes it, a late loser's is closed below
			results <- swiftAttempt{resp: resp, err: err, i: i}
		}()
	}
	launch(0)
	timer := time.NewTimer(after)
	defer timer.Stop()

	running := 1
	var failed *swiftAttempt
	for {
		select {
		case <-timer.C:
			if ctx.Err() != nil {
				continue
			}
			running++
			driverHedges.WithLabelValues(driver, op).Inc()
			launch(1)
		case r := <-results:
			running--
			if r.err == nil {
				if r.i == 1 {
					driverHedgesWon.WithLabelValues(driver, op).Inc()
				}
				if running > 0 {
					cancels[1-r.i]()
					go func() {
						if late := <-results; late.resp != nil {
							_ = late.resp.Body.Close()
						}
					}()
				}
				r.resp.Body = &cancelOnClose{ReadCloser: r.resp.Body, cancel: cancels[r.i]}
				return r.resp, nil
			}
			cancels[r.i]()
			if failed == nil || r.i == 0 {
				failed = &r
			}
			if running == 0 {
				return nil, failed.err
			}
		}
	}
}

// cancelOnClose releases an attempt's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}
