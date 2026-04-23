package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const maxVersioningBodyBytes = 4096

type VersioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

func (s *Server) handleGetBucketVersioning(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	status := "disabled"
	if s.db != nil {
		var dbStatus string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket).Scan(&dbStatus)
		if err == sql.ErrNoRows {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
			return
		}
		if err != nil {
			s.logger.Error("query versioning status", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		status = dbStatus
	}

	resp := VersioningConfiguration{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	if status == "Enabled" || status == "Suspended" {
		resp.Status = status
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutBucketVersioning(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxVersioningBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config VersioningConfiguration
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if config.Status != "Enabled" && config.Status != "Suspended" {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	result, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET versioning_status = $1, updated_at = NOW()
		 WHERE tenant_id = $2 AND name = $3`,
		config.Status, t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("update versioning status", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("versioning status updated",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.String("status", config.Status))

	w.WriteHeader(http.StatusOK)
}

func generateVersionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func getBucketVersioningStatus(ctx context.Context, db *sql.DB, tenantID, bucket string) string {
	if db == nil {
		return "disabled"
	}
	var status string
	err := db.QueryRowContext(ctx,
		`SELECT versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&status)
	if err != nil {
		return "disabled"
	}
	return status
}
