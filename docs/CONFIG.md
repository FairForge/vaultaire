# Configuration

**Configuration is environment variables only.** Vaultaire is one static Go
binary (`cmd/vaultaire`) that reads its settings from the process environment
at boot. There is no YAML file, no command-line flag, no `serve`/`migrate`/
`import` subcommand and no runtime configuration API. In production the
variables live in an env file that the systemd unit loads with
`EnvironmentFile=` (see `docs/DEPLOY.md`); in development you export them or
prefix the command (`JWT_SECRET=dev PORT=8000 ./bin/vaultaire`).

Two things in the repository look like configuration and are not:

- **`configs/*.yaml`** (`development.yaml`, `staging.yaml`, `production.yaml`)
  — nothing loads them. There is no YAML decoder in `cmd/` or `internal/`, and
  `config.Config`'s yaml-tagged fields are never read. The 30 s read/write
  timeouts and port 8080 they advertise are not what runs. Deletion is WP-R1-4
  (`docs/reviews/R1-server-wiring.md`, R1-09).
- **`.env.example`**'s `S3_ENDPOINT`, `S3_BUCKET`, `S3_PREFIX` — no reader. The
  `s3` driver is hard-wired to its endpoint, bucket and prefix
  (`internal/drivers/s3compat.go`); only `S3_ACCESS_KEY`/`S3_SECRET_KEY` are
  read. WP-R1-4 rewrites the file from the table below.

Likewise `ONEDRIVE_CLIENT_ID`, `ONEDRIVE_CLIENT_SECRET` and
`ONEDRIVE_TENANT_ID` are read by nothing; the OneDrive fleet driver uses the
`TENANT_N_*` family below.

## Storage-mode auto-detect

The primary backend is `STORAGE_MODE` when set. Otherwise
`cmd/vaultaire/main.go` picks the first of:

```
IDRIVE_ACCESS_KEY set     -> idrive
QUOTALESS_ACCESS_KEY set  -> quotaless
S3_ACCESS_KEY set         -> s3
GEYSER_ACCESS_KEY set     -> geyser
otherwise                 -> local
```

Production sets the iDrive pair and runs with `idrive` as primary. Note that
`internal/api/server.go` re-derives the mode for the dashboard with a copy of
this list that lacks the iDrive branch; the engine's choice (above) is the one
that places objects, and WP-R1-4 makes the server take the value from `main`
instead of re-deriving it.

## Variables

Copied from the root `CLAUDE.md` env table, plus the variables R1-09 found
read-but-undocumented (marked with their source file).

