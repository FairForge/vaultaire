package api

import (
	"database/sql"
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// restoreRequestXML is the S3 RestoreObject request body. GlacierJobParameters
// (Tier) is accepted for wire compatibility but ignored — Vail has one recall
// speed. rclone/aws-cli send: <RestoreRequest><Days>N</Days>...</RestoreRequest>
type restoreRequestXML struct {
	XMLName xml.Name `xml:"RestoreRequest"`
	Days    int32    `xml:"Days"`
}

// objectRestorer resolves the Restorer driver holding an object, or nil when
// the object's backend doesn't support restore (not archive-class).
func objectRestorer(ce *engine.CoreEngine, db *sql.DB, r *http.Request, tenantID, bucket, object string) (engine.Restorer, string, error) {
	var backendName string
	err := db.QueryRowContext(r.Context(), `
		SELECT COALESCE(backend_name, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, object).Scan(&backendName)
	if err != nil {
		return nil, "", err
	}
	if ce == nil {
		return nil, backendName, nil
	}
	drv, ok := ce.GetDriver(backendName)
	if !ok {
		return nil, backendName, nil
	}
	restorer, _ := drv.(engine.Restorer)
	return restorer, backendName, nil
}

// handleRestoreObject implements POST /{bucket}/{key}?restore (V18.2 minimum
// recall slice) with AWS Glacier wire semantics: 202 Accepted on a new recall,
// 409 RestoreAlreadyInProgress when one is running, 403 InvalidObjectState
// for objects whose backend has no restore concept.
func (s *Server) handleRestoreObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if s.db == nil {
		WriteS3Error(w, ErrNotImplemented, r.URL.Path, generateRequestID())
		return
	}

	restorer, backendName, lookupErr := objectRestorer(s.engine, s.db, r, t.ID, req.Bucket, req.Object)
	if lookupErr == sql.ErrNoRows {
		reqID := generateRequestID()
		if suggestion := keySuggestion(r.Context(), s.db, t.ID, req.Bucket, req.Object); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
		}
		return
	}
	if lookupErr != nil {
		s.logger.Error("restore: head-cache lookup failed", zap.Error(lookupErr))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if restorer == nil {
		// The object lives on a hot backend — restore is meaningless there.
		// AWS answers the same way for a RestoreObject on a STANDARD object.
		WriteS3ErrorWithContext(w, ErrInvalidObjectState, r.URL.Path, generateRequestID(),
			WithSuggestion("Only archive-tier (GLACIER) objects support restore. This object is on hot storage ("+engine.BackendToStorageClass(backendName)+") and is directly readable."))
		return
	}

	// Days: default 1, honor the body when present. Body may legitimately be
	// empty (aws-cli allows --restore-request omission for some flows).
	days := int32(1)
	if body, readErr := io.ReadAll(io.LimitReader(r.Body, 64*1024)); readErr == nil && len(body) > 0 {
		var restoreReq restoreRequestXML
		if xmlErr := xml.Unmarshal(body, &restoreReq); xmlErr != nil {
			WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
			return
		}
		if restoreReq.Days > 0 {
			days = restoreReq.Days
		}
	}

	if restoreErr := restorer.RestoreObject(r.Context(), t.NamespaceContainer(req.Bucket), req.Object, days); restoreErr != nil {
		switch {
		case errors.Is(restoreErr, engine.ErrRestoreAlreadyInProgress):
			WriteS3Error(w, ErrRestoreAlreadyInProgress, r.URL.Path, generateRequestID())
		case errors.Is(restoreErr, engine.ErrArchived):
			// Vail answers InvalidObjectState here too — but for RestoreObject
			// it means the OPPOSITE state: the object is fresh on the staging
			// disk and directly readable, so there is nothing to recall
			// (found live 2026-08-04; used to surface as a 500). AWS parity:
			// 403 InvalidObjectState, same as restoring a STANDARD object.
			WriteS3ErrorWithContext(w, ErrInvalidObjectState, r.URL.Path, generateRequestID(),
				WithSuggestion("This object is directly readable right now — no restore needed. Objects only need a restore after they migrate to tape (~13 days after upload)."))
		case isObjectMissingErr(restoreErr):
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		default:
			s.logger.Error("restore request failed",
				zap.Error(restoreErr),
				zap.String("bucket", req.Bucket),
				zap.String("key", req.Object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}

	s.logger.Info("object restore accepted",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.String("key", req.Object),
		zap.Int32("days", days))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusAccepted)
}
