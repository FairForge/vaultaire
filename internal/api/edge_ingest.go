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
	"github.com/FairForge/vaultaire/internal/dataplane"
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
	// edgeReasonSandbox: a test-mode (sandbox) tenant's request is the
	// origin's — its bytes go to `local` only (WP-DX-1, sandbox.go).
	edgeReasonSandbox = "sandbox"
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

// The wire types of the edge calls live in internal/dataplane (WP-EDGE-3:
// the Worker, a data node and the origin's own data path speak them);
// these aliases keep the handlers' names.
type (
	// edgeAdmitRequest is what the host describes.
	edgeAdmitRequest = dataplane.AdmitRequest
	// edgeExpect tells the host what the backend will enforce.
	edgeExpect = dataplane.Expect
	// edgeAdmitResponse is the decision.
	edgeAdmitResponse = dataplane.AdmitResponse
	// edgeWindow is a windowed read's shape (D-EDGE-32).
	edgeWindow = dataplane.Window
	// edgeCodecGrant is a codec admit's public key and numbers.
	edgeCodecGrant = dataplane.CodecGrant
	// edgeCommitRequest is what the host reports.
	edgeCommitRequest = dataplane.CommitRequest
	// edgeCommitChunk is one chunk of a codec commit.
	edgeCommitChunk = dataplane.CommitChunk
)

// edgeFeatureWindow: the host reads a windowed answer (D-EDGE-32).
const edgeFeatureWindow = dataplane.FeatureWindow

// edgeAdmitAttrs is what the row keeps of the client's request for the
// commit: the object's attributes and the Object Lock headers.
type edgeAdmitAttrs struct {
	Attrs     objectAttrs `json:"attrs"`
	LockMode  string      `json:"lock_mode,omitempty"`
	LockUntil string      `json:"lock_until,omitempty"`
	// UserAgent is the client's (RP64-11): the access-log row carries it.
	UserAgent string `json:"user_agent,omitempty"`
	// KeyID is the access key id that signed the request (never its
	// secret): the request log's key column (WP-LOG-1).
	KeyID string `json:"key_id,omitempty"`
	// ActingKey is the api_keys.id a write's admit counted to (WP-KEY-1:
	// none for the primary pair): the write's writer and holder.
	ActingKey string `json:"acting_key,omitempty"`
	// EgressKey / EgressBucket: the capped key and whether the bucket is
	// capped, found by a read's admit — its bytes are counted to them at
	// the commit (WP-KEY-1, F-39 (a)).
	EgressKey    string `json:"egress_key,omitempty"`
	EgressBucket bool   `json:"egress_bucket,omitempty"`
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
	// CheckLength: an aws-chunked write to a backend that does not enforce
	// x-amz-decoded-content-length (RP72-2, Lyve) — the PUT's commit HEADs
	// the blob, and the upload's Complete the assembled object, against the
	// admitted size.
	CheckLength bool `json:"check_length,omitempty"`
	// MirrorLeg (WP-EDGE-4): the leg this write is mirrored to, decided
	// once at the admit — the index's second links and the commit's copy
	// rows name it. "" = not mirrored.
	MirrorLeg string `json:"mirror_leg,omitempty"`
	// CodecPublic (WP-EDGE-4 PR D): the plain hashes this admit's index
	// answered as hits on the public catalogue's `_public` copy — the only
	// "hit" the commit may reference there.
	CodecPublic map[string]bool `json:"codec_public,omitempty"`
	// RS (WP-EDGE-4 PR B, edge_rs): the stripe geometry and each shard's
	// leg of an RS write — the commit records the shard rows from it.
	RS *edgeRSAttrs `json:"rs,omitempty"`
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
	// Host is the data-plane host the admit was made for (WP-EDGE-3,
	// migration 112): edge (the Worker), a node's registry name, slc (the
	// origin in-process); "" on a row written before = edge.
	Host string
}

// admitHost is the host an admit is made for: the caller on ctx (the gate
// or the in-process control plane), else the Worker.
func admitHost(ctx context.Context) string {
	if h, ok := dataplane.HostFrom(ctx); ok && h.Name != "" {
		return h.Name
	}
	return edgeHost
}

