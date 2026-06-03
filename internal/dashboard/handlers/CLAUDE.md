# internal/dashboard/handlers

HTTP handlers for the stored.ge customer dashboard. Each handler receives a pre-parsed `*template.Template` (base layout + page content) and renders it with data from PostgreSQL.

## Overview Handler (`overview.go`)

`HandleOverview(tmpl, db, logger, storageMode)` renders the main dashboard page:
- Reads session from context via `dashauth.GetSession(r.Context())`
- Queries 4 tables: `tenant_quotas`, `bandwidth_usage_daily`, `object_head_cache`, `api_keys`, `quota_usage_events`
- Fails gracefully to zeros when DB is nil or tables are empty
- `populateLocality(storageMode, data)` maps the active backend to a physical location (city, country, SVG coordinates) for the Data Locality card. Falls back to "local" (Salt Lake City) for unknown backends. Pre-computes SVG dot coordinates (LocalityDotX/Y) for a 200x100 world map.
- Template: `templates/customer/dashboard.html`

Helper functions are in `context.go`: `formatBytes` (human-readable sizes), `relativeTime` (time ago), `absInt64`, `sessionData`.

## Bucket Browser (`buckets.go`)

Three handlers:
- `HandleBuckets(tmpl, db, dataPath, logger)` — lists buckets from `buckets` table LEFT JOIN `object_head_cache` (includes empty buckets with count=0)
- `HandleCreateBucket(tmpl, db, dataPath, logger)` — validates S3-compatible name and region (via `drivers.IsValidRegion`, default `us-west-1`), creates directory at `{dataPath}/{name}`, persists to `buckets` table with region
- `HandleBucketObjects(tmpl, db, logger)` — lists objects in a bucket with prefix-based "folder" navigation (uses chi URL param `{name}`). Queries bucket visibility and tenant slug; for public-read buckets with a slug, sets `CDNBaseURL` and populates `ObjectRow.CDNURL` and `ObjectRow.PreviewType` fields for inline media previews and CDN copy buttons.

`previewTypeFromContentType(ct)` maps content types to preview categories: `image/*` → "image", `video/*` → "video", `audio/*` → "audio", `text/*`/json/xml/js → "text", everything else → "".

Bucket name validation: `^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$` + path traversal check.

Shared helpers in `context.go`: `sessionData(sd, page)` builds the base template data map, `formatBytes`, `relativeTime`.

## Bucket Settings (`bucket_settings.go`)

Two handlers:
- `HandleBucketSettings(tmpl, db, logger)` — GET renders bucket settings page: visibility toggle (private/public-read), CDN URL card with tabbed code examples, cache TTL, CORS origins, **region** (read-only card showing region + display name + EU badge). Checks `CanEnablePublicRead(tier)` for archive-tier restriction. Region data: `Region`, `RegionDisplay`, `IsEURegion`.
- `HandleUpdateBucketSettings(tmpl, db, logger)` — POST updates visibility, CORS origins, and cache_max_age_secs in `buckets` table. Validates visibility enum, clamps cache to 0–86400, enforces archive-tier restriction via `auth.CanEnablePublicRead()`. Uses flash messages for feedback. Region is NOT updatable.

Template: `templates/customer/bucket_settings.html`. Routes: `GET/POST /dashboard/buckets/{name}/settings`.

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

## Onboarding (`onboarding.go`)

`populateOnboarding(ctx, db, tenantID, r, data)` — called from `HandleOverview`, derives checklist state from `COUNT(*)` on `buckets`, `object_head_cache`, `webhook_endpoints`. Reads `access_key` from `tenants` for code examples (never exposes `secret_key`). Skips queries if `onboarding_dismissed=1` cookie is set.

`HandleDismissOnboarding(logger)` — POST `/dashboard/onboarding/dismiss`. Sets a 1-year cookie and returns 200. The dashboard template uses htmx to remove the card on success.

The onboarding card in `dashboard.html` shows a 3-item checklist (bucket, object, webhook) with tabbed code examples (AWS CLI, Python, JavaScript, cURL) pre-filled with the user's access key. Hidden when `AllDone` or dismissed.

## Free Tier Enforcement (Phase 5.11.10)

