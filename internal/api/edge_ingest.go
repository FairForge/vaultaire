package api

// The control plane of the edge mover (WP-EDGE-1 Part A, Stage 1a Prompt
// 44; plan 40.10 slice 1, the write half): POST /api/v1/edge/admit and
// POST /api/v1/edge/commit, the two calls the Cloudflare Worker on
// s3.stored.cloud (edge/s3-ingest) makes around a PutObject or UploadPart
// it streams straight to the backend. Mounted only when EDGE_SERVICE_TOKEN
// is set (edge_token.go); never an admin route.
//
// Admit: the Worker describes the client's request (method, host, raw path
// and query, every header; the client's Authorization INSIDE the JSON
// body, never as the call's own header — so the S3 credential gate,
// Architecture Decision 8, never captures the call) and the control plane
// decides as handlePutObject would, in the same order: SigV4 verified
// (auth.ValidateDescribed — one verifier, two entrances), the key's
// expiry, IP allow-list, operation and bucket scope, the tenant's
// suspension, the bucket, the names, the edge_ingest flag, the ONE
// placement decision (placement.go: anything but a whole unsealed write
// passes through), the backend (an EdgeWriter whose breaker is closed),
// Object Lock, the quota reservation, a minted blob address, a presigned
// link that signs content-length and the client's Content-MD5. Every
// refusal is relayed as the S3 XML the origin would have sent; a
// {edge:false} sends the request to the origin untouched; nothing pending
// is left behind a refusal.
//
// Commit: the Worker reports the backend's answer (it never touches the
// bytes — bench-results/EDGE-RELAY-2026-10-26.md; integrity is the
// backend's own plus the client's Content-MD5 signed into the link, which
// both backends enforce; a hex x-amz-content-sha256 claim is NOT verified
// on this path — the origin's is, docs/API.md says so); under the admit
// row's lock the control plane commits exactly as a PUT through SLC would
// (commitBlobWrite: head row, version row, bill; the access-log row; the
// notification and event), or records a part with the backend's ETag. A
// second commit of one admit answers the same (idempotent); a failed
// write releases the reservation and queues the address as orphan_write
// on the backend that was sent the body (placeObject's rule). The
// control plane never sees a body byte and holds no key for this part
// (contract lines 1, 4).

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
)

const (
	// edgeAdmitTTL is how long a presigned link lives; edgeAdmitExpiry how
	// long the admit row waits for its commit before the reaper collects it.
	edgeAdmitTTL    = 15 * time.Minute
	edgeAdmitExpiry = 20 * time.Minute
	// edgeAdmitBodyMax bounds the admit and commit JSON bodies.
	edgeAdmitBodyMax = 1 << 20
	// edgeHost is the metrics label of this data-plane host.
	edgeHost = "edge"
	// edgeOpPut / edgeOpPart / edgeOpGet are the admit's op values.
	edgeOpPut  = "put"
	edgeOpPart = "part"
	edgeOpGet  = "get"
	// edgeReasonSmall is the pass-through of a body under EDGE_MIN_BYTES.
	edgeReasonSmall = "small"
)

// The data-plane metrics (plan 40.10: every byte by host; Part B adds
// reads and the nodes their own host).
var (
	dataPlaneAdmits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_data_plane_admits_total",
		Help: "Edge admit decisions by host, op (put, part, get) and outcome: admitted, admitted_unverified (an aws-chunked write to a backend that does not verify its trailer, with no Content-MD5 — D-EDGE-30), pass_through (the origin takes the request), pass_through_small, refused (an S3 error relayed to the client), error (the control plane could not decide).",
	}, []string{"host", "op", "outcome"})
	dataPlaneBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_data_plane_bytes_total",
		Help: "Object bytes a data-plane host moved and the control plane committed, by host and op.",
	}, []string{"host", "op"})
)

func init() {
	for _, op := range []string{edgeOpPut, edgeOpPart, edgeOpGet} {
		for _, outcome := range []string{"admitted", "admitted_unverified", "pass_through", "pass_through_small", "refused", "error"} {
			dataPlaneAdmits.WithLabelValues(edgeHost, op, outcome)
		}
		dataPlaneBytes.WithLabelValues(edgeHost, op)
	}
	// An edge GET of a tenant past its allowance with the flag off is
	// counted where the origin counts it (Part B).
	egressWouldThrottle.WithLabelValues(edgeHost)
}

// edgeAdmitRequest is what the Worker describes.
type edgeAdmitRequest struct {
	Op        string            `json:"op"`
	Method    string            `json:"method"`
	Host      string            `json:"host"`
	Path      string            `json:"path"`
	Query     string            `json:"query"`
	Headers   map[string]string `json:"headers"`
	ClientIP  string            `json:"client_ip"`
	RequestID string            `json:"request_id"`
}

// edgeExpect tells the Worker what the backend will enforce.
type edgeExpect struct {
	MD5 bool `json:"md5"` // the link signs Content-MD5: the backend enforces it
}

