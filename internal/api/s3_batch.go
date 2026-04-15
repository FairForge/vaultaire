package api

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// maxBatchDeleteKeys is the S3 spec limit per DeleteObjects request.
const maxBatchDeleteKeys = 1000

// maxBatchDeleteBodyBytes caps the request body size to avoid unbounded
// memory use. 1000 keys × ~1KB per <Object> entry fits comfortably in 2 MiB.
const maxBatchDeleteBodyBytes = 2 * 1024 * 1024

// DeleteRequest is the inbound XML body for POST /{bucket}?delete.
type DeleteRequest struct {
	XMLName xml.Name           `xml:"Delete"`
	Quiet   bool               `xml:"Quiet"`
	Objects []DeleteRequestKey `xml:"Object"`
}

// DeleteRequestKey is a single key entry in a DeleteRequest.
type DeleteRequestKey struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

// DeleteResult is the outbound XML response for DeleteObjects.
type DeleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	Xmlns   string        `xml:"xmlns,attr"`
	Deleted []DeletedItem `xml:"Deleted,omitempty"`
	Errors  []DeleteError `xml:"Error,omitempty"`
}

// DeletedItem reports a successfully deleted key.
type DeletedItem struct {
	Key string `xml:"Key"`
}

// DeleteError reports a per-key failure within a batch delete.
type DeleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// handleDeleteObjects handles POST /{bucket}?delete — the S3 batch delete API.
//
// S3 DeleteObjects is idempotent per key: a missing key is reported as
// "Deleted" (not an error), matching AWS behavior. When <Quiet>true</Quiet>
// is set, only errors are returned.
func (s *Server) handleDeleteObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBatchDeleteBodyBytes+1))
	if err != nil {
		s.logger.Warn("batch delete: body read failed", zap.Error(err))
		WriteS3Error(w, ErrIncompleteBody, r.URL.Path, generateRequestID())
		return
	}
	if len(body) > maxBatchDeleteBodyBytes {
		WriteS3Error(w, ErrEntityTooLarge, r.URL.Path, generateRequestID())
		return
	}

	var delReq DeleteRequest
	if err := xml.Unmarshal(body, &delReq); err != nil {
		s.logger.Warn("batch delete: malformed XML", zap.Error(err))
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if len(delReq.Objects) == 0 {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}
	if len(delReq.Objects) > maxBatchDeleteKeys {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	bucket := req.Bucket
	container := t.NamespaceContainer(bucket)

	result := DeleteResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}

	for _, obj := range delReq.Objects {
		key := obj.Key
		if key == "" {
			if !delReq.Quiet {
				result.Errors = append(result.Errors, DeleteError{
					Key:     key,
					Code:    ErrInvalidRequest,
					Message: "Key is required",
				})
			} else {
				result.Errors = append(result.Errors, DeleteError{
					Code:    ErrInvalidRequest,
					Message: "Key is required",
				})
			}
			continue
		}

		delErr := s.engine.Delete(r.Context(), container, key)
		isMissing := delErr != nil && (strings.Contains(delErr.Error(), "no such file or directory") ||
			strings.Contains(delErr.Error(), "not found"))

		if delErr != nil && !isMissing {
			s.logger.Error("batch delete: engine delete failed",
				zap.Error(delErr),
				zap.String("container", container),
				zap.String("key", key))
			result.Errors = append(result.Errors, DeleteError{
				Key:     key,
				Code:    ErrInternalError,
				Message: "Internal error while deleting",
			})
			continue
		}

		// Success (or idempotent miss) — clear head cache and record as Deleted.
		if s.db != nil {
			_, _ = s.db.ExecContext(r.Context(), `
				DELETE FROM object_head_cache
				WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			`, t.ID, bucket, key)
		}

		if !delReq.Quiet {
			result.Deleted = append(result.Deleted, DeletedItem{Key: key})
		}
	}

	xmlData, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		s.logger.Error("batch delete: XML marshal failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)

	s.logger.Info("batch delete",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", bucket),
		zap.Int("requested", len(delReq.Objects)),
		zap.Int("deleted", len(result.Deleted)),
		zap.Int("errors", len(result.Errors)),
		zap.Bool("quiet", delReq.Quiet))
}