**Bucket creation** (`HandleCreateBucket` in `buckets.go`): queries `tenant_quotas` for tier. Free tier tenants with `>= FreeTierLimits.MaxBuckets` existing buckets get a `CreateError` message.

**API key generation** (`HandleGenerateKey` in `apikeys.go`): same pattern — queries tier and key count. Free tier tenants at the limit see `GenerateError`. Signature changed to accept `*sql.DB` as third parameter.

**Dashboard upgrade CTA** (`populateStorageUsage` in `overview.go`): sets `ShowUpgradeCTA = true` when tier is "free" and storage usage is >= 80%. The CTA card in `dashboard.html` links to `/dashboard/billing`.

## Bucket Analytics (`bucket_analytics.go`)

`HandleBucketAnalytics(tmpl, db, logger)` renders the CDN analytics page at `/dashboard/buckets/{name}/analytics`. Queries `cdn_stats_daily` for 24h/7d/30d download counts and bandwidth, `cdn_access_log` for top objects and geographic breakdown, and `buckets.bandwidth_budget_bytes` for budget gauge. Degrades to zero-state when DB is nil or tables are empty. Template: `templates/customer/bucket_analytics.html`.

Analytics link only appears in `bucket_objects.html` for public-read buckets (gated on `{{if .CDNBaseURL}}`).

## Account / GDPR (`account.go`)

Three handlers for GDPR compliance (Phase 5.14.1):
- `HandleExportData(db, logger)` — POST `/dashboard/settings/export`. Collects user profile, tenant, quota, buckets, objects, API keys, bandwidth (90d) into JSON. Returns as `Content-Disposition: attachment` download.
- `HandleRequestDeletion(db, sessions, logger)` — POST `/dashboard/settings/delete-account`. Requires password confirmation via bcrypt. Sets `deletion_scheduled_at` 30 days out and `status = 'pending_deletion'`. Flash message with scheduled date.
- `HandleCancelDeletion(db, logger)` — POST `/dashboard/settings/cancel-deletion`. Nulls `deletion_scheduled_at`/`deletion_reason`, sets `status = 'active'`.

`populateDeletionStatus(ctx, db, userID, data)` in `settings.go` queries `deletion_scheduled_at` from users and populates `DeletionScheduled` + `DeletionDate` template data.

## Compliance Dashboard (`compliance.go`)

Two handlers:
- `HandleCompliance(tmpl, db, logger)` — GET `/dashboard/compliance`. Queries all buckets for the tenant with compliance-relevant columns (SSE, Object Lock, versioning, logging, inventory, MFA delete). Computes per-bucket `IsFullyCompliant` (SSE + logging + versioning) and overall `ComplianceScore` (percentage of compliant buckets). Degrades to empty state when DB is nil.
- `HandleComplianceExport(db, logger)` — GET `/dashboard/compliance/export`. Same data as above, serialized as JSON with `Content-Disposition: attachment` header. Filename: `compliance-report-{date}.json`.

Shared `queryComplianceData(r, db, tenantID)` helper queries buckets table + `object_head_cache` for encryption percentages. Template: `templates/customer/compliance.html`.

## Billing (`billing.go`)

`HandleBilling`, `HandleUpgrade` (Stripe Checkout), `HandleManageBilling` (Stripe
Billing Portal). Renders plan, subscription status, value stack, and cost
comparison.

**Accrued charges (Phase 2.7)**: `populateAccruedCharges(ctx, db, data, tenantID)`
shows "≈ $X.XX this month" for metered-tier tenants (`standard`/`performance`
only). Storage is the live `tenant_quotas.storage_used_bytes` gauge; egress is the
current month's sum from `bandwidth_usage_daily`. Charge math reuses
`billing.AccruedCents(tier, storageBytes, egressBytes)`. Sets `IsMetered=false`
for fixed-price (Vault/free) tenants, so the card is hidden. Template card gated on
`{{if .IsMetered}}` in `billing.html`.

## Admin Waitlist (`waitlist.go`)

Admin-only view of pre-launch waitlist signups (captured by `POST /api/waitlist`
in the api package → `waitlist_signups`, migration 044).
- `HandleAdminWaitlist(tmpl, db, logger)` — GET `/admin/waitlist`: total count + the
  1000 most recent signups (email, source, date). Renders the `admin` layout.
