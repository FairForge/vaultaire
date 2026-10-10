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
| `vaultaire-alerts.yml` | `/etc/prometheus/rules/` | the box's original rules (installed 2026-04-04, no source in the repo until Prompt 2b.2 C5 — copied from the box, sha256 `c40d860b…`): VaultaireDown / NodeDown (critical, `up{job=…} == 0` for 1 min), VaultaireHighErrorRate (warning, `rate(vaultaire_errors_total[5m]) > 0.1` for 5 min), DiskSpaceLow / DiskSpaceCritical (< 10 % / < 5 % free on `/`), HighMemoryUsage (> 90 %), HighCPUUsage (> 90 % for 10 min) |
| `vaultaire-backends.yml`, `vaultaire-auth.yml`, `vaultaire-tls.yml`, `vaultaire-synthetic.yml`, `vaultaire-egress.yml`, `vaultaire-jobs.yml` | `/etc/prometheus/rules/` | alert rules: backend probes / auth failures / origin cert / customer-path canary / egress throttle / background jobs (Review R13 + WP-R10-9 + WP-R13-3 — install all six, checklist item 10). On SLC (checked 2026-10-09): `backends`, `tls`, `backup`, `longops` and `alerts` are installed (sha256 = repo); `auth`, `synthetic`, `egress`, `jobs` and `routing` are NOT |

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
rule were not live before. Still **not installed** (owner item):
`vaultaire-auth.yml`, `vaultaire-egress.yml`, `vaultaire-routing.yml`,
`vaultaire-synthetic.yml` (the last needs `SYNTHETIC_CHECK_URL` set first, or
its absent-series rule fires).

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
LyveProbeFailing, BackendWriteFailures warning/critical, BackendCircuitOpen,
VaultaireServerErrorRatio). They key off the `vaultaire_backend_*` series the
app exports from `/metrics` (`internal/api/prom_metrics.go`); the probes
behind `vaultaire_backend_health` are AUTHENTICATED (signed HeadBucket for
idrive/geyser via the driver, console `RSCustomerDetails` for lyve — root key,
one 403 retry), so a dead access key fires them. A TCP dial never did.

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
