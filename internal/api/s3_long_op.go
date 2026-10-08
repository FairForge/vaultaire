package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Long-running S3 operations (CompleteMultipartUpload, DeleteObjects) behind
// Cloudflare: the edge gives up on an origin that sends no byte for 100 s and
// answers 504, and the client's retry redoes the whole operation. On a slow
// backend both run far longer than that — a complete streams the assembled
// object into ONE backend PUT (~30 MB/s on a Sync bridge: 2 GiB ≈ 70 s), a
// 1,000-key batch delete costs ~1 s per key there (bench 2026-10-07).
//
// runLongS3Op does what AWS documents for these operations: an operation that
// is not done after longOpThreshold commits `200 OK` (Content-Type
// application/xml), then a single space every longOpInterval keeps the
// connection alive, and the outcome follows as the body — the result
// document, or an <Error> document with the operation's own code. SDKs read
// a 200 carrying <Error> on CompleteMultipartUpload/CopyObject as a failure
// (aws-sdk-go-v2, botocore). An operation that finishes before the threshold
// answers exactly as it would without this wrapper (its own status, headers
// and body).
//
// After the commit the response headers are gone: ETag is also in the
// CompleteMultipartUploadResult body (what SDKs read), x-amz-version-id is
// not delivered on that path (AWS has the same limitation for a slow
// complete). The XML declaration is dropped after the whitespace — a
// declaration not at the start of the document is invalid XML and Python's
// parser rejects it.
//
// The operation runs on a context detached from the client's cancellation
// (with its own longOpMaxDuration deadline): once the server holds the
// request it finishes it, so a client that goes away mid-assembly leaves a
// completed object with its head row, quota and upload status written
// together — never bytes on the backend without a row, or a half-done batch.
// The handler waits for the operation either way.
//
// A deploy is the one thing that can still cut it: the HTTP drain
// (cmd/vaultaire shutdownTimeout, 30 s) returns with these handlers still
// running, and the engine closing the database under them killed every
// complete, copy or batch that had already sent 200 + whitespace — several
// deploys a day. Every operation is registered while it runs
// (longOpRegistry: the gauge vaultaire_s3_long_ops_in_flight{op}, the
// `long_ops_in_flight` field of /health read from the slot's own port, which
// vaultaire-switch polls before it stops a slot); Server.Shutdown waits for
// them after the drain and before the trackers flush and the engine closes
// (drainLongOps) — up to longOpDrainBound measured from EACH operation's
// start, so an operation never gets more than that in a deploy and the
// script's wait and the process's own stay inside one budget. Past the bound
// the rest is cancelled (its context), logged with op/tenant/bucket/key/age
// and counted in vaultaire_s3_long_ops_abandoned_total{op}; a cancelled
// CompleteMultipartUpload leaves its upload active and its parts on disk, so
// the client's retry finishes the same upload (s3_multipart.go).

const (
	defaultLongOpThreshold = 10 * time.Second
	defaultLongOpInterval  = 10 * time.Second
	// longOpMaxDuration bounds an operation once detached from its client
	// (a 50 GiB complete at 30 MB/s is ~30 min).
	longOpMaxDuration = 6 * time.Hour
	// longOpDrainBound is how long a stopping process waits for a detached
	// operation, from the operation's start (decision 2026-10-08: the
	// longest measured operation is a 2 GiB copy through Cloudflare at
	// 120 s; vaultaire-switch waits the same 900 s and its lock is 1200 s).
	longOpDrainBound = 15 * time.Minute
	// longOpCancelGrace is how long drainLongOps lets a cancelled operation
	// unwind (answer its client, release its reservation) before returning.
	longOpCancelGrace = 5 * time.Second
)

// The three operations, as the metrics label them.
const (
	longOpComplete = "CompleteMultipartUpload"
	longOpCopy     = "CopyObject"
	longOpBatch    = "DeleteObjects"
)

var (
	longOpsInFlight = func() *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vaultaire_s3_long_ops_in_flight",
			Help: "Detached long S3 operations (CompleteMultipartUpload, CopyObject, DeleteObjects) running right now, by operation. A stopping process waits for them up to 15 min from each one's start.",
		}, []string{"op"})
		for _, op := range []string{longOpComplete, longOpCopy, longOpBatch} {
			g.WithLabelValues(op)
		}
		return g
	}()
	longOpsAbandoned = func() *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vaultaire_s3_long_ops_abandoned_total",
			Help: "Long S3 operations a stopping process cancelled because they were still running 15 min after their start, by operation. Each one has a Warn line with tenant, bucket, key and age.",
		}, []string{"op"})
		for _, op := range []string{longOpComplete, longOpCopy, longOpBatch} {
			c.WithLabelValues(op)
		}
		return c
	}()
)

// longOpInfo names an operation for the registry, the log and the metrics.
type longOpInfo struct {
	Op, Bucket, Key string
}

type longOpEntry struct {
	longOpInfo
	tenant  string
	started time.Time
	cancel  context.CancelFunc
}

// longOpRegistry is the set of detached operations in flight.
type longOpRegistry struct {
	mu  sync.Mutex
	ops map[*longOpEntry]struct{}
}

func (g *longOpRegistry) add(e *longOpEntry) {
	g.mu.Lock()
	if g.ops == nil {
		g.ops = make(map[*longOpEntry]struct{})
	}
	g.ops[e] = struct{}{}
	g.mu.Unlock()
	longOpsInFlight.WithLabelValues(e.Op).Inc()
}

