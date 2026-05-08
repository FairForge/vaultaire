package handlers

import (
	"context"
	"database/sql"
	"net/http"

	"go.uber.org/zap"
)

type OnboardingStatus struct {
	HasBucket  bool
	HasObject  bool
	HasWebhook bool
	AllDone    bool
	Dismissed  bool
	AccessKey  string
}

func populateOnboarding(ctx context.Context, db *sql.DB, tenantID string, r *http.Request, data map[string]any) {
	if db == nil {
		return
	}

	if c, err := r.Cookie("onboarding_dismissed"); err == nil && c.Value == "1" {
		data["OnboardingDismissed"] = true
		return
	}

	var bucketCount, objectCount, webhookCount int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`, tenantID).Scan(&bucketCount)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, tenantID).Scan(&objectCount)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhook_endpoints WHERE tenant_id = $1`, tenantID).Scan(&webhookCount)

	var accessKey string
	_ = db.QueryRowContext(ctx,
		`SELECT access_key FROM tenants WHERE id = $1`, tenantID).Scan(&accessKey)

	status := &OnboardingStatus{
		HasBucket:  bucketCount > 0,
		HasObject:  objectCount > 0,
		HasWebhook: webhookCount > 0,
		AccessKey:  accessKey,
	}
	status.AllDone = status.HasBucket && status.HasObject && status.HasWebhook

	data["Onboarding"] = status
}

func HandleDismissOnboarding(logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     "onboarding_dismissed",
			Value:    "1",
			Path:     "/",
			MaxAge:   365 * 24 * 60 * 60,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusOK)
	}
}
