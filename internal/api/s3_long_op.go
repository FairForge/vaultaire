package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
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
// script's wait and the process's own stay inside one budget. An operation
// past its bound is cut (its context), logged with op/tenant/bucket/key/age
// and written to s3_long_op_incidents (082) while the database is still
// open: Prometheus scrapes only the active slot, so the slot that is
// stopping cannot be the one that reports it — the active slot exports
// vaultaire_s3_long_ops_abandoned_total{op} from the table (Prompt 2a.2 G1).
//
// What a cut may stop is the backend write or delete itself, and nothing
// after it (Prompt 2a.2 G1: a cut that landed in the bookkeeping left a
// completed object without its bucket's default retention — a COMPLIANCE
// object a DELETE could remove — or new bytes under the old head row):
//   - the body a write streams is cut at once (cutReader): a backend never
//     commits an object whose body failed;
//   - the call itself runs on backendCallCtx, which ends longOpAnswerGrace
//     after the cut: a write whose whole body was sent is given that long to
//     answer, so bytes that landed get their row;
//   - everything after a backend call that succeeded — head row, version
//     row, Object Lock, quota, parity/Smart hooks, upload status — runs on
//     postCommit, detached from the cut, with its own short timeout.
// A cut CompleteMultipartUpload leaves its upload active and its parts on
// disk, so the client's retry finishes the same upload; a retry of one that
// had landed re-asserts its lock and version rows (s3_multipart.go).

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
	// longOpCancelGrace is how long drainLongOps lets a cut operation
	// unwind before returning: the answer grace of its backend call plus its
	// bookkeeping (vaultaire-switch's 900 s + this stays inside the unit's
	// TimeoutStopSec of 1000 s).
	longOpCancelGrace = 60 * time.Second
	// defaultLongOpAnswerGrace is how long a backend write or delete already
	// in flight when its operation is cut may still take to answer.
	defaultLongOpAnswerGrace = 20 * time.Second
	// postCommitTimeout bounds the bookkeeping that follows a backend call
	// that succeeded (postCommit).
	postCommitTimeout = 30 * time.Second
)

// errLongOpCut is what a cut operation's body reads return.
var errLongOpCut = errors.New("long S3 operation cut at shutdown")

// postCommit is the context of the bookkeeping after a backend write or
// delete that succeeded: detached from the operation's cut and from the
// client, bounded by postCommitTimeout. Values (tenant, gate) are kept.
func postCommit(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), postCommitTimeout)
}

// backendCallCtx is the context of the backend write or delete an operation
// makes: it ends `grace` after ctx does, not with it, so a call whose body
// is already complete can still answer (and its bytes get their row) when
// the operation is cut. The body itself is cut at once (cutReader).
func backendCallCtx(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	if grace <= 0 {
		grace = defaultLongOpAnswerGrace
	}
	bctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(grace, cancel) })
	return bctx, func() { stop(); cancel() }
}

// longOpAnswerGrace is the server's answer grace (tests shorten it).
func (s *Server) longOpAnswerGrace() time.Duration {
	if s.longOpGrace > 0 {
		return s.longOpGrace
	}
	return defaultLongOpAnswerGrace
}

// cutReader is a write's body that fails as soon as its operation is cut:
// the backend sees a broken body and stores nothing.
type cutReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cutReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, fmt.Errorf("%w: %w", errLongOpCut, err)
	}
	return c.r.Read(p)
}

// The three operations, as the metrics label them.
const (
	longOpComplete = "CompleteMultipartUpload"
	longOpCopy     = "CopyObject"
	longOpBatch    = "DeleteObjects"
)

// Outcomes of vaultaire_s3_long_op_total: a failure after the 200 is
// committed is invisible to the request metrics (they say 200).
const (
	longOpOK                = "ok"
	longOpErrorBeforeCommit = "error_before_commit"
	longOpErrorAfterCommit  = "error_after_commit"
	// defaultLongOpPreludeMax is how long an operation may spend in its
	// preconditions (before longOpBegin) without the keep-alive going out:
	// a dependency stuck there still gets the 200 before Cloudflare's 100 s.
	defaultLongOpPreludeMax = 60 * time.Second
)

