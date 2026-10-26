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
	// edgeOpPut / edgeOpPart are the admit's op values.
	edgeOpPut  = "put"
	edgeOpPart = "part"
	// edgeReasonSmall is the pass-through of a body under EDGE_MIN_BYTES.
	edgeReasonSmall = "small"
)

// The data-plane metrics (plan 40.10: every byte by host; Part B adds
// reads and the nodes their own host).
var (
	dataPlaneAdmits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_data_plane_admits_total",
		Help: "Edge admit decisions by host, op (put, part) and outcome: admitted, pass_through (the origin takes the request), refused (an S3 error relayed to the client), error (the control plane could not decide).",
	}, []string{"host", "op", "outcome"})
	dataPlaneBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_data_plane_bytes_total",
		Help: "Object bytes a data-plane host moved and the control plane committed, by host and op.",
	}, []string{"host", "op"})
)

func init() {
	for _, op := range []string{edgeOpPut, edgeOpPart} {
		for _, outcome := range []string{"admitted", "pass_through", "pass_through_small", "refused", "error"} {
			dataPlaneAdmits.WithLabelValues(edgeHost, op, outcome)
		}
		dataPlaneBytes.WithLabelValues(edgeHost, op)
	}
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
}

// edgeCommitRequest is what the Worker reports: the backend's answer. No
// digest and no byte count — the Worker is not in the byte path.
type edgeCommitRequest struct {
	AdmitID       string `json:"admit_id"`
	Outcome       string `json:"outcome"` // ok | backend_error | edge_error
	BackendStatus int    `json:"backend_status"`
	BackendCode   string `json:"backend_code"` // the backend's XML <Code>, when it answered an error; the subrequest's error otherwise
	BackendETag   string `json:"backend_etag"`
	DurationMS    int64  `json:"duration_ms"`
}

// edgeAdmitAttrs is what the row keeps of the client's request for the
// commit: the object's attributes and the Object Lock headers.
type edgeAdmitAttrs struct {
	Attrs     objectAttrs `json:"attrs"`
	LockMode  string      `json:"lock_mode,omitempty"`
	LockUntil string      `json:"lock_until,omitempty"`
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
}

// registerEdgeRoutes mounts the two calls when the token is loaded.
func (s *Server) registerEdgeRoutes() {
	if len(s.edge.token) == 0 {
		return
	}
	s.router.Post("/api/v1/edge/admit", s.handleEdgeAdmit)
	s.router.Post("/api/v1/edge/commit", s.handleEdgeCommit)
	s.logger.Info("edge ingest routes mounted: /api/v1/edge/admit, /api/v1/edge/commit")
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
	if req.Op != edgeOpPut && req.Op != edgeOpPart {
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
	case resp.Edge:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "admitted").Inc()
	case resp.Reason == edgeReasonSmall:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "pass_through_small").Inc()
	default:
		dataPlaneAdmits.WithLabelValues(edgeHost, req.Op, "pass_through").Inc()
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

