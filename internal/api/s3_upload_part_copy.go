package api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// maxCopyPartSize is the largest part UploadPartCopy copies: 5 GiB, AWS's
// limit for a part and for an UploadPartCopy range.
const maxCopyPartSize int64 = 5 << 30

// CopyPartResult is the XML body of a successful UploadPartCopy.
type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// copySourceRangeHeader is x-amz-copy-source-range: bytes=first-last, both
// ends required and inclusive (no suffix range, no open end, one range).
var copySourceRangeHeader = regexp.MustCompile(`^bytes=([0-9]+)-([0-9]+)$`)

// parseCopySourceRange reads x-amz-copy-source-range against a source of
// size bytes. A malformed range, one that does not lie inside the source,
// or one longer than 5 GiB is an error (AWS: 400 InvalidArgument).
func parseCopySourceRange(header string, size int64) (copySourceRange, error) {
	m := copySourceRangeHeader.FindStringSubmatch(header)
	if m == nil {
		return copySourceRange{}, fmt.Errorf("the range must be bytes=first-last, got %q", header)
	}
	first, err1 := strconv.ParseInt(m[1], 10, 64)
	last, err2 := strconv.ParseInt(m[2], 10, 64)
	if err1 != nil || err2 != nil || first > last {
		return copySourceRange{}, fmt.Errorf("the range %q is not valid", header)
	}
	if last >= size {
		return copySourceRange{}, fmt.Errorf("range specified is not valid for source object of size: %d", size)
	}
	n := last - first + 1
	if n > maxCopyPartSize {
		return copySourceRange{}, fmt.Errorf("the range %q is longer than a part may be (%d bytes)", header, maxCopyPartSize)
	}
	return copySourceRange{Ranged: true, Off: first, N: n}, nil
}

// handleUploadPartCopy handles UploadPartCopy (WP-R3-2): part N of an
// upload is a range of an existing object (x-amz-copy-source, optional
// x-amz-copy-source-range). aws-cli and rclone copy every object above
// their multipart threshold server-side through it (aws-cli: 8 MB).
//
// The source is resolved and opened exactly as CopyObject's (one resolver,
// one opener: s3_copy_source.go) — the current object or a version, at its
// address on its recorded backend, sealed or chunked, the x-amz-copy-
// source-if-* conditions — and staged exactly as an UploadPart's body (one
// stager: stageUploadPart), with the range's length as the declared size.
// The scoped-key gate on the source is the dispatcher's
// (copySourceScopeDenied, R5-02), as for CopyObject.
//
// It runs under the long-operation keep-alive as CopyObject does: a 5 GiB
// part from a slow backend outlasts Cloudflare's 100 s. Every refusal
// (source, range, conditions, quota) is decided before longOpBegin.
func (s *Server) handleUploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.runLongS3Op(w, r, longOpInfo{Op: longOpUploadPartCopy, Bucket: bucket, Key: object}, func(w http.ResponseWriter, r *http.Request) {
		s.uploadPartCopy(w, r, bucket, object)
	})
}

// uploadPartCopy is UploadPartCopy itself (see handleUploadPartCopy).
func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	uploadID, partNumber, ok := s.uploadPartTarget(w, r, t.ID, bucket, object)
	if !ok {
		return
	}

	copySource := r.Header.Get("x-amz-copy-source")
	srcBucket, srcKey, err := parseCopySource(copySource)
	if err != nil {
		s.logger.Warn("invalid x-amz-copy-source", zap.String("copy_source", copySource), zap.Error(err))
		WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
		return
	}
	srcVersionID := copySourceVersionID(copySource)
	src, ok := s.resolveCopySource(w, r, t.ID, srcBucket, srcKey, srcVersionID)
	if !ok {
		return
	}
	defer src.clearKey()
	// The destination upload's key (WP-SSEC-1 B.1/B.4): the part is sealed
	// under it as it is staged, whatever the source's shape.
	seal, ok := s.uploadPartSeal(w, r, t.ID, bucket, object, uploadID, partNumber)
	if !ok {
		return
	}
	defer seal.clear()
	if !src.Resolved {
		// No database: nothing is known of the source's size or address.
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("UploadPartCopy needs the object index. Copy the object with CopyObject instead."))
		return
	}
	if src.sealedS3() && s.sseService == nil {
		s.logger.Error("upload part copy of an encrypted source with no SSE service: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", srcBucket), zap.String("key", srcKey))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// The part: the range, or the whole source.
	rng := copySourceRange{Off: 0, N: src.Size}
	if h := r.Header.Get("x-amz-copy-source-range"); h != "" {
		if rng, err = parseCopySourceRange(h, src.Size); err != nil {
			WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(), WithMessage(err.Error()))
			return
		}
	} else if src.Size > maxCopyPartSize {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
			WithMessage(fmt.Sprintf("The specified copy source is larger than the maximum allowable size for a copy source: %d", maxCopyPartSize)),
			WithSuggestion("Copy it in parts of at most 5 GiB with x-amz-copy-source-range."))
		return
	}

	reader, err := s.openCopySource(r.Context(), t.ID, src, rng)
	if err != nil {
		s.writeCopySourceOpenError(w, r, t.ID, src, err)
		return
	}
	defer func() { _ = reader.Close() }()

	// The part's bytes are exactly the range's (or the source's recorded
	// size): a source that ends early or runs on is not the source — 503,
	// no part (the copy's errCopySourceSize rule). The read is the backend's
	// body, never the client's: its failure is a 503, never 400
	// IncompleteBody (D1.6). A shutdown's cut stops it at once.
	source := &readErrReader{r: reader}
	body := &exactSizeReader{r: &cutReader{ctx: r.Context(), r: source}, want: rng.N}
	etag, size, ok := s.stageUploadPart(w, r, t.ID, uploadID, partNumber, partBody{
		r:        body,
		declared: rng.N,
		seal:     seal,
		begin:    func() { longOpBegin(r) },
		failed: func(w http.ResponseWriter, err error, _ int64) bool {
			if s.copySourceReadFailed(w, r, t.ID, src, source, err) {
				return true
			}
			if errors.Is(err, errLongOpCut) {
				w.Header().Set("Retry-After", "30")
				WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
				return true
			}
			return false
		},
	})
	if !ok {
		return
	}

	if srcVersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcVersionID)
	}
	xmlData, err := xml.MarshalIndent(CopyPartResult{
		ETag:         etag,
		LastModified: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}, "", "  ")
	if err != nil {
		s.logger.Error("upload part copy: XML marshal failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	s.logger.Debug("copied part",
		zap.String("bucket", bucket), zap.String("key", object),
		zap.String("source", srcBucket+"/"+srcKey), zap.String("upload_id", uploadID),
		zap.Int("part", partNumber), zap.Int64("size", size), zap.String("etag", etag))
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)
}