// edgeAdmitResponse is the decision.
type edgeAdmitResponse struct {
	Edge        bool              `json:"edge"`
	Reason      string            `json:"reason,omitempty"` // why not (a pass-through), for the Worker's log line
	Refusal     *edgeRelay        `json:"refusal,omitempty"`
	AdmitID     string            `json:"admit_id,omitempty"`
	URL         string            `json:"url,omitempty"`
	Method      string            `json:"method,omitempty"`
	SendHeaders map[string]string `json:"send_headers,omitempty"`
	Backend     string            `json:"backend,omitempty"`
	StorageKey  string            `json:"storage_key,omitempty"`
	Expect      *edgeExpect       `json:"expect,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	// A GET (Part B): the status the backend must answer (206 — every link
	// names a Range, the whole object's for a whole read), the status the
	// CLIENT gets (200, or 206 for the client's own Range), the ORIGIN's
	// response headers the Worker serves in place of the backend's
	// (object_headers.go's set), and the admitted span in bytes — the
	// backend's Content-Length must equal it or the Worker falls back to the
	// origin, whose integrity path decides (RP66-2).
	ExpectStatus    int               `json:"expect_status,omitempty"`
	Status          int               `json:"status,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	ContentLength   int64             `json:"content_length,omitempty"`
	// Window (WP-EDGE-2 Part C, D-EDGE-32): a read of at least 64 MiB is
	// the Worker's windowed reader's — 16 MiB ranges, at most 6 in flight,
	// from Start over ContentLength bytes, each written in order; the link
	// signs no range.
	Window *edgeWindow `json:"window,omitempty"`
	// An aws-chunked write (WP-EDGE-2 Part C): StreamingTrailer is the
	// trailer the body's framing ends in (the codec decodes the framing and
	// VERIFIES it — D-EDGE-31), ContentLength the DECODED length. The mover
	// gets Headers instead of a presigned link: a HEADER-signed backend
	// request (lower-cased, authorization included) to send to URL with the
	// client's body untouched (D-EDGE-30, D-EDGE-33).
	StreamingTrailer string            `json:"streaming_trailer,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	// unverified: an admitted write whose bytes nothing checks — an
	// aws-chunked write to a backend that does not verify the trailer, with
	// no Content-MD5 the commit could compare (the metric's
	// admitted_unverified; never on the wire).
	unverified bool
	// Codec (WP-EDGE-2): the write is the HOST's to chunk, hash and seal —
	// no presigned PUT for the body; the host asks /api/v1/edge/index per
	// batch of chunk hashes, uploads the misses as 0x06 blobs and commits
	// the chunk list.
	Codec *edgeCodecGrant `json:"codec,omitempty"`
}

// edgeWindow is a windowed read's shape: the range size, the most ranges
// in flight (a Worker's connection limit), the span's first byte.
type edgeWindow struct {
	RangeBytes int64 `json:"range_bytes"`
	InFlight   int   `json:"in_flight"`
	Start      int64 `json:"start"`
}

// edgeCodecGrant is what a codec admit hands the data-plane host: the
// tenant's codec PUBLIC key (X-Wing, base64 in JSON) and its key version,
// and the two numbers the pipeline needs. Nothing here is a secret
// (contract line 4: nothing of the hierarchy travels but a public key).
type edgeCodecGrant struct {
	Version        int    `json:"version"`
	KEMPublicKey   []byte `json:"kem_public_key"`
	ChunkThreshold int64  `json:"chunk_threshold"`
	SegmentSize    int    `json:"segment_size"`
}

// edgeCommitRequest is what the Worker reports: the backend's answer. No
// digest and no byte count for a moved write — the Worker is not in the
// byte path there. A CODEC write reverses that on purpose (WP-EDGE-2): the
// host read every byte to chunk it, so it reports the chunk list and the
// whole body's MD5 (the digest is free once the bytes were read).
type edgeCommitRequest struct {
	AdmitID       string `json:"admit_id"`
	Outcome       string `json:"outcome"` // a write: ok | backend_error | edge_error; a GET: served | fallback | error
	BackendStatus int    `json:"backend_status"`
	BackendCode   string `json:"backend_code"` // the backend's XML <Code>, when it answered an error; the subrequest's error otherwise
	BackendETag   string `json:"backend_etag"`
	DurationMS    int64  `json:"duration_ms"`
	// A GET: the Content-Length the backend answered at its headers — what
	// is metered (the Worker cannot see the end of a stream it does not
	// touch; edge_get.go).
	BackendContentLength int64 `json:"backend_content_length,omitempty"`
	// A windowed GET (D-EDGE-32): the bytes the Worker wrote to the client,
	// reported when its stream ended — the sum of its ranges, or what went
	// out before a range failed and the body was cut.
	BytesSent int64 `json:"bytes_sent,omitempty"`
	// A codec write (WP-EDGE-2): the ordered chunk list and the body's MD5
	// (hex) — a PUT's ETag, a part's part digest.
	Chunks  []edgeCommitChunk `json:"chunks,omitempty"`
	BodyMD5 string            `json:"body_md5,omitempty"`
}

// edgeCommitChunk is one chunk of a codec commit, in body order: the PLAIN
// sha256 (hex; over the service-token channel for the duration of the call,
// never stored or logged — D-EDGE-16), its plaintext and sealed sizes, the
// backend's MD5 ETag of the sealed bytes for a chunk this host stored, and
// whether it was stored or a dedup hit.
type edgeCommitChunk struct {
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	SealedSize  int64  `json:"sealed_size,omitempty"`
	BackendETag string `json:"backend_etag,omitempty"`
	Status      string `json:"status"` // stored | hit
}

// edgeAdmitAttrs is what the row keeps of the client's request for the
// commit: the object's attributes and the Object Lock headers.
type edgeAdmitAttrs struct {
	Attrs     objectAttrs `json:"attrs"`
	LockMode  string      `json:"lock_mode,omitempty"`
	LockUntil string      `json:"lock_until,omitempty"`
	// UserAgent is the client's (RP64-11): the access-log row carries it.
	UserAgent string `json:"user_agent,omitempty"`
	// Status is what a GET answers the client (200, 206) — the access-log
	// row's status when the Worker served it.
	Status int `json:"status,omitempty"`
	// CodecVersion is the tenant key version a codec admit answered (the
	// index key and the public key are that version's — WP-EDGE-2).
	CodecVersion int `json:"codec_version,omitempty"`
	// CodecClaims are the chunk claims this admit's index calls took:
	// keyed hash → the claim's pending_since. The commit flips them ready;
	// an expiry or a failed commit drops them (the reaper's rule).
	CodecClaims map[string]time.Time `json:"codec_claims,omitempty"`
}

// edgeAdmitRow is an edge_admits row.
type edgeAdmitRow struct {
	ID            string
	TenantID      string
	Bucket        string
	Key           string
	Op            string
	UploadID      string
	PartNumber    int
	Backend       string
	StorageKey    string
	ContentLength int64
	ContentMD5    string
	ContentType   string
	Attrs         edgeAdmitAttrs
	StorageClass  string
	Floor         string
	ReservedBytes int64
	ReservedFloor string
	FloorReserved bool
	Credited      int64
	ClientIP      string
	RequestID     string
	State         string
	Result        json.RawMessage
	ExpiresAt     time.Time
	// Codec: the admit answered the codec shape (WP-EDGE-2) — no presigned
	// body PUT; the commit carries the chunk list.
	Codec bool
}

// registerEdgeRoutes mounts the two calls when the token is loaded.
func (s *Server) registerEdgeRoutes() {
	if len(s.edge.token) == 0 {
		return
	}
	s.router.Post("/api/v1/edge/admit", s.handleEdgeAdmit)
	s.router.Post("/api/v1/edge/commit", s.handleEdgeCommit)
	s.router.Post("/api/v1/edge/index", s.handleEdgeIndex)
	s.logger.Info("edge ingest routes mounted: /api/v1/edge/admit, /api/v1/edge/commit, /api/v1/edge/index")
}

// edgeNotFound answers a call that is not the Worker's: the route does not
// exist to anyone else (the recovery route's rule for its flag-off case).
func (s *Server) edgeNotFound(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

func writeEdgeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// edgeDescriptor builds the verifier's descriptor and the synthetic request
// the origin's own checks read (headers, path, query; the tenant and scope
// on its context once known).
func edgeDescriptor(req edgeAdmitRequest) (auth.RequestDescriptor, *http.Request, error) {
	h := http.Header{}
	for name, v := range req.Headers {
		h.Add(name, v)
	}
	cl := int64(-1)
	if v := h.Get("Content-Length"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			return auth.RequestDescriptor{}, nil, fmt.Errorf("content-length %q", v)
		}
		cl = n
	}
	d := auth.RequestDescriptor{Method: req.Method, Host: req.Host, Path: req.Path, RawQuery: req.Query, Headers: h, ContentLength: cl}
	if d.Path == "" || d.Path[0] != '/' {
		return d, nil, errors.New("path must start with /")
	}
	u, err := url.ParseRequestURI(d.Path)
	if err != nil {
		return d, nil, fmt.Errorf("path: %w", err)
	}
	u.RawQuery = d.RawQuery
	sr := &http.Request{Method: req.Method, URL: u, Host: req.Host, Header: h, ContentLength: cl, Body: http.NoBody}
	sr.RemoteAddr = req.ClientIP + ":0"
	return d, sr, nil
}

// handleEdgeAdmit is POST /api/v1/edge/admit.
func (s *Server) handleEdgeAdmit(w http.ResponseWriter, r *http.Request) {
	if !s.edgeCallAuthorized(r) {
		s.edgeNotFound(w, r)
		return
	}
	var req edgeAdmitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, edgeAdmitBodyMax)).Decode(&req); err != nil {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Op != edgeOpPut && req.Op != edgeOpPart && req.Op != edgeOpGet {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_op"})
		return
	}
	if s.db == nil || s.engine == nil {
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "error").Inc()
		writeEdgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	resp, status := s.edgeAdmit(r.Context(), req)
	switch {
	case status != http.StatusOK:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "error").Inc()
	case resp.Refusal != nil:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "refused").Inc()
	case resp.Edge && resp.unverified:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "admitted_unverified").Inc()
	case resp.Edge:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "admitted").Inc()
	case resp.Reason == edgeReasonSmall:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "pass_through_small").Inc()
	default:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "pass_through").Inc()
		// Why the origin took it — the one trace of a pass-through (Debug:
		// one line per declined request; the counter is the signal).
		s.logger.Debug("edge admit: pass-through", zap.String("op", req.Op), zap.String("reason", resp.Reason),
			zap.String("path", req.Path), zap.String("request_id", req.RequestID))
	}
	writeEdgeJSON(w, status, resp)
}

// passThrough is the {edge:false} answer with its reason.
func passThrough(reason string) (edgeAdmitResponse, int) {
	return edgeAdmitResponse{Edge: false, Reason: reason}, http.StatusOK
}

// refusalOf is the relayed S3 error a refusal writer wrote to rec.
func refusalOf(rec *relayRecorder) (edgeAdmitResponse, int) {
	return edgeAdmitResponse{Edge: false, Refusal: rec.relay()}, http.StatusOK
}

// edgePrelude is what every admit decides first (edgeAdmitPrelude): the
// caller, the names, the synthetic request the origin's own checks read.
type edgePrelude struct {
	d        auth.RequestDescriptor
	sr       *http.Request // on sctx
	sctx     context.Context
	rec      *relayRecorder
	tenantID string
	scope    *auth.KeyScope
	bucket   string
	key      string
}

// edgeAdmit is the decision. status is the HTTP status of the admit call
// itself (200 for every decision; 5xx when the control plane failed).
func (s *Server) edgeAdmit(ctx context.Context, req edgeAdmitRequest) (edgeAdmitResponse, int) {
	p, resp, status, decided := s.edgeAdmitPrelude(ctx, req)
	if decided {
		return resp, status
	}
	if req.Op == edgeOpGet {
		return s.edgeAdmitGet(req, p)
	}
	return s.edgeAdmitWrite(req, p)
}

// edgeAdmitPrelude is the part of the decision every op shares, in the
// order handleS3Request makes it: the descriptor, the method, SigV4 (the
// one verifier's edge entrance), the reserved tenant, the key's expiry, the
// suspension, the names, the operation and bucket scope, the system bucket,
// the bucket name, the key's length. decided = the answer is resp/status.
func (s *Server) edgeAdmitPrelude(ctx context.Context, req edgeAdmitRequest) (p edgePrelude, resp edgeAdmitResponse, status int, decided bool) {
	decide := func(r edgeAdmitResponse, st int) (edgePrelude, edgeAdmitResponse, int, bool) {
		return edgePrelude{}, r, st, true
	}
	d, sr, err := edgeDescriptor(req)
	if err != nil {
		return decide(passThrough("bad_descriptor"))
	}
	wantMethod, op := http.MethodPut, auth.OpPutObject
	switch req.Op {
	case edgeOpPart:
		op = auth.OpUploadPart
	case edgeOpGet:
		wantMethod, op = http.MethodGet, auth.OpGetObject
	}
	if req.Method != wantMethod {
		return decide(passThrough("not_" + strings.ToLower(wantMethod)))
	}
	sr = sr.WithContext(ctx)
	rec := newRelayRecorder()

	// Authentication: the one verifier's edge entrance, the origin's error
	// mapping (handleS3Request) relayed as the client's XML.
	tenantID, scope, err := s.s3Auth().ValidateDescribed(ctx, d, req.ClientIP)
	if err != nil {
		if d.Headers.Get("Authorization") == "" {
			return decide(passThrough("unsigned")) // the origin answers it (AccessDenied or a web page)
		}
		s.logger.Info("edge admit: authentication failed", zap.Error(err), zap.String("path", req.Path))
		reqID := generateRequestID()
		if errors.Is(err, auth.ErrIPDenied) {
			WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, reqID, WithSuggestion("This key is restricted by IP address."))
			recordAuthFailure(sr, "ip_denied", true)
			return decide(refusalOf(rec))
		}
		errCode := ErrAccessDenied
		switch {
		case errors.Is(err, auth.ErrCredentialUnavailable):
			errCode = ErrInternalError
		case errors.Is(err, auth.ErrAccessKeyRevoked):
			errCode = ErrInvalidAccessKeyId
		case errors.Is(err, auth.ErrSignatureMismatch):
			errCode = ErrSignatureDoesNotMatch
		case errors.Is(err, auth.ErrRequestTimeSkewed):
			errCode = ErrRequestTimeTooSkewed
		case errors.Is(err, auth.ErrInvalidContentSHA256):
			errCode = ErrInvalidArgument
		case strings.Contains(err.Error(), "invalid authorization format"), strings.Contains(err.Error(), "parse"):
			errCode = ErrSignatureDoesNotMatch
		}
		if hint := authErrorHint(err.Error()); hint != "" {
			WriteS3ErrorWithContext(rec, errCode, sr.URL.Path, reqID, WithSuggestion(hint))
		} else {
			WriteS3Error(rec, errCode, sr.URL.Path, reqID)
		}
		reason, known := authFailureReason(err)
		recordAuthFailure(sr, reason, known)
		return decide(refusalOf(rec))
	}
	if tenantID == "" || engine.IsReservedTenantID(tenantID) {
		WriteS3Error(rec, ErrAccessDenied, sr.URL.Path, generateRequestID())
		return decide(refusalOf(rec))
	}
	if scope != nil && auth.IsKeyExpired(scope.ExpiresAt) {
		WriteS3ErrorWithContext(rec, ErrExpiredPresignedRequest, sr.URL.Path, generateRequestID(),
			WithSuggestion("This access key expired at "+scope.ExpiresAt.UTC().Format(time.RFC3339)+". Create or rotate a key in the dashboard."))
		recordAuthFailure(sr, "expired", true)
		return decide(refusalOf(rec))
	}
	if isTenantSuspended(ctx, s.db, tenantID) {
		WriteS3Error(rec, ErrAccountSuspended, sr.URL.Path, generateRequestID())
		return decide(refusalOf(rec))
	}

	// The names, as the parser reads them (decoded path: bucket, key).
	path := strings.TrimPrefix(sr.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket == "" || key == "" {
		return decide(passThrough("no_key"))
	}
	if scope != nil {
		if !auth.OperationAllowed(scope.Permissions, op) {
			WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key does not have %s access.", op)))
			return decide(refusalOf(rec))
		}
		if !auth.CheckBucketScope(scope.BucketScope, bucket) {
			WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key is restricted to buckets: %s", strings.Join(scope.BucketScope, ", "))))
			return decide(refusalOf(rec))
		}
	}
	if tenant.IsSystemBucket(bucket) {
		WriteS3Error(rec, ErrNoSuchBucket, sr.URL.Path, generateRequestID())
		return decide(refusalOf(rec))
	}
	if !validateBucketName(bucket) {
		WriteS3Error(rec, ErrInvalidBucketName, sr.URL.Path, generateRequestID())
		return decide(refusalOf(rec))
	}
	if len(key) > maxS3KeyBytes {
		WriteS3Error(rec, ErrKeyTooLong, sr.URL.Path, generateRequestID())
		return decide(refusalOf(rec))
	}
	// What the origin's own handlers would read of this request.
	tn := &tenant.Tenant{ID: tenantID, Namespace: fmt.Sprintf("tenant/%s/", tenantID), Plan: "starter", Status: "active"}
	sctx := tenant.WithTenant(common.WithTenantID(auth.WithKeyScope(ctx, scope), tenantID), tn)
	sr = sr.WithContext(sctx)
	return edgePrelude{d: d, sr: sr, sctx: sctx, rec: rec, tenantID: tenantID, scope: scope, bucket: bucket, key: key}, edgeAdmitResponse{}, 0, false
}

// edgeAdmitWrite is the PUT / UploadPart decision after the prelude: the
// shapes the edge never takes, the body, the flag, the bucket's own checks,
// the ONE placement decision, the backend, Object Lock, the reservation,
// the address and the link.
func (s *Server) edgeAdmitWrite(req edgeAdmitRequest, p edgePrelude) (edgeAdmitResponse, int) {
	d, sr, sctx, rec, tenantID, bucket, key := p.d, p.sr, p.sctx, p.rec, p.tenantID, p.bucket, p.key
	var err error

	// The shapes the edge never takes: a copy, a sub-resource, a customer
	// key (sealed at the origin), a trailer the Worker cannot verify.
	if sr.Header.Get("x-amz-copy-source") != "" {
		return passThrough("copy")
	}
	if crypto.HasSSECHeaders(sr) {
		return passThrough("ssec")
	}
	// An aws-chunked body (aws-cli ≥ 2.23's default) is admitted on its
	// DECODED length when it is the unsigned-payload trailer shape with a
	// trailer the edge verifies (WP-EDGE-2 Part C): the mover forwards it
	// untouched under a header-signed backend request, the codec decodes
	// it. Any other streaming shape is the origin's.
	stream, why := edgeStreamingOf(sr)
	if why != "" {
		return passThrough(why)
	}
	size := d.ContentLength
	if stream != nil {
		size = stream.Decoded
	} else if size < 0 {
		WriteS3Error(rec, ErrMissingContentLength, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	// A small PutObject is the origin's (EDGE_MIN_BYTES): two round trips
	// fewer, and its bytes do not matter for SLC's bandwidth; an empty one
	// costs nothing to move. An UploadPart is the edge's whatever its size,
	// an empty last part included (D-EDGE-29, RP70-2): a part is never
	// alone — its upload is the unit, one upload is one path, and a small
	// tail passed to the origin's stager raced the codec parts of every
	// upload of ten parts or fewer.
	if req.Op == edgeOpPut {
		if size == 0 {
			return passThrough("empty")
		}
		if s.edge.minBytes > 0 && size < s.edge.minBytes {
			return passThrough(edgeReasonSmall)
		}
	}

	// The flag, per tenant.
	if s.flags == nil || !s.flags.Enabled(flagEdgeIngest, tenantID) {
		return passThrough("flag_off")
	}

	// The bucket (R85-1), the free-tier cap (R10-12), the key's room for
	// its address (WP-R2-1) — the origin's own checks on the synthetic
	// request, their refusals relayed.
	if req.Op == edgeOpPut && s.freeTierBucketCapBlocksWrite(sctx, tenantID, bucket) {
		writeFreeTierBucketCap(rec, sr)
		return refusalOf(rec)
	}
	if !s.requireWriteBucket(rec, sr, "put", tenantID, bucket) {
		return refusalOf(rec)
	}
	if why, tooLong := blobKeyTooLong(sctx, s.db, tenantID, bucket, key); tooLong {
		WriteS3ErrorWithContext(rec, ErrKeyTooLong, sr.URL.Path, generateRequestID(), WithSuggestion(why))
		return refusalOf(rec)
	}
	attrs, err := objectAttrsFromRequest(sr)
	if err != nil {
		WriteS3ErrorWithContext(rec, ErrInvalidRequest, sr.URL.Path, generateRequestID(), WithSuggestion(err.Error()))
		return refusalOf(rec)
	}
	wantMD5, md5Err := parseContentMD5(sr)
	if md5Err != nil {
		WriteS3Error(rec, ErrInvalidDigest, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	contentMD5 := ""
	if wantMD5 != "" {
		contentMD5 = strings.TrimSpace(sr.Header.Get("Content-MD5"))
	}

	// The ONE placement decision (WP-R3-1): a part asks "may a whole
	// unsealed write land in this bucket" — never the chunk threshold, an
	// edge upload is completed whole by the backend (D-EDGE-3).
	var storageClass, floor string
	var uploadAttrs objectAttrs
	sseRequested := sr.Header.Get("x-amz-server-side-encryption") == "AES256"
	if req.Op == edgeOpPut {
		storageClass = resolvePutStorageClass(sctx, s.db, s.engine, tierPlacementGate(s.flags), tenantID, bucket, sr.Header.Get("x-amz-storage-class"))
	} else {
		// A part's placement is the UPLOAD's (RP64-2): the Create recorded
		// the SSE request (089) and the SSE-C key's MD5 (096), and a part's
		// own headers never carry them. A keyless part on an SSE-C upload
		// is the origin's (uploadPartSeal answers its 400, or seals a keyed
		// part); an SSE-requested upload places whole_sealed below and
		// stays a staged one. Never admit a part the origin would refuse.
		var requestedClass string
		var uploadEnc uploadEncryption
		var aErr error
		uploadAttrs, requestedClass, uploadEnc, aErr = s.multipartUploadAttrs(sr, sr.URL.Query().Get("uploadId"))
		if aErr != nil && !errors.Is(aErr, sql.ErrNoRows) {
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
		if uploadEnc.SSECKeyMD5 != "" {
			return passThrough("ssec")
		}
		sseRequested = sseRequested || uploadEnc.SSERequested
		storageClass = resolvePutStorageClass(sctx, s.db, s.engine, tierPlacementGate(s.flags), tenantID, bucket, requestedClass)
	}
	floor = usage.FloorOf(storageClass)
	placeSize := size
	if req.Op == edgeOpPart {
		placeSize = 1
	}
	adapter := s.writeAdapter()
	placement := adapter.decidePlacement(sctx, placementRequest{
		TenantID: tenantID, Bucket: bucket, Class: storageClass, Size: placeSize,
		SSEC: false, SSERequested: sseRequested,
	})
	if placement.Refusal != nil {
		placement.Refusal.write(rec, sr)
		return refusalOf(rec)
	}
	// A chunked placement may be the codec's (WP-EDGE-2): with the
	// edge_codec flag and a tenant key version the HOST chunks, hashes and
	// seals; with the flag and no key version the whole PUT is admitted
	// (no_key — never a refusal, never a plaintext chunked write at the
	// edge); without the flag the origin takes it, as Part A.
	//
	// At the edge the codec's rule is the EDGE's (RP68-1): with the flag a
	// PUT is re-decided as a codec candidate (placementRequest.EdgeCodec —
	// one extra decision, only with the flag on), so a 4–64 MiB PUT the
	// origin would store whole is chunked here when the bucket allows it.
	codecPlacement := placement
	if req.Op == edgeOpPut && placement.Kind != placeChunked && s.flags.Enabled(flagEdgeCodec, tenantID) {
		codecPlacement = adapter.decidePlacement(sctx, placementRequest{
			TenantID: tenantID, Bucket: bucket, Class: storageClass, Size: placeSize,
			SSEC: false, SSERequested: sseRequested, EdgeCodec: true,
		})
	}
	if req.Op == edgeOpPut && codecPlacement.Refusal == nil && codecPlacement.Kind == placeChunked {
		resp, st, verdict := s.edgeAdmitCodecPut(req, p, size, attrs, storageClass, floor)
		switch {
		case verdict == edgeCodecTaken:
			return stream.annotate(resp, size), st
		case placement.Kind == placeWhole:
			// The origin would store it whole: Part A's whole PUT below.
		case verdict == edgeCodecWhole && placement.Kind == placeChunked:
			// Part A's whole PUT admits it below (no_key).
		default:
			return passThrough("placement_" + placement.Kind.String())
		}
	} else if req.Op == edgeOpPart {
		resp, st, verdict := s.edgeAdmitCodecPart(req, p, size, uploadAttrs, storageClass, floor)
		if verdict == edgeCodecTaken {
			return stream.annotate(resp, size), st
		}
		if placement.Kind != placeWhole {
			return passThrough("placement_" + placement.Kind.String())
		}
	} else if placement.Kind != placeWhole {
		return passThrough("placement_" + placement.Kind.String())
	}

	// The backend the placement names: the region driver of a pinned
	// bucket, else what the class resolves to on this engine (placeOn's
	// rule) — and it must be an EdgeWriter whose breaker is closed.
	backend, err := bucketRegionDriver(sctx, s.db, s.engine, tenantID, bucket)
	if err != nil {
		if errors.Is(err, errRegionDriverUnavailable) {
			return passThrough("region_unavailable") // the origin answers its 503
		}
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	if backend == "" {
		drivers := map[string]engine.Driver{}
		for _, name := range s.engine.GetDriverNames() {
			if drv, ok := s.engine.GetDriver(name); ok {
				drivers[name] = drv
			}
		}
		backend, _ = engine.ResolveStorageClass(storageClass, s.engine.GetPrimary(), drivers)
	}
	ew, err := s.edgeWriterFor(backend)
	if err != nil {
		return passThrough("backend:" + backend)
	}
	// The mover forwards an aws-chunked body only where the backend's
	// behaviour with a header-signed request is measured (Step 0.3 + the
	// bench § 1), and a PutObject only where the backend bounds the
	// signature's life: iDrive checks no x-amz-date, so a minted signature
	// would be a bearer for the object's address until the key rotates — a
	// part's dies with its upload (D-EDGE-33).
	var streamProps engine.EdgeStreamingProps
	if stream != nil {
		streamProps = ew.EdgeStreaming()
		switch {
		case !streamProps.Supported:
			return passThrough("aws_chunked_backend")
		case req.Op == edgeOpPut && streamProps.SignatureMaxAge <= 0:
			return passThrough("aws_chunked_unbounded")
		}
	}

	// Object Lock (R2-01): a retained key refuses the overwrite before
	// anything is reserved.
	if lockErr := checkObjectLock(sctx, s.db, tenantID, bucket, key, isObjectLockBypass(sr)); lockErr != nil {
		writeObjectLockRefusal(rec, sr, s.logger, lockErr)
		return refusalOf(rec)
	}
	// If-Match on a PUT, as HandlePut.
	if req.Op == edgeOpPut && sr.Header.Get("If-Match") != "" {
		var existingETag string
		if qErr := s.db.QueryRowContext(sctx, `SELECT etag FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			tenantID, bucket, key).Scan(&existingETag); qErr == nil && checkIfMatch(sr, existingETag) {
			rec.WriteHeader(http.StatusPreconditionFailed)
			return refusalOf(rec)
		}
	}

	// The reservation and the address; a refusal after it releases it
	// (nothing pending is left for a refused admit).
	row := edgeAdmitRow{TenantID: tenantID, Bucket: bucket, Key: key, Op: req.Op, Backend: backend,
		ContentLength: size, ContentMD5: contentMD5, ContentType: attrs.ContentType,
		Attrs: edgeAdmitAttrs{Attrs: attrs, LockMode: sr.Header.Get("x-amz-object-lock-mode"),
			LockUntil: sr.Header.Get("x-amz-object-lock-retain-until-date"), UserAgent: sr.Header.Get("User-Agent")},
		StorageClass: storageClass, Floor: floor, ClientIP: req.ClientIP, RequestID: req.RequestID}
	container := tenantID + "_" + bucket
	var presigned string
	var signed []string
	var streamHeaders map[string]string
	var presignErr error
	settled := false
	switch req.Op {
	case edgeOpPut:
		if s.quotaManager != nil {
			bill, ok, qErr := s.reserveWrite(sctx, tenantID, bucket, key, floor, size)
			if qErr != nil {
				s.logger.Error("edge admit: quota check failed", zap.Error(qErr), zap.String("tenant_id", tenantID))
				return edgeAdmitResponse{}, http.StatusInternalServerError
			}
			if !ok {
				WriteS3ErrorWithContext(rec, ErrQuotaExceeded, sr.URL.Path, generateRequestID(), WithSuggestion("Upgrade at "+billingURL()))
				return refusalOf(rec)
			}
			row.ReservedBytes, row.ReservedFloor, row.FloorReserved, row.Credited = bill.Reserved, bill.Floor, bill.FloorReserved, bill.Credited
			defer func() {
				if !settled {
					s.releaseQuota(context.WithoutCancel(sctx), tenantID, row.ReservedFloor, row.ReservedBytes)
				}
			}()
		}
		if row.StorageKey, err = newBlobAddress(key); err != nil {
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
		if stream != nil {
			streamHeaders, presigned, presignErr = ew.SignPut(sctx, container, row.StorageKey, size, stream.Trailer)
		} else {
			presigned, signed, presignErr = ew.PresignPut(sctx, container, row.StorageKey,
				engine.EdgePresign{ContentLength: size, ContentMD5: contentMD5, ContentType: attrs.ContentType, TTL: edgeAdmitTTL})
		}
	case edgeOpPart:
		uploadID := sr.URL.Query().Get("uploadId")
		partNumber, pErr := strconv.Atoi(sr.URL.Query().Get("partNumber"))
		if pErr != nil || partNumber < 1 || partNumber > maxPartNumber {
			WriteS3Error(rec, ErrInvalidPartNumber, sr.URL.Path, generateRequestID())
			return refusalOf(rec)
		}
		active, aErr := s.multipartUploadActive(sr, tenantID, bucket, key, uploadID)
		if aErr != nil {
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
		if !active {
			WriteS3Error(rec, ErrNoSuchUpload, sr.URL.Path, generateRequestID())
			return refusalOf(rec)
		}
		// The in-flight cap on the declared size (stageUploadPart's).
		if s.multipartMaxUploadBytes > 0 {
			var existing int64
			if qErr := s.db.QueryRowContext(sctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM multipart_parts WHERE upload_id = $1 AND part_number <> $2`,
				uploadID, partNumber).Scan(&existing); qErr != nil {
				return edgeAdmitResponse{}, http.StatusInternalServerError
			}
			if existing+size > s.multipartMaxUploadBytes {
				WriteS3ErrorWithContext(rec, ErrEntityTooLarge, sr.URL.Path, generateRequestID(),
					WithSuggestion(fmt.Sprintf("This part would push the upload past the %d-byte in-flight limit. Complete or abort the upload, or use fewer/smaller parts.", s.multipartMaxUploadBytes)))
				return refusalOf(rec)
			}
		}
		if s.quotaManager == nil {
			return passThrough("unbilled") // the edge part path reserves on the upload row; without a quota manager the origin stages it
		}
		res, rErr := reserveEdgePart(sctx, s.db, tenantID, uploadID, key, partNumber, size,
			func(cctx context.Context, storageKey string) (string, string, error) {
				cctx, cancel := context.WithTimeout(cctx, edgeBackendCallTimeout)
				defer cancel()
				buid, cErr := ew.CreateMultipart(cctx, container, storageKey, uploadAttrs.ContentType)
				return backend, buid, cErr
			})
		switch {
		case errors.Is(rErr, errUploadStaged):
			return passThrough("staged_parts")
		case errors.Is(rErr, errUploadCodec):
			// One upload is one path (D-EDGE-3): an edge-whole part on a
			// codec upload is the client's 400.
			WriteS3ErrorWithContext(rec, ErrInvalidRequest, sr.URL.Path, generateRequestID(), WithSuggestion(codecUploadRefusal))
			return refusalOf(rec)
		case errors.Is(rErr, errUploadEnded):
			WriteS3Error(rec, ErrNoSuchUpload, sr.URL.Path, generateRequestID())
			return refusalOf(rec)
		case errors.Is(rErr, errPartOverQuota):
			WriteS3ErrorWithContext(rec, ErrQuotaExceeded, sr.URL.Path, generateRequestID(),
				WithSuggestion("Parts in progress count against your storage. Upgrade at "+billingURL()+", or abort uploads you no longer need."))
			return refusalOf(rec)
		case rErr != nil:
			s.logger.Error("edge admit: part reservation failed", zap.Error(rErr), zap.String("upload_id", uploadID))
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
		row.UploadID, row.PartNumber, row.ReservedBytes, row.StorageKey = uploadID, partNumber, res.Reserved, res.Upload.StorageKey
		row.Backend = res.Upload.Backend
		if res.Upload.Backend != backend {
			// An upload begun on another backend (the placement moved
			// between its parts): its own writer signs the link.
			if ew, err = s.edgeWriterFor(res.Upload.Backend); err != nil {
				_ = releaseUploadPart(context.WithoutCancel(sctx), s.db, tenantID, uploadID, partNumber, res.Reserved)
				return passThrough("backend:" + res.Upload.Backend)
			}
			backend = res.Upload.Backend
		}
		defer func() {
			if !settled {
				if relErr := releaseUploadPart(context.WithoutCancel(sctx), s.db, tenantID, uploadID, partNumber, res.Reserved); relErr != nil {
					s.logger.Error("edge admit: part reservation not given back", zap.Error(relErr), zap.String("upload_id", uploadID))
				}
			}
		}()
		if stream != nil {
			streamHeaders, presigned, presignErr = ew.SignUploadPart(sctx, container, row.StorageKey, res.Upload.BackendUploadID, partNumber, size, stream.Trailer)
		} else {
			presigned, signed, presignErr = ew.PresignUploadPart(sctx, container, row.StorageKey, res.Upload.BackendUploadID, partNumber,
				engine.EdgePresign{ContentLength: size, ContentMD5: contentMD5, TTL: edgeAdmitTTL})
		}
	}
	if presignErr != nil {
		s.logger.Error("edge admit: presign failed", zap.Error(presignErr), zap.String("backend", backend))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	if stream != nil {
		// A header-signed request must sign exactly the aws-chunked shape and
		// never a length (the client's framing decides it).
		if cErr := edgeHeaderSigns(streamHeaders); cErr != nil {
			s.logger.Error("edge admit: the header-signed request does not sign what it must — refused", zap.Error(cErr), zap.String("backend", backend))
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
		return s.recordEdgeWriteAdmit(sctx, req, &row, &settled, edgeAdmitResponse{Edge: true, URL: presigned, Method: http.MethodPut,
			Headers: streamHeaders, StreamingTrailer: stream.Trailer, ContentLength: size, Backend: backend,
			Expect: &edgeExpect{MD5: contentMD5 != ""}, unverified: !streamProps.VerifiesTrailer && contentMD5 == ""})
	}
	// The owner's 2026-10-25 finding can never regress: a link that does
	// not sign the size (and the checksum, when the client sent one) is
	// never handed out.
	wantSigned := []string{"content-length"}
	if contentMD5 != "" {
		wantSigned = append(wantSigned, "content-md5")
	}
	if cErr := edgePresignSigns(presigned, wantSigned...); cErr != nil {
		s.logger.Error("edge admit: the presigned link does not sign what it must — refused", zap.Error(cErr), zap.String("backend", backend))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	sendHeaders := map[string]string{"content-length": strconv.FormatInt(size, 10)}
	for _, name := range signed {
		switch name {
		case "content-md5":
			sendHeaders["content-md5"] = contentMD5
		case "content-type":
			sendHeaders["content-type"] = attrs.ContentType
		case "host", "content-length":
		default:
			// A header the link signs that the Worker does not know: never
			// hand out a link the Worker cannot use.
			s.logger.Error("edge admit: the presigned link signs a header the Worker does not send", zap.String("header", name))
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
	}

	return s.recordEdgeWriteAdmit(sctx, req, &row, &settled, edgeAdmitResponse{Edge: true, URL: presigned, Method: http.MethodPut,
		SendHeaders: sendHeaders, Backend: backend, Expect: &edgeExpect{MD5: contentMD5 != ""}})
}

// recordEdgeWriteAdmit writes the admit row of a write admitted for the
// mover and completes its answer (the id, the address, the expiry);
// settled = the reservation is the row's from here.
func (s *Server) recordEdgeWriteAdmit(ctx context.Context, req edgeAdmitRequest, row *edgeAdmitRow, settled *bool, resp edgeAdmitResponse) (edgeAdmitResponse, int) {
	var err error
	if row.ID, err = newEdgeAdmitID(); err != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	row.ExpiresAt = time.Now().UTC().Add(edgeAdmitExpiry)
	if err := insertEdgeAdmit(ctx, s.db, *row); err != nil {
		s.logger.Error("edge admit: row not written", zap.Error(err))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	*settled = true
	s.logger.Info("edge admit", zap.String("op", req.Op), zap.String("tenant_id", row.TenantID), zap.String("bucket", row.Bucket),
		zap.String("key", row.Key), zap.String("backend", row.Backend), zap.Int64("size", row.ContentLength), zap.String("admit_id", row.ID),
		zap.Bool("aws_chunked", resp.StreamingTrailer != ""), zap.String("request_id", req.RequestID))
	resp.AdmitID, resp.StorageKey, resp.ExpiresAt = row.ID, row.StorageKey, &row.ExpiresAt
	return resp, http.StatusOK
}

// edgeVerifiedTrailers are the trailers an aws-chunked write may carry to
// be admitted at the edge: the ones the codec verifies (src/awschunked.js:
// aws-cli's default CRC64NVME and the CRC32 of older clients), the mover
// the same set (one rule; iDrive verifies these too).
var edgeVerifiedTrailers = map[string]bool{"x-amz-checksum-crc64nvme": true, "x-amz-checksum-crc32": true}

// edgeStreaming is an aws-chunked write the edge may take: its trailer and
// its decoded length.
type edgeStreaming struct {
	Trailer string
	Decoded int64
}

// edgeStreamingOf reads the aws-chunked shape of a write: nil and "" for a
// plain body; nil and a pass-through reason for a streaming shape the edge
// never takes (signed chunks — the client's key in every chunk signature —
// a trailer the edge does not verify, no decoded length); the shape
// otherwise.
func edgeStreamingOf(r *http.Request) (*edgeStreaming, string) {
	if !isAWSChunked(r) {
		return nil, ""
	}
	chunked := false
	for _, tok := range strings.Split(r.Header.Get("Content-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "aws-chunked") {
			chunked = true
		}
	}
	if r.Header.Get("x-amz-content-sha256") != "STREAMING-UNSIGNED-PAYLOAD-TRAILER" || !chunked {
		return nil, "aws_chunked"
	}
	trailer := strings.ToLower(strings.TrimSpace(r.Header.Get("x-amz-trailer")))
	if !edgeVerifiedTrailers[trailer] {
		return nil, "aws_chunked_trailer"
	}
	n, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("x-amz-decoded-content-length")), 10, 64)
	if err != nil || n < 0 {
		return nil, "aws_chunked"
	}
	return &edgeStreaming{Trailer: trailer, Decoded: n}, ""
}

// annotate marks a codec admit of an aws-chunked write: the trailer the
// host decodes and verifies, the decoded length (D-EDGE-31).
func (e *edgeStreaming) annotate(resp edgeAdmitResponse, decoded int64) edgeAdmitResponse {
	if e != nil && resp.Edge {
		resp.StreamingTrailer, resp.ContentLength = e.Trailer, decoded
	}
	return resp
}

// edgeHeaderSigns is the checker every header-signed request passes: its
// SignedHeaders name the aws-chunked shape, x-amz-date and the host — and
// never content-length.
func edgeHeaderSigns(headers map[string]string) error {
	auth := headers["authorization"]
	i := strings.Index(auth, "SignedHeaders=")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || i < 0 {
		return errors.New("header-signed request: not a SigV4 authorization")
	}
	list := auth[i+len("SignedHeaders="):]
	if j := strings.IndexByte(list, ','); j >= 0 {
		list = list[:j]
	}
	have := map[string]bool{}
	for _, n := range strings.Split(list, ";") {
		have[strings.TrimSpace(n)] = true
	}
	for _, n := range []string{"content-encoding", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-decoded-content-length", "x-amz-trailer"} {
		if !have[n] {
			return fmt.Errorf("header-signed request does not sign %s (signed: %s)", n, list)
		}
	}
	if have["content-length"] {
		return errors.New("header-signed request signs content-length: the client's framing decides it")
	}
	if headers["x-amz-content-sha256"] != "STREAMING-UNSIGNED-PAYLOAD-TRAILER" {
		return errors.New("header-signed request: not the streaming-trailer payload")
	}
	return nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// edgePresignSigns is the checker every admitted link passes: its
// X-Amz-SignedHeaders must name every header in names (a host-only link —
// which both backends accept any body on — fails it).
func edgePresignSigns(presigned string, names ...string) error {
	u, err := url.Parse(presigned)
	if err != nil {
		return fmt.Errorf("presigned link: %w", err)
	}
	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Signature") == "" {
		return errors.New("presigned link: not a SigV4 query signature")
	}
	have := map[string]bool{}
	for _, n := range strings.Split(strings.ToLower(q.Get("X-Amz-SignedHeaders")), ";") {
		have[strings.TrimSpace(n)] = true
	}
	for _, n := range names {
		if !have[strings.ToLower(n)] {
			return fmt.Errorf("presigned link does not sign %s (signed: %s)", n, q.Get("X-Amz-SignedHeaders"))
		}
	}
	return nil
}

// newEdgeAdmitID mints an admit id: a blob address's id shape (26 Crockford
// base32 characters, 130 random bits).
func newEdgeAdmitID() (string, error) {
	addr, err := newBlobAddress("")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(addr, "@"), nil
}

func insertEdgeAdmit(ctx context.Context, db *sql.DB, row edgeAdmitRow) error {
	attrs, err := json.Marshal(row.Attrs)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO edge_admits
			(id, tenant_id, bucket, object_key, op, upload_id, part_number, backend, storage_key, content_length,
			 content_md5, content_type, attrs, storage_class, floor, reserved_bytes, reserved_floor,
			 floor_reserved, credited, client_ip, request_id, state, expires_at, codec)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, 0), $8, $9, $10, NULLIF($11, ''), $12, $13, $14, $15,
		        $16, NULLIF($17, ''), $18, $19, $20, $21, 'pending', $22, $23)`,
		row.ID, row.TenantID, row.Bucket, row.Key, row.Op, row.UploadID, row.PartNumber, row.Backend, row.StorageKey, row.ContentLength,
		row.ContentMD5, row.ContentType, attrs, row.StorageClass, row.Floor, row.ReservedBytes, row.ReservedFloor,
		row.FloorReserved, row.Credited, row.ClientIP, row.RequestID, row.ExpiresAt, row.Codec)
	return err
}

// --- commit ---

// edgeCommitOutcome is the Worker's report of how the backend write ended.
const (
	edgeOutcomeOK           = "ok"
	edgeOutcomeBackendError = "backend_error"
	edgeOutcomeEdgeError    = "edge_error"
	// edgeOutcomeChecksumMismatch: the codec host decoded an aws-chunked
	// body whose trailer is not the body's (D-EDGE-31) — the client's 400.
	edgeOutcomeChecksumMismatch = "checksum_mismatch"
)

// handleEdgeCommit is POST /api/v1/edge/commit.
func (s *Server) handleEdgeCommit(w http.ResponseWriter, r *http.Request) {
	if !s.edgeCallAuthorized(r) {
		s.edgeNotFound(w, r)
		return
	}
	var req edgeCommitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, edgeAdmitBodyMax)).Decode(&req); err != nil {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.AdmitID == "" {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "admit_id_required"})
		return
	}
	if s.db == nil {
		writeEdgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	// The Worker's call ending is not a cancel (RP64-1; the codebase's own
	// rule for a chunk PUT and a long operation): the Worker aborts its
	// commit call at 10 s, and a disconnect used to cancel the context the
	// terminal UPDATE ran on — the head row, the version row and the bill
	// were committed while the admit row stayed pending, and the retry
	// re-ran the whole settle (a second version row of the same bytes).
	// The commit runs to its end on its own clock; the Worker's retry
	// reads the recorded result.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), edgeCommitBound)
	defer cancel()
	relay, status, err := s.edgeCommit(cctx, req)
	if err != nil {
		s.logger.Error("edge commit failed", zap.Error(err), zap.String("admit_id", req.AdmitID))
		writeEdgeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeEdgeJSON(w, status, relay)
}

var (
	errEdgeAdmitUnknown = errors.New("unknown admit")
	errEdgeAdmitExpired = errors.New("admit expired")
)

// edgeCommitBound is the commit's own clock, detached from the Worker's
// call (RP64-1): the row lock, the flip and the part settle are database
// work; the backend delete of a refused blob runs AFTER the transaction
// (edgeBackendCallTimeout of its own). Far beyond any of them; a commit
// that cannot finish inside it is a database that has stopped answering.
const edgeCommitBound = 2 * time.Minute

// edgeCommit is the commit under the admit row's lock: a terminal row
// answers its recorded result again (idempotent); a pending one is settled
// exactly once. The status is the commit call's own (200; 404 unknown; 410
// expired; 5xx a control-plane failure).
func (s *Server) edgeCommit(ctx context.Context, req edgeCommitRequest) (*edgeRelay, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := lockEdgeAdmit(ctx, tx, req.AdmitID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, http.StatusNotFound, errEdgeAdmitUnknown
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	switch row.State {
	case "committed", "failed":
		var replay edgeRelay
		if jErr := json.Unmarshal(row.Result, &replay); jErr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("replay the admit's result: %w", jErr)
		}
		return &replay, http.StatusOK, nil
	case "expired":
		return nil, http.StatusGone, errEdgeAdmitExpired
	}
	if time.Now().After(row.ExpiresAt) {
		// Past its expiry and not yet reaped: the reaper's rule, now.
		s.expireEdgeAdmit(ctx, tx, row)
		if err := tx.Commit(); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return nil, http.StatusGone, errEdgeAdmitExpired
	}

	rec := newRelayRecorder()
	state, bytes, after := s.settleEdgeAdmit(ctx, row, req, rec)
	relay := rec.relay()
	result, _ := json.Marshal(relay)
	if _, err := tx.ExecContext(ctx, `
		UPDATE edge_admits SET state = $2, backend_status = NULLIF($3, 0), backend_etag = NULLIF($4, ''), result = $5, finished_at = NOW()
		WHERE id = $1`, row.ID, state, req.BackendStatus, req.BackendETag, result); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("record the commit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if after != nil {
		// The backend delete of a refused blob (up to 40 s): after the row
		// is terminal and unlocked, never inside the transaction a retry
		// of this commit waits on (RP64-1).
		after()
	}
	if state == "committed" {
		dataPlaneBytes.WithLabelValues(edgeHost, row.Op).Add(float64(bytes))
	}
	s.logger.Info("edge commit", zap.String("op", row.Op), zap.String("tenant_id", row.TenantID), zap.String("bucket", row.Bucket),
		zap.String("key", row.Key), zap.String("backend", row.Backend), zap.String("state", state), zap.Int("status", relay.Status),
		zap.Int64("bytes", bytes), zap.Int("backend_status", req.BackendStatus), zap.Int64("duration_ms", req.DurationMS),
		zap.String("admit_id", row.ID), zap.String("request_id", row.RequestID))
	return relay, http.StatusOK, nil
}

// lockEdgeAdmit reads the row FOR UPDATE.
func lockEdgeAdmit(ctx context.Context, tx *sql.Tx, id string) (edgeAdmitRow, error) {
	var row edgeAdmitRow
	var uploadID, md5, reservedFloor sql.NullString
	var partNumber sql.NullInt64
	var attrs []byte
	var result []byte
	err := tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, bucket, object_key, op, upload_id, part_number, backend, storage_key, content_length,
		       content_md5, content_type, attrs, storage_class, floor, reserved_bytes, reserved_floor,
		       floor_reserved, credited, client_ip, request_id, state, result, expires_at, codec
		FROM edge_admits WHERE id = $1 FOR UPDATE`, id).Scan(
		&row.ID, &row.TenantID, &row.Bucket, &row.Key, &row.Op, &uploadID, &partNumber, &row.Backend, &row.StorageKey, &row.ContentLength,
		&md5, &row.ContentType, &attrs, &row.StorageClass, &row.Floor, &row.ReservedBytes, &reservedFloor,
		&row.FloorReserved, &row.Credited, &row.ClientIP, &row.RequestID, &row.State, &result, &row.ExpiresAt, &row.Codec)
	if err != nil {
		return row, err
	}
	row.UploadID, row.PartNumber = uploadID.String, int(partNumber.Int64)
	row.ContentMD5, row.ReservedFloor = md5.String, reservedFloor.String
	row.Result = result
	if len(attrs) > 0 {
		_ = json.Unmarshal(attrs, &row.Attrs)
	}
	return row, nil
}

// releaseEdgeAdmit gives an admit's reservation back (a failed or expired
// write). On its own context: the request's may be gone.
func (s *Server) releaseEdgeAdmit(ctx context.Context, row edgeAdmitRow) {
	ctx = context.WithoutCancel(ctx)
	switch row.Op {
	case edgeOpPut:
		s.releaseQuota(ctx, row.TenantID, row.ReservedFloor, row.ReservedBytes)
	case edgeOpPart:
		if err := releaseUploadPart(ctx, s.db, row.TenantID, row.UploadID, row.PartNumber, row.ReservedBytes); err != nil {
			s.logger.Error("edge: part reservation not given back — the reconcile and the upload's end release it",
				zap.Error(err), zap.String("upload_id", row.UploadID), zap.Int64("bytes", row.ReservedBytes))
		}
	}
}

// expireEdgeAdmit is the reaper's rule for a pending row past its expiry,
// inside the caller's transaction on the locked row. It BRANCHES ON op: a
// write's reservation goes back and a PUT's address is queued for the
// collector (the body may have landed with no commit); an expired GET
// (Part B) is a read whose commit was lost — its ADMITTED size is metered
// (the upper bound; the trade in docs/reviews/WP-EDGE-1B.md) and nothing is
// released or queued: its storage_key is the LIVE object's blob. Then
// state expired.
func (s *Server) expireEdgeAdmit(ctx context.Context, tx *sql.Tx, row edgeAdmitRow) {
	switch row.Op {
	case edgeOpGet:
		s.recordEdgeEgress(ctx, row.TenantID, row.Backend, row.ContentLength)
		s.logger.Info("edge get admit expired without a commit — the admitted size is metered",
			zap.String("tenant_id", row.TenantID), zap.String("bucket", row.Bucket), zap.String("key", row.Key),
			zap.Int64("bytes", row.ContentLength), zap.String("admit_id", row.ID))
	default:
		s.releaseEdgeAdmit(ctx, row)
		switch {
		case row.Codec:
			// A codec admit holds no whole blob — its pending chunk claims
			// are dropped (blobs the host reported landed and never
			// committed are the chunk orphan sweep's, WP-EDGE-2).
			s.dropEdgeCodecClaims(context.WithoutCancel(ctx), row)
		case row.Op == edgeOpPut:
			retireOrphanWrite(ctx, s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
		}
	}
	_, _ = tx.ExecContext(ctx, `UPDATE edge_admits SET state = 'expired', finished_at = NOW() WHERE id = $1`, row.ID)
}

// edgeDeleteBlob removes a blob the commit refused (a digest mismatch) on
// its backend, best effort, and queues it for the collector as well. Runs
// after the admit row's transaction committed (RP64-1): up to 40 s of
// backend call never holds the row's lock or a DB connection.
func (s *Server) edgeDeleteBlob(ctx context.Context, row edgeAdmitRow) {
	ctx = context.WithoutCancel(ctx)
	if drv, ok := s.engine.GetDriver(row.Backend); ok {
		dctx, cancel := context.WithTimeout(common.WithTenantID(ctx, row.TenantID), edgeBackendCallTimeout)
		defer cancel()
		if err := drv.Delete(dctx, row.TenantID+"_"+row.Bucket, row.StorageKey); err != nil && !isObjectMissingErr(err) {
			s.logger.Error("edge commit: the refused blob could not be removed from the backend — queued for the collector",
				zap.Error(err), zap.String("backend", row.Backend), zap.String("storage_key", row.StorageKey))
		}
	}
	retireOrphanWrite(ctx, s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
}

// settleEdgeAdmit decides and performs the commit of a pending admit,
// writing the client's answer to rec; returns the row's new state, the
// bytes committed and what runs AFTER the row's transaction commits (the
// backend delete of a refused blob, nil otherwise — RP64-1). The Worker is
// not in the byte path (it reports the backend's answer only), so the
// integrity the control plane relies on is the backend's: the signed
// content-length and Content-MD5 it enforced, and the ETag it answered —
// the body's MD5 on both backends (Step 0.2), the row's ETag.
func (s *Server) settleEdgeAdmit(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder) (string, int64, func()) {
	path := "/" + row.Bucket + "/" + row.Key
	reqID := row.RequestID
	if reqID == "" {
		reqID = generateRequestID()
	}
	fail := func(code string, opts ...ErrorOption) (string, int64, func()) {
		s.releaseEdgeAdmit(ctx, row)
		if row.Op == edgeOpPut {
			retireOrphanWrite(context.WithoutCancel(ctx), s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
		}
		WriteS3ErrorWithContext(rec, code, path, reqID, opts...)
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0, nil
	}
	if row.Op == edgeOpGet {
		state, n := s.settleEdgeGet(ctx, row, req, rec)
		return state, n, nil
	}
	if row.Codec {
		// The codec commit (WP-EDGE-2): the chunk list settled, the manifest
		// installed — or everything given back.
		if row.Op == edgeOpPut {
			state, n := s.commitEdgeCodecPut(ctx, row, req, rec)
			return state, n, nil
		}
		state, n := s.commitEdgeCodecPart(ctx, row, req, rec)
		return state, n, nil
	}
	deleteAfter := func() { s.edgeDeleteBlob(ctx, row) }
	if req.Outcome != edgeOutcomeOK {
		switch {
		case req.Outcome == edgeOutcomeBackendError && req.BackendStatus == http.StatusBadRequest &&
			(req.BackendCode == "BadDigest" || req.BackendCode == "InvalidDigest"):
			// The client's own Content-MD5 did not match its body: the
			// backend refused the write (nothing stored on either backend).
			return fail(ErrBadDigest)
		case req.Outcome == edgeOutcomeBackendError && req.BackendStatus == http.StatusBadRequest && req.BackendCode == ErrXAmzContentChecksumMismatch:
			// An aws-chunked body whose trailer is not its own: iDrive
			// verified it and refused the write (WP-EDGE-2 Part C).
			return fail(ErrXAmzContentChecksumMismatch)
		case req.Outcome == edgeOutcomeBackendError && (req.BackendCode == "IncompleteBody" || req.BackendStatus == http.StatusRequestTimeout ||
			req.BackendStatus == http.StatusLengthRequired):
			// The body ended short of the signed content-length.
			return fail(ErrIncompleteBody)
		case req.Outcome == edgeOutcomeBackendError && req.BackendCode == "NoSuchUpload" && row.Op == edgeOpPart:
			// The upload was aborted between the admit and the PUT (a
			// client racing its own Abort): the backend's verdict is the
			// client's, at once — not a 503 it would retry (RP64-9).
			return fail(ErrNoSuchUpload)
		default:
			rec.Header().Set("Retry-After", "30")
			s.logger.Warn("edge commit: the backend did not take the body — 503",
				zap.String("outcome", req.Outcome), zap.Int("backend_status", req.BackendStatus), zap.String("backend_code", req.BackendCode),
				zap.String("backend", row.Backend), zap.String("admit_id", row.ID))
			return fail(ErrServiceUnavailable)
		}
	}
	// The backend's ETag is the body's MD5 on every EdgeWriter backend: the
	// row records it, the client reads it back. A backend that answered
	// something else is refused — nothing is committed under an ETag a read
	// cannot verify (object_integrity.go).
	etag := strings.ToLower(strings.Trim(strings.TrimSpace(req.BackendETag), `"`))
	if len(etag) != 32 || !isHex(etag) {
		s.logger.Error("edge commit: the backend's ETag is not an MD5 — the blob is removed, 500",
			zap.String("backend", row.Backend), zap.String("backend_etag", req.BackendETag), zap.String("admit_id", row.ID))
		state, n, _ := fail(ErrInternalError, WithSuggestion("The backend answered no MD5 ETag for this write; nothing was stored. Retry."))
		return state, n, deleteAfter
	}
	if row.ContentMD5 != "" {
		// The link signed it and the backend enforced it: the ETag must be
		// that digest (a backend that let other bytes through is refused).
		want, _ := base64.StdEncoding.DecodeString(row.ContentMD5)
		if hex.EncodeToString(want) != etag {
			state, n, _ := fail(ErrBadDigest)
			return state, n, deleteAfter
		}
	}
	var state string
	var n int64
	switch row.Op {
	case edgeOpPut:
		state, n = s.commitEdgePut(ctx, row, rec, etag, reqID)
	default:
		state, n = s.commitEdgePart(ctx, row, req, rec, etag, reqID)
	}
	return state, n, nil
}

// commitEdgePut is the PUT's commit: commitBlobWrite exactly as HandlePut,
// then the lock, the log, the notification and the event.
func (s *Server) commitEdgePut(ctx context.Context, row edgeAdmitRow, rec *relayRecorder, etag, reqID string) (string, int64) {
	cctx := common.WithTenantID(context.WithoutCancel(ctx), row.TenantID)
	var bill *writeBill
	if s.quotaManager != nil {
		bill = &writeBill{Floor: row.ReservedFloor, Reserved: row.ReservedBytes, FloorReserved: row.FloorReserved, Credited: row.Credited}
	}
	attrs := row.Attrs.Attrs
	if attrs.ContentType == "" {
		attrs.ContentType = row.ContentType
	}
	flip, dbErr := commitBlobWrite(cctx, s.db, blobWrite{TenantID: row.TenantID, Bucket: row.Bucket, Key: row.Key, Bill: bill,
		Version: &versionWrite{Size: row.ContentLength, ETag: etag, ContentType: attrs.ContentType, Backend: row.Backend,
			Blob: versionBlobRef{StorageKey: row.StorageKey}},
		Upsert: func(tx *sql.Tx) error {
			return upsertWholeObjectHeadRow(cctx, tx, row.TenantID, row.Bucket, row.Key, row.StorageKey, row.ContentLength, etag,
				row.Backend, row.Floor, attrs)
		}})
	if dbErr != nil {
		s.releaseEdgeAdmit(ctx, row)
		retireOrphanWrite(cctx, s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
		sr := &http.Request{Method: http.MethodPut, URL: &url.URL{Path: "/" + row.Bucket + "/" + row.Key}}
		sr = sr.WithContext(cctx)
		switch {
		case refusedAtCommit(rec, sr, dbErr):
		case errors.Is(dbErr, errQuotaRace):
			WriteS3ErrorWithContext(rec, ErrQuotaExceeded, sr.URL.Path, reqID, WithSuggestion("Upgrade at "+billingURL()))
		default:
			s.logger.Error("edge commit: head row write failed", zap.Error(dbErr), zap.String("tenant_id", row.TenantID),
				zap.String("bucket", row.Bucket), zap.String("key", row.Key))
			WriteS3Error(rec, ErrInternalError, sr.URL.Path, reqID)
		}
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}
	// The bucket's default retention and the request's lock headers,
	// exactly as plain PUT (applyObjectLockOnPut reads the headers).
	lr := &http.Request{Header: http.Header{}}
	if row.Attrs.LockMode != "" {
		lr.Header.Set("x-amz-object-lock-mode", row.Attrs.LockMode)
		lr.Header.Set("x-amz-object-lock-retain-until-date", row.Attrs.LockUntil)
	}
	applyObjectLockOnPut(cctx, s.db, row.TenantID, row.Bucket, row.Key, lr)

	rec.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	rec.Header().Set("x-amz-request-id", reqID)
	rec.Header().Set("x-amz-object-size", strconv.FormatInt(row.ContentLength, 10))
	if flip.VersionID != "" {
		rec.Header().Set("x-amz-version-id", flip.VersionID)
	}
	rec.WriteHeader(http.StatusOK)
	s.recordEdgeAccess(row, http.StatusOK, row.ContentLength, 0)
	s.logger.Info("artifact stored", zap.String("tenant_id", row.TenantID), zap.String("s3.bucket", row.Bucket), zap.String("s3.object", row.Key),
		zap.String("backend", row.Backend), zap.String("etag", etag), zap.String("version_id", flip.VersionID),
		zap.Int64("size", row.ContentLength), zap.String("host", edgeHost))
	NewNotificationDispatcher(s.db, s.logger).Fire(row.TenantID, row.Bucket, "s3:ObjectCreated:Put", row.Key, row.ContentLength, etag)
	emitEvent(cctx, s.db, s.logger, "object.created", row.TenantID, map[string]interface{}{
		"bucket": row.Bucket, "key": row.Key, "size": row.ContentLength, "etag": etag,
	})
	return "committed", row.ContentLength
}

// commitEdgePart records the part the backend holds and settles its
// reservation (settleEdgePart), exactly as UploadPart installs a staged one.
func (s *Server) commitEdgePart(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder, md5Hex, reqID string) (string, int64) {
	cctx := context.WithoutCancel(ctx)
	path := "/" + row.Bucket + "/" + row.Key
	etag := `"` + md5Hex + `"`
	backendETag := strings.TrimSpace(req.BackendETag)
	sErr := settleEdgePart(cctx, s.db, row.TenantID, row.UploadID, row.PartNumber, row.ReservedBytes, row.ContentLength, etag, backendETag)
	switch {
	case sErr == nil:
	case errors.Is(sErr, errUploadEnded):
		// Ended meanwhile: its end released this part's reservation; the
		// backend upload is gone or going with it.
		WriteS3Error(rec, ErrNoSuchUpload, path, reqID)
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	case errors.Is(sErr, errUploadStaged):
		// Not an edge upload any more (cannot happen: edge is set once);
		// settleUploadPartWith gave the reservation back.
		WriteS3ErrorWithContext(rec, ErrInvalidRequest, path, reqID, WithSuggestion(edgeUploadRefusal))
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	case errors.Is(sErr, errPartOverQuota):
		WriteS3ErrorWithContext(rec, ErrQuotaExceeded, path, reqID, WithSuggestion("Parts in progress count against your storage. Upgrade at "+billingURL()+"."))
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	default:
		s.releaseEdgeAdmit(ctx, row)
		s.logger.Error("edge commit: failed to record part", zap.Error(sErr), zap.String("upload_id", row.UploadID))
		WriteS3Error(rec, ErrInternalError, path, reqID)
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}
	rec.Header().Set("ETag", etag)
	rec.Header().Set("x-amz-request-id", reqID)
	rec.WriteHeader(http.StatusOK)
	s.recordEdgeAccess(row, http.StatusOK, row.ContentLength, 0)
	return "committed", row.ContentLength
}

// recordEdgeAccess writes the access-log row the origin would have written
// for the client's request: the op, the client's IP and user agent (from
// the admit's description, RP64-11), the bytes in (a write) or out (a
// GET), the status.
func (s *Server) recordEdgeAccess(row edgeAdmitRow, status int, bytesIn, bytesOut int64) {
	if s.accessLogTracker == nil {
		return
	}
	op := auth.OpPutObject
	switch row.Op {
	case edgeOpPart:
		op = auth.OpUploadPart
	case edgeOpGet:
		op = auth.OpGetObject
	}
	if status == 0 {
		status = http.StatusOK
	}
	ua := row.Attrs.UserAgent
	if ua == "" {
		ua = "edge"
	}
	s.accessLogTracker.Record(context.Background(), s3AccessEvent{
		tenantID: row.TenantID, bucket: row.Bucket, objectKey: row.Key, operation: op, statusCode: status,
		bytesSent: bytesOut, bytesReceived: bytesIn, sourceIP: row.ClientIP, userAgent: ua, requestID: row.RequestID, loggedAt: time.Now(),
	})
}