// registerEdgeRoutes mounts the two calls when the token is loaded.
func (s *Server) registerEdgeRoutes() {
	if len(s.edge.token) == 0 {
		return
	}
	s.router.Post("/api/v1/edge/admit", s.handleEdgeAdmit)
	s.router.Post("/api/v1/edge/commit", s.handleEdgeCommit)
	s.router.Post("/api/v1/edge/index", s.handleEdgeIndex)
	s.router.Post("/api/v1/edge/heartbeat", s.handleEdgeHeartbeat)
	s.router.Post("/api/v1/edge/mirror", s.handleEdgeMirror)
	s.logger.Info("edge ingest routes mounted: /api/v1/edge/admit, /api/v1/edge/commit, /api/v1/edge/index, /api/v1/edge/heartbeat, /api/v1/edge/mirror")
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
	host, health, ok := s.edgeCaller(r)
	if !ok {
		s.edgeNotFound(w, r)
		return
	}
	if health != "up" {
		// A host the registry marks down or draining takes no new request
		// (its commits are still accepted: the bytes may have landed).
		w.Header().Set("Retry-After", "30")
		writeEdgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "host_" + health})
		return
	}
	r = r.WithContext(dataplane.WithHost(r.Context(), host))
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
	countEdgeAdmit(host.Name, req.Op, resp, status)
	if status == http.StatusOK && resp.Refusal == nil && !resp.Edge {
		// Why the origin took it — the one trace of a pass-through (Debug:
		// one line per declined request; the counter is the signal).
		s.logger.Debug("edge admit: pass-through", zap.String("op", req.Op), zap.String("reason", resp.Reason),
			zap.String("path", req.Path), zap.String("request_id", req.RequestID))
	}
	writeEdgeJSON(w, status, resp)
}

