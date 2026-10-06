# Deployment

How stored.ge's Vaultaire is built, shipped and run. Everything here is what
`.github/workflows/ci.yml`, `.github/workflows/deploy.yml`, the root
`CLAUDE.md` *Production* section and `deploy/monitoring/README.md` describe;
where a review left a gap it is cited by id. Hostnames and SSH aliases are
deliberately not in this file — "the production host" is the one box.

## What production is

- **One host** (Ubuntu 24.04) running **one static Go binary** under systemd,
  with a local PostgreSQL 16 and a local Prometheus. There is no container
  image, no Helm chart, no worker fleet and no second region. The object bytes
  live on the storage backends (`docs/DRIVERS.md`); the box holds the
  database, the chunk/multipart staging under `/tmp`, and the `local` driver's
  `DATA_PATH`.
- **Cloudflare** proxies `stored.ge` (and the CDN host); **HAProxy** on the box
  terminates TLS with a Let's Encrypt origin certificate, redirects http to
  https, sets HSTS, and forwards to the app on `127.0.0.1:8000`. HAProxy
  appends the real peer as the *last* `X-Forwarded-For` hop — the only header
  `internal/clientip` trusts (R1-01). Its `global` section carries
  `tune.h2.initial-window-size 262144` (set 2026-10-04). HAProxy 2.8's default
  HTTP/2 window is 65,535 bytes, which caps every h2 upload at window ÷ RTT
  (~1 MB/s from Dallas) — but a large window costs the other way: at 4 MiB,
  sixteen h2 streams on one connection fell from 308 to 35 MB/s on the box
  (HAProxy 2.8 buffers per stream; 3.1 sizes windows dynamically). 256 KB
  keeps multi-stream uploads at full speed. Uploads from Cloudflare are kept
  fast by the zone setting **`origin_max_http_version = 1`** (Cloudflare
  talks HTTP/1.1 to the origin, one connection per request, no h2 window at
  all): 22–27 MB/s through `stored.ge` from the box, 58 MB/s edge → origin.
  Leave `tune.h2.max-frame-size` at its default; 1 MiB frames made things
  worse. Revisit when HAProxy ≥ 3.1 is packaged for the box.
- **UFW** allows 22, 80 and 443 only. `deploy/ufw-cloudflare-lockdown.sh` is
  the script for narrowing 80/443 to Cloudflare's ranges.
- **Configuration is the env file** loaded by the unit's `EnvironmentFile=`
  (`docs/CONFIG.md`). Nothing else is read.

### Box layout

| Path | What |
|------|------|
| `/opt/vaultaire/bin/vaultaire` | the running binary; `vaultaire.prev` beside it is the previous release for rollback |
| `/opt/vaultaire/configs/.env` | the env file (`EnvironmentFile=`); also where the deploy job reads `DB_PASSWORD` for migrations |
| `/opt/vaultaire/data` | `DATA_PATH` of the `local` driver |
| `/opt/vaultaire/backups/` | nightly PostgreSQL dumps (below) |
| `/opt/vaultaire/monitoring/ntfy-bridge.py` | Alertmanager → ntfy push bridge |
| `/etc/systemd/system/vaultaire.service` | the unit. Stop timeout is the distro default (90 s) and the unit still `Wants=redis-server` — WP-R1-1 sets `TimeoutStopSec=45` and drops Redis |
| `/etc/prometheus/rules/*.yml` | alert rules (installed by hand from `deploy/monitoring/`) |
| `/etc/haproxy/haproxy.cfg`, `/etc/haproxy/certs/*-le.pem` | edge config and the origin cert the certbot deploy hook rebuilds |

The deploy user's sudo is limited to `systemctl {stop,start} vaultaire`,
`mv /tmp/vaultaire-linux …` and `chmod` — exactly the commands the pipeline
runs.

## The pipeline: push to `main`

`main` is protected: no direct pushes, CI must pass, merges are squash merges
(`gh pr merge --squash --delete-branch`; never `--admin`). A merge to `main`
triggers `deploy.yml`; a merge to `develop` runs the same job against the dev
server. `workflow_dispatch` is the manual lever (`gh workflow run deploy.yml
--ref main`) for a dropped trigger or a DR re-deploy. Deploys are queued per
branch and never cancelled mid-flight.

