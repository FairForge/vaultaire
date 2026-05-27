package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

func HandleExportData(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if db == nil {
			http.Error(w, "Database unavailable", http.StatusInternalServerError)
			return
		}

		data := collectExportData(r, db, sd.UserID, sd.TenantID, logger)

		filename := fmt.Sprintf("stored-ge-export-%s.json", time.Now().Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		_, _ = w.Write(data)
	}
}

func HandleRequestDeletion(db *sql.DB, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if db == nil {
			middleware.SetFlash(w, "error", "Database unavailable.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		password := r.FormValue("password")
		if password == "" {
			middleware.SetFlash(w, "error", "Password is required to delete your account.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		var passwordHash string
		err := db.QueryRowContext(r.Context(),
			`SELECT password_hash FROM users WHERE id = $1`, sd.UserID).Scan(&passwordHash)
		if err != nil {
			logger.Error("fetch password hash for deletion", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to verify password.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
			middleware.SetFlash(w, "error", "Incorrect password.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		reason := r.FormValue("reason")
		if reason == "" {
			reason = "User requested deletion"
		}

		scheduledAt := time.Now().Add(30 * 24 * time.Hour)
		_, err = db.ExecContext(r.Context(),
			`UPDATE users SET deletion_scheduled_at = $1, deletion_reason = $2, status = 'pending_deletion' WHERE id = $3 AND deletion_scheduled_at IS NULL`,
			scheduledAt, reason, sd.UserID)
		if err != nil {
			logger.Error("schedule account deletion", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to schedule account deletion.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success",
			fmt.Sprintf("Account scheduled for deletion on %s. You can cancel anytime before then.", scheduledAt.Format("January 2, 2006")))
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

func HandleCancelDeletion(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if db == nil {
			middleware.SetFlash(w, "error", "Database unavailable.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		_, err := db.ExecContext(r.Context(),
			`UPDATE users SET deletion_scheduled_at = NULL, deletion_reason = NULL, status = 'active' WHERE id = $1`,
			sd.UserID)
		if err != nil {
			logger.Error("cancel account deletion", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to cancel deletion.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success", "Account deletion has been cancelled.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

func collectExportData(r *http.Request, db *sql.DB, userID, tenantID string, logger *zap.Logger) []byte {
	data := map[string]interface{}{
		"exported_at": time.Now().UTC(),
		"format":      "json",
	}

	var email, company, role, status sql.NullString
	var createdAt sql.NullTime
	err := db.QueryRowContext(r.Context(),
		`SELECT email, company, role, status, created_at FROM users WHERE id = $1`, userID).
		Scan(&email, &company, &role, &status, &createdAt)
	if err == nil {
		u := map[string]interface{}{"id": userID}
		if email.Valid {
			u["email"] = email.String
		}
		if company.Valid {
			u["company"] = company.String
		}
		if role.Valid {
			u["role"] = role.String
		}
		if status.Valid {
			u["status"] = status.String
		}
		if createdAt.Valid {
			u["created_at"] = createdAt.Time
		}
		data["user"] = u
	}

	var tName, tPlan sql.NullString
	err = db.QueryRowContext(r.Context(),
		`SELECT name, plan FROM tenants WHERE id = $1`, tenantID).Scan(&tName, &tPlan)
	if err == nil {
		t := map[string]interface{}{"id": tenantID}
		if tName.Valid {
			t["name"] = tName.String
		}
		if tPlan.Valid {
			t["plan"] = tPlan.String
		}
		data["tenant"] = t
	}

	var storageUsed, storageLimit sql.NullInt64
	var tier sql.NullString
	err = db.QueryRowContext(r.Context(),
		`SELECT storage_used_bytes, storage_limit_bytes, tier FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&storageUsed, &storageLimit, &tier)
	if err == nil {
		q := map[string]interface{}{}
		if storageUsed.Valid {
			q["storage_used_bytes"] = storageUsed.Int64
		}
		if storageLimit.Valid {
			q["storage_limit_bytes"] = storageLimit.Int64
		}
		if tier.Valid {
			q["tier"] = tier.String
		}
		data["quota"] = q
	}

	bucketRows, err := db.QueryContext(r.Context(),
		`SELECT name, visibility, created_at FROM buckets WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err == nil {
		defer func() { _ = bucketRows.Close() }()
		var buckets []map[string]interface{}
		for bucketRows.Next() {
			var bName, bVis string
			var bCreated time.Time
			if bucketRows.Scan(&bName, &bVis, &bCreated) == nil {
				buckets = append(buckets, map[string]interface{}{
					"name": bName, "visibility": bVis, "created_at": bCreated,
				})
			}
		}
		data["buckets"] = buckets
	}

	objRows, err := db.QueryContext(r.Context(),
		`SELECT bucket, object_key, size, content_type, last_modified FROM object_head_cache WHERE tenant_id = $1 ORDER BY bucket, object_key`, tenantID)
	if err == nil {
		defer func() { _ = objRows.Close() }()
		var objects []map[string]interface{}
		for objRows.Next() {
			var oBucket, oKey, oCT string
			var oSize int64
			var oMod time.Time
			if objRows.Scan(&oBucket, &oKey, &oSize, &oCT, &oMod) == nil {
				objects = append(objects, map[string]interface{}{
					"bucket": oBucket, "key": oKey, "size": oSize,
					"content_type": oCT, "last_modified": oMod,
				})
			}
		}
		data["objects"] = objects
	}

	keyRows, err := db.QueryContext(r.Context(),
		`SELECT id, name, created_at FROM api_keys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err == nil {
		defer func() { _ = keyRows.Close() }()
		var keys []map[string]interface{}
		for keyRows.Next() {
			var kID, kName string
			var kCreated time.Time
			if keyRows.Scan(&kID, &kName, &kCreated) == nil {
				keys = append(keys, map[string]interface{}{
					"id": kID, "name": kName, "created_at": kCreated,
				})
			}
		}
		data["api_keys"] = keys
	}

	bwRows, err := db.QueryContext(r.Context(),
		`SELECT date, ingress_bytes, egress_bytes, requests FROM bandwidth_usage_daily WHERE tenant_id = $1 AND date >= NOW() - INTERVAL '90 days' ORDER BY date DESC`, tenantID)
	if err == nil {
		defer func() { _ = bwRows.Close() }()
		var bw []map[string]interface{}
		for bwRows.Next() {
			var bDate time.Time
			var ingress, egress, reqs int64
			if bwRows.Scan(&bDate, &ingress, &egress, &reqs) == nil {
				bw = append(bw, map[string]interface{}{
					"date": bDate.Format("2006-01-02"), "ingress_bytes": ingress,
					"egress_bytes": egress, "requests": reqs,
				})
			}
		}
		data["bandwidth_usage"] = bw
	}

	out, _ := json.MarshalIndent(data, "", "  ")
	return out
}
