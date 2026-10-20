package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const maxVersioningBodyBytes = 4096

type VersioningConfiguration struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	Xmlns     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MfaDelete string   `xml:"MfaDelete,omitempty"`
}

func (s *Server) handleGetBucketVersioning(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	status := "disabled"
	var mfaDeleteEnabled bool
	if s.db != nil {
		var dbStatus string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT versioning_status, mfa_delete_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket).Scan(&dbStatus, &mfaDeleteEnabled)
		if errors.Is(err, sql.ErrNoRows) {
			reqID := generateRequestID()
			if suggestion := bucketSuggestion(r.Context(), s.db, t.ID, req.Bucket); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
			}
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
	if mfaDeleteEnabled {
		resp.MfaDelete = "Enabled"
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
		WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
		return
	}

	var config VersioningConfiguration
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if config.Status != "Enabled" && config.Status != "Suspended" && config.Status != "" {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	// MfaDelete is validated like Status, and before the MFA check (RP19-9):
	// `<MfaDelete>Garbage</MfaDelete>` passed the `!= ""` gate, consumed a
	// valid code and wrote mfa_delete_enabled = false. AWS's two values
	// (PutBucketVersioning API reference: Enabled | Disabled).
	if config.MfaDelete != "Enabled" && config.MfaDelete != "Disabled" && config.MfaDelete != "" {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if config.Status == "" && config.MfaDelete == "" {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// The bucket's state (WP-R4-2, R4-10). A read that fails is a 503 +
	// Retry-After, never a guess (RP16-5): whether the request needs a code
	// depends on it.
	var lockEnabled, mfaDelete bool
	var versioning string
	err = s.db.QueryRowContext(r.Context(),
		`SELECT object_lock_enabled, mfa_delete_enabled, versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket).Scan(&lockEnabled, &mfaDelete, &versioning)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeNoSuchBucket(w, r, t.ID, req.Bucket)
		return
	}
	if err != nil {
		unknown := &mfaDeleteUnknownError{err: fmt.Errorf("read bucket state of %s: %w", req.Bucket, err)}
		s.logMFADeleteUnknown(unknown, req.Bucket)
		writeMFADeleteRefusal(w, r, unknown)
		return
	}

	// The second factor first: enabling or disabling MFA Delete carries
	// x-amz-mfa (AWS: both), and so does a change of the versioning state
	// while MFA Delete is on. A refusal is 403 before any state rule.
	if config.MfaDelete != "" || (config.Status != "" && mfaDelete) {
		if err := validateMFAHeader(r.Context(), s.auth, s.mfaService, t.ID, r); err != nil {
			// A read of the user's MFA state that failed is 503 +
			// Retry-After (RP19-10), the refusals 403.
			s.logMFADeleteUnknown(err, req.Bucket)
			writeMFADeleteRefusal(w, r, err)
			return
		}
	}

	// Then the state rules, each AWS's 409 InvalidBucketState:
	//   - versioning cannot be suspended on a bucket with Object Lock;
	//   - MFA Delete is gated on versioning Enabled — the request's Status
	//     when it carries one ("you must include Status whenever you use
	//     MfaDelete"), else the bucket's — never on the lock (R4-10: it was
	//     gated on object_lock_enabled).
	if config.Status == "Suspended" && lockEnabled {
		WriteS3ErrorWithContext(w, ErrInvalidBucketState, r.URL.Path, generateRequestID(), WithMessage(lockSuspendRefusal))
		return
	}
	effective := versioning
	if config.Status != "" {
		effective = config.Status
	}
	if config.MfaDelete == "Enabled" && effective != "Enabled" {
		WriteS3ErrorWithContext(w, ErrInvalidBucketState, r.URL.Path, generateRequestID(), WithMessage(mfaDeleteNeedsVersioningRefusal))
		return
	}

	// One write for both elements.
	var newMFA sql.NullBool
	if config.MfaDelete != "" {
		newMFA = sql.NullBool{Bool: config.MfaDelete == "Enabled", Valid: true}
	}
	var newStatus sql.NullString
	if config.Status != "" {
		newStatus = sql.NullString{String: config.Status, Valid: true}
	}
	_, err = s.db.ExecContext(r.Context(),
		`UPDATE buckets SET mfa_delete_enabled = COALESCE($1, mfa_delete_enabled),
		        versioning_status = COALESCE($2, versioning_status), updated_at = NOW()
		 WHERE tenant_id = $3 AND name = $4`,
		newMFA, newStatus, t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("update versioning configuration", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if config.Status != "" {
		s.logger.Info("versioning status updated",
			zap.String("tenant_id", t.ID),
			zap.String("bucket", req.Bucket),
			zap.String("status", config.Status))
	}

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
	return bucketVersioningStatus(ctx, db, tenantID, bucket)
}

// bucketVersioningStatus reads the status through q: inside a transaction,
// on the transaction's own connection (Prompt 2b 0.3: read through the pool
// while a transaction held a connection, a pool of N froze at N concurrent
// Complete retries).
func bucketVersioningStatus(ctx context.Context, q rowQuerier, tenantID, bucket string) string {
	var status string
	err := q.QueryRowContext(ctx,
		`SELECT versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&status)
	if err != nil {
		return "disabled"
	}
	return status
}