```
build   GOOS=linux GOARCH=amd64 go build ./cmd/vaultaire  (Go 1.25)  -> artifact
deploy  scp binary + internal/database/migrations/ to /tmp on the host, then over ssh:
        1. migrate   for f in $(ls /tmp/vaultaire-migrations/*.sql | sort); do
                        psql -w -v ON_ERROR_STOP=1 -h 127.0.0.1 -U vaultaire -d vaultaire -f "$f" </dev/null
                     done
        2. swap      sudo /usr/local/sbin/vaultaire-switch deploy /tmp/vaultaire-linux
                     = install into the idle slot, start it, wait for /health/live,
                       switch HAProxy to it, drain and stop the old slot
        3. rollback  manual: sudo /usr/local/sbin/vaultaire-switch rollback
                     (the previous build still sits in the other slot)
```

Points that matter:

- **Migrations are a sorted `psql` loop**, every file every deploy. There is no
  Go runner and no `schema_migrations` table; every migration must be
  idempotent (`CREATE … IF NOT EXISTS`). `-w` plus `</dev/null` makes a
  missing password fail instead of prompting — a prompt once ate the rest of
  the script as password attempts and skipped the binary swap while the job
  still exited 0. `ON_ERROR_STOP` makes a broken migration fail the deploy
  before the swap. Schema and runner rules: `docs/DATABASE.md`. During the
  drain window both builds run against the migrated schema, so a migration
  must stay additive (it always had to be: a rollback runs the old binary on
  the new schema too).
- **The gate is liveness, not readiness.** `/health/live` answers "does the
  new process boot and serve HTTP". `/health` reflects backend probes, which a
  rollback cannot fix — the old binary would report the same.
- **A dead-on-arrival build never takes traffic.** The switch script starts
  it in the idle slot and only moves HAProxy once it answers; otherwise it
  stops the slot, prints the journal and exits 1 — the job fails loudly and
  the active slot was never touched.
- The deploy runs under the GitHub `production` environment; its SSH key, host
  and user are environment secrets deployable only from `main`.
- **Docs-only pushes do not deploy** (`paths-ignore` on `deploy.yml`, 2026-10-06):
  `docs/`, `.private/`, `bench-results/`, `deploy/`, `cmd/tools/`, `tests/`,
  the `CLAUDE.md` files, `*_README.md`, lint/pre-commit config. The
  `go:embed`ed `internal/api/*.md` and the dashboard templates are NOT ignored.
  PRs merge through the **merge queue** (`merge_group` in `ci.yml`, the rule
  on the main ruleset): `gh pr merge --auto` enqueues; the PR is tested on the
  merge branch and squash-merged there, so strict status checks hold without
  every merge forcing every open PR to rebase.

### Zero-downtime deploys (2026-10-06)

Until 2026-10-06 a deploy was `systemctl stop; mv; systemctl start`: every
merge to `main` — docs included — dropped the service for ~15 s and every
upload in flight with it. Now the box runs **two slots**:

```
vaultaire@8000  "blue"    /opt/vaultaire/bin/vaultaire-8000     one of them active,
vaultaire@8001  "green"   /opt/vaultaire/bin/vaultaire-8001     the other stopped
/opt/vaultaire/ACTIVE_PORT        which one (the switch script's state)
/opt/vaultaire/bin/vaultaire      symlink to the active slot's binary
```

- **Unit**: `deploy/systemd/vaultaire@.service` → `/etc/systemd/system/vaultaire@.service`
  (`ExecStart=/bin/sh -c 'PORT=%i exec /opt/vaultaire/bin/vaultaire-%i'` — the
  slot's port must win over the `PORT` in `.env`, and `EnvironmentFile=`
  overrides `Environment=`). Exactly one slot is enabled at boot; the switch
  script enables the active one and disables the other. The old single
  `vaultaire.service` is stopped and disabled.
