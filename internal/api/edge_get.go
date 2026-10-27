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
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
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
	backendETag                                                     string
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
	// A conditional GET other than If-Match is the origin's (a 304 is
	// its answer; no S3 client on the API path sends one). If-Match is
	// evaluated HERE against the row, below — aws-cli's `s3 cp` sends
	// `If-Match: "<etag>"` with `Range: bytes=0-` on every download past
	// its multipart threshold (the lab, 2026-10-26: every 64 MiB download
	// went through the origin until it was), and a condition the row
	// satisfies has nothing left for the origin to decide.
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
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
		       COALESCE(h.body_md5, ''), COALESCE(h.backend_etag, ''),
		       `+objectLockColumns+`
		FROM object_head_cache h
		LEFT JOIN object_locks l ON l.tenant_id = h.tenant_id AND l.bucket = h.bucket AND l.object_key = h.object_key
		WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3`,
		tenantID, bucket, key).Scan(&row.contentType, &row.size, &row.etag, &row.updatedAt, &row.metadata, &row.backend, &row.encAlgo, &row.tags,
		&row.attrs.ContentDisposition, &row.attrs.ContentEncoding, &row.attrs.ContentLanguage, &row.echo.CacheControl, &row.echo.Expires, &row.echo.WebsiteRedirect,
		&row.isChunked, &row.floor, &row.storageKey, &row.bodyMD5, &row.backendETag, &row.lockMode, &row.lockUntil, &row.legalHold)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return passThrough("no_row") // the origin's NoSuchKey, with its suggestion
	case err != nil:
		s.logger.Error("edge admit get: head row read failed", zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", key))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	// The client's If-Match against the row, as evaluateConditionalGET
	// does: a condition the row fails is the origin's 412.
	if checkIfMatch(sr, row.etag) {
		return passThrough("conditional")
	}
	// The kinds the edge never reads: a chunked object (the content index),
	// a sealed one (a key on SLC), one stored with a Content-Encoding (the
	// Workers runtime re-encodes a Response body to match the header it is
	// given — `encodeBody: "automatic"` — while the backend's answer carries
	// the already-encoded bytes with no encoding header: the runtime would
	// gzip the gzip under the row's Content-Length; RP66-1), a vault-floor
	// one (tape, its restore semantics, the parity rebuild), a strict target
	// (sia's promotion, blomp, sync), a Smart-demoted one (its promotion),
	// an empty one.
	switch {
	case row.isChunked:
		return passThrough("chunked")
	case row.encAlgo != "":
		return passThrough("sealed")
	case row.attrs.ContentEncoding != "":
		return passThrough("content_encoding")
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

	// The link: the object's own address on its recorded backend; the
	// exact byte span the backend serves — ALWAYS a Range, the whole
	// object's for a whole read, so the link is bound to it and the
	// backend answers 206 either way (D-EDGE-11); and the If-Match the
	// backend enforces when the row's digest IS the backend's ETag
	// (edgeGetIfMatch, D-EDGE-10).
	_, blob := objectBlobAddress(tenantID, bucket, key, row.storageKey)
	container := tenantID + "_" + bucket
	ifMatch := edgeGetIfMatch(row.etag, row.bodyMD5, row.backendETag, row.size, row.updatedAt)
	start, end := int64(0), row.size-1
	if rng != nil {
		start, end = rng.start, rng.end
	}
	// A read of at least edgeWindowMin is the windowed reader's (D-EDGE-32:
	// the bench § 4 doubled both backends at 16 MiB × 6) when the Worker
	// says it reads one: the link signs no range — the Worker names each one
	// inside the admitted span — and if-match where provable; under it, or
	// for an older Worker, the exact span is signed (D-EDGE-11).
	var window *edgeWindow
	rangeValue := fmt.Sprintf("bytes=%d-%d", start, end)
	presign := engine.EdgePresign{Range: rangeValue, IfMatch: ifMatch, TTL: edgeAdmitTTL}
	wantSigned := []string{"range"}
	if length >= edgeWindowMin && req.Has(edgeFeatureWindow) {
		window = &edgeWindow{RangeBytes: edgeWindowRange, InFlight: edgeWindowInFlight, Start: start}
		presign.Range, presign.Window, rangeValue, wantSigned = "", true, "", nil
	}
	presigned, signed, presignErr := ew.PresignGet(sctx, container, blob, presign)
	if presignErr != nil {
		s.logger.Error("edge admit get: presign failed", zap.Error(presignErr), zap.String("backend", row.backend))
		return edgeAdmitResponse{}, http.StatusInternalServerError
	}
	if ifMatch != "" {
		wantSigned = append(wantSigned, "if-match")
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
			if window != nil {
				s.logger.Error("edge admit get: a windowed read's link signs a range — refused")
				return edgeAdmitResponse{}, http.StatusInternalServerError
			}
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
	// The id the client sees is the one the admit row, the access-log row
	// and the Worker's log line carry (RP66-5): a customer quoting it from a
	// failed download can be found.
	respReqID := req.RequestID
	if respReqID == "" {
		respReqID = generateRequestID()
	}
	hrec.Header().Set("x-amz-request-id", respReqID)
	responseHeaders := hrec.relay().Headers

	// The row: nothing reserved, nothing minted — the admit remembers what
	// the commit (or the reaper) meters and logs. The backend answers 206
	// to every link (a Range always); the client's status is the origin's.
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
		ExpectStatus: http.StatusPartialContent, Status: status, ResponseHeaders: responseHeaders, Backend: row.backend, StorageKey: blob,
		ContentLength: length, Window: window, ExpiresAt: &admit.ExpiresAt}, http.StatusOK
}

// The windowed reader (D-EDGE-32, the SPEED bench § 4): a read of at least
// edgeWindowMin bytes is read by the Worker as edgeWindowRange ranges with
// at most edgeWindowInFlight in flight (a Worker's six connections).
const (
	edgeWindowMin      = 64 << 20
	edgeWindowRange    = 16 << 20
	edgeWindowInFlight = 6
)

// edgeGetIfMatch is the If-Match a read's link signs: the backend's own
// ETag where the row recorded it at the write (backend_etag, RP72-1 —
// migration 112: every mover commit and every origin write whose driver
// reports it), else the row's digest ONLY when it is provably the backend's
// ETag (D-EDGE-10) — an edge-assembled multipart (a -N ETag with no
// body_md5: the backend's own answer, recorded at Complete), or an object
// the origin wrote as ONE backend PutObject (at most the single-PUT cutoff
// IN FORCE WHEN THE ROW WAS WRITTEN: its ETag is the body's MD5 — the
// row's single ETag, or the body_md5 of an origin-assembled multipart). A
// larger origin-written object is a backend multipart whose -N the row
// does not know: no If-Match, the immutable address is the guard. (The
// lab, 2026-10-26: a 64 MiB origin-written object's link signed body_md5
// and both backends answered 412; RP72-1: a 20 MiB row of before #1011 —
// 16 MiB parts then — was signed its MD5 against the moved 32 MiB cutoff
// and read through the origin on every read, for ever.) A digest that
// turns out wrong is a 412 → the origin serves: never other bytes.
func edgeGetIfMatch(etag, bodyMD5, backendETag string, size int64, written time.Time) string {
	if b := strings.Trim(strings.TrimSpace(backendETag), `"`); b != "" {
		return `"` + b + `"`
	}
	etag = strings.Trim(etag, `"`)
	multipart := strings.Contains(etag, "-")
	switch {
	case multipart && bodyMD5 == "":
		return `"` + etag + `"` // the backend's own -N (an edge-assembled upload)
	case size <= singlePutMaxBytesAt(written):
		return `"` + integrityDigest(bodyMD5, etag) + `"` // one backend PUT: the body's MD5
	default:
		return ""
	}
}