### Server and database

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | 8000 | Server port. `/metrics`, `/health*` and everything else are served on this one port |
| `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` | localhost, 5432, vaultaire, (the code default is the maintainer's OS user — set it), "" | PostgreSQL connection. The DB is optional: without one the process degrades to a no-DB fallback (HEAD answers 503, no auth/billing/dashboard) |
| `DATA_PATH` | /tmp/vaultaire-data | Local storage directory for the always-registered `local` driver. `internal/api/management_routes.go` still defaults to `/tmp/vaultaire` — one of the two mismatches WP-R1-4 unifies |
| `STORAGE_MODE` | auto-detect | Force the primary backend (`idrive`, `quotaless`, `s3`, `geyser`, `local`) |
| `JWT_SECRET` | — | **Required** — JWT signing key for the JSON API and dashboard |
| `VAULTAIRE_ENDPOINT` | http://localhost:8000 | The public S3 endpoint echoed to customers (registration response, dashboard, credential download). Prod sets `https://stored.ge`. Read in `internal/auth/handlers.go` and `internal/api/server.go` (R1-09) |
| `VAULTAIRE_BASE_URL` | http://localhost:8000 | Base URL for OAuth callbacks |
| `VERIFY_SECRET` | — | HMAC secret for email verification tokens |
| `SIGNUPS_ENABLED` | true | Default for the `signups` feature flag (1.13): `false` closes public signups (web form, `/auth/register` API, OAuth signup — all gated at `auth.CreateUserWithTenant`); a `feature_flags` DB row overrides this env in either direction at runtime. Existing-user login always works |
| `SIGV4_ENFORCE` | (enforced) | `false` is the emergency fallback to access-key-existence-only auth if a client canonicalisation bug surfaces in production. Anything else = full signature verification. `internal/auth/sigv4.go` (R1-09) |
| `SECURITY_POLICY_URL` | https://stored.ge/legal/aup | `Policy:` line of `/.well-known/security.txt` (RFC 9116, 5.5.6); contact is fixed to security@stored.ge |
| `ENCRYPTION_MASTER_KEY` | — | SSE-S3 master key (64 hex chars = 32 bytes). Absent = encryption disabled (prod does not set it today — R8) |
| `TLS_CERT_PROBE_TARGETS` | — (off) | Comma-separated `sni@host:port` (or bare `sni` = `sni:443`) whose served leaf certificate expiry is exported as `vaultaire_tls_cert_expiry_timestamp_seconds{sni}` (hourly verified TLS handshake; an expired/mis-issued cert still reports via the x509 error's leaf). Prod points both public names at the local HAProxy = the origin LE cert. Rules: `deploy/monitoring/vaultaire-tls.yml` |

### Upload path and background jobs

| Variable | Default | Purpose |
|----------|---------|---------|
| `MULTIPART_MAX_UPLOAD_BYTES` | 53687091200 (50 GiB) | Per-upload in-flight byte cap for multipart parts (0 = unlimited) |
| `CHUNK_PUT_CONCURRENCY` | 8 | Parallel chunk-store workers per chunked PUT (1 = sequential stores) |
| `CHUNK_GET_PREFETCH` | 4 | Chunks fetched ahead of the write cursor per chunked GET (1 = sequential) |
| `MULTIPART_ABANDON_HOURS` | 48 | Reaper aborts active multipart uploads idle longer than this |
| `MULTIPART_TERMINAL_RETENTION_DAYS` | 7 | Reaper purges completed/aborted multipart rows older than this |
| `SMART_DEMOTION_HOT_FRACTION`, `SMART_DEMOTION_IDLE_DAYS`, `SMART_DEMOTION_MIN_AGE_DAYS`, `SMART_DEMOTION_MAX_GB_PER_RUN`, `SMART_DEMOTION_TIERS` | 0.15, 14, 3, 500, standard | Smart-tier demotion job (5.15.8) knobs; the job itself is gated by the `smart_demotion` feature flag (default OFF). Invalid values are logged and ignored. Do not enable the flag before WP-R13-1 (Review R13-04) |
| `SYNTHETIC_CHECK_URL` | — (off) | Public S3 endpoint the in-process customer-path canary signs real requests against (prod: `https://stored.ge`). Off when unset (Review R13-13, checklist item 4) |
| `SYNTHETIC_CHECK_ACCESS_KEY`, `SYNTHETIC_CHECK_SECRET_KEY` | — | Key pair of a dedicated synthetic tenant. Both required with the URL, or the check stays off (logged at Error) |
| `SYNTHETIC_CHECK_BUCKET` | `synthetic-check` | Bucket the cycle writes into — create it for the synthetic tenant first; the check never creates it |
| `SYNTHETIC_CHECK_INTERVAL` | `2m` | Cycle period (Go duration, minimum `30s`): PUT → HEAD → GET (bytes compared) → DELETE → GET (404), 20 s per request |
| `SYNTHETIC_CHECK_REGION` | `us-east-1` | SigV4 credential-scope region (the verifier accepts any region string) |

### Storage backends

Every driver except `local` is registered only when its key variable is set.
See `docs/DRIVERS.md` for what each driver does once registered.

| Variable | Default | Purpose |
|----------|---------|---------|
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | — | Registers the `s3` (s3compat) driver. Endpoint, bucket and prefix are hard-wired in the driver |
| `S3COMPAT_INSECURE_TLS` | (off) | `true`/`1` disables TLS certificate verification for the `s3` driver only (logged as a warning). `internal/drivers/s3compat.go` (R1-09) |
| `LYVE_ACCESS_KEY`, `LYVE_SECRET_KEY`, `LYVE_REGION` | region: us-west-1 | Seagate Lyve Cloud 2 — buckets are homed per region (`stored-<region>`); see `internal/drivers/lyve_README.md` |
| `LYVE_PROBE_ACCESS_KEY`, `LYVE_PROBE_SECRET_KEY`, `LYVE_PROBE_CUSTOMER` | unset → the probe is the driver's signed HeadBucket; customer `v01` | Root key for the console `RSCustomerDetails` probe (root-only). It never falls back to `LYVE_*` (R7-19): the data-plane key should be the scoped `vaultaire-prod` user, which the console refuses; see `deploy/monitoring/README.md` |
| `QUOTALESS_ACCESS_KEY`, `QUOTALESS_SECRET_KEY`, `QUOTALESS_ENDPOINT` | — | Quotaless storage (dormant in prod). `main.go` and `server.go` default the endpoint differently (`us.` vs `io.quotaless.cloud:8000`) — WP-R1-4 |
| `GEYSER_ACCESS_KEY`, `GEYSER_SECRET_KEY` | — | Geyser tape S3 credentials |
| `GEYSER_BUCKET`, `GEYSER_ENDPOINT` | — | Geyser bucket name and endpoint URL |
| `IDRIVE_ACCESS_KEY`, `IDRIVE_SECRET_KEY` | — | iDrive E2 S3 credentials (prod primary) |
| `IDRIVE_ENDPOINT`, `IDRIVE_REGION` | `https://s3.<region>.idrivee2.com`, `us-central-1` | The primary's endpoint and region. `IDRIVE_REGION` is also the **default bucket region** (served by the primary through the engine); prod = `us-central-1` (Dallas) |
| `IDRIVE_BUCKET` | `vaultaire` | The one fixed bucket every iDrive driver (primary and regional) stores into, keys prefixed `t-<tenant>/`. `internal/drivers/idrive.go` (R1-09) |
| `IDRIVE_<REGION>_ACCESS_KEY`, `IDRIVE_<REGION>_SECRET_KEY`, `IDRIVE_<REGION>_ENDPOINT` | endpoint: `IDriveRegions` table | **Enables** a region (WP-R7-1): an `idrive-<region>` driver is registered only when the region's own key pair is set (region id upper-cased, `-`→`_`, e.g. `IDRIVE_US_WEST_2_ACCESS_KEY`), its fixed `IDRIVE_BUCKET` is created in that region at boot if absent, and it is probed. There is no fallback to the primary pair (403 elsewhere). Regions without a pair cannot be chosen for a bucket (S3 400 `InvalidLocationConstraint`, dashboard option disabled). Account regions: `us-central-1 us-west-2 us-west-4 us-southwest-1 us-southeast-1 us-midwest-1 us-east-1 eu-west-1 eu-west-3 eu-west-4 eu-central-1 eu-south-1 ap-northeast-1` (`internal/drivers/idrive_regions.go`); `deploy/scripts/idrive-region-env.sh` turns the reseller key file into these lines |
| `R2_ACCOUNT_ID`, `R2_ACCESS_KEY`, `R2_SECRET_KEY` | — | Cloudflare R2 S3 credentials. Registers the `r2` driver — **public buckets / CDN origin only, never a tier**: public-read buckets resolve to the internal `PUBLIC` storage class → R2 (`api.resolvePutStorageClass`); no other placement touches it |
| `R2_JURISDICTION`, `R2_BUCKET` | default, `vaultaire-public` | R2 jurisdiction endpoint (`default`\|`eu`\|`us`\|`fedramp`; `us` endpoint fails TLS as of 2026-09-24) and the single fixed bucket public objects live in (tenant-prefixed keys) |
| `TENANT_N_ID`, `TENANT_N_CLIENT_ID`, `TENANT_N_SECRET`, `TENANT_N_USER` (N = 1..15) | — | OneDrive fleet (`permafrost` driver, internal parity backend, not customer-facing). `TENANT_1_ID` set = driver registered; a tenant missing any of the four is skipped with a warning. `internal/drivers/onedrive.go` (R1-09). Never renumber tenants: placement is FNV-hashed over the index |
| `VAULTAIRE_TUNED_TRANSPORT` | (on) | `false` makes every S3-class driver use `http.DefaultClient` — no timeouts, no connection tuning, no DNS cache. Debug escape hatch only. `internal/drivers/transport.go` (R1-09) |

### Billing (Stripe)

| Variable | Default | Purpose |
|----------|---------|---------|
| `STRIPE_SECRET_KEY` | — | Stripe API key (sk_test_... or sk_live_...) |
| `STRIPE_WEBHOOK_SECRET` | — | Stripe webhook endpoint secret (whsec_...). **Required with `STRIPE_SECRET_KEY`**: `/webhook/stripe` is mounted only when both are set (an empty secret used to skip signature verification — R10-01). The handler answers 500 and records nothing when an event cannot be applied, so Stripe retries (R10-02). Contract in `internal/billing/CLAUDE.md` |
| `STRIPE_PRICE_STANDARD_ANNUAL`, `STRIPE_PRICE_STANDARD_MONTHLY`, `STRIPE_PRICE_VAULT_ANNUAL`, `STRIPE_PRICE_VAULT_MONTHLY`, `STRIPE_PRICE_PINHOT_ANNUAL`, `STRIPE_PRICE_PINHOT_MONTHLY` | — | Whole-TB house checkout (dashboard plan Phase 1): one recurring price per line × period, quantity = TB. All six must be set; at boot (and hourly) each is fetched and asserted against `internal/api/landing/prices.json` (amount, USD, interval; Vault monthly = volume tiers with the $4.99 minimum) — a mismatch logs an error and keeps checkout closed. Recipe in `internal/billing/CLAUDE.md`. The page itself is gated by the `quota_checkout` flag (default off); the overview's house is gated by `house_overview` (default off) |
| `STRIPE_PRICE_VAULT3`, `STRIPE_PRICE_VAULT9`, `STRIPE_PRICE_VAULT18`, `STRIPE_PRICE_VAULT36`, `STRIPE_PRICE_STANDARD` | — | Legacy pack plans registered by `registerStripePlans` in `internal/api/server.go` (labels "$2.99/mo", "$3.99/TB/mo" — retired prices). A code leftover: leave unset. Removing the table is a work package |
| `STRIPE_METER_STORAGE`, `STRIPE_METER_EGRESS` | — | Stripe Billing Meter event-names for metered tiers (both required to enable metered reporting). Metered billing is not pursued (quota-sold, no overage); leave unset |

### OAuth and email

| Variable | Default | Purpose |
|----------|---------|---------|
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | — | Google OAuth client |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | — | GitHub OAuth App |
| `EMAIL_PROVIDER` | (log) | `resend` or `smtp` selects a real sender; anything else (including unset — what prod runs today) is `LogSender`, which logs recipient, subject and body sizes only and sends nothing. `internal/email/email.go` (R1-09) |
| `EMAIL_FROM` | noreply@stored.ge | From address for every sender |
| `RESEND_API_KEY` | — | Required with `EMAIL_PROVIDER=resend`; empty falls back to the log sender with a warning |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD` | — | Required (host + port) with `EMAIL_PROVIDER=smtp`; missing falls back to the log sender with a warning |

## Operator notes

1. **Stripe needs both secrets and a pinned API version.** Setting
   `STRIPE_SECRET_KEY` alone mounts nothing: `/webhook/stripe` exists only when
   `STRIPE_WEBHOOK_SECRET` is set too. The Stripe webhook endpoint must be
   created pinned to API version **2023-08-16** (stripe-go v75 rejects every
   other version; R10-03) with the events listed in `internal/billing/CLAUDE.md`.
   The six `STRIPE_PRICE_*_{ANNUAL,MONTHLY}` prices are verified against
   `prices.json` at boot; a mismatch keeps checkout closed and logs why.
2. **iDrive regions need their own key pairs.** The primary pair only serves
   `IDRIVE_REGION`. A customer can pick another region for a bucket only when
   `IDRIVE_<REGION>_ACCESS_KEY` and `_SECRET_KEY` are set for it — the reseller
   account mints one pair per region, and a region left on the primary pair is
   a permanent 403. Generate the lines with
   `deploy/scripts/idrive-region-env.sh` and restart; the region's bucket is
   created at boot.

Tests read `DATABASE_URL` (CI) or default to the `vaultaire_test` database
(`internal/testutil`); create it with `make test-db` and never point tests at
the shared dev database — several tests drop and recreate tables.
