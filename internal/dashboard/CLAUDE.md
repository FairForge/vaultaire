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

| Path | Auth | Purpose |
|------|------|---------|
| `/static/*` | none | CSS, JS assets |
| `/login` | none | Login page |
| `/register` | none | Registration page |
| `/logout` | none | Clears session, redirects |
| `/dashboard/*` | session | Customer dashboard |
| `/admin/*` | session + admin role | Admin panel |

## Templates

Base layout defines blocks: `title`, `head`, `nav`, `content`. Pages override these blocks. Phase 1 will add per-page template files in `templates/auth/`, `templates/customer/`, `templates/admin/`.