// s3SinglePutCutoffBefore is when the origin's S3-class uploader moved from
// 16 MiB to 32 MiB parts (#1011, d7532439: its Deploy ran 19:52:34 →
// 19:57:38 UTC on 2026-10-27; the minute after its end, so a row the old
// slot wrote while the switch ran is read under the old rule — a row the
// new slot wrote then loses only its If-Match). A row written before it was
// one backend PutObject only up to s3SinglePutMaxBytesBefore (RP72-1).
var s3SinglePutCutoffBefore = time.Date(2026, 10, 27, 19, 58, 0, 0, time.UTC)

// s3SinglePutMaxBytesBefore is the single-PUT cutoff before #1011 (the
// 16 MiB part size of #279, 2026-06-02).
const s3SinglePutMaxBytesBefore = 16 << 20

// singlePutMaxBytesAt is the largest body the origin wrote as ONE backend
// PutObject at the time a row was written (its updated_at: every whole
// write upserts it).
func singlePutMaxBytesAt(written time.Time) int64 {
	if !written.IsZero() && written.Before(s3SinglePutCutoffBefore) {
		return s3SinglePutMaxBytesBefore
	}
	return drivers.S3SinglePutMaxBytes
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
		if req.BytesSent > 0 {
			// A windowed read: the bytes the Worker wrote, its ranges summed.
			n = req.BytesSent
		}
		switch {
		case n == 0:
			// No Content-Length at the backend's headers (neither backend
			// answers a ranged GET chunked today): no length check was
			// possible, the admitted size is metered (RP66-7).
			s.logger.Debug("edge commit get: no backend content length — the admitted size is metered",
				zap.Int64("admitted", row.ContentLength), zap.String("admit_id", row.ID))
			n = row.ContentLength
		case n < 0, n > row.ContentLength:
			// More than the admitted bytes (a backend serving a longer object
			// under an If-Match that passed cannot happen — the address is
			// immutable): clamped to what was admitted. The 0.2.1 Worker
			// falls back on any mismatch before serving (RP66-2).
			s.logger.Warn("edge commit get: the backend's content length is not the admitted one — the admitted size is metered",
				zap.Int64("backend_content_length", req.BackendContentLength), zap.Int64("admitted", row.ContentLength), zap.String("admit_id", row.ID))
			n = row.ContentLength
		}
		// The edge serves bytes the origin's integrity reader never sees:
		// beyond the Worker's length check the body is unverified by design,
		// and the counter says so, once per served read (RP66-2).
		objectIntegrityUnverified.WithLabelValues(integrityPathEdge, "unverified").Inc()
		// The origin refreshes last_accessed on every cache hit (the
		// smart_demotion job reads it): an edge read is a read (RP66-3).
		// Fire-and-forget, as the origin's own touch.
		if s.db != nil {
			go func() { // #nosec G118 -- fire-and-forget access-time touch, the origin's own pattern
				_, _ = s.db.ExecContext(context.Background(), `
					UPDATE object_head_cache SET last_accessed = NOW()
					WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
					row.TenantID, row.Bucket, row.Key)
			}()
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
	case edgeGetError, edgeGetFallback:
		if req.BytesSent <= 0 {
			return s.settleEdgeGetFallback(row, req, rec)
		}
		// A windowed read cut mid-object: a range failed after bytes had
		// gone to the client, whose body was cut (never a clean short end).
		// What was sent is metered — the client has it — and the backend's
		// answer charges its breaker; the origin served nothing.
		n := min(req.BytesSent, row.ContentLength)
		s.recordEdgeEgress(cctx, row.TenantID, row.Backend, n)
		s.recordEdgeAccess(row, http.StatusInternalServerError, 0, n)
		if s.engine != nil {
			if rerr := edgeBackendReadError(req); rerr != nil {
				s.engine.RecordReadOutcome(row.Backend, rerr)
			}
		}
		s.logger.Warn("edge commit get: a windowed read was cut mid-object — what was sent is metered",
			zap.Int64("bytes_sent", n), zap.Int64("admitted", row.ContentLength), zap.Int("backend_status", req.BackendStatus),
			zap.String("backend_code", req.BackendCode), zap.String("backend", row.Backend), zap.String("admit_id", row.ID))
		rec.WriteHeader(http.StatusOK)
		return "failed", n
	default:
		return s.settleEdgeGetFallback(row, req, rec)
	}
}

// settleEdgeGetFallback is the commit of a read the origin served.
func (s *Server) settleEdgeGetFallback(row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder) (string, int64) {
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
