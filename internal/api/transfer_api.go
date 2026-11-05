package api

// WP-XFER-1's management routes (docs/API.md § Transfers):
//
//	POST   /api/v1/manage/transfers             create (owner/admin): the source is
//	                                            checked with ONE signed ListObjectsV2
//	                                            page before the row is written
//	GET    /api/v1/manage/transfers             the tenant's jobs, newest first
//	GET    /api/v1/manage/transfers/{id}        one job's progress
//	DELETE /api/v1/manage/transfers/{id}        cancel: the key nulled, nothing written after
//	POST   /api/v1/manage/transfers/{id}/resume a paused job runs again
//
// No answer ever carries the source key, its id included.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/usage"
)

func (s *Server) registerTransferRoutes(r chi.Router) {
	r.Get("/transfers", s.handleMgmtListTransfers)
	r.Get("/transfers/{id}", s.handleMgmtGetTransfer)
	r.With(s.requireRole(auth.ActionTransfer)).Post("/transfers", s.handleMgmtCreateTransfer)
	r.With(s.requireRole(auth.ActionTransfer)).Delete("/transfers/{id}", s.handleMgmtCancelTransfer)
	r.With(s.requireRole(auth.ActionTransfer)).Post("/transfers/{id}/resume", s.handleMgmtResumeTransfer)
}

// transferCreate is the create body.
type transferCreate struct {
	Source struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		Prefix    string `json:"prefix"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	} `json:"source"`
	Bucket          string `json:"bucket"` // the destination, the caller's tenant's
	RateBytesPerSec int64  `json:"rate_bytes_per_sec"`
}

// transferView is a job as the API shows it — never a key.
type transferView struct {
	ID          string     `json:"id"`
	Bucket      string     `json:"bucket"`
	Source      sourceView `json:"source"`
	State       string     `json:"state"`
	Paused      string     `json:"paused_reason,omitempty"`
	PausedCode  string     `json:"paused_code,omitempty"`
	Listed      int64      `json:"listed"`
	Copied      int64      `json:"copied"`
	Skipped     int64      `json:"skipped"`
	Failed      int64      `json:"failed"`
	Bytes       int64      `json:"bytes"`
	Cursor      string     `json:"cursor"`
	ListedAll   bool       `json:"listed_all"`
	Rate        int64      `json:"rate_bytes_per_sec"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	KeyRetained bool       `json:"source_key_held"`
}

type sourceView struct {
	Endpoint string `json:"endpoint"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix,omitempty"`
}

func viewOf(t *transferRow) transferView {
	return transferView{ID: t.ID, Bucket: t.Bucket, Source: sourceView{t.Endpoint, t.Region, t.SrcBucket, t.Prefix},
		State: t.State, Paused: t.PausedReason, PausedCode: t.PausedCode, Listed: t.Listed, Copied: t.Copied, Skipped: t.Skipped,
		Failed: t.Failed, Bytes: t.Bytes, Cursor: t.Cursor, ListedAll: t.ListedAll, Rate: t.Rate, LastError: t.LastError,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt, KeyRetained: t.accessKey.Valid}
}

