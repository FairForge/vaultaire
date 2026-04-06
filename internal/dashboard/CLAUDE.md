# internal/dashboard

Web dashboard for stored.ge customers and admins. Uses htmx + Go templates, embedded into the single binary.

## Architecture

- **embed.go** — `//go:embed` bundles `templates/` and `static/` into the binary
- **router.go** — `RegisterRoutes(r, deps)` mounts all dashboard routes on the chi router. Must be called BEFORE the S3 catch-all in `server.go`.
- **auth/** — session management (PostgreSQL-backed `DBStore` or in-memory `MemoryStore`)
- **handlers/** — HTTP handlers (Phase 1 fills these in)
- **templates/layouts/** — shared HTML layouts (`base.html`, `admin.html`)
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
| `/dashboard/*` | GET | session | Customer dashboard |
| `/admin/*` | GET | session + admin role | Admin panel |

## Auth Flow

1. User submits login/register form (POST)
2. Handler validates credentials / creates account via `deps.Auth`
3. On success: creates session in `deps.Sessions` with 24h TTL
4. Sets `vaultaire_session` cookie
5. Redirects to `/dashboard`
6. On error: re-renders form with `.Error` message and preserved form values

## Templates

Base layout defines blocks: `title`, `head`, `nav`, `content`. Pages override these blocks. Phase 1 will add per-page template files in `templates/auth/`, `templates/customer/`, `templates/admin/`.
