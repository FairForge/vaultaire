package handlers

// WP-XFER-1 PR B: the Transfers page — the list with progress, start (the
// source's fields; the secret a password input, never echoed back), cancel,
// resume. The writes go through the API's own transfer logic (Transfers,
// injected by internal/api — the dashboard does not import it), so the
// dashboard and POST /api/v1/manage/transfers check and store exactly the
// same. Every write is auth.ActionTransfer (owner, admin); everyone reads.

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
)

// TransferForm is the start form — the secret is not kept on it.
type TransferForm struct {
	Endpoint, Region, SourceBucket, Prefix, AccessKey, Bucket string
}

// TransferService is the API's transfer logic.
type TransferService interface {
	// CreateTransfer checks the source and writes the job; problem is the
	// sentence the page shows for a refusal ("" = created).
	CreateTransfer(ctx context.Context, tenantID, actor string, f TransferForm, secret string) (id, problem string, err error)
	// CancelTransfer / ResumeTransfer act on the tenant's own job; false =
	// no such job, or not in a state that allows it.
	CancelTransfer(ctx context.Context, tenantID, actor, id string) (bool, error)
	ResumeTransfer(ctx context.Context, tenantID, actor, id string) (bool, error)
}

// TransferDeps are the page's dependencies.
type TransferDeps struct {
	DB      *sql.DB
	Service TransferService // nil = the page is 404
	Logger  *zap.Logger
}

type transferRowView struct {
	ID, Bucket, SourceEndpoint, SourceBucket, SourcePrefix string
	State, PausedReason, PausedError, LastError            string
	Listed, Copied, Skipped, Failed, Bytes                 int64
	ListedAll                                              bool
	CreatedAt                                              time.Time
}

// PausedText is the paused reason in words.
func (t transferRowView) PausedText() string {
	switch t.PausedReason {
	case "quota":
		return "your storage is full: make room or buy more, then resume"
	case "source_denied":
		return "the source refused the key: check it, then resume"
	}
	return t.PausedReason
}

// BytesText is the bytes copied, humanly.
func (t transferRowView) BytesText() string { return formatBytes(t.Bytes) }

// HandleTransfersPage is GET and POST /dashboard/transfers.
func HandleTransfersPage(tmpl *template.Template, d TransferDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || d.DB == nil || d.Service == nil {
			http.NotFound(w, r)
			return
		}
		data := sessionData(sd, "transfers")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		data["CanManage"] = auth.Can(sessionRole(sd), auth.ActionTransfer)
		form := TransferForm{}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			form = TransferForm{Endpoint: strings.TrimSpace(r.FormValue("endpoint")), Region: strings.TrimSpace(r.FormValue("region")),
				SourceBucket: strings.TrimSpace(r.FormValue("source_bucket")), Prefix: r.FormValue("prefix"),
				AccessKey: strings.TrimSpace(r.FormValue("access_key")), Bucket: r.FormValue("bucket")}
			_, problem, err := d.Service.CreateTransfer(r.Context(), sd.TenantID, sd.UserID, form, r.FormValue("secret_key"))
			switch {
			case err != nil:
				d.Logger.Error("transfers page: create", zap.Error(err))
				data["Error"], status = "The transfer could not be started. Try again.", http.StatusInternalServerError
			case problem != "":
				data["Error"], status = problem, http.StatusBadRequest
			default:
				middleware.SetFlash(w, "success", "The transfer is started: it begins within a minute.")
				http.Redirect(w, r, "/dashboard/transfers", http.StatusSeeOther)
				return
			}
		}
		data["Form"] = form
		list, err := listTransfers(r.Context(), d.DB, sd.TenantID)
		if err != nil {
			d.Logger.Error("transfers page: list", zap.Error(err))
		}
		data["Transfers"] = list
		buckets, err := transferBuckets(r.Context(), d.DB, sd.TenantID)
		if err != nil {
			d.Logger.Error("transfers page: buckets", zap.Error(err))
		}
		data["Buckets"] = buckets
		renderPage(w, tmpl, data, status, d.Logger)
	}
}

// HandleTransferCancel is POST /dashboard/transfers/{id}/cancel.
func HandleTransferCancel(d TransferDeps) http.HandlerFunc {
	return transferAct(d, "cancel", func(ctx context.Context, sd *dashauth.SessionData, id string) (bool, error) {
		return d.Service.CancelTransfer(ctx, sd.TenantID, sd.UserID, id)
	}, "The transfer is cancelled; its key is deleted.")
}

// HandleTransferResume is POST /dashboard/transfers/{id}/resume.
func HandleTransferResume(d TransferDeps) http.HandlerFunc {
	return transferAct(d, "resume", func(ctx context.Context, sd *dashauth.SessionData, id string) (bool, error) {
		return d.Service.ResumeTransfer(ctx, sd.TenantID, sd.UserID, id)
	}, "The transfer resumes within a minute.")
}

func transferAct(d TransferDeps, what string, act func(context.Context, *dashauth.SessionData, string) (bool, error), done string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || d.Service == nil {
			http.NotFound(w, r)
			return
		}
		ok, err := act(r.Context(), sd, chi.URLParam(r, "id"))
		switch {
		case err != nil:
			d.Logger.Error("transfers page: "+what, zap.Error(err))
			middleware.SetFlash(w, "error", "That did not work. Try again.")
		case !ok:
			middleware.SetFlash(w, "error", "No such transfer in a state that allows it.")
		default:
			middleware.SetFlash(w, "success", done)
		}
		http.Redirect(w, r, "/dashboard/transfers", http.StatusSeeOther)
	}
}

func listTransfers(ctx context.Context, db *sql.DB, tenantID string) ([]transferRowView, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, bucket, source_endpoint, source_bucket, source_prefix, state, paused_reason, paused_error,
		last_error, listed, copied, skipped, failed, bytes_copied, listed_all, created_at
		  FROM transfer_jobs WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 50`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []transferRowView
	for rows.Next() {
		var t transferRowView
		if err := rows.Scan(&t.ID, &t.Bucket, &t.SourceEndpoint, &t.SourceBucket, &t.SourcePrefix, &t.State, &t.PausedReason, &t.PausedError,
			&t.LastError, &t.Listed, &t.Copied, &t.Skipped, &t.Failed, &t.Bytes, &t.ListedAll, &t.CreatedAt); err != nil {
			return out, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func transferBuckets(ctx context.Context, db *sql.DB, tenantID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM buckets WHERE tenant_id = $1 AND name NOT LIKE '\_%' AND NOT COALESCE(ssec_required, false)
		ORDER BY name LIMIT 500`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