// edgeAdmit is the decision. status is the HTTP status of the admit call
// itself (200 for every decision; 5xx when the control plane failed).
func (s *Server) edgeAdmit(ctx context.Context, req edgeAdmitRequest) (edgeAdmitResponse, int) {
	d, sr, err := edgeDescriptor(req)
	if err != nil {
		return passThrough("bad_descriptor")
	}
	if req.Method != http.MethodPut {
		return passThrough("not_put")
	}
	sr = sr.WithContext(ctx)
	rec := newRelayRecorder()

	// Authentication: the one verifier's edge entrance, the origin's error
	// mapping (handleS3Request) relayed as the client's XML.
	tenantID, scope, err := s.s3Auth().ValidateDescribed(ctx, d, req.ClientIP)
	if err != nil {
		if d.Headers.Get("Authorization") == "" {
			return passThrough("unsigned") // the origin answers it (AccessDenied or a web page)
		}
		s.logger.Info("edge admit: authentication failed", zap.Error(err), zap.String("path", req.Path))
		reqID := generateRequestID()
		if errors.Is(err, auth.ErrIPDenied) {
			WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, reqID, WithSuggestion("This key is restricted by IP address."))
			recordAuthFailure(sr, "ip_denied", true)
			return refusalOf(rec)
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
		return refusalOf(rec)
	}
	if tenantID == "" || engine.IsReservedTenantID(tenantID) {
		WriteS3Error(rec, ErrAccessDenied, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	if scope != nil && auth.IsKeyExpired(scope.ExpiresAt) {
		WriteS3ErrorWithContext(rec, ErrExpiredPresignedRequest, sr.URL.Path, generateRequestID(),
			WithSuggestion("This access key expired at "+scope.ExpiresAt.UTC().Format(time.RFC3339)+". Create or rotate a key in the dashboard."))
		recordAuthFailure(sr, "expired", true)
		return refusalOf(rec)
	}
	if isTenantSuspended(ctx, s.db, tenantID) {
		WriteS3Error(rec, ErrAccountSuspended, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}

	// The names, as the parser reads them (decoded path: bucket, key).
	path := strings.TrimPrefix(sr.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket == "" || key == "" {
		return passThrough("no_key")
	}
	op := auth.OpPutObject
	if req.Op == edgeOpPart {
		op = auth.OpUploadPart
	}
	if scope != nil {
		if !auth.OperationAllowed(scope.Permissions, op) {
			WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key does not have %s access.", op)))
			return refusalOf(rec)
		}
		if !auth.CheckBucketScope(scope.BucketScope, bucket) {
			WriteS3ErrorWithContext(rec, ErrAccessDenied, sr.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key is restricted to buckets: %s", strings.Join(scope.BucketScope, ", "))))
			return refusalOf(rec)
		}
	}
	if tenant.IsSystemBucket(bucket) {
		WriteS3Error(rec, ErrNoSuchBucket, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	if !validateBucketName(bucket) {
		WriteS3Error(rec, ErrInvalidBucketName, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	if len(key) > maxS3KeyBytes {
		WriteS3Error(rec, ErrKeyTooLong, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	// What the origin's own handlers would read of this request.
	tn := &tenant.Tenant{ID: tenantID, Namespace: fmt.Sprintf("tenant/%s/", tenantID), Plan: "starter", Status: "active"}
	sctx := tenant.WithTenant(common.WithTenantID(auth.WithKeyScope(ctx, scope), tenantID), tn)
	sr = sr.WithContext(sctx)

	// The shapes the edge never takes: a copy, a sub-resource, a customer
	// key (sealed at the origin), a trailer the Worker cannot verify.
	if sr.Header.Get("x-amz-copy-source") != "" {
		return passThrough("copy")
	}
	if crypto.HasSSECHeaders(sr) {
		return passThrough("ssec")
	}
	// An aws-chunked body (aws-cli v2's CRC32 trailer over TLS) is the
	// origin's: decoding it would put JavaScript in the byte path
	// (bench-results/EDGE-RELAY-2026-10-26.md); the Worker classifies it
	// so, and the control plane says the same for a Worker that did not.
	if isAWSChunked(sr) {
		return passThrough("aws_chunked")
	}
	size := d.ContentLength
	if size < 0 {
		WriteS3Error(rec, ErrMissingContentLength, sr.URL.Path, generateRequestID())
		return refusalOf(rec)
	}
	if size == 0 {
		return passThrough("empty") // an empty object costs nothing to move
	}
	// A small body is the origin's (EDGE_MIN_BYTES): two round trips fewer,
	// and its bytes do not matter for SLC's bandwidth.
	if s.edge.minBytes > 0 && size < s.edge.minBytes {
		return passThrough(edgeReasonSmall)
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
	if req.Op == edgeOpPut {
		storageClass = resolvePutStorageClass(sctx, s.db, s.engine, tierPlacementGate(s.flags), tenantID, bucket, sr.Header.Get("x-amz-storage-class"))
	} else {
		_, requestedClass, _, aErr := s.multipartUploadAttrs(sr, sr.URL.Query().Get("uploadId"))
		if aErr != nil && !errors.Is(aErr, sql.ErrNoRows) {
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
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
		SSEC: false, SSERequested: sr.Header.Get("x-amz-server-side-encryption") == "AES256",
	})
	if placement.Refusal != nil {
		placement.Refusal.write(rec, sr)
		return refusalOf(rec)
	}
	if placement.Kind != placeWhole {
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
			LockUntil: sr.Header.Get("x-amz-object-lock-retain-until-date")},
		StorageClass: storageClass, Floor: floor, ClientIP: req.ClientIP, RequestID: req.RequestID}
	container := tenantID + "_" + bucket
	var presigned string
	var signed []string
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
		presigned, signed, presignErr = ew.PresignPut(sctx, container, row.StorageKey,
			engine.EdgePresign{ContentLength: size, ContentMD5: contentMD5, ContentType: attrs.ContentType, TTL: edgeAdmitTTL})
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
		uploadAttrs, _, _, aErr := s.multipartUploadAttrs(sr, uploadID)
		if aErr != nil {
			return edgeAdmitResponse{}, http.StatusInternalServerError
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
		presigned, signed, presignErr = ew.PresignUploadPart(sctx, container, row.StorageKey, res.Upload.BackendUploadID, partNumber,
			engine.EdgePresign{ContentLength: size, ContentMD5: contentMD5, TTL: edgeAdmitTTL})
	}
	if presignErr != nil {
		s.logger.Error("edge admit: presign failed", zap.Error(presignErr), zap.String("backend", backend))
		return edgeAdmitResponse{}, http.StatusInternalServerError
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

	row.ID, err = newEdgeAdmitID()
	if err != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	row.ExpiresAt = time.Now().UTC().Add(edgeAdmitExpiry)
	if err := insertEdgeAdmit(sctx, s.db, row); err != nil {
		s.logger.Error("edge admit: row not written", zap.Error(err))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	settled = true
	s.logger.Info("edge admit", zap.String("op", req.Op), zap.String("tenant_id", tenantID), zap.String("bucket", bucket),
		zap.String("key", key), zap.String("backend", backend), zap.Int64("size", size), zap.String("admit_id", row.ID),
		zap.String("request_id", req.RequestID))
	return edgeAdmitResponse{Edge: true, AdmitID: row.ID, URL: presigned, Method: http.MethodPut, SendHeaders: sendHeaders,
		Backend: backend, StorageKey: row.StorageKey, Expect: &edgeExpect{MD5: contentMD5 != ""},
		ExpiresAt: &row.ExpiresAt}, http.StatusOK
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
			 floor_reserved, credited, client_ip, request_id, state, expires_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, 0), $8, $9, $10, NULLIF($11, ''), $12, $13, $14, $15,
		        $16, NULLIF($17, ''), $18, $19, $20, $21, 'pending', $22)`,
		row.ID, row.TenantID, row.Bucket, row.Key, row.Op, row.UploadID, row.PartNumber, row.Backend, row.StorageKey, row.ContentLength,
		row.ContentMD5, row.ContentType, attrs, row.StorageClass, row.Floor, row.ReservedBytes, row.ReservedFloor,
		row.FloorReserved, row.Credited, row.ClientIP, row.RequestID, row.ExpiresAt)
	return err
}

// --- commit ---

// edgeCommitOutcome is the Worker's report of how the backend write ended.
const (
	edgeOutcomeOK           = "ok"
	edgeOutcomeBackendError = "backend_error"
	edgeOutcomeEdgeError    = "edge_error"
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
	relay, status, err := s.edgeCommit(r.Context(), req)
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
	state, bytes := s.settleEdgeAdmit(ctx, row, req, rec)
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
		       floor_reserved, credited, client_ip, request_id, state, result, expires_at
		FROM edge_admits WHERE id = $1 FOR UPDATE`, id).Scan(
		&row.ID, &row.TenantID, &row.Bucket, &row.Key, &row.Op, &uploadID, &partNumber, &row.Backend, &row.StorageKey, &row.ContentLength,
		&md5, &row.ContentType, &attrs, &row.StorageClass, &row.Floor, &row.ReservedBytes, &reservedFloor,
		&row.FloorReserved, &row.Credited, &row.ClientIP, &row.RequestID, &row.State, &result, &row.ExpiresAt)
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
// inside the caller's transaction on the locked row: the reservation back,
// a PUT's address queued for the collector (the body may have landed with
// no commit), state expired.
func (s *Server) expireEdgeAdmit(ctx context.Context, tx *sql.Tx, row edgeAdmitRow) {
	s.releaseEdgeAdmit(ctx, row)
	if row.Op == edgeOpPut {
		retireOrphanWrite(ctx, s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
	}
	_, _ = tx.ExecContext(ctx, `UPDATE edge_admits SET state = 'expired', finished_at = NOW() WHERE id = $1`, row.ID)
}

// edgeDeleteBlob removes a blob the commit refused (a digest mismatch) on
// its backend, best effort, and queues it for the collector as well.
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
// writing the client's answer to rec; returns the row's new state and the
// bytes committed. The Worker is not in the byte path (it reports the
// backend's answer only), so the integrity the control plane relies on is
// the backend's: the signed content-length and Content-MD5 it enforced, and
// the ETag it answered — the body's MD5 on both backends (Step 0.2), the
// row's ETag.
func (s *Server) settleEdgeAdmit(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder) (string, int64) {
	path := "/" + row.Bucket + "/" + row.Key
	reqID := row.RequestID
	if reqID == "" {
		reqID = generateRequestID()
	}
	fail := func(code string, opts ...ErrorOption) (string, int64) {
		s.releaseEdgeAdmit(ctx, row)
		if row.Op == edgeOpPut {
			retireOrphanWrite(context.WithoutCancel(ctx), s.db, s.log(), row.TenantID, row.Bucket, row.StorageKey, row.Backend)
		}
		WriteS3ErrorWithContext(rec, code, path, reqID, opts...)
		s.recordEdgeAccess(row, rec.status, 0)
		return "failed", 0
	}
	if req.Outcome != edgeOutcomeOK {
		switch {
		case req.Outcome == edgeOutcomeBackendError && req.BackendStatus == http.StatusBadRequest &&
			(req.BackendCode == "BadDigest" || req.BackendCode == "XAmzContentChecksumMismatch" || req.BackendCode == "InvalidDigest"):
			// The client's own Content-MD5 did not match its body: the
			// backend refused the write (nothing stored on either backend).
			return fail(ErrBadDigest)
		case req.Outcome == edgeOutcomeBackendError && (req.BackendCode == "IncompleteBody" || req.BackendStatus == http.StatusRequestTimeout ||
			req.BackendStatus == http.StatusLengthRequired):
			// The body ended short of the signed content-length.
			return fail(ErrIncompleteBody)
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
		s.edgeDeleteBlob(ctx, row)
		return fail(ErrInternalError, WithSuggestion("The backend answered no MD5 ETag for this write; nothing was stored. Retry."))
	}
	if row.ContentMD5 != "" {
		// The link signed it and the backend enforced it: the ETag must be
		// that digest (a backend that let other bytes through is refused).
		want, _ := base64.StdEncoding.DecodeString(row.ContentMD5)
		if hex.EncodeToString(want) != etag {
			s.edgeDeleteBlob(ctx, row)
			return fail(ErrBadDigest)
		}
	}
	switch row.Op {
	case edgeOpPut:
		return s.commitEdgePut(ctx, row, rec, etag, reqID)
	default:
		return s.commitEdgePart(ctx, row, req, rec, etag, reqID)
	}
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
		s.recordEdgeAccess(row, rec.status, 0)
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
	s.recordEdgeAccess(row, http.StatusOK, row.ContentLength)
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
		s.recordEdgeAccess(row, rec.status, 0)
		return "failed", 0
	case errors.Is(sErr, errUploadStaged):
		// Not an edge upload any more (cannot happen: edge is set once);
		// settleUploadPartWith gave the reservation back.
		WriteS3ErrorWithContext(rec, ErrInvalidRequest, path, reqID, WithSuggestion(edgeUploadRefusal))
		s.recordEdgeAccess(row, rec.status, 0)
		return "failed", 0
	case errors.Is(sErr, errPartOverQuota):
		WriteS3ErrorWithContext(rec, ErrQuotaExceeded, path, reqID, WithSuggestion("Parts in progress count against your storage. Upgrade at "+billingURL()+"."))
		s.recordEdgeAccess(row, rec.status, 0)
		return "failed", 0
	default:
		s.releaseEdgeAdmit(ctx, row)
		s.logger.Error("edge commit: failed to record part", zap.Error(sErr), zap.String("upload_id", row.UploadID))
		WriteS3Error(rec, ErrInternalError, path, reqID)
		s.recordEdgeAccess(row, rec.status, 0)
		return "failed", 0
	}
	rec.Header().Set("ETag", etag)
	rec.Header().Set("x-amz-request-id", reqID)
	rec.WriteHeader(http.StatusOK)
	s.recordEdgeAccess(row, http.StatusOK, row.ContentLength)
	return "committed", row.ContentLength
}

// recordEdgeAccess writes the access-log row the origin would have written
// for the client's request: the op, the client's IP, the bytes, the status.
func (s *Server) recordEdgeAccess(row edgeAdmitRow, status int, bytes int64) {
	if s.accessLogTracker == nil {
		return
	}
	op := auth.OpPutObject
	if row.Op == edgeOpPart {
		op = auth.OpUploadPart
	}
	if status == 0 {
		status = http.StatusOK
	}
	s.accessLogTracker.Record(context.Background(), s3AccessEvent{
		tenantID: row.TenantID, bucket: row.Bucket, objectKey: row.Key, operation: op, statusCode: status,
		bytesReceived: bytes, sourceIP: row.ClientIP, userAgent: "edge", requestID: row.RequestID, loggedAt: time.Now(),
	})
}
