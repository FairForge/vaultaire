package api

// The read half of the edge mover (WP-EDGE-1 Part B, Stage 1a Prompt 45;
// plan 40.10 slice 1): a GetObject (plain, or one Range) of a tenant with
// the edge_ingest flag whose head row is a WHOLE UNSEALED object on an
// EdgeWriter backend, at least EDGE_MIN_BYTES, is admitted on SLC and
// streamed by the Worker from a presigned ranged GET; everything else
// passes through to the origin, which keeps the failover path (the
// breaker, the fan-out, the parity rebuild, the tape semantics, the egress
// throttle's pacing).
//
// The admit answers the ORIGIN's response headers (object_headers.go's
// set, built as HandleGet builds them): the Worker serves those, never the
// backend's. Nothing is metered at admit.
//
// The commit comes AT THE BACKEND'S HEADERS, not at the body's end — the
// Worker hands the backend's body to the client untouched, and a stream it
// does not touch gives it no event when it finishes
// (bench-results/EDGE-RELAY-2026-10-26.md). `served` meters the backend's
// Content-Length ONCE through the origin's own egress entry point (the
// live month counter + bandwidth_usage_daily, so the throttle, the alert
// ladder and EgressStatus see one number), writes the access-log row with
// the client's IP and user agent, the object.downloaded event and the
// data-plane bytes; `fallback` / `error` meter nothing (the origin serves
// and meters its own) and carry the backend's status into the breaker.
// The trades: a client that aborts mid-body is metered for the whole
// object; a lost commit is metered for the admitted size by the reaper.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
)

// The outcomes the Worker reports for a GET.
const (
	edgeGetServed   = "served"   // the backend answered expect_status: the body is streaming to the client
	edgeGetFallback = "fallback" // the backend answered something else: the origin served the request
	edgeGetError    = "error"    // the subrequest failed before an answer: the origin served the request
)

// edgeGetRow is what the admit reads of the head row.
type edgeGetRow struct {
	contentType, etag, backend, encAlgo, storageKey, floor, bodyMD5 string
	size                                                            int64
	updatedAt                                                       time.Time
	metadata, tags                                                  []byte
	attrs                                                           objectAttrs
	echo                                                            putEchoHeaders
	isChunked                                                       bool
	lockMode                                                        string
	lockUntil                                                       sql.NullTime
	legalHold                                                       sql.NullBool
}

