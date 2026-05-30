package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

type BucketCompliance struct {
	Name              string `json:"name"`
	Region            string `json:"region"`
	RegionDisplay     string `json:"region_display"`
	IsEURegion        bool   `json:"is_eu_region"`
	SSEEnabled        bool   `json:"sse_enabled"`
	ObjectLockEnabled bool   `json:"object_lock_enabled"`
	RetentionMode     string `json:"retention_mode,omitempty"`
	RetentionDays     int    `json:"retention_days,omitempty"`
	VersioningEnabled bool   `json:"versioning_enabled"`
	LoggingEnabled    bool   `json:"logging_enabled"`
	InventoryEnabled  bool   `json:"inventory_enabled"`
	MFADeleteEnabled  bool   `json:"mfa_delete_enabled"`
	EncryptedObjects  int64  `json:"encrypted_objects"`
	TotalObjects      int64  `json:"total_objects"`
	EncryptionPct     int    `json:"encryption_pct"`
	IsFullyCompliant  bool   `json:"is_fully_compliant"`
}

type complianceReport struct {
	GeneratedAt      string             `json:"generated_at"`
	ComplianceScore  int                `json:"compliance_score"`
	TotalBuckets     int                `json:"total_buckets"`
	CompliantBuckets int                `json:"compliant_buckets"`
	Buckets          []BucketCompliance `json:"buckets"`
}

func HandleCompliance(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "compliance")
		buckets, score, compliant := queryComplianceData(r, db, sd.TenantID)

		data["Buckets"] = buckets
		data["ComplianceScore"] = score
		data["TotalBuckets"] = len(buckets)
		data["CompliantBuckets"] = compliant
		data["HasBuckets"] = len(buckets) > 0

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render compliance", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func HandleComplianceExport(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		buckets, score, compliant := queryComplianceData(r, db, sd.TenantID)

		report := complianceReport{
			GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
			ComplianceScore:  score,
			TotalBuckets:     len(buckets),
			CompliantBuckets: compliant,
			Buckets:          buckets,
		}

		out, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			logger.Error("marshal compliance report", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		filename := fmt.Sprintf("compliance-report-%s.json", time.Now().Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		_, _ = w.Write(out)
	}
}

func queryComplianceData(r *http.Request, db *sql.DB, tenantID string) ([]BucketCompliance, int, int) {
	if db == nil {
		return nil, 0, 0
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT name, region, sse_enabled, object_lock_enabled,
		       default_retention_mode, default_retention_days,
		       versioning_status, logging_enabled, inventory_enabled,
		       mfa_delete_enabled
		FROM buckets WHERE tenant_id = $1 ORDER BY name ASC`, tenantID)
	if err != nil {
		return nil, 0, 0
	}
	defer func() { _ = rows.Close() }()

	var buckets []BucketCompliance
	for rows.Next() {
		var b BucketCompliance
		var versioningStatus string
		if err := rows.Scan(
			&b.Name, &b.Region, &b.SSEEnabled, &b.ObjectLockEnabled,
			&b.RetentionMode, &b.RetentionDays,
			&versioningStatus, &b.LoggingEnabled, &b.InventoryEnabled,
			&b.MFADeleteEnabled,
		); err != nil {
			continue
		}

		b.RegionDisplay = drivers.RegionDisplayName(b.Region)
		b.IsEURegion = drivers.IsEURegion(b.Region)
		b.VersioningEnabled = versioningStatus == "Enabled"

		_ = db.QueryRowContext(r.Context(), `
			SELECT COUNT(*) FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND encryption_algorithm != ''`,
			tenantID, b.Name).Scan(&b.EncryptedObjects)

		_ = db.QueryRowContext(r.Context(), `
			SELECT COUNT(*) FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2`,
			tenantID, b.Name).Scan(&b.TotalObjects)

		if b.TotalObjects > 0 {
			b.EncryptionPct = int(b.EncryptedObjects * 100 / b.TotalObjects)
		}

		b.IsFullyCompliant = b.SSEEnabled && b.LoggingEnabled && b.VersioningEnabled

		buckets = append(buckets, b)
	}

	compliant := 0
	for _, b := range buckets {
		if b.IsFullyCompliant {
			compliant++
		}
	}

	score := 0
	if len(buckets) > 0 {
		score = compliant * 100 / len(buckets)
	}

	return buckets, score, compliant
}