func (s *Server) handleMgmtCreateTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	if s.db == nil || tenantID == "" {
		writeManagementError(w, ErrTypeUnavailable, "unavailable", "no database", "")
		return
	}
	var in transferCreate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "the body must be a JSON transfer", "")
		return
	}
	view, status, errType, code, msg, param := s.createTransfer(r.Context(), tenantID, actorOf(r), in)
	if errType != "" {
		if status == http.StatusNotImplemented {
			writeJSON(w, status, managementError{Error: managementErrorBody{Type: ErrTypeInvalidRequest, Code: code, Message: msg, Param: param, RequestID: getRequestID(w)}})
			return
		}
		writeManagementError(w, errType, code, msg, param)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func actorOf(r *http.Request) string {
	a, _ := r.Context().Value(userIDKey).(string)
	return a
}

// createTransfer validates, checks the source with one signed listing page,
// and writes the row with the key sealed.
func (s *Server) createTransfer(ctx context.Context, tenantID, actor string, in transferCreate) (view transferView, status int, errType, code, msg, param string) {
	bad := func(code, msg, param string) (transferView, int, string, string, string, string) {
		return transferView{}, http.StatusBadRequest, ErrTypeInvalidRequest, code, msg, param
	}
	src := in.Source
	endpoint, err := validTransferEndpoint(src.Endpoint)
	if err != nil {
		return bad("invalid_source_endpoint", err.Error(), "source.endpoint")
	}
	switch {
	case strings.TrimSpace(src.Bucket) == "":
		return bad("missing_source_bucket", "name the source bucket", "source.bucket")
	case src.AccessKey == "" || src.SecretKey == "":
		return bad("missing_source_key", "the source needs an access key and its secret (a read-only key)", "source.access_key")
	case len(src.Prefix) > 1024 || len(src.Region) > 64 || len(src.AccessKey) > 256 || len(src.SecretKey) > 512:
		return bad("invalid_source", "a source field is too long", "source")
	}
	rateCap := s.transfers().cfg.rateMax
	rateBPS := in.RateBytesPerSec
	switch {
	case rateBPS == 0:
		rateBPS = rateCap
	case rateBPS < 1<<20 || rateBPS > rateCap:
		return bad("invalid_rate", fmt.Sprintf("rate_bytes_per_sec is 1 MiB/s to %d", rateCap), "rate_bytes_per_sec")
	}
	// The destination: the caller's own bucket, never Private (the job holds
	// no customer key), never the vault floor (D-XFER-2).
	var ssecRequired bool
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(ssec_required, false) FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, in.Bucket).Scan(&ssecRequired)
	if errors.Is(err, sql.ErrNoRows) {
		return transferView{}, http.StatusNotFound, ErrTypeNotFound, "bucket_not_found", "no such bucket of yours", "bucket"
	}
	if err != nil {
		return transferView{}, http.StatusInternalServerError, ErrTypeAPI, "internal_error", "the bucket could not be read", ""
	}
	if ssecRequired {
		return transferView{}, http.StatusConflict, ErrTypeConflict, "destination_private",
			"a Private bucket takes only writes with your own key, which a transfer does not hold: transfer into a Standard bucket", "bucket"
	}
	class := resolvePutStorageClass(ctx, s.db, s.engine, tierPlacementGate(s.flags), tenantID, in.Bucket, "")
	if usage.FloorOf(class) == usage.FloorVault {
		return transferView{}, http.StatusNotImplemented, ErrTypeInvalidRequest, "NotImplemented",
			"a transfer into a Vault bucket is not supported yet (D-XFER-2): transfer into a Standard bucket", "bucket"
	}
	// One signed listing page: the source answers, and to this key.
	srcClient := newTransferSource(endpoint, src.Region, src.Bucket, src.Prefix, src.AccessKey, src.SecretKey)
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	_, _, lerr := srcClient.list(lctx, "", 1)
	cancel()
	if lerr != nil {
		class, why := classifySourceError(lerr)
		if class == sourceDenied {
			return bad("source_denied", "the source refused this key: "+why, "source.access_key")
		}
		return bad("source_unreachable", "the source did not answer a listing: "+why, "source.endpoint")
	}
	id := uuid.NewString()
	plain, wrapped, version, err := s.sealTransferSecret(ctx, tenantID, id, src.SecretKey)
	if err != nil {
		return transferView{}, http.StatusInternalServerError, ErrTypeAPI, "internal_error", "the source key could not be sealed", ""
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO transfer_jobs (id, tenant_id, bucket, source_endpoint, source_region, source_bucket, source_prefix,
		access_key_id, secret_key, secret_wrapped, secret_key_version, rate_bytes_per_sec, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		id, tenantID, in.Bucket, endpoint, src.Region, src.Bucket, src.Prefix, src.AccessKey, plain, wrapped, version, rateBPS, actor); err != nil {
		return transferView{}, http.StatusInternalServerError, ErrTypeAPI, "internal_error", "the transfer could not be written", ""
	}
	audit.Record(ctx, s.db, audit.Entry{TenantID: tenantID, UserID: actor, Action: "transfer.created", Resource: "transfer:" + id,
		Metadata: map[string]any{"bucket": in.Bucket, "source_endpoint": endpoint, "source_bucket": src.Bucket, "source_prefix": src.Prefix, "wrapped": len(wrapped) > 0}})
	t, err := s.loadTransfer(ctx, tenantID, id)
	if err != nil {
		return transferView{}, http.StatusInternalServerError, ErrTypeAPI, "internal_error", "the transfer could not be read back", ""
	}
	return viewOf(t), http.StatusCreated, "", "", "", ""
}

func (s *Server) loadTransfer(ctx context.Context, tenantID, id string) (*transferRow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, sql.ErrNoRows
	}
	return scanTransfer(s.db.QueryRowContext(ctx, `SELECT `+transferColumns+` FROM transfer_jobs WHERE id = $1 AND tenant_id = $2`, id, tenantID))
}