func (g *longOpRegistry) remove(e *longOpEntry) {
	g.mu.Lock()
	_, ok := g.ops[e]
	delete(g.ops, e)
	g.mu.Unlock()
	if ok {
		longOpsInFlight.WithLabelValues(e.Op).Dec()
	}
}

func (g *longOpRegistry) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ops)
}

func (g *longOpRegistry) snapshot() []*longOpEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*longOpEntry, 0, len(g.ops))
	for e := range g.ops {
		out = append(out, e)
	}
	return out
}

// longOps is the server's registry (built on first use: bare test servers).
func (s *Server) longOps() *longOpRegistry {
	s.longOpsOnce.Do(func() { s.longOpsReg = &longOpRegistry{} })
	return s.longOpsReg
}

// drainLongOps waits for the detached operations in flight until each has
// had `bound` since its start, cancels the ones still running, logs and
// counts them, and returns how many were cancelled. Called by Server.Shutdown
// after the HTTP drain (no new operation can start) and before the trackers
// flush and the engine closes the database.
func (s *Server) drainLongOps(bound time.Duration) int {
	reg := s.longOps()
	for {
		ops := reg.snapshot()
		if len(ops) == 0 {
			return 0
		}
		var youngest time.Time
		for _, e := range ops {
			if e.started.After(youngest) {
				youngest = e.started
			}
		}
		if wait := time.Until(youngest.Add(bound)); wait > 0 {
			time.Sleep(min(wait, 50*time.Millisecond))
			continue
		}
		for _, e := range ops {
			s.log().Warn("long S3 operation abandoned at shutdown",
				zap.String("op", e.Op), zap.String("tenant", e.tenant),
				zap.String("bucket", e.Bucket), zap.String("key", e.Key),
				zap.Duration("age", time.Since(e.started)))
			longOpsAbandoned.WithLabelValues(e.Op).Inc()
			e.cancel()
		}
		deadline := time.Now().Add(longOpCancelGrace)
		for reg.count() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return len(ops)
	}
}

func (s *Server) longOpTimings() (threshold, interval time.Duration) {
	threshold, interval = s.longOpThreshold, s.longOpInterval
	if threshold <= 0 {
		threshold = defaultLongOpThreshold
	}
	if interval <= 0 {
		interval = defaultLongOpInterval
	}
	return threshold, interval
}

// bufferedResponse captures what an operation writes (its responses are a
// few KiB of XML) so it can be replayed — as is, or after a commit.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) code() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

// runLongS3Op runs op (an S3 handler body) with the keep-alive described
// above, registered under info while it runs. op must not hijack or stream:
// it writes one small response.
func (s *Server) runLongS3Op(w http.ResponseWriter, r *http.Request, info longOpInfo, op func(http.ResponseWriter, *http.Request)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), longOpMaxDuration)
	defer cancel()
	opReq := r.WithContext(ctx)

	entry := &longOpEntry{longOpInfo: info, started: time.Now(), cancel: cancel}
	if t, err := tenant.FromContext(r.Context()); err == nil && t != nil {
		entry.tenant = t.ID
	}
	reg := s.longOps()
	reg.add(entry)

	rec := &bufferedResponse{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer reg.remove(entry)
		// The goroutine is outside net/http's per-request recover: a panic
		// here would take the process down.
		defer func() {
			if p := recover(); p != nil {
				s.log().Error("long S3 operation panicked", zap.Any("panic", p), zap.String("path", r.URL.Path))
				rec.header = http.Header{}
				rec.status = 0
				rec.body.Reset()
				WriteS3Error(rec, ErrInternalError, r.URL.Path, generateRequestID())
			}
		}()
		op(rec, opReq)
	}()

	threshold, interval := s.longOpTimings()
	timer := time.NewTimer(threshold)
	defer timer.Stop()
	select {
	case <-done:
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.code())
		_, _ = w.Write(rec.body.Bytes())
		return
	case <-timer.C:
	}

	// Commit: 200, then whitespace until the operation is done. Write errors
	// (the client left) are ignored — the operation finishes regardless.
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			_, _ = w.Write(committedBody(rec, r.URL.Path))
			_ = rc.Flush()
			s.log().Info("long S3 operation answered after keep-alive",
				zap.String("path", r.URL.Path), zap.Int("status", rec.code()))
			return
		case <-tick.C:
			_, _ = w.Write([]byte(" "))
			_ = rc.Flush()
		}
	}
}

// committedBody is the document that follows the keep-alive: the
// operation's own XML without its declaration, or — when it failed with
// something that is not an S3 error document — an InternalError document.
func committedBody(rec *bufferedResponse, resource string) []byte {
	body := bytes.TrimSpace(rec.body.Bytes())
	if bytes.HasPrefix(body, []byte("<?xml")) {
		if end := bytes.Index(body, []byte("?>")); end >= 0 {
			body = bytes.TrimSpace(body[end+2:])
		}
	}
	if rec.code() < 300 && len(body) > 0 {
		return body
	}
	if rec.code() >= 300 && bytes.HasPrefix(body, []byte("<Error")) {
		return body
	}
	doc, err := xml.Marshal(S3Error{
		Code:      ErrInternalError,
		Message:   fmt.Sprintf("%s (status %d)", errorMessages[ErrInternalError], rec.code()),
		Resource:  resource,
		RequestID: generateRequestID(),
	})
	if err != nil {
		return []byte("<Error><Code>InternalError</Code></Error>")
	}
	return doc
}

// log is the server's logger, or a no-op one for bare test servers.
func (s *Server) log() *zap.Logger {
	if s.logger == nil {
		return zap.NewNop()
	}
	return s.logger
}