var longOpOutcomes = func() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_s3_long_op_total",
		Help: "Long S3 operations by outcome: ok, error_before_commit (a real 4xx/5xx), error_after_commit (an <Error> document inside a committed 200 — invisible to the request metrics). Rules: deploy/monitoring/vaultaire-longops.yml.",
	}, []string{"op", "outcome"})
	for _, op := range []string{longOpComplete, longOpCopy, longOpBatch} {
		for _, o := range []string{longOpOK, longOpErrorBeforeCommit, longOpErrorAfterCommit} {
			c.WithLabelValues(op, o)
		}
	}
	return c
}()

// longOpGate is what an operation closes when its cheap preconditions are
// resolved and its slow work starts (longOpBegin): the keep-alive threshold
// runs from there, so a refusal decided before it keeps its own status
// however long the lookups took (a slow source head lookup on CopyObject
// used to turn NoSuchKey into 200 + <Error>). An operation that never
// begins gets the keep-alive after longOpPreludeMax anyway.
type longOpGate struct {
	once  sync.Once
	begun chan struct{}
}

type longOpGateKey struct{}

type longOpFailedKey struct{}

// longOpFailed marks an operation that answers 200 as failed all the same —
// a DeleteObjects whose every key is an error entry: the request metrics
// say 200, so the outcome counter must not say ok. No-op outside
// runLongS3Op.
func longOpFailed(r *http.Request) {
	if f, ok := r.Context().Value(longOpFailedKey{}).(*atomic.Bool); ok && f != nil {
		f.Store(true)
	}
}

// longOpFinished counts an operation's outcome; one that failed inside its
// committed 200 while this process is stopping is also written to
// s3_long_op_incidents (the active slot reports it: this one is not
// scraped any more). A cut operation already has its row.
func (s *Server) longOpFinished(e *longOpEntry, outcome string, failed bool) {
	if failed && outcome == longOpOK {
		outcome = longOpErrorAfterCommit
	}
	longOpOutcomes.WithLabelValues(e.Op, outcome).Inc()
	if outcome == longOpErrorAfterCommit && s.draining.Load() && !e.cut.Load() {
		s.recordLongOpIncident(e, longOpErrorAfterCommit, time.Since(e.started))
	}
}

// longOpBegin marks the start of the operation's slow work. No-op outside
// runLongS3Op.
func longOpBegin(r *http.Request) {
	if g, ok := r.Context().Value(longOpGateKey{}).(*longOpGate); ok && g != nil {
		g.once.Do(func() { close(g.begun) })
	}
}

func longOpOutcome(code int, committed bool) string {
	switch {
	case code < 300:
		return longOpOK
	case committed:
		return longOpErrorAfterCommit
	default:
		return longOpErrorBeforeCommit
	}
}

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
	// cutAt is when drainLongOps cut the operation (read and written by the
	// drain only); cut says so to the operation's own goroutine.
	cutAt time.Time
	cut   atomic.Bool
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