- **HAProxy**: every backend (`s3_backend`, `api_backend`, `dashboard_backend`)
  lists `server blue 127.0.0.1:8000 …` and `server green 127.0.0.1:8001 …`, the
  idle one with `disabled`; a `frontend metrics_internal` on `127.0.0.1:8010`
  routes to `api_backend` so Prometheus (`targets: ['localhost:8010']`) always
  scrapes the active slot, whichever it is. The admin socket
  (`/run/haproxy/admin.sock`, level admin) is what the script drives.
- **The switch** (`deploy/scripts/vaultaire-switch`, installed at
  `/usr/local/sbin/vaultaire-switch`, root:root 0755; the deploy user may run
  it through `deploy/sudoers/vaultaire-deploy`): `deploy <binary>` installs
  into the idle slot, `systemctl restart vaultaire@<idle>`, waits up to 90 s
  for `/health/live`, then `set server <be>/<new> state ready` + `health up`
  on the three backends, waits for HAProxy to report it UP, sets the old
  server to `drain` (no new connections, open ones finish), rewrites
  `haproxy.cfg` so a reload lands in the same state (validated with
  `haproxy -c` first; a backup is `haproxy.cfg.bak-switch`), writes
  `ACTIVE_PORT`, moves the symlink, flips boot enablement, waits up to 10 min
  for the old slot's sessions to reach 0, sets it `maint` and stops it.
  `rollback` is the same switch towards the idle slot, whose binary is the
  previous build. `status` prints both slots and what HAProxy says.
- **Why one active slot, not two**: the pending-TOTP secret of an MFA enrolment
  and the credential cache are per process, the egress month counter is one
  per process, and the synthetic check would run twice — none of that is safe
  with two instances taking traffic. During the drain window the old slot only
  finishes what it already had.
- **Cutover record (2026-10-06, 16:48–16:50 UTC)**: the template, script and
  sudoers installed; `vaultaire-8000` and `vaultaire-8001` copied from the
  running binary; HAProxy backends and the metrics frontend added and reloaded
  (the first edit anchored on `default_backend s3_backend` in the stats block
  and left an invalid file on disk — the running process was unaffected; it was
  restored from the backup and redone on the `^backend` line); Prometheus
  target moved to `:8010`; `vaultaire-switch deploy` run twice with the current
  build — the first started `vaultaire@8001`, switched, drained and stopped the
  legacy `vaultaire.service` (which held `:8000`), the second went back to
  `vaultaire@8000`; 8.5 s each, 0 non-200 answers on a 1 s probe of
  `stored.ge/health/live` across the first. #605 and #604 then deployed
  through the script (9 s from install to active).

### CI (`ci.yml`)

Every push and PR: PostgreSQL 15 service container, all migrations applied
with `ON_ERROR_STOP`, `go build ./...`, `go test -race ./...` (with
`DATABASE_URL` and `JWT_SECRET`), golangci-lint v2.4.0, then a smoke boot that
must answer `/health/live` and `/status`. A second job drives the landing
page's house builder in headless Chrome. The Security workflow (`security.yml`)
runs gosec (green since Review R15; `make gosec` is the same command), Trivy
and govulncheck. There is no nightly workflow any more (its unauthenticated
benchmarks measured nothing — `docs/SCALE_TESTING.md`); the load gate is
`tests/load/`.

## Backups

A cron job at **03:00 UTC** runs `/opt/vaultaire/bin/pg-backup.sh`: `pg_dump |
gzip` of the database plus a tarball of `configs/` and the Prometheus rules,
into `/opt/vaultaire/backups/`, **7 files retained**, all **on the box**. The
dump is plain SQL, so a restore is `zcat <dump> | psql`, not `pg_restore`
(and needs psql ≥ 16.10 for the `\restrict` header). Nothing leaves the host
yet: off-box encrypted copies, `-Fc` format, `chmod 600` and a written restore
runbook are WP-R9-7 (`docs/reviews/R9-database.md`, R9-12). A restore loses
whatever was written only to local disk since the dump (`DATA_PATH`
containers, multipart staging, and up to 24 h of head-cache/quota/chunk-ref
rows).

## Monitoring

