package api

import (
	"database/sql"
	"encoding/xml"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const maxLoggingBodyBytes = 4096

// BucketLoggingStatus is the S3 XML response for GET ?logging.
type BucketLoggingStatus struct {
	XMLName        xml.Name        `xml:"BucketLoggingStatus"`
	Xmlns          string          `xml:"xmlns,attr,omitempty"`
	LoggingEnabled *LoggingEnabled `xml:"LoggingEnabled,omitempty"`
}

type LoggingEnabled struct {
	TargetBucket string `xml:"TargetBucket"`
	TargetPrefix string `xml:"TargetPrefix,omitempty"`
}

func (s *Server) handleGetBucketLogging(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(xml.Header))
		_, _ = w.Write([]byte(`<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`))
		return
	}

	var enabled bool
	var targetBucket, prefix sql.NullString
	err = s.db.QueryRowContext(r.Context(),
		`SELECT logging_enabled, logging_target_bucket, logging_prefix
		 FROM buckets WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket).Scan(&enabled, &targetBucket, &prefix)
	if err == sql.ErrNoRows {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}
	if err != nil {
		s.logger.Error("query bucket logging config", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	resp := BucketLoggingStatus{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	if enabled && targetBucket.Valid && targetBucket.String != "" {
		resp.LoggingEnabled = &LoggingEnabled{
			TargetBucket: targetBucket.String,
			TargetPrefix: prefix.String,
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutBucketLogging(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoggingBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config BucketLoggingStatus
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	// Verify the source bucket exists.
	var exists bool
	err = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
		t.ID, req.Bucket).Scan(&exists)
	if err != nil || !exists {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}

	// If LoggingEnabled is nil or empty target, disable logging.
	if config.LoggingEnabled == nil || config.LoggingEnabled.TargetBucket == "" {
		_, err = s.db.ExecContext(r.Context(),
			`UPDATE buckets SET logging_enabled = FALSE, logging_target_bucket = NULL, logging_prefix = '', updated_at = NOW()
			 WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket)
		if err != nil {
			s.logger.Error("disable bucket logging", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		s.logger.Info("bucket logging disabled",
			zap.String("tenant_id", t.ID),
			zap.String("bucket", req.Bucket))
		w.WriteHeader(http.StatusOK)
		return
	}

	targetBucket := config.LoggingEnabled.TargetBucket
	targetPrefix := config.LoggingEnabled.TargetPrefix

	// Reject self-referential logging (prevents infinite loops).
	if targetBucket == req.Bucket {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
			WithSuggestion("Target bucket must be different from the source bucket to prevent logging loops."))
		return
	}

	// Verify target bucket exists and belongs to the same tenant.
	err = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
		t.ID, targetBucket).Scan(&exists)
	if err != nil || !exists {
		WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, generateRequestID(),
			WithSuggestion("Target bucket does not exist or belongs to a different account."))
		return
	}

	_, err = s.db.ExecContext(r.Context(),
		`UPDATE buckets SET logging_enabled = TRUE, logging_target_bucket = $3, logging_prefix = $4, updated_at = NOW()
		 WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket, targetBucket, targetPrefix)
	if err != nil {
		s.logger.Error("enable bucket logging", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("bucket logging enabled",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.String("target_bucket", targetBucket),
		zap.String("prefix", targetPrefix))

	w.WriteHeader(http.StatusOK)
}