- `HandleAdminWaitlistExport(db, logger)` — GET `/admin/waitlist/export`: streams **all**
  signups as a CSV download (`encoding/csv`, `Content-Disposition: attachment`).
Both redirect to `/login` without a session; nil-DB degrades to empty/zero. Linked
from the admin sidebar nav. Template: `templates/admin/waitlist.html`.

## Admin Revenue Dashboard (`admin_revenue.go`)

`HandleAdminRevenue(tmpl, db, logger)` — GET `/admin/revenue`: real MRR from two
sources: fixed-price Vault plans (`planMonthlyCents` lookup, prices from
VAULT_SERIES_ECONOMICS.md) + metered tiers (standard/performance via
`billing.AccruedCents`). Cards: MRR, Active Subs, New This Month, Churn
(count + rate from `subscriptions` table). Revenue-by-tier table groups fixed plans
and metered tiers separately. Top 10 customers by storage, ordered DESC. Optional
SVG bar chart of new MRR added per month (last 12 months, derived from
`subscriptions.created_at` — no historical MRR snapshot table exists).

Also fixes `admin.go:queryAdminStats` which previously used the non-existent
`used_bytes` column — corrected to `storage_used_bytes`.

Helper: `planMonthlyCents(plan)` covers vault1/3/5/10/18/50/100; free/starter/metered
return 0. `formatCents(cents)` → "$X.XX".

Template: `templates/admin/revenue.html`. Nil-DB and empty-state both render 200
with "$0.00 MRR" zero-state.

## Admin Cost Dashboard (`admin_costs.go`)

`HandleAdminCosts(tmpl, db, logger)` — GET `/admin/costs`: estimated backend spend,
per-tenant margin (revenue minus cost), and negative-margin alerts.

Cost model: `backendCostPerTBCents` map (geyser=155, idrive=330, hetzner=381,
onedrive/gorilla/local/edge=0) + fixed costs (Geyser $155/mo floor, Gorilla
configurable). `tierBackend(plan, tier)` maps vault*→geyser, standard/performance→idrive,
free→local. Egress cost is a first-class column (currently $0 — matters when BYOB/edge
land).

Revenue side reuses `planMonthlyCents` (fixed plans) and `billing.AccruedCents`
(metered tiers) from admin_revenue.go. Margin = revenue − cost per tenant.

Cards: Est. Monthly Spend (sum + fixed floors), Blended COGS/TB, Gross Margin %,
Negative-Margin Tenants. Tables: cost-by-backend (storage, variable, fixed, total)
and per-tenant margin table with negative margins highlighted in red.

Projected month-end: linear extrapolation from current day-of-month. Caption notes
estimates are from intended tier→backend mapping, not live backend.

Template: `templates/admin/costs.html`. Nil-DB and empty-state both render 200
with $0.00 zero-state.

## Admin Customer Support (`admin_support.go`)

Three handlers for the admin customer support view (Phase 3.7):
- `HandleAdminSupport(tmpl, db, logger)` — GET `/admin/support`: unified customer search
  across `tenants.email` (ILIKE), `tenants.id`, `tenants.access_key`, and
  `tenants.stripe_customer_id` (exact match). Returns up to 50 results.
- `HandleCustomerDetail(tmpl, db, logger)` — GET `/admin/support/{id}`: full customer
  context page. Reuses `loadTenantDetail` for the info card + bandwidth, then adds:
  **Timeline** (UNION ALL of `events` + `stripe_events`, last 50), **S3 Error Log**
  (status_code >= 400 from `s3_access_log`, last 20), **Internal Notes** (`admin_notes`
  JOIN `users`), and **Quick Actions** (link to tenant management).
- `HandleAddNote(db, logger)` — POST `/admin/support/{id}/notes`: inserts into
  `admin_notes`, validates non-empty + max 2000 chars, flash + redirect.

Helper functions: `searchCustomers`, `queryTimeline`, `queryS3Errors`, `queryNotes`.
Types: `supportResult`, `timelineEvent`, `errorLogEntry`, `adminNote`.

Templates: `templates/admin/support.html` (search page),
`templates/admin/support_detail.html` (customer detail).

Migration: `045_admin_notes.sql` — `admin_notes` table (tenant_id, admin_user_id → users, note, created_at).

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
