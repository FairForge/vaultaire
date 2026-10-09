package api

import (
	"database/sql"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// maxBatchDeleteKeys is the S3 spec limit per DeleteObjects request.
const maxBatchDeleteKeys = 1000

// batchDeleteConcurrency is how many keys of one DeleteObjects request are
// deleted at once (Sync: ~1 s per delete, ~14/s per account — 16 in flight
// keeps a bridge busy without queueing the whole batch on its slots).
const batchDeleteConcurrency = 16

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
//
// The keys are deleted batchDeleteConcurrency at a time (results in request
// order, a key named twice is deleted once) under the long-operation
// keep-alive (s3_long_op.go): on Sync a delete costs ~1 s, so a 1,000-key
// batch run one key after another outlasted Cloudflare's 100 s and the
// client's disconnect stopped it halfway.
func (s *Server) handleDeleteObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	s.runLongS3Op(w, r, longOpInfo{Op: longOpBatch, Bucket: req.Bucket}, func(w http.ResponseWriter, r *http.Request) {
		s.deleteObjects(w, r, req)
	})
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBatchDeleteBodyBytes+1))
	if err != nil {
		s.logger.Warn("batch delete: body read failed", zap.Error(err))
		code := ErrIncompleteBody
		if errors.Is(err, auth.ErrContentSHA256Mismatch) {
			code = ErrXAmzContentSHA256Mismatch
		}
		WriteS3Error(w, code, r.URL.Path, generateRequestID())
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
	// A batch against a bucket that does not exist is NoSuchBucket, before
	// the keep-alive can commit a 200 (it used to answer every key Deleted).
	if exists, bErr := s.longOpBucketExists(r, t.ID, bucket); bErr != nil {
		s.log().Error("batch delete: bucket lookup failed", zap.Error(bErr), zap.String("bucket", bucket))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	} else if !exists {
		s.writeNoSuchBucket(w, r, t.ID, bucket)
		return
	}

	result := DeleteResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}

	// The request is valid and the caller known: from here the keep-alive
	// may commit.
	longOpBegin(r)

	// One outcome per distinct key, computed in parallel; reported per
	// requested entry in request order.
	first := make(map[string]int, len(delReq.Objects))
	var unique []string
	for _, obj := range delReq.Objects {
		if _, seen := first[obj.Key]; !seen {
			first[obj.Key] = len(unique)
			unique = append(unique, obj.Key)
		}
	}
	outcomes := make([]*DeleteError, len(unique))
	sem := make(chan struct{}, batchDeleteConcurrency)
	// The notification targets and the webhooks are read once for the whole
	// batch; a key fires nothing when there are none.
	aftermath := s.objectDeleteAftermath().forBatch(r.Context(), t.ID, bucket)
	var wg sync.WaitGroup
	for i, key := range unique {
		sem <- struct{}{}
		// A shutdown cut the batch: the keys not started yet are not
		// attempted — an InternalError the client retries, never a guess.
		if r.Context().Err() != nil {
			<-sem
			outcomes[i] = &DeleteError{Key: key, Code: ErrInternalError,
				Message: "Not attempted: the server is restarting. Retry this key."}
			continue
		}
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			// This goroutine is outside net/http's per-request recover and
			// outside runLongS3Op's (the op goroutine only): a panic in one
			// key's delete took the whole process down. It is that key's
			// error entry; the other keys complete and the response is
			// still well-formed.
			defer func() {
				if p := recover(); p != nil {
					s.log().Error("batch delete: key delete panicked",
						zap.Any("panic", p), zap.String("tenant_id", t.ID),
						zap.String("bucket", bucket), zap.String("key", key))
					outcomes[i] = &DeleteError{Key: key, Code: ErrInternalError, Message: "Internal error while deleting"}
				}
			}()
			outcomes[i] = s.batchDeleteKey(r, t, bucket, container, key, aftermath)
		}(i, key)
	}
	wg.Wait()
	// Per-key notifications and webhooks: in key order, on the process-wide
	// delivery pool, after the response has been built (they do not block it).
	aftermath.fanout.deliverInOrder(aftermath, t.ID, bucket, func(key string) int { return first[key] })

	for _, obj := range delReq.Objects {
		if e := outcomes[first[obj.Key]]; e != nil {
			result.Errors = append(result.Errors, *e)
			continue
		}
		if !delReq.Quiet {
			result.Deleted = append(result.Deleted, DeletedItem{Key: obj.Key})
		}
	}
	// Every key failed: a 200 to the request metrics, a failure to the
	// outcome counter (error_before_commit, or error_after_commit when the
	// keep-alive had already committed the 200).
	if len(result.Errors) == len(delReq.Objects) {
		longOpFailed(r)
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

// batchDeleteKey deletes one key of a DeleteObjects batch and returns its
// error entry, or nil when the key is deleted (or was already missing — S3
// batch delete is idempotent per key). Safe to run concurrently for
// distinct keys.
func (s *Server) batchDeleteKey(r *http.Request, t *tenant.Tenant, bucket, container, key string, aftermath objectDeleteAftermath) *DeleteError {
	if key == "" {
		return &DeleteError{Key: key, Code: ErrInvalidRequest, Message: "Key is required"}
	}

	if lockErr := checkObjectLock(r.Context(), s.db, t.ID, bucket, key, isObjectLockBypass(r)); lockErr != nil {
		// Locked = AccessDenied; a lookup that failed (a database error, a
		// shutdown's cut) says nothing about a lock.
		if !errors.Is(lockErr, errObjectLocked) {
			s.logger.Error("batch delete: object lock lookup failed", zap.Error(lockErr), zap.String("key", key))
			return &DeleteError{Key: key, Code: ErrInternalError, Message: "Internal error while deleting"}
		}
		return &DeleteError{
			Key:     key,
			Code:    ErrAccessDenied,
			Message: errorMessages[ErrAccessDenied] + " " + lockDeniedHint(r),
		}
	}

	// Chunked objects live under _chunks/, not container/key: their
	// delete decrements GCI ref counts (mirrors single-key HandleDelete)
	// so dedup GC can reclaim the physical chunks. backend_name is the
	// routing truth: without the hint a DELETE after a restart went to
	// the primary alone, was answered "not found", and the bytes stayed
	// on the real backend while the head row went (R6-05, WP-R6-1).
	var isChunked bool
	var recordedBackend string
	if s.db != nil {
		if rowErr := s.db.QueryRowContext(r.Context(),
			`SELECT is_chunked, COALESCE(backend_name, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, key).Scan(&isChunked, &recordedBackend); rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
			// Never guess "whole" for a possibly chunked object (R8-07).
			s.logger.Error("batch delete: head cache read failed", zap.Error(rowErr), zap.String("key", key))
			return &DeleteError{
				Key: key, Code: ErrInternalError, Message: "Internal error while deleting",
			}
		}
	}

	var delErr error
	// A chunked object's manifest is released together with its head row
	// below, in one transaction (R8-08); there is no backend delete.
	if !isChunked || s.gci == nil {
		if recordedBackend != "" && s.engine != nil {
			noteRecordedBackend(s.engine, s.logger, "delete_objects", recordedBackend)
			s.engine.HintBackend(container, key, recordedBackend)
		}
		// A delete already in flight when a shutdown cuts the batch may
		// still answer for longOpAnswerGrace: bytes it removed get their
		// row settled (s3_long_op.go).
		dctx, dcancel := backendCallCtx(r.Context(), s.longOpAnswerGrace())
		delErr = s.engine.Delete(dctx, container, key)
		dcancel()
		// A miss is idempotent (AWS behaviour) in every shape a driver
		// produces it — the SDK's NoSuchKey/NotFound included (R6-25);
		// an unreachable backend is NOT a miss (R6-02).
		if delErr != nil && isObjectMissingErr(delErr) {
			delErr = nil
		}
	}

	if delErr != nil {
		s.logger.Error("batch delete: delete failed",
			zap.Error(delErr),
			zap.String("container", container),
			zap.String("key", key))
		if errors.Is(delErr, engine.ErrAllBackendsUnavailable) {
			// The backend holding the bytes is unreachable: the row stays
			// and the client retries this key (Prompt 2b.2 C2).
			return &DeleteError{Key: key, Code: ErrServiceUnavailable, Message: errorMessages[ErrServiceUnavailable]}
		}
		return &DeleteError{
			Key:     key,
			Code:    ErrInternalError,
			Message: "Internal error while deleting",
		}
	}

	// Success (or idempotent miss): the one aftermath single DELETE runs —
	// billing record, lock row, Smart copy, parity shards, notification
	// (object_delete_shared.go), detached from the cut.
	if err := aftermath.settle(r.Context(), t.ID, bucket, key, isChunked); err != nil {
		// The row (and a chunked manifest) is still there: the client
		// retries this key — a backend miss, then the row.
		s.logger.Error("batch delete: delete bookkeeping failed",
			zap.Error(err), zap.String("key", key))
		return &DeleteError{
			Key: key, Code: ErrInternalError, Message: "Internal error while deleting",
		}
	}

	return nil
}
