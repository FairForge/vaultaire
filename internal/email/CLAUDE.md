# internal/email

Email service abstraction for Vaultaire. Provides a `Sender` interface with three implementations: Resend API, SMTP, and the dev-mode log sender.

## Architecture

- **email.go** — `Sender` interface, `NewSender` factory (reads `EMAIL_PROVIDER`), `LogSender`, per-recipient rate limiter (10/min sliding window)
- **resend.go** — `ResendSender`: raw `net/http` POST to Resend API (no SDK dependency)
- **smtp.go** — `SMTPSender`: stdlib `net/smtp` with STARTTLS (port 587) and implicit TLS (port 465), multipart/alternative MIME
- **templates.go** — `//go:embed` HTML templates, `RenderVerification`, `RenderPasswordReset` (html/template — contextual escaping; the text alternative is the HTML with tags stripped)
- **templates/*.html** — Branded email templates with inline CSS (no external resources)

## Environment Variables

| Variable | Required | Purpose |
|----------|----------|---------|
| `EMAIL_PROVIDER` | No | `resend`, `smtp`, or unset (log sender) |
| `EMAIL_FROM` | No | Sender address (default `noreply@stored.ge`) |
| `RESEND_API_KEY` | When `resend` | Resend API key |
| `SMTP_HOST` | When `smtp` | SMTP server hostname |
| `SMTP_PORT` | When `smtp` | SMTP port (587 for STARTTLS, 465 for implicit TLS) |
| `SMTP_USER` | No | SMTP auth username |
| `SMTP_PASSWORD` | No | SMTP auth password |

**Production sets none of these** (checked 2026-09-30, Review R14): prod runs the `LogSender`. Wiring a provider is the last open launch item on the e-mail side (sequence 5.6).

## Which flows send mail

`Sender` is injected via `dashboard.Deps.Email` and constructed once in `api.NewServer` (`server.go`). Every call site, and what happens when the send fails:

| Flow | Call site | Template | On send error |
|---|---|---|---|
| Password reset (web form) | `dashboard/router.go` `handleForgotPassword` | `password_reset.html` | logged; the page always shows the same success text (anti-enumeration — deliberate) |
| Password reset (JSON API) | `api/server.go` `handlePasswordReset` | `password_reset.html` | logged; always `{"message": "If that email is registered…"}` (deliberate) |
| Verify e-mail (manual *Resend* button) | `dashboard/handlers/email_verify.go` | `verification.html` | logged, **user is still told "sent"** (WP-R14-3) |
| Bandwidth alert (admin-set `bandwidth_limit_bytes`, 80/95 %) | `api/bandwidth_alerts.go` | plain text (same body as html and text) | logged at Warn |
| Spending-cap alert (metered billing — dormant, no `STRIPE_METER_*` in prod) | `billing/metered.go` | plain text | logged at Warn |

Nothing else sends mail: **registration sends no verification or welcome e-mail** (the welcome template was dead code and was removed in R14), account deletion, billing `past_due`, and abuse reports send nothing (R10-35 / WP-R10-3).

Links are built from `VAULTAIRE_BASE_URL` only (`s.baseURL` / `deps.BaseURL`), never from the request's `Host` header.

## LogSender

The zero-config fallback logs **recipient, subject and body sizes only** — never the body. A reset or verification body carries a bearer token, and journald is not a place for those (Review R14-01 / R1-08: before this the full HTML landed in `journalctl -u vaultaire` on prod). Developers who need the link in dev run `EMAIL_PROVIDER=smtp` against a local MailHog/Mailpit, or read the token from the database.

## Rate Limiting

Per-recipient, 10 emails per minute sliding window. Shared across all email types. Returns `ErrRateLimited` when exceeded.

## Known gaps (WP-R14-3)

SMTP path: non-ASCII subjects are sent raw (no RFC 2047 encoding), bodies are UTF-8 under `7bit`, no `Date`/`Message-ID`, no CRLF guard on headers. Resend path: `http.DefaultClient` has no timeout. Text alternative keeps HTML entities. Resend-verification handler swallows the send error.

## Testing

```bash
go test ./internal/email/... -v -race
```

Tests cover: log sender (asserts the body never reaches the log), rate limiting, Resend API mock, SMTP MIME building, template rendering, factory env routing.