// edgeAdmitGet is the GET decision after the prelude. Every shape the edge
// does not take is a pass-through to the origin (`{edge:false, reason}`);
// the admit refuses nothing of its own — the origin's refusals (a missing
// key, a 416, a delete marker, a sealed object without its key) are the
// origin's to write.
func (s *Server) edgeAdmitGet(req edgeAdmitRequest, p edgePrelude) (edgeAdmitResponse, int) {
	sr, sctx, tenantID, bucket, key := p.sr, p.sctx, p.tenantID, p.bucket, p.key

	// Only a plain GetObject: ?versionId, response-* overrides and every
	// sub-resource are the origin's (aws-sdk-go-v2's x-id names the
	// operation, D-EDGE-6).
	for name := range sr.URL.Query() {
		if name != "x-id" {
			return passThrough("query")
		}
	}
	// A conditional GET: no S3 client on the API path sends one; the
	// origin evaluates it against the row (a residual, not a relay path).
	for _, h := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if sr.Header.Get(h) != "" {
			return passThrough("conditional")
		}
	}
	if crypto.HasSSECHeaders(sr) {
		return passThrough("ssec")
	}
	if s.flags == nil || !s.flags.Enabled(flagEdgeIngest, tenantID) {
		return passThrough("flag_off")
	}

	// The bucket: a public one is the origin's (its ACL, its CDN path).
	var visibility, vStatus string
	err := s.db.QueryRowContext(sctx, `SELECT visibility, COALESCE(versioning_status, '') FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&visibility, &vStatus)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return passThrough("no_bucket")
	case err != nil:
		s.logger.Error("edge admit get: bucket read failed", zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	if visibility == "public-read" {
		return passThrough("public")
	}

	// The head row — the same read HandleGet makes (the lock row joined).
	var row edgeGetRow
	err = s.db.QueryRowContext(sctx, `
		SELECT h.content_type, h.size_bytes, h.etag, h.updated_at, COALESCE(h.metadata, '{}'), COALESCE(h.backend_name, ''), COALESCE(h.encryption_algorithm, ''), COALESCE(h.tags, '{}'), COALESCE(h.content_disposition, ''), COALESCE(h.content_encoding, ''), COALESCE(h.content_language, ''), COALESCE(h.cache_control, ''), COALESCE(h.http_expires, ''), COALESCE(h.website_redirect_location, ''), h.is_chunked, h.floor, COALESCE(h.storage_key, ''),
		       COALESCE(h.body_md5, ''),
		       `+objectLockColumns+`
		FROM object_head_cache h
		LEFT JOIN object_locks l ON l.tenant_id = h.tenant_id AND l.bucket = h.bucket AND l.object_key = h.object_key
		WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3`,
		tenantID, bucket, key).Scan(&row.contentType, &row.size, &row.etag, &row.updatedAt, &row.metadata, &row.backend, &row.encAlgo, &row.tags,
		&row.attrs.ContentDisposition, &row.attrs.ContentEncoding, &row.attrs.ContentLanguage, &row.echo.CacheControl, &row.echo.Expires, &row.echo.WebsiteRedirect,
		&row.isChunked, &row.floor, &row.storageKey, &row.bodyMD5, &row.lockMode, &row.lockUntil, &row.legalHold)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return passThrough("no_row") // the origin's NoSuchKey, with its suggestion
	case err != nil:
		s.logger.Error("edge admit get: head row read failed", zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", key))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	// The kinds the edge never reads: a chunked object (the content index),
	// a sealed one (a key on SLC), a vault-floor one (tape, its restore
	// semantics, the parity rebuild), a strict target (sia's promotion,
	// blomp, sync), a Smart-demoted one (its promotion), an empty one.
	switch {
	case row.isChunked:
		return passThrough("chunked")
	case row.encAlgo != "":
		return passThrough("sealed")
	case row.floor == usage.FloorVault:
		return passThrough("vault")
	case row.backend == "" || engine.IsStrictTarget(row.backend):
		return passThrough("backend:" + row.backend)
	case s.smartPromoter != nil && row.backend == s.smartPromoter.ColdBackend:
		return passThrough("demoted")
	case row.size <= 0:
		return passThrough("empty")
	}
	ew, err := s.edgeWriterFor(row.backend)
	if err != nil {
		return passThrough("backend:" + row.backend) // not an edge writer, or its breaker is open: the origin's failover path
	}

	// One Range at most, satisfiable: a multi-range is served whole by the
	// origin (RFC 9110 §14.2) and an unsatisfiable one is its 416.
	var rng *httpRange
	if rh := sr.Header.Get("Range"); rh != "" {
		var perr error
		if rng, perr = parseRangeHeader(rh, row.size); perr != nil {
			if errors.Is(perr, errMultiRange) {
				return passThrough("multi_range")
			}
			return passThrough("range")
		}
	}
	length := row.size
	if rng != nil {
		length = rng.length
	}
	if s.edge.minBytes > 0 && length < s.edge.minBytes {
		return passThrough(edgeReasonSmall)
	}

	// A tenant the egress throttle would pace reads through the origin,
	// which paces it as D-25 says (slow, refused only past MAX_STREAMS):
	// the Worker never paces, and the bytes through SLC are then the
	// throttle rate. (Not a 503 from admit: one on every GET past the
	// allowance would turn D-25's trickle into hard failures.) With the
	// flag off the decision is counted where the origin counts it.
	if s.egress != nil {
		d := s.egress.decide(sctx, s.egress.tenant(sctx, tenantID))
		if d.over && d.enforced {
			return passThrough("throttled")
		}
		if d.over {
			egressWouldThrottle.WithLabelValues(edgeHost).Inc()
		}
	}

	// The version this response names, as HandleGet names it: the latest
	// version on a versioned bucket (a delete marker is the origin's 404),
	// "null" for an object with no version row there, none elsewhere.
	versionID := ""
	if vStatus == "Enabled" || vStatus == "Suspended" {
		var latest string
		var marker bool
		vErr := s.db.QueryRowContext(sctx, `SELECT version_id, is_delete_marker FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`, tenantID, bucket, key).Scan(&latest, &marker)
		switch {
		case vErr == nil && marker:
			return passThrough("delete_marker")
		case vErr == nil:
			versionID = latest
		case errors.Is(vErr, sql.ErrNoRows):
			versionID = "null"
		default:
			s.logger.Error("edge admit get: version read failed", zap.Error(vErr), zap.String("tenant_id", tenantID), zap.String("key", key))
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
	}

	// The link: the object's own address on its recorded backend, the
	// Range the backend serves, the If-Match it enforces — the digest
	// every whole GET verifies against (COALESCE(body_md5, etag)), which
	// IS the backend's ETag for every shape an EdgeWriter backend holds.
	_, blob := objectBlobAddress(tenantID, bucket, key, row.storageKey)
	container := tenantID + "_" + bucket
	ifMatch := `"` + integrityDigest(row.bodyMD5, row.etag) + `"`
	rangeValue := ""
	if rng != nil {
		rangeValue = fmt.Sprintf("bytes=%d-%d", rng.start, rng.end)
	}
	presigned, signed, presignErr := ew.PresignGet(sctx, container, blob, engine.EdgePresign{Range: rangeValue, IfMatch: ifMatch, TTL: edgeAdmitTTL})
	if presignErr != nil {
		s.logger.Error("edge admit get: presign failed", zap.Error(presignErr), zap.String("backend", row.backend))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	wantSigned := []string{"if-match"}
	if rangeValue != "" {
		wantSigned = append(wantSigned, "range")
	}
	if cErr := edgePresignSigns(presigned, wantSigned...); cErr != nil {
		s.logger.Error("edge admit get: the presigned link does not sign what it must — refused", zap.Error(cErr), zap.String("backend", row.backend))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	sendHeaders := map[string]string{}
	for _, name := range signed {
		switch name {
		case "if-match":
			sendHeaders["if-match"] = ifMatch
		case "range":
			sendHeaders["range"] = rangeValue
		case "host":
		default:
			s.logger.Error("edge admit get: the presigned link signs a header the Worker does not send", zap.String("header", name))
			return edgeAdmitResponse{}, http.StatusInternalServerError
		}
	}

	// The ORIGIN's response headers (object_headers.go, the one writer),
	// built exactly as HandleGet builds them.
	contentType := row.contentType
	if contentType == "" {
		contentType = s.readAdapter().detectContentType(key)
	}
	oh := objectHeaders{ETag: row.etag, LastModified: row.updatedAt, ContentType: contentType, Size: row.size,
		StorageClass: engine.CustomerStorageClass(row.floor, row.backend), VersionID: versionID, Metadata: row.metadata,
		TagCount: tagCount(row.tags), Disposition: row.attrs.ContentDisposition, Encoding: row.attrs.ContentEncoding, Language: row.attrs.ContentLanguage,
		CacheControl: row.echo.CacheControl, Expires: row.echo.Expires, WebsiteRedirect: row.echo.WebsiteRedirect}
	oh.LockMode, oh.LockRetainUntil, oh.LegalHold = lockHeadersOf(row.lockMode, row.lockUntil, row.legalHold)
	hrec := newRelayRecorder()
	status := http.StatusOK
	if rng != nil {
		oh.write(hrec, headersRange, rng)
		status = http.StatusPartialContent
	} else {
		oh.write(hrec, headersWhole, nil)
	}
	hrec.Header().Set("x-amz-request-id", generateRequestID())
	responseHeaders := hrec.relay().Headers

	// The row: nothing reserved, nothing minted — the admit remembers what
	// the commit (or the reaper) meters and logs.
	admit := edgeAdmitRow{TenantID: tenantID, Bucket: bucket, Key: key, Op: edgeOpGet, Backend: row.backend, StorageKey: blob,
		ContentLength: length, ContentType: contentType, Floor: row.floor, ClientIP: req.ClientIP, RequestID: req.RequestID,
		Attrs: edgeAdmitAttrs{UserAgent: sr.Header.Get("User-Agent"), Status: status}}
	if admit.ID, err = newEdgeAdmitID(); err != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	admit.ExpiresAt = time.Now().UTC().Add(edgeAdmitExpiry)
	if err := insertEdgeAdmit(sctx, s.db, admit); err != nil {
		s.logger.Error("edge admit get: row not written", zap.Error(err))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	s.logger.Info("edge admit", zap.String("op", edgeOpGet), zap.String("tenant_id", tenantID), zap.String("bucket", bucket),
		zap.String("key", key), zap.String("backend", row.backend), zap.Int64("size", length), zap.String("range", rangeValue),
		zap.String("admit_id", admit.ID), zap.String("request_id", req.RequestID))
	return edgeAdmitResponse{Edge: true, AdmitID: admit.ID, URL: presigned, Method: http.MethodGet, SendHeaders: sendHeaders,
		ExpectStatus: status, ResponseHeaders: responseHeaders, Backend: row.backend, StorageKey: blob, ExpiresAt: &admit.ExpiresAt}, http.StatusOK
}

// settleEdgeGet is the GET's commit under the admit row's lock: `served`
// meters and logs the read exactly as the origin would have at the end of
// its own response; anything else is the origin's serve, and the backend's
// status feeds the breaker. Both answers to the Worker are an
// acknowledgement (it has already answered the client).
func (s *Server) settleEdgeGet(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder) (string, int64) {
	cctx := common.WithTenantID(context.WithoutCancel(ctx), row.TenantID)
	switch req.Outcome {
	case edgeGetServed:
		n := req.BackendContentLength
		if n <= 0 || n > row.ContentLength {
			// The backend's Content-Length is the truth of the body it sent;
			// none, or more than the admitted bytes (a backend serving a
			// longer object under an If-Match that passed cannot happen —
			// the address is immutable), falls back to what was admitted.
			s.logger.Warn("edge commit get: the backend's content length is not the admitted one — the admitted size is metered",
				zap.Int64("backend_content_length", req.BackendContentLength), zap.Int64("admitted", row.ContentLength), zap.String("admit_id", row.ID))
			n = row.ContentLength
		}
		s.recordEdgeEgress(cctx, row.TenantID, row.Backend, n)
		status := row.Attrs.Status
		if status == 0 {
			status = http.StatusOK
		}
		s.recordEdgeAccess(row, status, 0, n)
		if s.engine != nil {
			s.engine.RecordReadOutcome(row.Backend, nil)
		}
		s.logger.Info("artifact retrieved", zap.String("s3.bucket", row.Bucket), zap.String("s3.object", row.Key),
			zap.String("backend", row.Backend), zap.Int64("bytes", n), zap.String("host", edgeHost))
		emitEvent(cctx, s.db, s.logger, "object.downloaded", row.TenantID, map[string]interface{}{
			"bucket": row.Bucket, "key": row.Key, "size": n,
		})
		rec.WriteHeader(http.StatusOK)
		return "committed", n
	default:
		// The origin served it (the Worker's fallback is fetch(request)):
		// nothing metered here. The backend's answer is a read outcome of
		// that backend — a 5xx, a 403 (a dead key) or no answer charges
		// its breaker as the origin's own read would; a 404 is logged
		// loud (the row names a blob the backend does not hold) and a 412
		// (the digest moved under an immutable address) is impossible by
		// construction — neither is the backend's health.
		if s.engine != nil {
			if rerr := edgeBackendReadError(req); rerr != nil {
				s.engine.RecordReadOutcome(row.Backend, rerr)
			}
		}
		level := s.logger.Warn
		if req.BackendStatus == http.StatusNotFound {
			level = s.logger.Error
		}
		level("edge commit get: the backend did not serve the object — the origin did",
			zap.String("outcome", req.Outcome), zap.Int("backend_status", req.BackendStatus), zap.String("backend_code", req.BackendCode),
			zap.String("backend", row.Backend), zap.String("storage_key", row.StorageKey), zap.String("tenant_id", row.TenantID),
			zap.String("bucket", row.Bucket), zap.String("key", row.Key), zap.String("admit_id", row.ID))
		rec.WriteHeader(http.StatusOK)
		return "failed", 0
	}
}

// edgeBackendReadError is the read outcome a failed edge GET charges to the
// backend's breaker, with the engine's own rules (RecordReadOutcome): nil
// for an answer that is the object's state, not the backend's health.
func edgeBackendReadError(req edgeCommitRequest) error {
	switch {
	case req.Outcome == edgeGetServed:
		return nil
	case req.BackendStatus == http.StatusNotFound:
		return nil // an object-level miss: never a breaker charge (IsObjectMiss)
	case req.BackendStatus == http.StatusPreconditionFailed:
		return nil // the object changed under the read: engine.ErrObjectChanged's rule
	case req.BackendStatus == http.StatusRequestedRangeNotSatisfiable:
		return nil
	case req.BackendStatus == 0:
		return fmt.Errorf("edge get: the backend did not answer (%s): %w", req.BackendCode, engine.ErrTimeout)
	default:
		return fmt.Errorf("edge get: the backend answered %d %s", req.BackendStatus, req.BackendCode)
	}
}

// recordEdgeEgress is THE egress write of an edge-served read (Step 0.3 of
// docs/reviews/WP-EDGE-1B.md): the origin's own entry point, so the live
// month counter (the throttle's decide, EgressStatus), the alert ladder
// and bandwidth_usage_daily see one number. It is exactly what a finished
// origin response does: the bytes counted on the tenant's egressSpan as
// they were written, then BandwidthTracker.recordResponse hands the
// unrecorded bytes to the daily row (the month split, the backend
// attribution). An unknown backend attributes to none, as the origin's
// BackendUsed would.
func (s *Server) recordEdgeEgress(ctx context.Context, tenantID, backend string, n int64) {
	if n <= 0 || tenantID == "" {
		return
	}
	cw := &countingResponseWriter{bytesWritten: n}
	if s.egress != nil {
		cw.span = s.egress.span(context.WithoutCancel(ctx), tenantID)
		cw.span.count(n)
	}
	if s.bandwidthTracker != nil {
		s.bandwidthTracker.recordResponse(tenantID, backend, 0, cw)
	} else if cw.span != nil {
		// No tracker (a process without the bandwidth tables): the bytes
		// stay on the live counter only, as an origin response's would.
		s.logger.Warn("edge egress not recorded: no bandwidth tracker", zap.String("tenant_id", tenantID), zap.Int64("bytes", n))
	}
}