```
app :8000/metrics  <- Prometheus (:9090, on the box, 10 s scrape)
                        └─ rules /etc/prometheus/rules/*.yml
                             └─ Alertmanager (127.0.0.1:9093, clustering off)
                                  └─ ntfy-bridge (127.0.0.1:9095)  ->  ntfy.sh topic  ->  phone
```

- `/metrics` is served by the app on the same port as everything else; it is a
  real Prometheus registry (`internal/api/prom_metrics.go`) with Go/process
  collectors, `vaultaire_requests_total` / `_errors_total` (5xx),
  `vaultaire_backend_health{backend}` and the probe/breaker/write-failure
  series, the auth-failure series (`vaultaire_auth_failures_total{reason,key_known}`),
  the dashboard sign-in counters and the TLS expiry gauge. It should be denied
  at HAProxy (WP-R1-1, still open).
- Rules live in `deploy/monitoring/`: `vaultaire-backends.yml` (probe
  failures, write failures, breaker open, 5xx ratio), `vaultaire-auth.yml`
  (credential-stuffing signal, not yet installed), `vaultaire-tls.yml`
  (origin cert < 14 d / < 7 d). The original `VaultaireDown`,
  `VaultaireHighErrorRate` and node rules exist only on the box. Rules are
  **not** deployed by the pipeline: `sudo install -m 0644 <file>
  /etc/prometheus/rules/ && promtool check rules … && sudo systemctl reload
  prometheus`.
- Backend probes are authenticated (signed HeadBucket, Lyve console with the
  `LYVE_PROBE_*` root pair) so a dead key pages; a TCP dial never did. Probes
  feed metrics and `/health*` only — they never change routing.
- Alert delivery is push-only (ntfy app or `https://ntfy.sh/<topic>`; the
  topic name is the access control and lives in `/etc/default/ntfy-bridge`,
  not in git). The Alertmanager UI is loopback-only — reach it through an SSH
  tunnel. Full install and test recipe: `deploy/monitoring/README.md`.

## Troubleshooting

| Question | Where to look |
|----------|---------------|
| Is the process up? | `curl -s https://stored.ge/health/live` (200 = serving); `systemctl status vaultaire` |
| Is it ready / which backends are healthy? | `curl -s https://stored.ge/health` — JSON with per-backend probe state and counts; `curl -s localhost:8000/health/backends` on the box for the detailed list (keep it off the public edge); `/health/ready` for the readiness verdict; `/status` is the HTML page |
| What is running? | `curl -s https://stored.ge/version` — still the hard-coded `0.1.0` until WP-R1-3 stamps the commit at build |
| Logs | `journalctl -u vaultaire -f` (Zap JSON; request ids in `X-Request-Id` / the S3 `RequestId`). Password-reset bodies are no longer logged (R14-01) |
| Metrics right now | `curl -s localhost:8000/metrics \| grep vaultaire_backend_health` on the box; `localhost:9090` for Prometheus, `curl -s localhost:9090/api/v1/rules` for loaded rules |
| Deploy did not run | `gh run list --workflow deploy.yml`; `gh workflow run deploy.yml --ref main` to re-trigger |
| Deploy rolled back | the job log says which gate failed; `journalctl -u vaultaire --since -10m` for the new binary's boot error (a missing `JWT_SECRET`, a migration that broke a query the new code needs) |
| Roll back by hand | repeat the pipeline's own steps: `sudo systemctl stop vaultaire; cp /opt/vaultaire/bin/vaultaire.prev /tmp/vaultaire-linux; sudo mv /tmp/vaultaire-linux /opt/vaultaire/bin/vaultaire; sudo chmod +x …; sudo systemctl start vaultaire` |
| Backend key dead | `vaultaire_backend_health{backend="…"}` is 0 and `BackendProbeFailing` fires; rotate the key in the env file and `sudo systemctl restart vaultaire` (env is read at boot only) |
| Cert expiry alert | `sudo certbot renew --cert-name <sni> --force-renewal`; `--dry-run` is a false signal on this box (staging CA) |
| Change a feature flag | `PUT /api/v1/admin/flags/{key}` with an admin JWT, or the dashboard `/admin/flags`; ~15 s to take effect, no restart |

Never `pkill` by pattern on the host; the unit is the only supervisor.