// countEdgeAdmit counts one admit decision for the host that asked.
func countEdgeAdmit(host, op string, resp edgeAdmitResponse, status int) {
	switch {
	case status != http.StatusOK:
		dataPlaneAdmits.WithLabelValues(host, op, "error").Inc()
	case resp.Refusal != nil:
		dataPlaneAdmits.WithLabelValues(host, op, "refused").Inc()
	case resp.Edge && resp.Unverified:
		dataPlaneAdmits.WithLabelValues(host, op, "admitted_unverified").Inc()
	case resp.Edge:
		dataPlaneAdmits.WithLabelValues(host, op, "admitted").Inc()
	case resp.Reason == edgeReasonSmall:
		dataPlaneAdmits.WithLabelValues(host, op, "pass_through_small").Inc()
	default:
		dataPlaneAdmits.WithLabelValues(host, op, "pass_through").Inc()
	}
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
	// A tenant-scoped (BYO) data-plane host admits ONE tenant (contract
	// line 13: the tenant is the unit of disclosure — a customer's own node
	// never sees another's request admitted). WP-EDGE-3.
	if h, ok := dataplane.HostFrom(ctx); ok && h.TenantScoped() && h.TenantID != tenantID {
		WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, generateRequestID(),
			WithSuggestion("This endpoint serves one account only: use your own endpoint or s3.stored.cloud."))
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
	if isSandbox(ctx, s.db, tenantID) {
		return decide(passThrough(edgeReasonSandbox)) // WP-DX-1: never a backend link
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
	if req.Op == edgeOpPut && placement.Kind != placeChunked && s.edgeCodecFor(tenantID) {
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
			LockUntil: sr.Header.Get("x-amz-object-lock-retain-until-date"), UserAgent: sr.Header.Get("User-Agent"), KeyID: attemptedAccessKey(sr),
			ActingKey: writerKeyOf(sctx)},
		StorageClass: storageClass, Floor: floor, ClientIP: req.ClientIP, RequestID: req.RequestID}
	row.Attrs.CheckLength = stream != nil && !streamProps.EnforcesDecodedLength
	container := tenantID + "_" + bucket
	var presigned string
	var signed []string
	var streamHeaders map[string]string
	var presignErr error
	settled := false
	switch req.Op {
	case edgeOpPut:
		if s.quotaManager != nil {
			bill, ok, capName, qErr := s.reserveWrite(sctx, tenantID, bucket, key, floor, size)
			if qErr != nil {
				s.logger.Error("edge admit: quota check failed", zap.Error(qErr), zap.String("tenant_id", tenantID))
				return edgeAdmitResponse{}, http.StatusInternalServerError
			}
			if !ok {
				writeQuotaRefusal(rec, sr.URL.Path, capName)
				return refusalOf(rec)
			}
			row.ReservedBytes, row.ReservedFloor, row.FloorReserved, row.Credited = bill.Reserved, bill.Floor, bill.FloorReserved, bill.Credited
			defer func() {
				if !settled {
					s.releaseQuota(context.WithoutCancel(sctx), tenantID, row.ReservedFloor, row.ReservedBytes, row.holder())
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
			setStoredCap(rec, refusedCap(rErr))
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
		if req.Op == edgeOpPut {
			// An aws-chunked PUT is signed for one backend: its mirror is the
			// blob_copies job's (pending at the commit).
			row.Attrs.MirrorLeg = s.mirrorFor(sctx, tenantID, bucket, backend)
		}
		return s.recordEdgeWriteAdmit(sctx, req, &row, &settled, edgeAdmitResponse{Edge: true, URL: presigned, Method: http.MethodPut,
			Headers: streamHeaders, StreamingTrailer: stream.Trailer, ContentLength: size, Backend: backend,
			Expect: &edgeExpect{MD5: contentMD5 != ""}, Unverified: !streamProps.VerifiesTrailer && contentMD5 == ""})
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

	resp := edgeAdmitResponse{Edge: true, URL: presigned, Method: http.MethodPut,
		SendHeaders: sendHeaders, Backend: backend, Expect: &edgeExpect{MD5: contentMD5 != ""}}
	if req.Op == edgeOpPut {
		// WP-EDGE-4 PR B: RS k+m for a host that stripes (edge_rs, dark).
		if grant, rsAttrs := s.edgeRSGrantFor(sctx, req, tenantID, bucket, backend, container, row.StorageKey, size); grant != nil {
			row.Attrs.RS = rsAttrs
			return s.recordEdgeWriteAdmit(sctx, req, &row, &settled, edgeAdmitResponse{Edge: true, Method: http.MethodPut, Backend: backend, RS: grant})
		}
	}
	if req.Op == edgeOpPut {
		// WP-EDGE-4: a mirrored PUT gets the leg's link beside the
		// placement's, signing the same headers (the host sends one set);
		// a leg that cannot sign it is the blob_copies job's copy.
		if leg := s.mirrorFor(sctx, tenantID, bucket, backend); leg != "" {
			row.Attrs.MirrorLeg = leg
			if murl, msigned, mErr := s.presignMirrorPut(sctx, leg, container, row.StorageKey,
				engine.EdgePresign{ContentLength: size, ContentMD5: contentMD5, ContentType: attrs.ContentType, TTL: edgeAdmitTTL}); mErr == nil && sameHeaderSet(signed, msigned) {
				resp.URLs = []string{presigned, murl}
			} else if mErr != nil {
				s.logger.Warn("edge admit: the mirror leg could not sign — the copy is the blob_copies job's", zap.Error(mErr), zap.String("leg", leg))
			}
		}
	}
	return s.recordEdgeWriteAdmit(sctx, req, &row, &settled, resp)
}

// presignMirrorPut signs a PUT of the same address on the mirror leg.
func (s *Server) presignMirrorPut(ctx context.Context, leg, container, storageKey string, p engine.EdgePresign) (string, []string, error) {
	ew, err := s.edgeWriterFor(leg)
	if err != nil {
		return "", nil, err
	}
	return ew.PresignPut(ctx, container, storageKey, p)
}

// sameHeaderSet: two links sign the same headers.
func sameHeaderSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, h := range a {
		set[strings.ToLower(h)] = true
	}
	for _, h := range b {
		if !set[strings.ToLower(h)] {
			return false
		}
	}
	return true
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
	if row.Host == "" {
		row.Host = admitHost(ctx)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO edge_admits
			(id, tenant_id, bucket, object_key, op, upload_id, part_number, backend, storage_key, content_length,
			 content_md5, content_type, attrs, storage_class, floor, reserved_bytes, reserved_floor,
			 floor_reserved, credited, client_ip, request_id, state, expires_at, codec, host)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, 0), $8, $9, $10, NULLIF($11, ''), $12, $13, $14, $15,
		        $16, NULLIF($17, ''), $18, $19, $20, $21, 'pending', $22, $23, $24)`,
		row.ID, row.TenantID, row.Bucket, row.Key, row.Op, row.UploadID, row.PartNumber, row.Backend, row.StorageKey, row.ContentLength,
		row.ContentMD5, row.ContentType, attrs, row.StorageClass, row.Floor, row.ReservedBytes, row.ReservedFloor,
		row.FloorReserved, row.Credited, row.ClientIP, row.RequestID, row.ExpiresAt, row.Codec, row.Host)
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
	host, _, ok := s.edgeCaller(r)
	if !ok {
		s.edgeNotFound(w, r)
		return
	}
	r = r.WithContext(dataplane.WithHost(r.Context(), host))
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
	// The admit's key is the write's (and a read's egress cap): the commit
	// request carries no S3 credential of its own (WP-KEY-1).
	ctx = withWriterKey(ctx, row.Attrs.ActingKey)
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
		dataPlaneBytes.WithLabelValues(row.Host, row.Op).Add(float64(bytes))
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
		       floor_reserved, credited, client_ip, request_id, state, result, expires_at, codec, COALESCE(host, '`+edgeHost+`')
		FROM edge_admits WHERE id = $1 FOR UPDATE`, id).Scan(
		&row.ID, &row.TenantID, &row.Bucket, &row.Key, &row.Op, &uploadID, &partNumber, &row.Backend, &row.StorageKey, &row.ContentLength,
		&md5, &row.ContentType, &attrs, &row.StorageClass, &row.Floor, &row.ReservedBytes, &reservedFloor,
		&row.FloorReserved, &row.Credited, &row.ClientIP, &row.RequestID, &row.State, &result, &row.ExpiresAt, &row.Codec, &row.Host)
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

// holder is who besides the tenant the admit charged (WP-KEY-1).
func (row edgeAdmitRow) holder() usage.Holder {
	return usage.Holder{KeyID: row.Attrs.ActingKey, Bucket: row.Bucket}
}

// releaseEdgeAdmit gives an admit's reservation back (a failed or expired
// write). On its own context: the request's may be gone.
func (s *Server) releaseEdgeAdmit(ctx context.Context, row edgeAdmitRow) {
	ctx = context.WithoutCancel(ctx)
	switch row.Op {
	case edgeOpPut:
		s.releaseQuota(ctx, row.TenantID, row.ReservedFloor, row.ReservedBytes, row.holder())
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
		if row.Host != dataplane.HostSLC { // host=slc: the origin metered what it streamed
			s.recordEdgeEgress(ctx, row.TenantID, row.Backend, row.ContentLength)
			s.countEdgeCapEgress(ctx, row, row.ContentLength)
		}
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
			s.queueEdgePutOrphans(ctx, row)
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
	s.queueEdgePutOrphans(ctx, row)
}

// queueEdgePutOrphans queues an uncommitted PUT's address for the collector
// on its backend and — mirrored (WP-EDGE-4) — on the mirror leg, which the
// host may have written too.
func (s *Server) queueEdgePutOrphans(ctx context.Context, row edgeAdmitRow) {
	retireOrphanWrite(ctx, s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
	if row.Attrs.MirrorLeg != "" {
		retireOrphanWrite(ctx, s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Attrs.MirrorLeg)
	}
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
			s.queueEdgePutOrphans(context.WithoutCancel(ctx), row)
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
	if row.Attrs.RS != nil && row.Op == edgeOpPut {
		state, n := s.settleEdgeRS(ctx, row, req, rec, reqID)
		return state, n, nil
	}
	deleteAfter := func() { s.edgeDeleteBlob(ctx, row) }
	if req.Outcome != edgeOutcomeOK {
		switch {
		case req.Outcome == edgeOutcomeChecksumMismatch:
			// The host decoded the aws-chunked body beside the backend and
			// its trailer is not the body's (host=slc, RP74-4): the client's
			// 400, the blob the backend took removed.
			state, n, _ := fail(ErrXAmzContentChecksumMismatch)
			if row.Op == edgeOpPut {
				return state, n, deleteAfter
			}
			return state, n, nil
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
	if row.Op == edgeOpPut && row.Attrs.CheckLength {
		// The backend took the aws-chunked body whatever length it decoded
		// to (RP72-2): the blob's size must be the admitted one — the row,
		// the bill and the reservation say it.
		size, hErr := s.edgeBlobSize(ctx, row)
		switch {
		case hErr != nil:
			rec.Header().Set("Retry-After", "30")
			s.logger.Warn("edge commit: the blob's size could not be checked — 503, removed", zap.Error(hErr),
				zap.String("backend", row.Backend), zap.String("admit_id", row.ID))
			state, n, _ := fail(ErrServiceUnavailable)
			return state, n, deleteAfter
		case size != row.ContentLength:
			s.logger.Warn("edge commit: the backend stored another length than the client signed — IncompleteBody, removed",
				zap.Int64("stored", size), zap.Int64("signed", row.ContentLength), zap.String("backend", row.Backend), zap.String("admit_id", row.ID))
			state, n, _ := fail(ErrIncompleteBody)
			return state, n, deleteAfter
		}
	}
	var state string
	var n int64
	switch row.Op {
	case edgeOpPut:
		state, n = s.commitEdgePut(ctx, row, req, rec, etag, reqID)
	default:
		state, n = s.commitEdgePart(ctx, row, req, rec, etag, reqID)
	}
	return state, n, nil
}

// edgeBlobSize is the size the backend holds at an admitted PUT's address
// (one HEAD — engine.ObjectHeader, which every EdgeWriter has).
func (s *Server) edgeBlobSize(ctx context.Context, row edgeAdmitRow) (int64, error) {
	drv, ok := s.engine.GetDriver(row.Backend)
	if !ok {
		return 0, fmt.Errorf("backend %s is not registered", row.Backend)
	}
	h, ok := drv.(engine.ObjectHeader)
	if !ok {
		return 0, fmt.Errorf("backend %s cannot be asked for an object's size", row.Backend)
	}
	hctx, cancel := context.WithTimeout(common.WithTenantID(context.WithoutCancel(ctx), row.TenantID), edgeBackendCallTimeout)
	defer cancel()
	info, err := h.HeadObject(hctx, row.TenantID+"_"+row.Bucket, row.StorageKey)
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// commitEdgePut is the PUT's commit: commitBlobWrite exactly as HandlePut,
// then the lock, the log, the notification and the event.
func (s *Server) commitEdgePut(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder, etag, reqID string) (string, int64) {
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
			// The backend's ETag the Worker reported is the row's
			// backend_etag (RP72-1): checked above to be the body's MD5.
			if err := upsertWholeObjectHeadRow(cctx, tx, row.TenantID, row.Bucket, row.Key, row.StorageKey, row.ContentLength, etag,
				row.Backend, row.Floor, attrs, etag); err != nil {
				return err
			}
			if row.Attrs.MirrorLeg == "" {
				return nil
			}
			// WP-EDGE-4: the copy on the mirror leg, in the same transaction
			// — ready when the leg answered the body's MD5, else owed.
			mirrorETag := ""
			if len(req.BackendETags) > 1 {
				mirrorETag = req.BackendETags[1]
			}
			state, metag := mirrorCopyState(etag, mirrorETag)
			return recordBlobCopy(cctx, tx, blobCopy{TenantID: row.TenantID, Bucket: row.Bucket, Key: row.Key, StorageKey: row.StorageKey,
				Source: row.Backend, Leg: row.Attrs.MirrorLeg, State: state, ETag: metag, Size: row.ContentLength})
		}})
	if dbErr != nil {
		s.releaseEdgeAdmit(ctx, row)
		s.queueEdgePutOrphans(cctx, row)
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
	if row.Attrs.MirrorLeg != "" {
		switch {
		case req.MirrorCut:
			blobCopiesTotal.WithLabelValues(blobCopyCut).Inc() // RP76-2: the copy is owed
		case req.MirrorUnbudgeted:
			blobCopiesTotal.WithLabelValues(blobCopyUnbudgeted).Inc() // RP78-1: the copy is owed
		}
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
		zap.Int64("size", row.ContentLength), zap.String("host", row.Host))
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
		setStoredCap(rec, refusedCap(sErr))
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
	if s.accessLogTracker == nil || row.Host == dataplane.HostSLC {
		// host=slc: the origin's own dispatch writes the request's row.
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
	host := row.Host // the data-plane host that served it (the request log, WP-LOG-1)
	if host == "" {
		host = "edge"
	}
	s.accessLogTracker.Record(context.Background(), s3AccessEvent{
		tenantID: row.TenantID, bucket: row.Bucket, objectKey: row.Key, operation: op, statusCode: status,
		bytesSent: bytesOut, bytesReceived: bytesIn, sourceIP: row.ClientIP, userAgent: ua, requestID: row.RequestID, loggedAt: time.Now(),
		host: host, keyID: row.Attrs.KeyID,
	})
}
