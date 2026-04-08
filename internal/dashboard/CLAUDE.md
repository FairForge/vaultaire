# internal/dashboard

Web dashboard for stored.ge customers and admins. Uses htmx + Go templates, embedded into the single binary.

## Architecture

- **embed.go** — `//go:embed` bundles `templates/` and `static/` into the binary
- **router.go** — `RegisterRoutes(r, deps)` mounts all dashboard routes on the chi router. Must be called BEFORE the S3 catch-all in `server.go`.
- **auth/** — session management (PostgreSQL-backed `DBStore` or in-memory `MemoryStore`)
- **handlers/** — HTTP handlers (`overview.go` renders dashboard with real data from DB)
- **templates/layouts/** — shared HTML layouts (`base.html`, `admin.html`)
- **templates/customer/** — customer page templates (`dashboard.html` = overview)
- **static/css/** — `style.css`
- **static/js/** — `htmx.min.js` (v2.0.4, vendored)

## Session Auth

Sessions use the `dashboard_sessions` PostgreSQL table. The `SessionStore` interface has two implementations:
- `DBStore` — production, PostgreSQL-backed, hourly cleanup goroutine
- `MemoryStore` — tests and local dev without DB

Session data is injected into request context via `RequireSession` middleware. Handlers call `dashauth.GetSession(r.Context())` to get `{UserID, TenantID, Email, Role}`.

Cookie: `vaultaire_session`, HttpOnly, Secure, SameSite=Lax.

## Routes

| Path | Method | Auth | Purpose |
|------|--------|------|---------|
| `/static/*` | GET | none | CSS, JS assets |
| `/login` | GET | none | Login page |
| `/login` | POST | none | Validate credentials, create session, redirect to /dashboard |
| `/register` | GET | none | Registration page |
| `/register` | POST | none | Create user+tenant, create session, redirect to /dashboard |
| `/logout` | GET | none | Delete session, clear cookie, redirect to /login |
| `/dashboard/` | GET | session | Overview: storage gauge, bandwidth, stats, activity |
| `/dashboard/buckets` | GET | session | Bucket list with counts + sizes |
| `/dashboard/buckets` | POST | session | Create new bucket (validates name, creates directory) |
| `/dashboard/buckets/{name}` | GET | session | Object browser with prefix navigation |
| `/dashboard/apikeys` | GET | session | List API keys with status |
| `/dashboard/apikeys` | POST | session | Generate new API key (shows secret once) |
| `/dashboard/apikeys/{id}/revoke` | POST | session | Revoke an API key |
| `/dashboard/usage` | GET | session | Usage detail: storage, bandwidth, SVG chart, daily table |
| `/dashboard/settings` | GET | session | Settings: profile, password, notifications |
| `/dashboard/settings/profile` | POST | session | Update company name |
| `/dashboard/settings/password` | POST | session | Change password (validates current) |
| `/dashboard/settings/notifications` | POST | session | Update notification preferences |
| `/dashboard/settings/mfa` | GET | session | 2FA setup page (QR code, backup codes) |
| `/dashboard/settings/mfa/enable` | POST | session | Confirm TOTP code to enable 2FA |
| `/dashboard/settings/mfa/disable` | POST | session | Disable 2FA (requires password) |
| `/login/verify-2fa` | GET | none | 2FA verification page (during login) |
| `/login/verify-2fa` | POST | none | Validate TOTP/backup code, complete login |
| `/dashboard/billing` | GET | session | Billing: plan, upgrade, value stack, cost comparison |
| `/dashboard/billing/upgrade` | POST | session | Redirect to Stripe Checkout for chosen plan |
| `/dashboard/billing/portal` | POST | session | Redirect to Stripe Billing Portal |
| `/admin/tenants/{id}/bandwidth-limit` | POST | session + admin | Update tenant bandwidth limit |
| `/admin/tenants/{id}/reset-mfa` | POST | session + admin | Reset user's 2FA |
| `/admin/*` | GET | session + admin role | Admin panel |

## Auth Flow

1. User submits login/register form (POST)
2. Handler validates credentials / creates account via `deps.Auth`
3. If MFA enabled: sets `mfa_pending` cookie (5-min TTL), redirects to `/login/verify-2fa`
4. `/login/verify-2fa` validates TOTP code or backup code, then creates session
5. On success (no MFA or MFA verified): creates session in `deps.Sessions` with 24h TTL
6. Sets `vaultaire_session` cookie
7. Redirects to `/dashboard`
8. On error: re-renders form with `.Error` message and preserved form values

## MFA Pending Store

`MFAPendingStore` is an in-memory store holding intermediate state between password validation and TOTP verification. Entries expire after 5 minutes. Token is stored in `mfa_pending` cookie (HttpOnly, Secure, SameSite=Lax). Get() consumes the entry (single use). Peek() reads without consuming (for retries).

## Templates

Base layout defines blocks: `title`, `head`, `nav`, `content`. Pages override these blocks. Customer page templates are in `templates/customer/`, parsed from embedded FS and combined with base layout in `RegisterRoutes`.

## Dashboard Overview (Phase 1.2)

`handlers/overview.go` queries DB directly (no QuotaManager interface needed — just `*sql.DB`):
- Storage: `tenant_quotas` (used, limit, tier)
- Bandwidth: `bandwidth_usage_daily` (current month ingress+egress)
- Counts: `object_head_cache` (distinct buckets, total objects), `api_keys` (per user)
- Activity: `quota_usage_events` (last 5, with operation, key, size, time)

Gracefully degrades to zeros when DB is nil (tests, local dev without PostgreSQL).
