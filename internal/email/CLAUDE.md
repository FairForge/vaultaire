# internal/email

Email service abstraction for Vaultaire. Provides a `Sender` interface with three implementations: Resend API, SMTP, and the dev-mode log sender.

## Architecture

- **email.go** — `Sender` interface, `NewSender` factory (reads `EMAIL_PROVIDER`), `LogSender`, per-recipient rate limiter (10/min sliding window)
- **resend.go** — `ResendSender`: raw `net/http` POST to Resend API (no SDK dependency)
- **smtp.go** — `SMTPSender`: stdlib `net/smtp` with STARTTLS (port 587) and implicit TLS (port 465), multipart/alternative MIME
- **templates.go** — `//go:embed` HTML templates, `RenderVerification`, `RenderPasswordReset`, and (WP-PAY-3) `RenderPaymentReceipt(ReceiptMail)`, `RenderRenewalReminder(ReminderMail)`, `RenderHouseLapsed(LapsedMail)` — typed data, nothing here reads a table (html/template — contextual escaping; the text alternative is the HTML with tags stripped)
- **templates/*.html** — Branded email templates with inline CSS (no external resources)

## Environment Variables

| Variable | Required | Purpose |
|----------|----------|---------|
| `EMAIL_PROVIDER` | No | `resend`, `smtp`, or unset (log sender) |
| `EMAIL_FROM` | No | Sender address (default `noreply@<canonical domain>` from `internal/hosts`) |
| `RESEND_API_KEY` | When `resend` | Resend API key |
| `SMTP_HOST` | When `smtp` | SMTP server hostname |
| `SMTP_PORT` | When `smtp` | SMTP port (587 for STARTTLS, 465 for implicit TLS) |
| `SMTP_USER` | No | SMTP auth username |
| `SMTP_PASSWORD` | No | SMTP auth password |

**Production runs `smtp`** since 2026-10-12: `EMAIL_PROVIDER=smtp`, `SMTP_HOST=smtp.mx.cloudflare.net` (the Cloudflare Email Service on `stored.cloud`, `docs/CLOUDFLARE.md` item 7; verified on the prod env 2026-10-18). Before that (checked 2026-09-30, Review R14) prod ran the `LogSender`. Payment reminders, receipts and the house-lapsed notice (WP-PAY-3, `docs/PAYMENTS.md` § 3.8) send through this sender via `payments.Mailer`.

## Which flows send mail

`Sender` is injected via `dashboard.Deps.Email` and constructed once in `api.NewServer` (`server.go`). Every call site, and what happens when the send fails:

| Flow | Call site | Template | On send error |
|---|---|---|---|
| Password reset (web form) | `dashboard/router.go` `handleForgotPassword` | `password_reset.html` | logged; the page always shows the same success text (anti-enumeration — deliberate) |
| Password reset (JSON API) | `api/server.go` `handlePasswordReset` | `password_reset.html` | logged; always `{"message": "If that email is registered…"}` (deliberate) |
| Verify e-mail (manual *Resend* button) | `dashboard/handlers/email_verify.go` | `verification.html` | logged, **user is still told "sent"** (WP-R14-3) |
| Bandwidth alert (admin-set `bandwidth_limit_bytes`, 80/95 %) | `api/bandwidth_alerts.go` | plain text (same body as html and text) | logged at Warn |

| Payment receipt (WP-PAY-3) | `payments.Mailer.SendReceipt` ← the Stripe webhook on a NEWLY settled invoice | `payment_receipt.html` | one Warn, never a 500 (a replay sends nothing) |
| Renewal reminders (WP-PAY-3) | `payments.Mailer.SendReminder` ← the `payment_reminders` job (hourly) | `renewal_reminder.html` (step-aware copy: 14 d before, 3 d before, in grace, last day) | counted `failed`, noted on the run, the step retried next hour |
| House lapsed (WP-PAY-3) | `payments.Mailer.SendHouseLapsed` ← the `entitlement_lapse` job after its Revoke | `house_lapsed.html` | one Warn |

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

**WP-PAY-5 (Prompt 25):** `renewal_reminder.html` has a `SwitchTo` branch (a customer who asked to switch to bitcoin: the copy says so and the button is "Pay the next period in bitcoin"); `renewal_stopped.html` / `RenderRenewalStopped(RenewalStoppedMail{…, Resumed})` is the bulk stop (or resume) notice. Test `TestRenderRenewalReminder_SwitchOffer`.
