# deploy/monitoring

Alert delivery for the SLC production box (launch sequence 5.2). Prometheus
already evaluates the rules in `/etc/prometheus/rules/vaultaire-alerts.yml`
(VaultaireDown, VaultaireHighErrorRate, disk/memory/CPU, NodeDown); these
files make the alerts actually reach a human.

## Pipeline

```
Prometheus (:9090)
  └─ alerting → Alertmanager (127.0.0.1:9093, clustering disabled)
       └─ webhook → ntfy-bridge (127.0.0.1:9095, python3 stdlib)
            └─ ntfy.sh JSON publish → phone/browser push on the topic
```

The ntfy topic name is the only access control on a public ntfy.sh topic, so
it is **not** committed. It lives in `/etc/default/ntfy-bridge` on the server
(`NTFY_TOPIC=...`), which systemd injects via `EnvironmentFile`.

## Files

| File | Installs to | Purpose |
|------|-------------|---------|
| `alertmanager.yml` | `/etc/prometheus/alertmanager.yml` | route → ntfy webhook receiver, send_resolved, critical-inhibits-warning |
| `ntfy-bridge.py` | `/opt/vaultaire/monitoring/ntfy-bridge.py` | Alertmanager webhook → readable ntfy push (UTF-8-safe JSON publish) |
| `ntfy-bridge.service` | `/etc/systemd/system/ntfy-bridge.service` | sandboxed systemd unit (DynamicUser) |
| `vaultaire-routing.yml` | `/etc/prometheus/rules/` | routing truth (WP-R7-5): head rows whose bytes the recorded backend does not have (warn / page on a jump), rows on a backend no driver is registered under (info), two names on one store (page), reads of such rows (warn) |
| `vaultaire-backup.yml` | `/etc/prometheus/rules/` | BackupStale / BackupOffboxStale (critical) and BackupRunIncomplete (warning, Prompt 2a.2) — > 26 h or series absent — on the three timestamps `deploy/scripts/pg-backup.sh` writes into its own directory `/opt/vaultaire/metrics/` (user1), which node_exporter reads through root-owned symlinks (see "Backup metrics" below). Installed 2026-10-08; the run timestamp + symlink layout are the 2a.2 change |
| `vaultaire-longops.yml` | `/etc/prometheus/rules/` | LongOpFailingAfterCommit (warning: ≥ 3 `<Error>` documents inside a committed 200 in 15 min, per op) and LongOpsAbandonedAtShutdown (warning: a stopping slot cancelled a CompleteMultipartUpload / CopyObject / DeleteObjects still running 15 min after its start) on `vaultaire_s3_long_op_total{op,outcome}` / `vaultaire_s3_long_ops_abandoned_total{op}` (`internal/api/s3_long_op.go`, #635). Since Prompt 2a.2 G1 (#644) also LongOpsFailedDuringDrain (warning: `vaultaire_s3_long_ops_drain_errors_total{op}`). Both shutdown counters are exported by the ACTIVE slot from `s3_long_op_incidents` (migration 082), which the stopping slot writes before its DB closes — the stopping slot itself is never scraped, so the in-process counter of #635 could not fire. Installed on SLC 2026-10-09 (sha256 = repo) |
| `vaultaire-multipart.yml` | `/etc/prometheus/rules/` | MultipartStagingRefusing (warning): an UploadPart was answered 503 SlowDown because the staging filesystem (prod: the 61 GiB tmpfs `/tmp`) would fall below `MULTIPART_STAGING_MIN_FREE` (`vaultaire_multipart_staging_refusals_total`, WP-R10-8 Part C). Parts are reserved against each tenant's quota since then; this guard bounds the sum across tenants. Installed on SLC 2026-10-14 (sha256 = repo; 48 rules load) |
| `vaultaire-drivers.yml` | `/etc/prometheus/rules/` | a backend that does not answer (WP-R6-3, Stage 1a Prompt 9): DriverHedgesWinning (warning: more than one in twenty hedges of a driver's small calls answered first over 15 min, ≥ 3 wins — its edge is stalling; `vaultaire_driver_hedges_total` / `_hedges_won_total`) and DriverOpSlow (warning: the primary's p99 of `put` or `get_first_byte` above 10 s for 15 min on `vaultaire_driver_op_seconds`). **Not installed by the deploy** (`deploy/**` is hand-installed): the owner recipe in "Installing a rules file" below |
| `vaultaire-alerts.yml` | `/etc/prometheus/rules/` | the box's original rules (installed 2026-04-04, no source in the repo until Prompt 2b.2 C5 — copied from the box, sha256 `c40d860b…`): VaultaireDown / NodeDown (critical, `up{job=…} == 0` for 1 min), VaultaireHighErrorRate (warning, `rate(vaultaire_errors_total[5m]) > 0.1` for 5 min), DiskSpaceLow / DiskSpaceCritical (< 10 % / < 5 % free on `/`), HighMemoryUsage (> 90 %), HighCPUUsage (> 90 % for 10 min) |
| `vaultaire-backends.yml`, `vaultaire-auth.yml`, `vaultaire-tls.yml`, `vaultaire-synthetic.yml`, `vaultaire-egress.yml`, `vaultaire-jobs.yml` | `/etc/prometheus/rules/` | alert rules: backend probes / auth failures / origin cert / customer-path canary / egress throttle / background jobs (Review R13 + WP-R10-9 + WP-R13-3 — install all six, checklist item 10). On SLC (checked 2026-10-11): `alerts`, `backends`, `backup`, `jobs`, `longops`, `tls`, `auth` and `egress` are installed (sha256 = repo, 42 rules); `routing` is held back (it fires on the WP-R7-5 backlog — see below) and `synthetic` waits for `SYNTHETIC_CHECK_URL` |

## Backup metrics (node_exporter textfile collector)

**Verified on Ubuntu 26.04.1 (2026-10-10, after the in-place upgrade):**
nothing in the collector changed — the textfile directory, its ownership and
the two `vaultaire_backup_*` series read as before, and the 12:29 UTC backup
run on PostgreSQL 18 updated both; Prometheus loads the rule files from
`/etc/prometheus/rules/*.yml`. **Installed vs repo on 2026-10-10** (sha256):
`vaultaire-alerts`, `-backends`, `-backup`, `-jobs`, `-longops`, `-tls` =
repo; 35 rules load. `vaultaire-jobs.yml` had **never been installed** until
13:48 UTC that day (Prompt 2b.3 D2 put it there) — `JobStale`,
`PeriodicJobStale`, `JobFailing`, `AccountDeletionDeferred` and the stale-copy
rule were not live before.

**2026-10-11 (Prompt 2b.4 E4.1):** `vaultaire-auth.yml` and
`vaultaire-egress.yml` installed (sha256 = repo; 35 → 41 rules), then
`vaultaire-backends.yml` re-installed with `SyncPartialUnavailable` (42).
None of the new rules fired in the hours after. (`BackendWriteFailures` and
`BackendCircuitOpen` did, from 03:50 UTC — the iDrive key's object calls
answering AccessDenied, found by the Prompt 2b.4 E3 bench; cleared once the
primary moved to `lyve` at 12:45.) **`vaultaire-routing.yml` is held back**
(on the box as `/root/vaultaire-routing.yml.held-2b4`): installed at 03:08 UTC
it fired at once — `RoutingTruthMissing` on `idrive` and `local` (missing
ratio 1: the rows of the replaced iDrive reseller account and the old `local`
rows) and `RoutingUnknownBackendRows` pending (2,613 + 3 `onedrive` rows, 62
NULL). That is the known WP-R7-5 backlog, not a new fault; installed, it is a
warning in ntfy forever. Install it after the owner's routing-truth backfill
(`docs/reviews/WP-R7-5.md`, bench tenants → erase). `vaultaire-synthetic.yml`
stays out until `SYNTHETIC_CHECK_URL` is set (its absent-series rule fires).

`pg-backup.sh` runs as user1 (cron 03:00 UTC) and writes three `.prom` files —
`vaultaire_backup_{,offbox_,run_}last_success_timestamp_seconds.prom` — into
**its own directory `/opt/vaultaire/metrics/`** (user1, 0755; files 0644 by
atomic rename). node_exporter 1.7.0 on the box reads ONE textfile directory
(`--collector.textfile.directory`, default `/var/lib/prometheus/node-exporter`,
not repeatable in that version), so that directory holds one root-owned
**symlink** per backup file; node_exporter follows them (it opens each `*.prom`
entry by path). The textfile directory itself goes back to `root:root 0755`:
until 2a.2 it was `root:user1 0775` without the sticky bit, so the app user
could replace the root-owned `apt.prom` / `nvme.prom` / `ipmitool_sensor.prom`.

Install (once, as root on slc-vaultaire-01; the script itself goes to
`/opt/vaultaire/bin/pg-backup.sh` as before):

```bash
sudo -u user1 install -d -m 0755 /opt/vaultaire/metrics
# carry the current timestamps over so nothing reads as absent until 03:00
for m in vaultaire_backup_last_success_timestamp_seconds vaultaire_backup_offbox_last_success_timestamp_seconds; do
  sudo -u user1 cp -p /var/lib/prometheus/node-exporter/$m.prom /opt/vaultaire/metrics/$m.prom
done
for m in vaultaire_backup_last_success_timestamp_seconds vaultaire_backup_offbox_last_success_timestamp_seconds vaultaire_backup_run_last_success_timestamp_seconds; do
  ln -sfn /opt/vaultaire/metrics/$m.prom /var/lib/prometheus/node-exporter/$m.prom
done
chown root:root /var/lib/prometheus/node-exporter && chmod 0755 /var/lib/prometheus/node-exporter
install -m 0755 -o user1 -g user1 deploy/scripts/pg-backup.sh /opt/vaultaire/bin/pg-backup.sh
install -m 0644 deploy/monitoring/vaultaire-backup.yml /etc/prometheus/rules/
promtool check rules /etc/prometheus/rules/vaultaire-backup.yml && systemctl reload prometheus
# one run now: writes the run timestamp the new symlink points at
sudo -u user1 /opt/vaultaire/bin/pg-backup.sh; tail -4 /opt/vaultaire/logs/backup.log
```

Without that hand run `vaultaire_backup_run_last_success_timestamp_seconds` is
absent until 03:00 UTC: `BackupRunIncomplete` fires 10 min after the rules
reload, and the dangling symlink sets `node_textfile_scrape_error 1` (the
collector skips a file it cannot open).
Check: `curl -s 127.0.0.1:9100/metrics | grep -E '^vaultaire_backup|node_textfile_scrape_error'`.

## Install (already done on slc-vaultaire-01, 2026-08-03)

```bash
apt-get install -y prometheus-alertmanager
# Single-node box with only public IPs: gossip clustering must be disabled
# or Alertmanager exits at boot ("no private IP found").
cat > /etc/default/prometheus-alertmanager <<'EOF'
ARGS="--cluster.listen-address= --web.listen-address=127.0.0.1:9093"
EOF
install -m 0755 ntfy-bridge.py /opt/vaultaire/monitoring/ntfy-bridge.py
install -m 0644 ntfy-bridge.service /etc/systemd/system/ntfy-bridge.service
install -m 0644 alertmanager.yml /etc/prometheus/alertmanager.yml
printf 'NTFY_TOPIC=%s\n' "vaultaire-slc-$(openssl rand -hex 6)" > /etc/default/ntfy-bridge
chmod 600 /etc/default/ntfy-bridge
systemctl daemon-reload
systemctl enable --now ntfy-bridge
systemctl restart prometheus-alertmanager
```

Prometheus needs no change — `alerting: alertmanagers: localhost:9093` was
already in `/etc/prometheus/prometheus.yml`.

## Subscribing (the human end)

Install the ntfy app (Android/iOS/desktop, https://ntfy.sh) and subscribe to
the topic from `/etc/default/ntfy-bridge` on the server, or watch it in a
browser at `https://ntfy.sh/<topic>`.

## Testing

Synthetic alert through the full Alertmanager → bridge → ntfy chain:

```bash
curl -XPOST http://localhost:9093/api/v2/alerts -H 'Content-Type: application/json' \
  -d '[{"labels":{"alertname":"AlertDeliveryTest","severity":"critical"},
        "annotations":{"summary":"delivery test","description":"ignore"}}]'
```

Real end-to-end (VaultaireDown; ~90s of prod downtime):
`sudo systemctl stop vaultaire@$(cat /opt/vaultaire/ACTIVE_PORT)` (the active
slot; HAProxy then has no server), wait for the push (10s scrape + 1m `for:` +
10s group_wait ≈ 85s), `sudo systemctl start vaultaire@$(cat /opt/vaultaire/ACTIVE_PORT)`. Verified 2026-08-03:
delivered at ~85s, RESOLVED notice followed after restart.

## Alert rules in this directory

`vaultaire-backends.yml` — backend-outage rules (BackendProbeFailing,
PrimaryBackendProbeFailing, LyveProbeFailing, BackendWriteFailures
warning/critical, BackendCircuitOpen warning + critical on the primary,
VaultaireServerErrorRatio). They key off the `vaultaire_backend_*` series the
app exports from `/metrics` (`internal/api/prom_metrics.go`) and
`vaultaire_primary_backend{backend}` (1 for the write primary — the rules
join `on(backend)`); the probes behind `vaultaire_backend_health` are
AUTHENTICATED (signed HeadBucket for idrive/geyser via the driver, console
`RSCustomerDetails` for lyve — root key, one 403 retry), so a dead access key
fires them. A TCP dial never did.

**Object canary (Prompt 2b.5 F2.1).** A key can pass HeadBucket and
ListObjects and be refused every object call — iDrive on 2026-10-11, 9+ hours
with `/health` "healthy". Every S3-class probe (`idrive`, `idrive-<region>`,
`wasabi`, `lyve`, `r2`, `s3compat`) therefore also HEADs
`t-_probe/_probe/<hostname>` (reserved tenant `_probe`; written on the first
probe when absent, re-written once if it disappears, then the probe fails;
a HEAD 403 — S3's answer for an absent key to a credential without
ListBucket — gets the one PUT too, which a dead key refuses);
any error is `vaultaire_backend_health` 0, `vaultaire_backend_probe_failures_total`
+1 and the SDK error in the Warn line and `last_error`. Never on `geyser`
(tape) or `permafrost`; `sync` keeps its PROPFIND. **Cost:** one HEAD per
probe (30 s; 60 s on Lyve's console probe) per backend ≈ 86,400 requests a
month per backend, below every vendor's free request floor, plus one tiny
object each. `/health` also reports `degraded` with a `reason` while the
PRIMARY's breaker is open (it never answers 503 — HAProxy checks it).

**Client bodies are the client's (WP-R2-3).** An upload whose body the client
did not finish (it stopped mid-body, Cloudflare cut a > 100 MB single PUT, the
aws-chunked framing broke off) is answered 400 `IncompleteBody` (408 for a
body below Sync's minimum rate), never a 5xx, never a breaker charge, and is
counted in `vaultaire_client_body_errors_total{op=PutObject|UploadPart,
reason=short|gone|framing|too_slow}`. **No rule pages on it**, on purpose:
it is client traffic. Read it when a customer reports failed uploads or
when `VaultaireServerErrorRatio` is quiet but uploads fail: a step in
`reason="gone"` is a proxy or a client timing out (compare Cloudflare's 413s
and 524s), `reason="framing"` is one client's broken aws-chunked encoder.
Before Prompt 2b.2 C1 five of them in a minute could open the primary's
breaker (R2-09); until WP-R2-3 the chunked PUT still answered them 500, an
Error line and a 5xx count each (R108-9).

**A backend that does not answer (WP-R6-3).** Every first-byte bound on
the data path is ≤ 30 s (`internal/drivers/CLAUDE.md`, "The bound
invariant"): a HEAD, DELETE or listing page 15 s, a GET's first byte 25 s,
a PUT its per-size deadline (10 s floor), a Complete 10 min, Geyser 5 min.
The probe's HeadBucket and the canary's HeadObject, every HeadObject/Exists,
listing page and DeleteObject on an S3-class driver is **hedged**: sent once
more on a fresh connection after `DRIVER_HEDGE_AFTER` (2 s), first answer
wins (`vaultaire_driver_hedges_total{driver,op}`, `_hedges_won_total`;
rule DriverHedgesWinning). Every driver call made through the engine is
timed in `vaultaire_driver_op_seconds{driver,op}` (head, get_first_byte,
put, delete, list, probe; a GET to its first byte, a PUT without the
client's body time; rule DriverOpSlow on the primary).
**`vaultaire_backend_health` is 0 after TWO failed probes in a row** (≥ 30 s
of silence across two probes) and 1 again after one success; one failed
probe still counts in `vaultaire_backend_probe_failures_total` and shows its
error in `/health/backends` (`last_error`) and the Warn line, but no longer
makes `/health` `degraded`. On 2026-10-17 most of Wasabi's 199
`backend health check failed` lines in 2.7 days were single failures
(`docs/reviews/WP-P9.md` has the count).

The original rules (`VaultaireDown`, `VaultaireHighErrorRate`, node rules)
live only on the box in `/etc/prometheus/rules/vaultaire-alerts.yml`.
`vaultaire_errors_total` counts 5xx responses since 2026-09-22 (it was a
never-incremented stub before), so `VaultaireHighErrorRate` can now fire.

### Adding / updating a rule

1. Edit the YAML here, merge (rules are not deployed automatically).
2. On the box:
   ```bash
   sudo cp deploy/monitoring/vaultaire-*.yml /etc/prometheus/rules/
   sudo promtool check rules /etc/prometheus/rules/*.yml
   sudo systemctl reload prometheus      # SIGHUP re-reads rule_files
   curl -s localhost:9090/api/v1/rules | jq '.data.groups[].name'
   ```
3. Every rule needs `labels.severity` + `annotations.summary/description` —
   the ntfy bridge builds the push from exactly those.

### Env knobs for the Lyve probe

`LYVE_PROBE_ACCESS_KEY` / `LYVE_PROBE_SECRET_KEY` (the account ROOT key — the
console action is root-only) and `LYVE_PROBE_CUSTOMER` (default `v01`). There
is deliberately **no fallback** to `LYVE_ACCESS_KEY`/`LYVE_SECRET_KEY`
(Review R7-19): the data-plane pair is meant to be the scoped `vaultaire-prod`
IAM user, which the console refuses, so a fallback would turn that key
rotation into a false alert. Without the probe pair the Lyve probe is the
driver's signed `HeadBucket` on `stored-<LYVE_REGION>`; only when no Lyve
driver is registered does it degrade to the old TCP dial.

`BackendProbeFlapping` (warning, Prompt 2b.5 F3.11): more than 10 failed
probes of one backend in an hour. Measured on prod 2026-10-09..11: the
`wasabi` HeadBucket timed out at the 15 s probe timeout 1–6 times an hour
(`vaultaire_backend_probe_failures_total{backend="wasabi"}` +35 in 11 h,
latency otherwise 33 ms) and health never stayed 0 for 5 minutes, so nothing
fired — while wasabi, third in the STANDARD write candidates, took 185
fall-over chunk PUTs during the iDrive outage.

Also probed since R7: every `idrive-<region>` driver that has its own
`IDRIVE_<REGION>_ACCESS_KEY`/`_SECRET_KEY` pair (signed HeadBucket, starts
staggered across one 30 s interval — regions running on the primary pair are
a known 403 and are skipped), and `permafrost` (authenticated Graph call on
one rotating fleet account; `PermafrostProbeFailing` is a 15-minute warning,
and `BackendProbeFailing` excludes it).

`vaultaire-jobs.yml` — the background jobs (WP-R13-3). Five rules — the fifth,
`StaleCopyLostWrite` (critical, WP-R13-2), fires when the delete of a stale
copy of an object removed bytes a write of the same key had just stored (the
log line names tenant, bucket and key; the customer must upload again). The
other four:
`JobStale` (a daily job — `inventory`, `dedup_gc`, `retention`,
`account_deletion`, `smart_demotion` — with no success in 36 h, `for: 30m`),
`PeriodicJobStale` (an hourly job, or the 5-minute access-log delivery, with
none in 3 h), `JobFailing` (three failed runs of one job in 6 h) and
`AccountDeletionDeferred` (the erasure runner could not finish a tenant whose
grace period ended). The staleness rules read
`vaultaire_job_last_success_timestamp_seconds{job_name}`, which the app reads from
the `job_runs` table on every scrape (15 s cache) — so it is right the moment
a new process starts. It replaces `vaultaire_retention_last_run_timestamp_seconds`
and `vaultaire_account_deletion_last_run_timestamp_seconds`, which were set
only after a run in the same process and read 0 after every deploy:
`RetentionJobStale` (`(time() - gauge) > 48h`, `for: 0m`, formerly in
`vaultaire-synthetic.yml`) was true after every deploy until 03:30 UTC. A job
that has never succeeded reads 0 and goes stale after the rule's `for`; a job
this process does not run (smart demotion without both backends) exports no
series. `vaultaire_job_runs_total{job_name,outcome}` starts at 0 for every outcome
at boot, so the first failure after a restart counts. Triage:
`GET /api/v1/admin/jobs` (admin JWT) is `job_runs` as JSON — last outcome,
error text or note, rows, next run; `POST /api/v1/admin/jobs/<job>/run`
starts one run (202, 409 while one is running). The label is `job_name`, not
`job`: Prometheus sets `job` on every scraped series (the scrape job,
`vaultaire` here) and stores a scraped `job` label as `exported_job`, so a
rule on `{job="retention"}` would never match. `internal/api/job_rules_test.go`
checks that every job the server registers is named in exactly one staleness
rule and that every series the file reads is exported.

`dedup_gc` (RP8-1): a backend whose chunks cannot be asked (a dead key, an
open breaker, no driver) is one Warn per run, not one Error per chunk; the
count is `vaultaire_dedup_gc_unswept_chunks{backend}`, read from
`job_runs.result`. No rule pages on it: the run stays `ok` and the chunks stay
candidates. A count that does not fall after the backend is back is worth a
look (`POST /api/v1/admin/jobs/dedup_gc/run`).

`vaultaire-routing.yml` — routing truth (WP-R7-5). `object_head_cache.backend_name`
is where an object's bytes are supposed to be; the daily `routing_truth` job
(05:30 UTC) samples rows per backend and asks the RECORDED backend only.
`RoutingTruthMissing` (warning) fires when the last run found any sampled row
whose bytes the backend does not have; `RoutingTruthMissingJump` (critical) when
a backend's missing share is a tenth higher than yesterday's run (`offset 25h`)
— a lost disk, a wiped bucket, a dead account (prod, 2026-09-21: the reseller
account changed and 2,007 `idrive` rows pointed at the old one; a known backlog
is the standing warning, not a daily page). Both read
`vaultaire_routing_truth_last_run_missing_ratio{backend}`, which the app reads
from `job_runs.result` on every scrape — right after a restart too. A backend
that could not be asked (an open breaker, a NoSuchBucket, a timeout) is an
`error`, never a miss. `RoutingUnknownBackendRows` (info, `for: 1h`) reads
`vaultaire_routing_unknown_backend_rows{table,backend}` — rows whose backend no
driver is registered under, per table, read from the database; `backend=""` is
a row with none. `RoutingSharedStore` (critical): two registered names write
into one store, which makes every stale-copy delete (WP-R13-2) a delete of a
live object. `RoutingUnknownBackendReads` (warning): customers are reading rows
that route to no driver. Triage: `GET /api/v1/admin/routing-truth` (admin JWT),
`journalctl -u 'vaultaire@*' | grep 'routing truth'`, the plan in
`docs/reviews/WP-R7-5.md`. `internal/api/job_rules_test.go` checks the series.

`vaultaire-egress.yml` — the egress allowance throttle (WP-R10-9): one
info-level rule, `EgressThrottleActive` (`vaultaire_egress_throttled_tenants > 0`
for 10 min) — information, not an outage: that many tenants are past their
monthly allowance and downloading at their capped rate until the 1st (UTC).
The series (`vaultaire_egress_{throttled_tenants,throttle_engaged_total{surface},would_throttle_total{surface},throttle_rejected_total{surface},throttled_bytes_total}`,
`internal/api/egress_writer.go`) carry no tenant label; the tenant is in the
`egress allowance spent` log line and on `/admin/tenants/{id}`. While the
`egress_throttle` flag is off, `would_throttle_total` is the dry run: it
counts the responses that would have been paced. Exempt a tenant (the
synthetic-check tenant, demos) with a per-tenant flag row, flag off.

`vaultaire-auth.yml` — credential-attack signal (Review R11-10, pre-launch
checklist item 3): `AuthFailuresAgainstRealKey` (critical, >1/s for 5 min
against one `key_hash`), `AuthFailuresKnownKeysElevated` (warning) and
`AuthFailuresUnknownKeyStorm` (warning, scanner noise far above the
background). Keys off `vaultaire_auth_failures_total{reason,key_known}` and
`vaultaire_auth_failures_by_key_total{key_hash}` (`internal/api/auth_metrics.go`):
`key_known="true"` means the access key id EXISTS (settled by a lookup even on the presign path, whose verifier answers Expired/TooSkewed before it looks the id up — #519) — a stuffing run against a
real customer key moves that series, scanners stay on `"false"`; `key_hash` is
sha256(id)[:8], emitted for known keys only, so cardinality is bounded by the
number of real keys and the id never appears in a label. To map a hash back:
`SELECT access_key FROM tenants` / `key_id FROM api_keys`, hash each. The same
file carries the dashboard sign-in rules (Review R12):
`DashboardLoginFailuresElevated` (warning, >0.5/s over 10 min) off
`vaultaire_dashboard_login_failures_total{reason}` (reason = bad_password |
unknown_user | locked | bad_code | replayed_code) and
`DashboardAccountLockouts` (warning, ≥3 lockouts in 15 min) off
`vaultaire_dashboard_login_lockouts_total` — both from
`internal/dashboard/metrics.go`, registered on the server's registry. Install
like the other rule files (not yet on SLC — checklist item 10).

## Known limits

- **Push only.** ntfy.sh rejects anonymous email publishing, and the box has
  no SMTP (seq 5.6). When an email provider lands, add an `email_configs`
  receiver alongside the webhook — or pay for ntfy and set the `email` field
  in the bridge.
- Alertmanager web UI is loopback-only; reach it via SSH tunnel
  (`ssh -L 9093:localhost:9093 vaultaire-slc`).

`vaultaire-tls.yml` — origin certificate expiry (TLSCertExpiringSoon <14d
warning, TLSCertExpiryCritical <7d critical, TLSCertProbeFailing). Keys off
`vaultaire_tls_cert_expiry_timestamp_seconds{sni}` from `/metrics`
(`internal/api/cert_expiry.go`), which needs `TLS_CERT_PROBE_TARGETS` in the
prod `.env` — on SLC: `stored.ge@127.0.0.1:443,stored.cloud@127.0.0.1:443`
(dial local HAProxy with the public SNI = the origin Let's Encrypt cert, not
Cloudflare's edge cert). Installed on SLC 2026-09-24 alongside the backend
rules. If it fires: `sudo certbot renew --cert-name <sni> --force-renewal`
(the deploy hook rebuilds `/etc/haproxy/certs/*-le.pem` and reloads haproxy).
Note `certbot renew --dry-run` is NOT a valid renewal test on this box:
certbot 2.9 asks the STAGING CA about the production cert and fails with
"Certificate not found"; query ARI on the production directory instead
(`/acme/renewal-info/<certID>` → `suggestedWindow`).

## Installing a rules file

```bash
sudo install -m 0644 vaultaire-<name>.yml /etc/prometheus/rules/
promtool check rules /etc/prometheus/rules/vaultaire-<name>.yml
sudo systemctl reload prometheus
curl -s localhost:9090/api/v1/rules | python3 -c 'import sys,json; print([g["name"] for g in json.load(sys.stdin)["data"]["groups"]])'
```