// drainLongOps waits for the detached operations in flight, cuts each one
// when it has had `bound` since its own start (an operation 14 min old gets
// one more minute, not the youngest one's fifteen), logs it and writes its
// s3_long_op_incidents row, and returns how many it cut once none is left —
// or once every one left has overstayed longOpCancelGrace after its cut.
// Called by Server.Shutdown after the HTTP drain (no new operation can
// start) and before the trackers flush and the engine closes the database.
func (s *Server) drainLongOps(bound time.Duration) int {
	reg := s.longOps()
	s.draining.Store(true)
	cut := 0
	for {
		ops := reg.snapshot()
		if len(ops) == 0 {
			return cut
		}
		now := time.Now()
		overstayed := true
		for _, e := range ops {
			if e.cutAt.IsZero() && !now.Before(e.started.Add(bound)) {
				e.cutAt = now
				e.cut.Store(true)
				age := now.Sub(e.started)
				s.log().Warn("long S3 operation abandoned at shutdown",
					zap.String("op", e.Op), zap.String("tenant", e.tenant),
					zap.String("bucket", e.Bucket), zap.String("key", e.Key),
					zap.Duration("age", age))
				s.recordLongOpIncident(e, longOpAbandoned, age)
				e.cancel()
				cut++
			}
			if e.cutAt.IsZero() || now.Before(e.cutAt.Add(longOpCancelGrace)) {
				overstayed = false
			}
		}
		if overstayed {
			return cut
		}
		time.Sleep(10 * time.Millisecond)
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
	gate := &longOpGate{begun: make(chan struct{})}
	opReq := r.WithContext(context.WithValue(ctx, longOpGateKey{}, gate))

	failed := &atomic.Bool{}
	opReq = opReq.WithContext(context.WithValue(opReq.Context(), longOpFailedKey{}, failed))

	entry := &longOpEntry{longOpInfo: info, started: time.Now(), cancel: cancel}
	if t, err := tenant.FromContext(r.Context()); err == nil && t != nil {
		entry.tenant = t.ID
	}
	reg := s.longOps()
	reg.add(entry)
	// Out of the registry only once the answer is written and flushed: a
	// stopping process waits for the registry, and an entry removed when the
	// operation returned let it exit before the last bytes went out.
	defer reg.remove(entry)

	rec := &bufferedResponse{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	preludeMax := s.longOpPreludeMax
	if preludeMax <= 0 {
		preludeMax = defaultLongOpPreludeMax
	}
	prelude := time.NewTimer(preludeMax)
	defer prelude.Stop()
	var thresholdC <-chan time.Time
	begun := gate.begun
	for committed := false; !committed; {
		select {
		case <-done:
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.code())
			_, _ = w.Write(rec.body.Bytes())
			_ = http.NewResponseController(w).Flush()
			s.longOpFinished(entry, longOpOutcome(rec.code(), false), failed.Load())
			return
		case <-begun:
			begun = nil
			timer := time.NewTimer(threshold)
			defer timer.Stop()
			thresholdC = timer.C
		case <-thresholdC:
			committed = true
		case <-prelude.C:
			committed = true
		}
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
			s.longOpFinished(entry, longOpOutcome(rec.code(), true), failed.Load())
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

// longOpBucketExists is the bucket check a long operation makes before
// longOpBegin, ListObjects' rule (a registry row, or objects recorded under
// the name for buckets older than the registry). No DB = exists.
func (s *Server) longOpBucketExists(r *http.Request, tenantID, bucket string) (bool, error) {
	if s.db == nil {
		return true, nil
	}
	var exists bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)
		    OR EXISTS(SELECT 1 FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2)`,
		tenantID, bucket).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("bucket %s lookup: %w", bucket, err)
	}
	return exists, nil
}

// longOpDrainBoundFromEnv is LONG_OP_DRAIN_BOUND (a Go duration, 1s–15m;
// default and ceiling longOpDrainBound): a lab lowers it to watch a cut
// happen. Never above 15 min — vaultaire-switch's wait and the unit's
// TimeoutStopSec are sized for it. A rejected value is logged at Warn and
// the default kept (R13-19).
func longOpDrainBoundFromEnv(logger *zap.Logger, getenv func(string) string) time.Duration {
	raw := getenv("LONG_OP_DRAIN_BOUND")
	if raw == "" {
		return longOpDrainBound
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Second || d > longOpDrainBound {
		logger.Warn("LONG_OP_DRAIN_BOUND rejected; the default is kept",
			zap.String("value", raw), zap.Duration("default", longOpDrainBound))
		return longOpDrainBound
	}
	return d
}
