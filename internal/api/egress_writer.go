package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// egressWriter is the throttle: it wraps the response of a GetObject or a
// /cdn request and paces the OBJECT BODY (a 200 or 206) through the tenant's
// token bucket while the tenant is past its allowance. Everything else the
// handler writes — an error document, a 304, a 416 — passes straight through.
//
// It is installed at dispatch, before the handler runs, so no path inside a
// handler can send object bytes around it.
type egressWriter struct {
	http.ResponseWriter
	ctx     context.Context
	m       *egressMeter
	t       *egressTenant
	surface string
	slots   *atomic.Int32 // the tenant's throttled-stream count for this surface

	wroteHeader bool
	body        bool // the response is an object body (200/206)

	evaluated bool
	sinceEval int64
	evalAt    time.Time
	pacing    bool // this response is waiting on the token bucket
	slot      bool // it holds one of the tenant's throttled-stream slots
	engaged   bool // counted in engaged_total
	would     bool // counted in would_throttle_total
	closed    bool
}

func (w *egressWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.body = code == http.StatusOK || code == http.StatusPartialContent
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends p. Handlers hand over a 16 MiB chunk, or a whole decrypted
// object of up to 256 MiB, in one call: it is cut into pieces no larger than
// the token bucket's burst, and the tenant's position is re-read every
// egressReevalBytes so a download that crosses the allowance mid-stream
// slows down. A paced wait ends at once when the client goes away.
func (w *egressWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.body = true // an implicit 200
	}
	if !w.body {
		return w.ResponseWriter.Write(p)
	}

	total := 0
	for len(p) > 0 {
		// A paced response also re-reads on the clock: at the floor rate a
		// megabyte takes 16 s, and a raised allowance should not wait for it.
		if !w.evaluated || w.sinceEval >= egressReevalBytes ||
			(w.pacing && w.m.now().Sub(w.evalAt) >= w.m.reevalEvery) {
			w.evaluate()
		}
		n := int64(len(p))
		if left := egressReevalBytes - w.sinceEval; n > left {
			n = left
		}
		if w.pacing {
			if slice := w.t.sliceBytes.Load(); n > slice {
				n = slice
			}
			if err := w.t.limiter.WaitN(w.ctx, int(n)); err != nil {
				return total, err
			}
		}
		wn, err := w.ResponseWriter.Write(p[:n])
		total += wn
		w.sinceEval += int64(wn)
		if w.pacing {
			egressThrottledBytes.Add(float64(wn))
			// Push the slice out now: the next one may be seconds away and
			// the proxies in front count silence.
			if f, ok := w.ResponseWriter.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// evaluate decides whether this response is paced from here on.
func (w *egressWriter) evaluate() {
	w.evaluated = true
	w.sinceEval = 0
	w.evalAt = w.m.now()
	d := w.m.decide(w.ctx, w.t)
	switch {
	case !d.over:
		w.release()
	case !d.enforced:
		// Flag off (or this tenant exempt): nothing is slowed, the decision
		// is counted.
		w.release()
		if !w.would {
			w.would = true
			egressWouldThrottle.WithLabelValues(w.surface).Inc()
		}
	default:
		if !w.slot {
			// Crossed the allowance mid-response: it joins the paced set.
			// A response already sending cannot be refused, so the stream
			// guard does not apply here.
			w.slot = true
			w.slots.Add(1)
		}
		if !w.engaged {
			w.engaged = true
			egressEngaged.WithLabelValues(w.surface).Inc()
		}
		w.pacing = true
	}
}

// release stops pacing and gives the throttled-stream slot back.
func (w *egressWriter) release() {
	w.pacing = false
	if w.slot {
		w.slot = false
		w.slots.Add(-1)
	}
}

// close ends the response's hold on the tenant entry. Idempotent.
func (w *egressWriter) close() {
	if w.closed {
		return
	}
	w.closed = true
	w.release()
	w.t.streams.Add(-1)
	w.t.lastSeen.Store(w.m.now().UnixNano())
}

// Flush passes through, so a streaming handler keeps its flushes.
func (w *egressWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *egressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Throttle metrics. Registered on the server's registry in prom_metrics.go
// (the default registry is not exported). No tenant label: the tenant is in
// the "egress allowance spent" log line and on the admin tenant page.
var (
	egressEngaged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_egress_throttle_engaged_total",
		Help: "Object responses paced because the tenant was past its egress allowance, by surface (s3, cdn).",
	}, []string{"surface"})
	egressWouldThrottle = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_egress_would_throttle_total",
		Help: "Object responses that would have been paced but were not: the egress_throttle flag is off for the tenant.",
	}, []string{"surface"})
	egressRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_egress_throttle_rejected_total",
		Help: "Object requests refused by the stream guard (more than EGRESS_THROTTLE_MAX_STREAMS paced responses for one tenant).",
	}, []string{"surface"})
	egressThrottledBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_egress_throttled_bytes_total",
		Help: "Object bytes sent through the egress throttle (at the paced rate).",
	})
)

func init() {
	for _, surface := range []string{egressSurfaceS3, egressSurfaceCDN} {
		egressEngaged.WithLabelValues(surface)
		egressWouldThrottle.WithLabelValues(surface)
		egressRejected.WithLabelValues(surface)
	}
}
