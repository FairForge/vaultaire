package api

import (
	"context"
	"database/sql"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const maxLockBodyBytes = 8192

// XML types for Object Lock configuration

type ObjectLockConfiguration struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	Xmlns             string          `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *ObjectLockRule `xml:"Rule,omitempty"`
}

type ObjectLockRule struct {
	DefaultRetention *DefaultRetention `xml:"DefaultRetention"`
}

type DefaultRetention struct {
	Mode string `xml:"Mode"`
	Days int    `xml:"Days,omitempty"`
}

// XML types for per-object retention

type RetentionConfig struct {
	XMLName         xml.Name `xml:"Retention"`
	Xmlns           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// XML types for per-object legal hold

type LegalHoldConfig struct {
	XMLName xml.Name `xml:"LegalHold"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

// --- Bucket-level Object Lock ---

func (s *Server) handleGetObjectLockConfiguration(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	resp := ObjectLockConfiguration{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}

	if s.db != nil {
		var enabled bool
		var mode string
		var days int
		err := s.db.QueryRowContext(r.Context(),
			`SELECT object_lock_enabled, default_retention_mode, default_retention_days
			 FROM buckets WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket).Scan(&enabled, &mode, &days)
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
			s.logger.Error("query object lock config", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}

		if enabled {
			resp.ObjectLockEnabled = "Enabled"
		}
		if mode != "" && days > 0 {
			resp.Rule = &ObjectLockRule{
				DefaultRetention: &DefaultRetention{
					Mode: mode,
					Days: days,
				},
			}
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutObjectLockConfiguration(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config ObjectLockConfiguration
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	enabled := config.ObjectLockEnabled == "Enabled"
	mode := ""
	days := 0

	if config.Rule != nil && config.Rule.DefaultRetention != nil {
		dr := config.Rule.DefaultRetention
		if dr.Mode != "GOVERNANCE" && dr.Mode != "COMPLIANCE" {
			WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
			return
		}
		if dr.Days <= 0 {
			WriteS3Error(w, ErrInvalidRetentionPeriod, r.URL.Path, generateRequestID())
			return
		}
		mode = dr.Mode
		days = dr.Days
	}

	result, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET object_lock_enabled = $1, default_retention_mode = $2,
		 default_retention_days = $3, updated_at = NOW()
		 WHERE tenant_id = $4 AND name = $5`,
		enabled, mode, days, t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("update object lock config", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}

	s.logger.Info("object lock config updated",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.Bool("enabled", enabled),
		zap.String("mode", mode),
		zap.Int("days", days))

	w.WriteHeader(http.StatusOK)
}

// --- Per-object retention ---

func (s *Server) handleGetObjectRetention(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	resp := RetentionConfig{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}

	if s.db != nil {
		var mode string
		var retainUntil sql.NullTime
		err := s.db.QueryRowContext(r.Context(),
			`SELECT retention_mode, retain_until_date FROM object_locks
			 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, req.Bucket, req.Object).Scan(&mode, &retainUntil)
		if err == sql.ErrNoRows {
			reqID := generateRequestID()
			if suggestion := keySuggestion(r.Context(), s.db, t.ID, req.Bucket, req.Object); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
			}
			return
		}
		if err != nil {
			s.logger.Error("query object retention", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		resp.Mode = mode
		if retainUntil.Valid {
			resp.RetainUntilDate = retainUntil.Time.UTC().Format(time.RFC3339)
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutObjectRetention(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config RetentionConfig
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if config.Mode != "GOVERNANCE" && config.Mode != "COMPLIANCE" {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	retainUntil, err := time.Parse(time.RFC3339, config.RetainUntilDate)
	if err != nil {
		WriteS3Error(w, ErrInvalidRetentionPeriod, r.URL.Path, generateRequestID())
		return
	}

	if config.Mode == "COMPLIANCE" {
		var existingMode string
		var existingUntil sql.NullTime
		err := s.db.QueryRowContext(r.Context(),
			`SELECT retention_mode, retain_until_date FROM object_locks
			 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, req.Bucket, req.Object).Scan(&existingMode, &existingUntil)
		if err == nil && existingMode == "COMPLIANCE" && existingUntil.Valid {
			if retainUntil.Before(existingUntil.Time) {
				WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
				return
			}
		}
	}

	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			retention_mode = EXCLUDED.retention_mode,
			retain_until_date = EXCLUDED.retain_until_date,
			updated_at = NOW()
	`, t.ID, req.Bucket, req.Object, config.Mode, retainUntil)
	if err != nil {
		s.logger.Error("upsert object retention", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("object retention set",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.String("object", req.Object),
		zap.String("mode", config.Mode),
		zap.Time("retain_until", retainUntil))

	w.WriteHeader(http.StatusOK)
}

// --- Per-object legal hold ---

func (s *Server) handleGetObjectLegalHold(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	status := "OFF"
	if s.db != nil {
		var hold bool
		err := s.db.QueryRowContext(r.Context(),
			`SELECT legal_hold FROM object_locks
			 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, req.Bucket, req.Object).Scan(&hold)
		if err == sql.ErrNoRows {
			// No lock row — legal hold is OFF
		} else if err != nil {
			s.logger.Error("query legal hold", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		} else if hold {
			status = "ON"
		}
	}

	resp := LegalHoldConfig{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		Status: status,
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutObjectLegalHold(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config LegalHoldConfig
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if config.Status != "ON" && config.Status != "OFF" {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	hold := config.Status == "ON"

	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO object_locks (tenant_id, bucket, object_key, legal_hold, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			legal_hold = EXCLUDED.legal_hold,
			updated_at = NOW()
	`, t.ID, req.Bucket, req.Object, hold)
	if err != nil {
		s.logger.Error("upsert legal hold", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("legal hold updated",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.String("object", req.Object),
		zap.String("status", config.Status))

	w.WriteHeader(http.StatusOK)
}

// --- Lock enforcement ---

// checkObjectLock checks whether an object is protected by Object Lock.
// Returns nil if the operation is allowed, or an error string if blocked.
func checkObjectLock(ctx context.Context, db *sql.DB, tenantID, bucket, key string, bypassGovernance bool) error {
	if db == nil {
		return nil
	}

	var mode string
	var retainUntil sql.NullTime
	var hold bool
	err := db.QueryRowContext(ctx,
		`SELECT retention_mode, retain_until_date, legal_hold FROM object_locks
		 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&mode, &retainUntil, &hold)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	if hold {
		return errObjectLocked
	}

	if retainUntil.Valid && retainUntil.Time.After(time.Now()) {
		switch mode {
		case "COMPLIANCE":
			return errObjectLocked
		case "GOVERNANCE":
			if !bypassGovernance {
				return errObjectLocked
			}
		}
	}

	return nil
}

var errObjectLocked = &objectLockedError{}

type objectLockedError struct{}

func (e *objectLockedError) Error() string {
	return "Object is protected by Object Lock"
}

// applyObjectLockOnPut applies lock headers or bucket defaults after a successful PUT.
func applyObjectLockOnPut(ctx context.Context, db *sql.DB, tenantID, bucket, key string, r *http.Request) {
	if db == nil {
		return
	}

	lockMode := r.Header.Get("x-amz-object-lock-mode")
	lockUntil := r.Header.Get("x-amz-object-lock-retain-until-date")

	if lockMode != "" && lockUntil != "" {
		if lockMode != "GOVERNANCE" && lockMode != "COMPLIANCE" {
			return
		}
		retainUntil, err := time.Parse(time.RFC3339, lockUntil)
		if err != nil {
			return
		}
		_, _ = db.ExecContext(ctx, `
			INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				retention_mode = EXCLUDED.retention_mode,
				retain_until_date = EXCLUDED.retain_until_date,
				updated_at = NOW()
		`, tenantID, bucket, key, lockMode, retainUntil)
		return
	}

	var defaultMode string
	var defaultDays int
	err := db.QueryRowContext(ctx,
		`SELECT default_retention_mode, default_retention_days FROM buckets
		 WHERE tenant_id = $1 AND name = $2 AND object_lock_enabled = TRUE`,
		tenantID, bucket).Scan(&defaultMode, &defaultDays)
	if err != nil || defaultMode == "" || defaultDays <= 0 {
		return
	}

	retainUntil := time.Now().Add(time.Duration(defaultDays) * 24 * time.Hour)
	_, _ = db.ExecContext(ctx, `
		INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			retention_mode = EXCLUDED.retention_mode,
			retain_until_date = EXCLUDED.retain_until_date,
			updated_at = NOW()
	`, tenantID, bucket, key, defaultMode, retainUntil)
}

// isObjectLockBypass returns true if the bypass-governance-retention header is set.
func isObjectLockBypass(r *http.Request) bool {
	v := r.Header.Get("x-amz-bypass-governance-retention")
	b, _ := strconv.ParseBool(v)
	return b
}
