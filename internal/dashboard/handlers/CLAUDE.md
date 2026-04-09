# internal/dashboard/handlers

HTTP handlers for the stored.ge customer dashboard. Each handler receives a pre-parsed `*template.Template` (base layout + page content) and renders it with data from PostgreSQL.

## Overview Handler (`overview.go`)

`HandleOverview(tmpl, db, logger)` renders the main dashboard page:
- Reads session from context via `dashauth.GetSession(r.Context())`
- Queries 4 tables: `tenant_quotas`, `bandwidth_usage_daily`, `object_head_cache`, `api_keys`, `quota_usage_events`
- Fails gracefully to zeros when DB is nil or tables are empty
- Template: `templates/customer/dashboard.html`

Helper functions are in `context.go`: `formatBytes` (human-readable sizes), `relativeTime` (time ago), `absInt64`, `sessionData`.

## Bucket Browser (`buckets.go`)

Three handlers:
- `HandleBuckets(tmpl, db, dataPath, logger)` — lists buckets from `object_head_cache` (distinct buckets with counts/sizes)
- `HandleCreateBucket(tmpl, db, dataPath, logger)` — validates S3-compatible name, creates directory at `{dataPath}/{name}`
- `HandleBucketObjects(tmpl, db, logger)` — lists objects in a bucket with prefix-based "folder" navigation (uses chi URL param `{name}`)

Bucket name validation: `^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$` + path traversal check.

Shared helpers in `context.go`: `sessionData(sd, page)` builds the base template data map, `formatBytes`, `relativeTime`.

## API Key Management (`apikeys.go`)

Three handlers:
- `HandleAPIKeys(tmpl, authSvc, logger)` — lists all keys for current user via `auth.ListAPIKeys()`
- `HandleGenerateKey(tmpl, authSvc, logger)` — creates key via `auth.GenerateAPIKey()`, shows secret once
- `HandleRevokeKey(authSvc, logger)` — revokes key via `auth.RevokeAPIKey()`, redirects back

Uses `auth.AuthService` directly (not DB queries) since keys are in-memory + DB-backed.

## Bandwidth Chart (`bandwidth_chart.go`)

Shared bandwidth chart logic used by both customer usage page and admin tenant detail:
- `ChartBar` — SVG bar coordinates struct
- `BandwidthDay` — date + ingress/egress totals
- `BuildChartBars(days)` — pure function: converts bandwidth days to SVG bar data (600x200 chart area)
- `QueryBandwidthDays(ctx, db, tenantID)` — fetches last 30 days from `bandwidth_usage_daily`
- `QueryMonthBandwidth(ctx, db, tenantID)` — current month ingress/egress/requests totals

## Usage Page (`usage.go`)

`HandleUsage(tmpl, db, logger)` renders the detailed usage page:
- Storage gauge with percentage, limit, tier
- Current month bandwidth (ingress, egress, requests)
- 30-day SVG bar chart (stacked ingress/egress bars, server-rendered)
- Daily breakdown table (date, ingress, egress, total, requests)
- htmx auto-refresh on chart via `hx-trigger="every 30s"`
- Delegates to shared `bandwidth_chart.go` functions for chart building and bandwidth queries

Queries: `tenant_quotas`, `bandwidth_usage_daily` (30 days). Chart bars are `ChartBar` structs with pre-computed SVG coordinates.

## Settings Page (`settings.go`)

Four handlers:
- `HandleSettings(tmpl, authSvc, db, logger)` — GET renders profile, password, and notification forms
- `HandleUpdateProfile(tmpl, authSvc, db, logger)` — POST updates company in DB + in-memory
- `HandleChangePassword(tmpl, authSvc, db, logger)` — POST validates current password via `authSvc.ChangePassword()`, enforces min length + match + different-from-current
- `HandleUpdateNotifications(tmpl, authSvc, db, logger)` — POST saves email notification preference via `authSvc.SetUserPreferences()`

Uses both `*auth.AuthService` (password change, preferences) and `*sql.DB` (company column, member-since date).

## MFA Handlers (`mfa.go`)

Three customer handlers + one admin handler:
- `HandleMFASetup(tmpl, authSvc, mfaSvc, logger)` — GET renders QR code, secret, and backup codes. Redirects if already enabled.
- `HandleMFAEnable(settingsTmpl, authSvc, mfaSvc, logger)` — POST validates TOTP code against pending secret, enables MFA via `authSvc.EnableMFA()`.
- `HandleMFADisable(settingsTmpl, authSvc, logger)` — POST requires password confirmation, disables MFA via `authSvc.DisableMFA()`.
- `HandleAdminResetMFA(authSvc, logger)` — POST admin endpoint to reset a user's 2FA.

QR code rendered client-side via `qrcode-generator` CDN library. Backup codes passed as comma-separated hidden field during enable confirmation.

## Legacy Handlers

Files like `dashboard.go` etc. are stubs from before Phase 0 with inline terminal-style templates. They are NOT wired into the router. Remaining phases will rewrite them.

## Pattern

```go
func HandleXxx(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        sd := dashauth.GetSession(r.Context())
        data := map[string]any{ /* session fields + page data */ }
        tmpl.ExecuteTemplate(w, "base", data)
    }
}
```

## Testing

Tests use a minimal base template (no embedded FS) and inject session data directly into context. Run: `go test ./internal/dashboard/handlers/ -v`
