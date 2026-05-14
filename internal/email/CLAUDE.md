# internal/email

Email service abstraction for Vaultaire. Provides a `Sender` interface with three implementations: Resend API, SMTP, and dev-mode log sender.

## Architecture

- **email.go** — `Sender` interface, `NewSender` factory (reads `EMAIL_PROVIDER` env var), `LogSender`, per-recipient rate limiter (10/min sliding window)
- **resend.go** — `ResendSender`: raw `net/http` POST to Resend API (no SDK dependency)
- **smtp.go** — `SMTPSender`: stdlib `net/smtp` with STARTTLS (port 587) and implicit TLS (port 465), multipart/alternative MIME
- **templates.go** — `//go:embed` HTML templates, `RenderVerification`, `RenderPasswordReset`, `RenderWelcome`
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

## Integration

`Sender` is injected via `dashboard.Deps.Email` and constructed in `server.go`. Handlers that send email:
- `HandleResendVerification` (dashboard) — email verification
- `handleForgotPassword` (dashboard) — password reset
- `handlePasswordReset` (API) — password reset via JSON API

The `LogSender` fallback is always non-nil and zero-config — dev mode logs full emails at Info level so developers can grab verification/reset links from logs.

## Rate Limiting

Per-recipient, 10 emails per minute sliding window. Shared across all email types. Returns `ErrRateLimited` when exceeded.

## Testing

```bash
go test ./internal/email/... -v -race
```

9 tests covering: log sender, rate limiting, Resend API mock, SMTP MIME building, template rendering, factory env routing.