func (s *Server) handleMgmtListTransfers(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	if s.db == nil || tenantID == "" {
		writeManagementError(w, ErrTypeUnavailable, "unavailable", "no database", "")
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+transferColumns+` FROM transfer_jobs WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 100`, tenantID)
	if err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "the transfers could not be read", "")
		return
	}
	defer func() { _ = rows.Close() }()
	out := []transferView{}
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			writeManagementError(w, ErrTypeAPI, "internal_error", "the transfers could not be read", "")
			return
		}
		out = append(out, viewOf(t))
	}
	if err := rows.Err(); err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "the transfers could not be read", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": out})
}

func (s *Server) transferOr404(w http.ResponseWriter, r *http.Request) (*transferRow, bool) {
	tenantID := activeTenant(r)
	if s.db == nil || tenantID == "" {
		writeManagementError(w, ErrTypeUnavailable, "unavailable", "no database", "")
		return nil, false
	}
	t, err := s.loadTransfer(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "transfer_not_found", "no such transfer", "id")
		return nil, false
	}
	return t, true
}

func (s *Server) handleMgmtGetTransfer(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.transferOr404(w, r); ok {
		writeJSON(w, http.StatusOK, viewOf(t))
	}
}

func (s *Server) handleMgmtCancelTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferOr404(w, r)
	if !ok {
		return
	}
	if _, err := s.db.ExecContext(r.Context(), transferEndSQL, t.ID, transferCancelled, ""); err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "the transfer could not be cancelled", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: t.TenantID, UserID: actorOf(r), Action: "transfer.cancelled", Resource: "transfer:" + t.ID})
	t, err := s.loadTransfer(r.Context(), t.TenantID, t.ID)
	if err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "the transfer could not be read back", "")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(t))
}

func (s *Server) handleMgmtResumeTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferOr404(w, r)
	if !ok {
		return
	}
	if t.State != transferPaused {
		writeManagementError(w, ErrTypeConflict, "transfer_not_paused", "only a paused transfer resumes (this one is "+t.State+")", "id")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `UPDATE transfer_jobs SET state = 'running', paused_reason = '', paused_error = '', updated_at = NOW()
		WHERE id = $1 AND state = 'paused'`, t.ID); err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "the transfer could not be resumed", "")
		return
	}
	t, err := s.loadTransfer(r.Context(), t.TenantID, t.ID)
	if err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "the transfer could not be read back", "")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(t))
}

// transferDashboard is the dashboard's door to the same logic
// (handlers.TransferService): create, cancel and resume exactly as the
// management API does.
type transferDashboard struct{ s *Server }

func (d transferDashboard) CreateTransfer(ctx context.Context, tenantID, actor string, f handlers.TransferForm, secret string) (string, string, error) {
	var in transferCreate
	in.Source.Endpoint, in.Source.Region, in.Source.Bucket, in.Source.Prefix = f.Endpoint, f.Region, f.SourceBucket, f.Prefix
	in.Source.AccessKey, in.Source.SecretKey, in.Bucket = f.AccessKey, secret, f.Bucket
	v, status, errType, _, msg, _ := d.s.createTransfer(ctx, tenantID, actor, in)
	switch {
	case errType == "":
		return v.ID, "", nil
	case status >= 500 && status != http.StatusNotImplemented:
		return "", "", fmt.Errorf("create a transfer: %s", msg)
	}
	return "", msg, nil
}

func (d transferDashboard) CancelTransfer(ctx context.Context, tenantID, actor, id string) (bool, error) {
	t, err := d.s.loadTransfer(ctx, tenantID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := d.s.db.ExecContext(ctx, transferEndSQL, t.ID, transferCancelled, "")
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		audit.Record(ctx, d.s.db, audit.Entry{TenantID: tenantID, UserID: actor, Action: "transfer.cancelled", Resource: "transfer:" + t.ID})
	}
	return n > 0, nil
}

func (d transferDashboard) ResumeTransfer(ctx context.Context, tenantID, _ string, id string) (bool, error) {
	t, err := d.s.loadTransfer(ctx, tenantID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := d.s.db.ExecContext(ctx, `UPDATE transfer_jobs SET state = 'running', paused_reason = '', paused_error = '', updated_at = NOW()
		WHERE id = $1 AND state = 'paused'`, t.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
