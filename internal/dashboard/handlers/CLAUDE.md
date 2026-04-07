# internal/dashboard/handlers

HTTP handlers for the stored.ge customer dashboard. Each handler receives a pre-parsed `*template.Template` (base layout + page content) and renders it with data from PostgreSQL.

## Overview Handler (`overview.go`)

`HandleOverview(tmpl, db, logger)` renders the main dashboard page:
- Reads session from context via `dashauth.GetSession(r.Context())`
- Queries 4 tables: `tenant_quotas`, `bandwidth_usage_daily`, `object_head_cache`, `api_keys`, `quota_usage_events`
- Fails gracefully to zeros when DB is nil or tables are empty
- Template: `templates/customer/dashboard.html`

Helper functions: `formatBytes` (human-readable sizes), `relativeTime` (time ago), `absInt64`.

## Bucket Browser (`buckets.go`)

Three handlers:
- `HandleBuckets(tmpl, db, dataPath, logger)` — lists buckets from `object_head_cache` (distinct buckets with counts/sizes)
- `HandleCreateBucket(tmpl, db, dataPath, logger)` — validates S3-compatible name, creates directory at `{dataPath}/{name}`
- `HandleBucketObjects(tmpl, db, logger)` — lists objects in a bucket with prefix-based "folder" navigation (uses chi URL param `{name}`)

Bucket name validation: `^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$` + path traversal check.

Shared helpers: `sessionData(sd, page)` builds the base template data map. `formatBytes` and `relativeTime` from `overview.go`.

## API Key Management (`apikeys.go`)

Three handlers:
- `HandleAPIKeys(tmpl, authSvc, logger)` — lists all keys for current user via `auth.ListAPIKeys()`
- `HandleGenerateKey(tmpl, authSvc, logger)` — creates key via `auth.GenerateAPIKey()`, shows secret once
- `HandleRevokeKey(authSvc, logger)` — revokes key via `auth.RevokeAPIKey()`, redirects back

Uses `auth.AuthService` directly (not DB queries) since keys are in-memory + DB-backed.

## Legacy Handlers

Files like `dashboard.go`, `buckets.go`, `usage.go`, etc. are stubs from before Phase 0 with inline terminal-style templates. They are NOT wired into the router. Phase 1.3+ will rewrite them to follow the `overview.go` pattern.

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
