# Deployment

How stored.ge's Vaultaire is built, shipped and run. Everything here is what
`.github/workflows/ci.yml`, `.github/workflows/deploy.yml`, the root
`CLAUDE.md` *Production* section and `deploy/monitoring/README.md` describe;
where a review left a gap it is cited by id. Hostnames and SSH aliases are
deliberately not in this file — "the production host" is the one box.

## What production is

- **One host** (Ubuntu 26.04.1, kernel 7.0 — upgraded in place from 24.04 on
  2026-10-10, two reboots at 12:11 and 12:24 UTC: the 24.04 full-upgrade, then
  the release upgrade) running **one static Go binary** under systemd, with a local
  PostgreSQL 18 (18.6, `pg_upgradecluster -m dump` from 16 the same day; the
  old **16/main cluster is parked, stopped, on port 5433** with the
  `postgresql-16` package installed — the rollback until it is dropped, see
  Backups), Redis 8.0.5
  and a local Prometheus. There is no container
  image, no Helm chart, no worker fleet and no second region. The object bytes
  live on the storage backends (`docs/DRIVERS.md`); the box holds the
  database, the chunk/multipart staging under `/tmp`, and the `local` driver's
  `DATA_PATH`.
- **Cloudflare** proxies `stored.ge` (and the CDN host); **HAProxy 3.2.9**
  (OpenSSL 3.5.5) on the box
  terminates TLS with a Let's Encrypt origin certificate, redirects http to
  https, sets HSTS, and forwards to the active app slot (`127.0.0.1:8000` or `:8001`, below). HAProxy
  appends the real peer as the *last* `X-Forwarded-For` hop — the only header
  `internal/clientip` trusts (R1-01). With OpenSSL 3.5 the origin negotiates
  the post-quantum hybrid **X25519MLKEM768** (Cloudflare → origin included),
  and OpenSSH 10.2 negotiates `mlkem768x25519-sha256`. Its `global` section carries
  `tune.h2.initial-window-size 262144` = **256 KiB** (set 2026-10-04; the
  comment above it said 4 MiB until 2026-10-10 — corrected, Prompt 2b.3 D3). Measured on HAProxy 2.8: its default
  HTTP/2 window is 65,535 bytes, which caps every h2 upload at window ÷ RTT
  (~1 MB/s from Dallas) — but a large window costs the other way: at 4 MiB,
  sixteen h2 streams on one connection fell from 308 to 35 MB/s on the box
  (2.8 buffers per stream; 3.1+ sizes windows dynamically). 256 KiB
  keeps multi-stream uploads at full speed. Uploads from Cloudflare are kept
  fast by the zone setting **`origin_max_http_version = 1`** (Cloudflare
  talks HTTP/1.1 to the origin, one connection per request, no h2 window at
  all): 22–27 MB/s through `stored.ge` from the box, 58 MB/s edge → origin.
  Leave `tune.h2.max-frame-size` at its default; 1 MiB frames made things
  worse. The box now runs 3.2, so the window (and the h1 origin setting) can
  be re-measured; until then both stay as they are. **What 3.2.9 offers for
  that bench** (its keyword dump, `haproxy -dKcfg`): `tune.h2.fe.rxbuf` /
  `tune.h2.be.rxbuf` (a receive-buffer budget per *connection* shared by its
  streams — the way to give one stream a large window without 16 streams
  each holding one, which is exactly the 2.8 trade-off above), separate
  `tune.h2.fe.`/`be.initial-window-size`, `tune.h2.fe.max-total-streams` and
  `tune.h2.fe.glitches-threshold` (closes a connection that sends too many
  invalid frames — abuse protection, worth a value). Not changed: tuning is a
  bench, not a docs PR.
- **`haproxy.cfg` is box state, not in the repo**: `vaultaire-switch` renders
  it (the active slot's `server` lines enabled, the other's `disabled`) on
  every deploy, so a repo copy would be stale after the next switch. Edit it
  the way the switch does — under `/run/vaultaire-switch.lock` (flock), into a
  temp file, `haproxy -c -q` on the temp file, timestamped backup, atomic
  `mv`, `systemctl reload haproxy` (seamless: the old worker exits 0), with
  the `server` lines matching `/opt/vaultaire/ACTIVE_PORT`. 2026-10-10: the
  `http_front` `http-request redirect` moved above its `use_backend` (3.2
  warned "an 'http-request' rule placed after a 'use_backend' rule will still
  be processed before" — no behaviour change: plain http still 302s to https,
  ACME paths still reach `acme_backend`); `haproxy -c` is now clean
  (backup `haproxy.cfg.bak-2b3-20261010T130312Z`).
- **UFW** allows 22, 80 and 443 only. `deploy/ufw-cloudflare-lockdown.sh` is
  the script for narrowing 80/443 to Cloudflare's ranges.
- **Configuration is the env file** loaded by the unit's `EnvironmentFile=`
  (`docs/CONFIG.md`). Nothing else is read.

### Box layout

| Path | What |
|------|------|
| `/opt/vaultaire/bin/vaultaire-8000`, `vaultaire-8001` | the two slots' binaries (zero-downtime deploys, below); `vaultaire` is a symlink to the active one, the idle one holds the previous build |
| `/opt/vaultaire/ACTIVE_PORT` | which slot is active (`vaultaire-switch`'s state) |
| `/opt/vaultaire/configs/.env` | the env file (`EnvironmentFile=`); also where the deploy job reads `DB_PASSWORD` for migrations |
| `/opt/vaultaire/data` | `DATA_PATH` of the `local` driver; `data/multipart` = the multipart staging directory once `MULTIPART_STAGING_DIR` is set (below) |
| `/opt/vaultaire/backups/` | nightly PostgreSQL dumps (below) |
| `/opt/vaultaire/monitoring/ntfy-bridge.py` | Alertmanager → ntfy push bridge |
| `/etc/systemd/system/vaultaire@.service` | the slot template (`deploy/systemd/vaultaire@.service`). The single-unit `vaultaire.service` is **masked** since 2026-10-06 (its file kept at `/root/vaultaire.service.retired-20261006`) so nothing can start the old build on `:8000` |
| `/usr/local/sbin/vaultaire-switch` | the deploy/restart/rollback script (`deploy/scripts/vaultaire-switch`) |
| `/opt/vaultaire/bin/health-check.sh` | cron every 5 min, probes the active slot (`deploy/scripts/health-check.sh`) |
| `/etc/prometheus/rules/*.yml` | alert rules (installed by hand from `deploy/monitoring/`) |
| `/etc/haproxy/haproxy.cfg`, `/etc/haproxy/certs/*-le.pem` | edge config and the origin cert the certbot deploy hook rebuilds |

### Multipart staging

Multipart parts are staged on local disk between UploadPart and
CompleteMultipartUpload, under `MULTIPART_STAGING_DIR` (WP-R3-5,
`internal/api/multipart_staging_dir.go`). The default is
`/tmp/vaultaire-multipart`. On this box `/tmp` is a **61 GiB tmpfs**: RAM,
wiped by `systemd-tmpfiles` at every boot, and shared with the Sync stripe
staging and the pack store. `MULTIPART_STAGING_MIN_FREE` refuses a part there
below 30 GiB free (503 SlowDown).

**Prod: the NVMe data disk since 2026-10-21 14:26 UTC** (`.env` backup
`.env.bak-staging-20261021T1426`; boot line `"dir":"/opt/vaultaire/data/multipart",
"default":false`, 3.67 TB free), with the same `MULTIPART_STAGING_MIN_FREE`
rule (10 % of the 3.6 TB filesystem ≈ 360 GiB kept free). No upload was
active at the switch.

1. Add `MULTIPART_STAGING_DIR=/opt/vaultaire/data/multipart` to
   `/opt/vaultaire/configs/.env`.
2. Run `sudo vaultaire-switch restart`.
3. Read the boot line: `journalctl -u 'vaultaire@*' --since -5min -o cat | grep '"multipart staging"'`
   must show `"dir":"/opt/vaultaire/data/multipart","default":false` and the
   free bytes. A rejected value logs `MULTIPART_STAGING_DIR rejected` at Warn
   and stages in the default.

What to know:

- The directory is created 0700 as `user1` at boot when absent.
- **Both slots must see the same directory.** They read it from the one
  `EnvironmentFile`, run as the same user and have no `PrivateTmp=`, so a
  part staged on the old slot is there for the new one after a switch. Never
  give the slots different values.
- Uploads in flight at the restart that changes it keep their parts in the
  old directory. Their Complete answers `InvalidPart` ("upload the part
  again": the part files are not found in the new one). The parts left in
  the old directory are no longer swept by the reaper; the tmpfs empties at
  the next boot. Make the change when `active` uploads are few:
  `SELECT count(*) FROM multipart_uploads WHERE status = 'active'`.
- On the data disk a staged part survives a reboot, so an upload can be
  completed after one; on the tmpfs it could not.

The deploy user's passwordless sudo is pinned to four exact invocations
(`deploy/sudoers/vaultaire-deploy`): `vaultaire-switch deploy /tmp/vaultaire-linux`,
`restart`, `rollback` and `status`. sudo matches arguments literally, so no
other file or verb is reachable.

### Memory (Stage 1a Prompt 14 Part C — installed 2026-10-21)

Read on the box on 2026-10-19 (`systemctl cat vaultaire@8001`, `free -g`): the
unit sets **no `MemoryHigh=`, no `MemoryMax=` and no `GOMEMLIMIT`**; the box
has 121 GiB with ~4 GiB used. The process now bounds what the large GETs
(`LARGE_GET_PARALLEL_BUDGET_BYTES`, 2 GiB) and the chunked PUTs
(`CHUNK_PUT_BUDGET_BYTES`, a quarter of `GOMEMLIMIT` when one is set, else
2 GiB) may hold, but with no `GOMEMLIMIT` the Go runtime only collects at
`GOGC`'s pace and the kernel's OOM killer is the only ceiling. Two slots
overlap during a switch, so each gets under half the box. Recommended, in
`deploy/systemd/vaultaire@.service` (`[Service]`) — **installed 2026-10-21
14:26 UTC** (unit backup `/etc/systemd/system/vaultaire@.service.bak-20261021T1426`;
`systemctl show vaultaire@8001 -p MemoryHigh -p MemoryMax -p Environment`):

```ini
Environment=GOMEMLIMIT=40GiB
MemoryHigh=48G
MemoryMax=56G
```

`GOMEMLIMIT=40GiB` makes the runtime collect harder as it nears 40 GiB and
sets the chunked-PUT budget's default to 10 GiB (≈ 200 concurrent chunked
writes at the typical 48 MiB reserve; `CHUNK_PUT_BUDGET_BYTES` overrides);
`MemoryHigh=` throttles the slot above 48 GiB, `MemoryMax=` kills it above
56 GiB — two slots at 56 GiB still leave the box 9 GiB. Install with
`systemctl daemon-reload` and a `vaultaire-switch restart` (the running slot
keeps its old limits until it is restarted). Note `EnvironmentFile=` overrides
`Environment=`, so `GOMEMLIMIT` can equally go in `.env`.

### Host tuning (applied 2026-10-21)

Measured and applied on slc-vaultaire-01 (Ryzen 9 9950X, 32 threads, 121 GiB,
ixgbe 10 GbE `enp1s0f0`, NVMe). Files in `deploy/host/`:

| What | Before | After | How (persistent) |
|---|---|---|---|
| CPU governor / EPP | `powersave` / `balance_performance` (cores at ~58 %) | `performance` / `performance` (~4.4 GHz) | `vaultaire-host-tuning.service` → `/usr/local/sbin/vaultaire-host-tuning.sh` at boot |
| NIC rings | RX/TX 512 of 8192 (`rx_missed_errors` 1,156, `rx_dropped` 476k in 11 days) | 4096 | same script (resizing resets the link ~1 s; only when not already 4096) |
| NIC IRQ affinity | every queue on every CPU, irqbalance off (CPU 16 had 110 M interrupts, most 2–27 M) | queue `TxRx-n` → CPU n (busiest CPU 5 % of an ingest's interrupts, all 32 used) | same script |
| TCP | `tcp_mtu_probing=0`, `tcp_notsent_lowat` unlimited | 1, 131072 | `/etc/sysctl.d/91-vaultaire-tcp.conf` (the 2026-04-20 buffers/BBR stay in `99-vaultaire.conf`) |
| PostgreSQL 18 | `shared_buffers` 160 MB, `effective_cache_size` 5 GB, `random_page_cost` 4, `effective_io_concurrency` 16, `max_wal_size` 1 GB, `wal_compression` off, `jit` on | 2 GB, 64 GB, 1.1, 200, 8 GB, zstd, off | `postgresql-tuning.sql` (ALTER SYSTEM; backup `postgresql.auto.conf.bak-tuning-20261021T1426`; restart took 2.3 s, no app errors). DB is 381 MB with a 99.6 % hit ratio, so this is headroom, not a fix |
| App memory | none | `GOMEMLIMIT=40GiB`, `MemoryHigh=48G`, `MemoryMax=56G` | the unit (§ Memory) |
| Ephemeral ports (added 2026-10-25) | `ip_local_port_range 1024 65535` (the 2026-04-20 tuning) could hand a service port out as an outbound source port: the active slot's own HTTPS connection sat on local :8000 and the new slot failed `bind: address already in use` on five restart attempts | `net.ipv4.ip_local_reserved_ports = 3000,4918-4922,5432,6379,8000-8001,8010,8100,8404,9000-9007,9090,9093,9095,9097,9100,9980-9982` (every listener on the box: the slots, Postgres, Redis, the Sync bridges, s3d, HAProxy stats, Prometheus/Alertmanager/exporters) | appended to `/etc/sysctl.d/99-vaultaire.conf`, applied live. A new listening service needs its port added here or a restart can fail the same way |
| Multipart staging | `/tmp` tmpfs (RAM) | NVMe | `.env` (§ Multipart staging) |

**Kept as is:** HAProxy `tune.bufsize 10485760`. A/B on 2026-10-21: 1 MB cut
32 × 4 MiB PUTs through HAProxy from 1,007–1,070 to 595–761 MB/s (p90 157 →
340–424 ms); 64 MiB bodies were unchanged. Reverted (backup
`/etc/haproxy/haproxy.cfg.bak-bufsize-20261021T1428`). It holds up to ~20 MB per
active connection — watch it if concurrent transfers reach the thousands.

**Before → after** (`tierbench`/`ingestbench`, bench tenant, a bucket on the
iDrive primary; L1 = localhost, L2 = `s3.stored.ge` through HAProxy; one run
each, so ±20 % is noise):

| | before L1 / L2 | after L1 / L2 |
|---|---|---|
| 4 KiB PUT / GET / DELETE p50 | 34/30/32 · 34/31/32 ms | 35/31/31 · 34/29/31 ms |
| single PUT 8 MiB | 36 · 64 MB/s | 61 · 70 MB/s |
| single PUT 64 MiB | 86 · 147 MB/s | 189 · 217 MB/s |
| single GET 64 MiB | 157 · 162 MB/s | 172 · 180 MB/s |
| 32 × 4 MiB ingest | 1,078 · 974 MB/s | 708 · 1,108 MB/s |
| 16 × 64 MiB ingest | 1,040 · 1,051 MB/s | 1,058 · 1,097 MB/s |

Small operations are bound by the backend (~30 ms to iDrive), not the box.
Single-stream uploads gained the most (the governor: one core hashes and
seals a stream). The large-GET failures of the "before" run were Lyve answering
`500 InternalError` to chunk blobs written 2026-10-12/14 (HEAD 200, GET 500 —
a vendor fault under investigation), not the host.

**Re-checked 2026-10-22** (`bench-results/HOST-TUNING-CHECK-2026-10-22.md`): the boot unit
run under systemd (`systemctl start`, exit 0, no link reset), raw iDrive / Lyve / Wasabi
unchanged before and after it, the customer path at the 10-21 "after" numbers. A reboot
has not tested its boot ordering yet.

## The pipeline: push to `main`

`main` is protected: no direct pushes, CI must pass, merges are squash merges
(`gh pr merge --squash --delete-branch`; never `--admin`). A merge to `main`
triggers `deploy.yml`; a merge to `develop` runs the same job against the dev
server. `workflow_dispatch` is the manual lever (`gh workflow run deploy.yml
--ref main`) for a dropped trigger or a DR re-deploy. Deploys are queued per
branch and never cancelled mid-flight.

```
build   GOOS=linux GOARCH=amd64 scripts/build-release.sh vaultaire-linux  -> artifact
        (the one reproducible recipe: go.mod's toolchain go1.26.10, CGO off, -trimpath,
         -buildvcs=false, BuildDate = the COMMIT time; then scripts/repro-check.sh builds
         twice more from fresh copies and fails the deploy unless all three match — RP37-9)
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
  `/usr/local/sbin/vaultaire-switch`, root:root 0755; the deploy user's sudo is
  pinned to `deploy /tmp/vaultaire-linux`, `restart`, `rollback`, `status`).
  One run at a time (`flock /run/vaultaire-switch.lock`).
  - `deploy /tmp/vaultaire-linux` accepts only that path, as a regular file
    (not a symlink) owned by the deploy user and starting with an ELF header;
    keeps the idle slot's current build aside, installs the new one into the
    idle slot and `systemctl restart vaultaire@<idle>`.
  - **Gates before any traffic moves** (each failure stops the idle slot,
    restores its previous build and exits 1 — the active slot is never
    touched): `/health/live` within 90 s; the process listening on the port
    is that slot's systemd `MainPID` (not a stray process holding the port);
    `haproxy.cfg` rendered for the switched state validates with `haproxy -c`;
    every `set server` on the admin socket answers cleanly; HAProxy reports
    the new server UP within 30 s (otherwise it goes back to `maint`).
  - Then: the old server to `drain`, the validated config installed
    atomically (temp file in `/etc/haproxy` + rename; timestamped
    `haproxy.cfg.bak-switch-*`, the last five kept), `ACTIVE_PORT`, the
    symlink and boot enablement flipped, then the wait for the old slot: up
    to 10 min (`DRAIN_TIMEOUT`) for its sessions to reach 0, and up to 15 min
    (`LONG_OP_TIMEOUT`) while it reports detached long S3 operations
    (`long_ops_in_flight` on its own `/health`: a CompleteMultipartUpload,
    CopyObject or DeleteObjects that has already sent 200 + keep-alive — plus,
    since Prompt 14 (RP14-1), `chunk_puts_in_flight`: chunk blob PUTs that
    outlived their request and finish on their own within 90 s; the script
    adds the two), then `maint` and stop. On SIGTERM the process itself drains
    HTTP for 30 s, waits for those operations, cuts each one when it is 15 min
    old (its own start; `LONG_OP_DRAIN_BOUND`), logs it and writes it to
    `s3_long_op_incidents`, then waits for the chunk PUTs (each bounded by
    90 s + the row flip that follows it)
    (the active slot exports `vaultaire_s3_long_ops_abandoned_total` from that
    table), drains the webhook/notification queue for up to 30 s (the rest is
    recorded as failed `dropped: shutdown`), flushes and exits
    (`TimeoutStopSec=1000`). A cut stops only the backend write or delete
    itself; the bookkeeping after a call that succeeded always finishes. Before 2026-10-08 the only protection was the
    session wait: an operation whose client was still connected is a session
    and was waited for up to the 10 min `DRAIN_TIMEOUT`; one whose client had
    gone (the operation runs detached from it), or one older than that, got
    the process's 30 s and then the engine closed under it. No such cut was
    observed in production: the longest operation measured is a 2 GiB copy
    through Cloudflare at 120 s, and the #634 live proof — a 2 GiB complete
    during `vaultaire-switch restart`, 200 after 32 s, byte-exact — had its
    client attached, so the session wait alone would have covered it too.
    Prompt 2a.2 (2026-10-09, #644) proved the detached case on prod: a 2 GiB
    complete into a Sync-tier bucket, client killed 15 s in (after the 200
    was committed), then `vaultaire-switch restart` — the old slot kept
    running while `/health` said `long_ops_in_flight: 1`, the operation
    answered at 25.8 s and was stopped 0.2 s later; the GET was byte-exact.
    The cut itself (an operation still running at its bound) was proven in a
    lab with `LONG_OP_DRAIN_BOUND=1s` and a 22 MB/s backend: cut at 30.2 s
    (after the 30 s HTTP drain), an `s3_long_op_incidents` row written, the
    next process exporting `vaultaire_s3_long_ops_abandoned_total 1`, the
    upload still `active`, and the retry completing byte-exact. A failed
    `/health` poll of a running slot now counts as unknown (`?`), never 0:
    the wait continues to `LONG_OP_TIMEOUT`.
  - `restart` runs the same path with the **active** build copied into the
    idle slot — the zero-downtime way to pick up `.env` edits. `rollback`
    switches to the idle slot (the previous build) through the same gates.
    `status` prints both slots, the legacy unit's state and HAProxy's view.
  - The deploy job runs every migration with `PGOPTIONS='-c lock_timeout=5s'`
    (the old build keeps serving while they run, so a DDL that must wait for a
    lock fails the deploy instead of stalling the table) and, after the
    switch, fails unless `https://stored.ge/version` reports the commit it
    deployed. **Migrations must stay additive**: the build before is still
    serving the migrated schema until the switch, and `rollback` runs it
    against the new schema.
  - **A migration that gives an artefact an address** (087: whole blobs at
    `<key>@<id>`; 092: chunk manifests the same way) has one more check. The
    previous build reads by the KEY, and during the two-slot window and after
    a rollback it does not find nothing: it finds the RETIRED artefact the
    new build left at `<key>`. Measured with the real `e209950` beside 092
    (WP-P8 Part 0, 2026-10-17): a legacy chunked object overwritten on the new
    slot was answered by the old slot with **200 and the old bytes under the
    new ETag**, or with a prefix of them when the new object was smaller.
    Before such a migration, count the legacy rows it can affect (they were
    0 at 087 and 092). If any exist, ship a guard one deploy ahead: the old
    reader refuses a row whose artefact `retired_blobs` lists. Owner decision
    2026-10-17: this checklist line, no guard built until it is needed.
  - **Rollback past the wrapped S3 secrets is impossible once
    `credential_plaintext_drop` is on** (plan 43.7, Stage 1a Prompt 42 PR C).
    From then on writers store an S3 secret only wrapped under its tenant's
    key, and the `key_reencrypt` job nulls the plaintext of every older row
    whose wrap opens back to it. A build older than PR B (migration 108,
    #969) reads only the plaintext column: every S3 request would answer
    `SignatureDoesNotMatch`, and turning the flag off does not bring the
    nulled secrets back. **`vaultaire-switch` refuses such a build** (chosen
    over a runbook line: the mistake is a one-way door and a refusal cannot be
    forgotten in an incident): `deploy`, `restart` and `rollback` all go
    through `start_and_switch`, which first checks that the slot's binary
    carries the wrap's HKDF label `stored/credential/v1` (every build since
    PR B does; `TestCredentialWrap_TheSwitchMarkerIsTheHKDFLabel` holds the
    script and the code to one string). A binary without it starts only while
    the flag's global row reads `f` (`runuser -u postgres -- psql` on
    `DB_NAME`); on, or unreadable, it is refused before the slot is touched
    (`… predates the wrapped S3 secrets … credential_plaintext_drop is on`).
    A restore of the database from before the drop (`§ Restoring the
    database`) brings the plaintext back with it — that, not a rollback, is
    the way to an older build. The script is installed by hand like every
    `deploy/**` change.
- **The `edge_ingest` flag stays OFF until the build that knows edge uploads
  is the active slot** (WP-EDGE-1 Part A, migration 109, Stage 1a Prompt
  44). The migration is additive, but the PREVIOUS build cannot Complete an
  edge multipart upload (it knows no `backend_upload_id`) and answers its
  staged-part refusal as a plain part: during a two-slot overlap a Worker
  admitted part and an old-slot Complete would disagree. So: `vaultaire-switch
  deploy` first, the old slot stopped, then `edge_ingest` on for tenant
  zero through the admin API (`PUT /api/v1/admin/flags/edge_ingest`), never
  a psql row. The Worker itself needs no order: an origin without the token
  or the routes answers its admit with a 404 and the Worker passes the
  request through.
- **The read half (WP-EDGE-1 Part B, migration 110, Stage 1a Prompt 45)
  needs no flag order of its own.** Migration 110 only widens the op CHECK
  (`'get'`); the previous build never writes such a row. Its reaper, though,
  selects every expired pending row with NO op filter and treats it as a
  lost PUT (release the reservation, queue `storage_key` as `orphan_write`)
  — on a `get` row that address is the LIVE object's blob. During the
  two-slot overlap that is safe for two reasons, and write them down rather
  than "the previous build ignores get rows": the 20-minute admit TTL
  outlasts the old slot's drain (no `get` row the new slot writes expires
  before the old slot has stopped), and the collector's `blobNamedBy`
  re-checks a queued address under the key's lock before any delete (the
  head row names it, so it is kept). The Worker (0.2.0) against an origin
  of the previous build: the admit answers `invalid_op` 400 for `get`, which
  the Worker treats as "admit unavailable" and passes the request through —
  deploy the origin first, then `wrangler deploy`; the other order costs
  nothing but a pass-through.
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
  build (8.5 s each, 0 non-200 answers on a 1 s probe of
  `stored.ge/health/live`). #605 and #604 then deployed through the script.
- **Hardening record (2026-10-06, 20:30–21:10 UTC)**, after the post-merge
  review: sudoers pinned on the box (the open entry allowed any arguments) and
  the legacy unit masked; the hardened script (#608) installed by hand
  (`deploy/**` is in `paths-ignore`: **script, sudoers, unit and
  health-check changes are installed on the box by hand**, never by the
  pipeline) and proven with a live `vaultaire-switch restart` (9.6 s, 217/217
  public probes 200) before #608 merged; #608–#612 then deployed through it.

### CI (`ci.yml`)

Every push and PR: PostgreSQL 18 service container (prod's major), all migrations applied
with `ON_ERROR_STOP`, `go build ./...`, `go test -race ./...` (with
`DATABASE_URL` and `JWT_SECRET`), golangci-lint v2.14.0, then a smoke boot that
must answer `/health/live` and `/status`. A second job drives the landing
page's house builder in headless Chrome. The Security workflow (`security.yml`)
runs gosec (green since Review R15; `make gosec` is the same command), Trivy
and govulncheck. There is no nightly workflow any more (its unauthenticated
benchmarks measured nothing — `docs/SCALE_TESTING.md`); the load gate is
`tests/load/`.

## Backups

A cron job at **03:00 UTC** (user1) runs `/opt/vaultaire/bin/pg-backup.sh`
(`deploy/scripts/pg-backup.sh`, installed by hand — `deploy/**` never deploys;
tests: `bash deploy/scripts/pg-backup_test.sh`, run by CI). In this order:
`pg_dump | gzip` of the database into `/opt/vaultaire/backups/` (size and
`CREATE TABLE` asserted), then **the dump goes off the box at once** to Sync.com
— Stored's own end-to-end-encrypted account, through the local `sync-webdav`
bridge on `:4918` (`rclone` WebDAV; credentials
`/opt/vaultaire/configs/sync-backup.env`, user1 0600) — into
`_ops/backups/YYYY/MM/` under the bridge's `/vaultaire/` mount, size-verified;
only then the tarballs of `configs/` (stays on the box: secrets) and of the
Prometheus rules (goes off-box too). **7 days** kept on the box, **30 days** on
Sync, every file private (`umask 077`). A failed stage is a failed run
(`BACKUP FAILED - <stage>` in `/opt/vaultaire/logs/backup.log`), but a tar that
fails no longer holds the dump's upload back (that is how the off-box copy was
missing from 2026-10-05 to -07: a root-only file in `configs/`).

**A second off-box copy goes to Blomp** (since 2026-10-21): right after the Sync
upload, the dump (and later the rules tar) is uploaded through an `rclone`
**crypt** remote — `blompcrypt`, over the swift remote `blompbk` — into
`_ops/backups/YYYY/MM/` of the Blomp container, size-verified. Blomp's storage
URL is plain HTTP, so file contents AND names are encrypted on the box first.
The Blomp login is the driver's own `BLOMP_USER` / `BLOMP_PASSWORD`
(/`BLOMP_CONTAINER`) read from `configs/.env`; the crypt passwords are in
`/opt/vaultaire/configs/blomp-backup.env` (user1 0600, rclone-obscured) and in
plain form ONLY in the owner's Keychain (`vaultaire-blomp-backup-crypt`,
`vaultaire-blomp-backup-salt`) — **without them the Blomp copies cannot be
read**. The leg is optional and never fails the run: no `blomp-backup.env` =
skipped (one log line), any failure = one `OFFBOX2 FAILED - …` line; 30 days
kept by the name's date like Sync (Blomp's listing lags minutes, so a file it
does not list yet goes on a later run).

**The dump is sealed with `age` before it exists on disk** (WP-P42, plan 43.7,
`docs/surfaces/TRUST.md` item 12): with `/opt/vaultaire/configs/backup-age-recipients.txt`
present the pipeline is `pg_dump | gzip | age -R <that file>` →
`vaultaire_<ts>.sql.gz.age`; the size assert counts the gzip stream and the
`CREATE TABLE` assert the SQL stream on their way to `age`, the file is written
under `.dump-*/` and renamed only after both pass, so no plaintext byte of the
dump is written. The same ciphertext goes to Sync and (crypt again) to Blomp.
- **The recipients file** holds the owner's OFFLINE age public keys only — one
  `age1…` line each (`age-keygen -y <identity>`), `#` comments and blank lines
  allowed; `root:root 0644`. **The recipient kind is owner decision F-36;
  recommended: two post-quantum identities** (`age-keygen -pq`, recipients
  `age1pq1…`) — one on paper in the owner's safe, one sealed into the F-14
  escrow. Not a YubiKey: `age-plugin-yubikey` is classic P-256, has no Ubuntu
  package, and age ≥ 1.3 refuses a file that mixes post-quantum and classic
  recipients (`incompatible recipients: can't mix post-quantum and classic
  recipients`) — all lines of the file are one kind. **The identities are
  never on the box.** A line that is not an `age1` key, an `AGE-SECRET-KEY-…`
  line (refused and never logged — remove it and rotate that identity), an
  `AGE-PLUGIN-…` line (an identities file, not a recipients file), a file with
  no recipient, a dangling symlink, no `age` binary, or a file `age` itself
  refuses (mixed kinds; `age1pq1…` under an age older than 1.3) =
  `BACKUP FAILED - stage=encrypt`, **no dump that night** (`BackupStale`
  pages) — never a silent plaintext.
- **The age binary is the pinned upstream release, not apt's** (RP62-1): the
  box's apt candidate is 1.2.1, which has no post-quantum recipients.
  `sudo bash deploy/scripts/age-install.sh` installs `age` + `age-keygen`
  v1.3.2 into `/usr/local/bin` after checking the tarball's sha256 against the
  pin in the script (a mismatch installs nothing); CI installs the same pin and
  runs the real-age checks of `pg-backup_test.sh` (5g fails, never skips, under
  `CI=true`). Every `Backup completed` line names the age version that sealed
  the dump. A bump changes the version and sha256 in that script together.
- **One run at a time:** the script holds `flock` on `backups/.lock`; a second
  run while one is going is `BACKUP FAILED - stage=lock` (its sweep of stale
  `.dump-*` work directories would delete the first one's). A hand run still
  belongs outside 02:30–03:30 UTC so it never costs the night's dump.
- **No recipients file** = the plaintext `.sql.gz` of before, one
  `WARNING - stage=encrypt: … PLAINTEXT` line per run and
  `vaultaire_backup_dump_encrypted 0` → `BackupDumpPlaintext` (warning, 26 h,
  also when the gauge is absent — its textfile symlink is installed like the
  others). Prod: absent until the owner's ceremony.
- **Retention** retires both shapes by the same clocks (7 days on the box by
  mtime, 30 days off it by the name's date).
- **Restore** happens where an identity is, never on the box with one left behind:
  `age -d -i <identity> vaultaire_<ts>.sql.gz.age | gunzip | psql -d <db>` (age
  ≥ 1.3 for an `age1pq1…` file). A plaintext
  `.sql.gz` from before: `gunzip -c f | psql -d <db>`. Lab 2026-10-25 (WP-P42):
  a throwaway identity, a run of the script with the real `age 1.3.2` against a
  migrated test DB, the file decrypted and restored into a scratch DB — every
  table's row count equal.
- **What this does not cover:** the point-in-time archive (§ Point-in-time
  recovery: R2 `stored-pg-pitr`, ≤ 28 days of base backups + WAL) holds the same
  rows, encrypted `aes-256-cbc` under a passphrase that is ON THE BOX. So the
  exact claim is: the nightly dumps are sealed to the owner's offline keys; the
  PITR archive is encrypted under a key held on the box (RP62-7). Until
  `credential_plaintext_drop` is ON and every S3 key rotated, both hold
  plaintext S3 secrets.

- **Alerting:** the script writes `vaultaire_backup_last_success_timestamp_seconds`
  (dump passed its asserts) and `vaultaire_backup_offbox_last_success_timestamp_seconds`
  (dump on Sync, size verified) into node_exporter's textfile directory
  `/var/lib/prometheus/node-exporter/` (`root:user1 0775`); rules
  `deploy/monitoring/vaultaire-backup.yml` page `BackupStale` /
  `BackupOffboxStale` after 26 h without one, or when the series is absent.
  The Blomp leg writes `vaultaire_backup_offbox2_last_success_timestamp_seconds`;
  `BackupSecondOffboxStale` WARNS (Sync still holds a copy) after 26 h or when
  absent. Its textfile symlink is installed like the others.
- **`DB_PASSWORD`** is read from `configs/.env` as everything after the first
  `=` with one level of surrounding quotes removed (what systemd's
  `EnvironmentFile` does) — a password containing `=` or wrapped in quotes
  works; `cut -d= -f2` used to truncate it.
- **Retention is by the date in the file name, never by modtime:** the Sync
  bridge reports every file as modified in January 1970; a `--min-age` sweep
  deleted fresh backups on 2026-10-07 (caught and fixed the same run).
- **A restore puts the transparency log behind what its key signed** (RP37-11):
  the dump holds `translog_*` as of 03:00, the key may have signed up to 24 h
  more. The high-water file `/opt/vaultaire/data/translog/highwater` is not in
  the dump, so **the log freezes** (RP42-1, Stage 1a Prompt 33): every append
  is refused while the database holds fewer entries than the file
  (`TranslogFrozen` pages; the boot's `server_build` waits and is retried by
  the checkpoint job, a ceremony answers 503 `log_frozen`, an admin read is
  refused) and the checkpoint job refuses to sign (`TranslogCheckpointRefused`).
  **The slots may be started and serve customers meanwhile**; only the log
  waits. Follow `docs/TRANSPARENCY.md` § After a database restore:
  `translog-restore -from <the witness's directory> -highwater <file>` against
  the live database puts the lost entries back byte for byte from the off-box
  witness's cache (K4b), the log thaws and the job signs again under the same
  origin; without a witness cache that reaches the file's size, the fallback is
  a new origin (F-33). **The box lost too:** write the witness's `cosigned` body
  as the high-water file BEFORE the first slot starts on the new box. Never
  delete the high-water file to make the refusal go away: that unfreezes a log
  that is not the one the key signed.
- **Restore:** `set -a; . /opt/vaultaire/configs/sync-backup.env; set +a;
  rclone lsf -R syncbk:_ops/backups` → `rclone copyto syncbk:_ops/backups/<y>/<m>/<file> .`
  → `zcat <dump> | psql` (plain SQL, psql ≥ 16.10 for the `\restrict` header).
  Verified 2026-10-07: the downloaded dump is byte-identical to the local one.
- **Restore from Blomp** (the box gone too): on any machine with rclone, export
  `RCLONE_CONFIG_BLOMPBK_TYPE=swift`, `_AUTH=https://authenticate.blomp.com/v2.0`,
  `_AUTH_VERSION=2`, `_TENANT=storage`, `_USER`/`_KEY` = the Blomp login (Keychain
  `blomp`), `RCLONE_CONFIG_BLOMPCRYPT_TYPE=crypt`,
  `_REMOTE=blompbk:<container>/_ops/backups`, `_PASSWORD=$(rclone obscure <Keychain
  vaultaire-blomp-backup-crypt>)`, `_PASSWORD2=$(rclone obscure <…-salt>)`; then
  `rclone lsf -R blompcrypt:` → `rclone copyto blompcrypt:<y>/<m>/<file> .`.
- **History:** the run failed on 2026-10-05, -06 and -07 (`config tar failed`,
  then a bridge 500) — two root-only `.env` copies from the 2026-10-04 primary
  swap were unreadable to user1 and the dump's upload sat behind the tar; the
  order is fixed and every `.env*` in `configs/` is `root:user1 0640` or
  owner-only. Keep new `.env` backups readable by group user1.
- **Restore drill on PostgreSQL 18 — 2026-10-10 13:04 UTC (Prompt 2b.3 D3):**
  the newest off-box dump (`vaultaire_20261010_122900.sql.gz`, 33 MB, taken on
  18.6 after the upgrade) downloaded from Sync in 3 s, sha256 = the local copy;
  `createdb vaultaire_drill` + `gzip -dc | psql -v ON_ERROR_STOP=1` (psql 18.6)
  restored in 3 s with 0 errors: 96 tables in both, 1,065,969 rows vs
  1,065,980 in prod at that minute — the only differences `events` (+8) and
  `quota_usage_events` (+3), written since 12:29; `users` 5, `tenants` 5,
  `api_keys` 8, `buckets` 34, `object_head_cache` 5,421, `object_versions`
  205,604, `vault_parity` 6, `audit_logs` 22 equal; 313 MB vs 314 MB. Dropped
  after. The steps: the Restore line above, then a per-table `count(*)` over
  `information_schema.tables` in both databases, then `dropdb`.
- **Dropping the PG 16 rollback** (owner's call, not done): with the drill
  passed and three nightly dumps on 18 by then, proposed **2026-10-13**:
  `sudo pg_dropcluster 16 main && sudo apt purge postgresql-16 postgresql-client-16`,
  and the pre-upgrade dumps `/root/pre-2604-20261010T120755Z/` (pg_dumpall,
  `/etc`, configs, binaries — secrets inside) deleted the same day, with the
  upgrade session's scratchpad copy of them.
- Still open from WP-R9-7: `-Fc` format and the drill as a script.

## Point-in-time recovery

Since **2026-10-24** (Stage 1a Prompt 40) the PostgreSQL 18 cluster is archived
continuously by **pgBackRest 2.58** (Ubuntu package), stanza `vaultaire`, into the
private R2 bucket **`stored-pg-pitr`** (path `/vaultaire`; never `vaultaire-public`),
**`aes-256-cbc`** with a passphrase that exists in `/etc/pgbackrest/pgbackrest.conf`
(`postgres:postgres 0640`) and in the owner's Keychain (`vaultaire-pgbackrest-cipher`)
only — **without it the repository cannot be read**. The nightly `pg_dump` (§ Backups)
stays as the independent logical copy.

- **RPO ≤ ~61 s:** `archive_timeout = 60s` switches a WAL segment every minute the
  cluster writes; `archive-async` pushes it (switch → archived: median 0.68 s, max
  1.24 s over 10 segments, 2026-10-24).
- **RTO 12.8 s measured** for the 473 MB cluster (the drill below: restore 6.9 s,
  accepting connections at 10.2 s, promoted at 12.8 s).
- **Backups:** a full every **Sunday 01:30 UTC**, a differential **Monday–Saturday
  01:30** (`vaultaire-pitr-full.timer` / `-diff.timer` → `vaultaire-pitr-backup@{full,diff}.service`,
  as postgres). The first full: 473.1 MB in 7.6 s, 79.4 MB in the repository (zstd).
- **Retention — a code fact:** `repo1-retention-full=4` (count) of weekly fulls, so the
  oldest restorable point is at most 28 days old; WAL older than the oldest full is
  expired with it. The produce page and the privacy policy say a deleted record stays
  in a backup up to 30 days; `internal/api/produce_test.go` parses the install script
  and fails if either moves, and **`PGPITRRetentionTooLong`** fires at 31 days.
  Expected repository size: ~80 MB per full + diffs + ~1 day of WAL per day kept
  (the WAL directory is ~80 MB) — a few GB at four weeks.
- **What it holds:** every database of the cluster (`vaultaire`, `vaultaire_prod`, the
  Sia `indexd_zen` and the lab databases, `pitr_canary`) at every moment of those 28
  days, including the rows of accounts erased since — the produce page says so.
- **Alerts** (`deploy/monitoring/vaultaire-pitr.yml`, installed 2026-10-24 sha256
  `f69bf673…`; the drill rules added by Prompt 41): `PGArchiveFailing` / `PGArchiveStale`
  page, `PGPITRBackupStale`, `PGPITRRetentionTooLong`, `PGPITRMetricsStale` warn — from
  `/var/lib/vaultaire-pitr/vaultaire_pitr.prom`, written each minute by
  `vaultaire-pitr-metrics.timer` (as postgres) and read through the symlink
  `/var/lib/prometheus/node-exporter/vaultaire_pitr.prom`; `PGPITRDrillStale` (no
  passing drill in 35 d, or no file) and `PGPITRDrillFailed` warn — from the drill's own
  `vaultaire_pitr_drill.prom` beside it (its own symlink), written by every run of
  `pg-pitr-drill.sh`, PASS, FAIL or cut.
- **The drill runs itself** (Stage 1a Prompt 41 item 4, RP57-13):
  `vaultaire-pitr-drill.timer` → `vaultaire-pitr-drill.service` (root — it needs
  `runuser` and deletes the scratch cluster — `Nice=10`, `IOSchedulingClass=idle`) on the
  **2nd of each month at 02:10 UTC**, clear of the 01:30 backups and the 03:00 dump.
  `systemctl list-timers vaultaire-pitr-drill.timer`, `journalctl -u vaultaire-pitr-drill`.

### Install (once; again after an R2 key rotation)

`deploy/scripts/pg-pitr-install.sh` (tests: `bash deploy/scripts/pg-pitr_test.sh`, run
by CI) is the source; the box gets a copy. As root, from a checkout:

```bash
# the passphrase alone on stdin, into a 0600 file the installer reads and deletes
security find-generic-password -s vaultaire-pgbackrest-cipher -w | ssh vaultaire-slc 'umask 077; cat > /tmp/pitr-cipher'
sudo bash deploy/scripts/pg-pitr-install.sh /tmp/pitr-cipher
```

It installs the package (and `jq`, which the metrics writer needs), writes the conf (S3
key from `R2_*` in `configs/.env`, the endpoint `<R2_ACCOUNT_ID>.r2.cloudflarestorage.com`),
copies `pg-pitr-metrics.sh` and `pg-pitr-drill.sh` to `/usr/local/lib/vaultaire-pitr/`
(root 0755), installs the units, runs `stanza-create`, THEN enables the four timers
(backups, metrics, the monthly drill — after the stanza exists, so an install that
straddles 01:30 cannot fire a backup against a missing stanza), and sets
`archive_mode = on`, `archive_command = 'pgbackrest --stanza=vaultaire archive-push %p'`,
`archive_timeout = '60s'` with `ALTER SYSTEM` (backing up `postgresql.auto.conf` as
`.bak-pitr-<ts>` first, only when one differs from `pg_settings`' RAW value —
`current_setting()` pretty-prints `60s` as `1min`, which is why every re-run used to
re-ALTER and leave a new backup, RP58-5). Re-running it is a no-op when nothing changed.
A passphrase that differs from the installed one is refused: a repository's passphrase
never changes. Then, by hand:

1. **The restart `archive_mode` needs**, at a quiet minute: `vaultaire-switch status`
   and `gh run list --workflow deploy.yml` show no deploy running, then
   `sudo systemctl restart postgresql@18-main`. 2026-10-24: 22:29:34.9 → 22:29:37.3 UTC
   (2.4 s); the app logged 0 errors in 22:29:04–22:30:37 (66 lines); PostgreSQL ended
   23 connections (7 app, 15 `indexd_zen`, 1 s3d lab), all reconnected.
2. `sudo -u postgres pgbackrest --stanza=vaultaire check` — a segment reaches R2.
3. The first full: `sudo systemctl start vaultaire-pitr-backup@full.service`.
4. The rules: `promtool test rules` with `vaultaire-pitr.yml` and its test copied into
   one directory, `install -m 0644` into `/etc/prometheus/rules/`, reload (README).

### The drill

```bash
sudo /usr/local/lib/vaultaire-pitr/pg-pitr-drill.sh
```

Writes a row to `pitr_canary` (a scratch database on the live cluster), notes the
server's time T and the live `object_head_cache` count, writes a second row, waits for
that WAL to be archived (`pgbackrest check`), then restores **to T into a scratch
cluster** — `/var/lib/postgresql/18/pitr-drill`, port 5499, Unix socket in its own
directory, no TCP, `--archive-mode=off` and `archive_mode=off` on the command line, so
the promoted scratch cluster pushes nothing into the live repository (checked: no
timeline-2 file in `archive/vaultaire/18-1` after the drill) — and checks: the first
row present, the second absent, `object_head_cache` equal to the count at T (± 25).
It stops and deletes the scratch cluster, pass or fail (`PITR_DRILL_KEEP=1` keeps a
failed one) — and when cut by SIGINT/SIGTERM (an interrupted drill used to leave the
postmaster on 5499 and its directory, and the next drill refused). The scratch overrides
(`shared_buffers 256MB`, `max_wal_size 1GB`, `archive_mode off`, …) are APPENDED to the
restored `postgresql.auto.conf` — prod's tuning rides in with the backup and auto.conf is
read after `postgresql.conf`, so the scratch cluster used to start prod-sized (RP58-6);
the safety settings also ride `pg_ctl -o`, which outranks every file. It refuses a
scratch directory that exists, is the live one, or contains it. Every run writes
`/var/lib/vaultaire-pitr/vaultaire_pitr_drill.prom` (`…_drill_success`,
`…_drill_last_success_timestamp_seconds` kept across a FAIL, `…_drill_rto_seconds`).
**2026-10-24 22:33:44 UTC: `PITR DRILL PASS target=2026-10-24 22:33:46.946318+00
restore_s=6.910 accept_s=10.225 rto_s=12.842 ohc_restored=507 ohc_live_at_t=507`.**

### Restoring the database (the real thing)

The order matters: what writes by itself between the disaster and the restore?

1. **Stop BOTH app slots first** — a slot that boots appends `server_build` to the
   transparency log, and a running slot writes rows: `sudo systemctl stop
   vaultaire@8000 vaultaire@8001` (`vaultaire-switch` has no "stop all"; HAProxy
   answers 503 meanwhile). Stop the cluster's other writers too — the restore rewinds
   every database on it: `sudo -u user1 XDG_RUNTIME_DIR=/run/user/$(id -u user1)
   systemctl --user stop 'indexd@*' 's3d@*'` — and the backup timers:
   `sudo systemctl stop vaultaire-pitr-full.timer vaultaire-pitr-diff.timer`.
2. **Pause merges:** a merge to `main` deploys and starts a slot.
   `gh workflow disable deploy.yml` (from a checkout), and nothing enters the queue.
3. **Stop PostgreSQL and keep the damaged cluster:** `sudo systemctl stop
   postgresql@18-main`; a clean stop archives the last segments — check
   `ls /var/lib/postgresql/18/main/pg_wal/archive_status/*.ready` is empty (else
   `sudo -u postgres pgbackrest --stanza=vaultaire --no-archive-async archive-push
   /var/lib/postgresql/18/main/pg_wal/<segment>` for each while the files are still there) — then `sudo mv /var/lib/postgresql/18/main
   /var/lib/postgresql/18/main.broken-$(date -u +%Y%m%dT%H%M)` and
   `sudo -u postgres install -d -m 0700 /var/lib/postgresql/18/main`.
4. **Restore to the LATEST point unless told otherwise:**
   `sudo -u postgres pgbackrest --stanza=vaultaire restore`, then
   `sudo systemctl start postgresql@18-main` and wait for
   `SELECT pg_is_in_recovery()` = `f`. Every `tenant_key_versions` row created after
   the chosen point is gone with it, and whatever was sealed under such a key **can
   never be opened** — the reason the default is the latest point. Even the latest
   loses up to a minute: objects written in it have bytes on a backend and no row —
   orphans, found by the `chunk_orphans` job (`POST /api/v1/admin/chunk-orphans`, dry
   run) and `cmd/tools/target-orphans`.
5. **An earlier point** (`--type=time --target='<UTC time>' --target-action=promote`,
   only when the owner says so — an operator's mistake to undo) means more: every
   object written after it is such an orphan, and every object deleted after it comes
   back as a row naming a blob `retired_blobs` has already deleted — the daily
   `routing_truth` job reports them `missing`.
6. **The transparency log:** read `docs/TRANSPARENCY.md` § After a database restore.
   A restore to before the newest signed checkpoint leaves the database behind the
   high-water file and the log **freezes by design** (`TranslogFrozen`,
   `TranslogCheckpointRefused`) until `translog-restore` re-appends the lost entries
   from the witness. Never delete the high-water file.
7. **Back in service:** `sudo systemctl start vaultaire@$(cat /opt/vaultaire/ACTIVE_PORT)`
   (HAProxy points at it), the other writers and the timers again,
   `gh workflow enable deploy.yml`; then a new full at once
   (`sudo systemctl start vaultaire-pitr-backup@full.service`) — the restore opened a
   new timeline, archived into the same repository. Keep `main.broken-*` until the
   owner says otherwise.

**The box lost too:** a new box with PostgreSQL 18, then
`pg-pitr-install.sh --restore-box <cipher-file>` (the passphrase from the Keychain, the
R2 key from the Cloudflare dashboard in `configs/.env`): the package, the conf and the
helpers only — it skips `stanza-create`, which would refuse the fresh cluster's system
identifier and, under `set -e`, used to kill the installer mid-script during the
disaster (RP58-7); no timer is enabled and nothing is ALTERed (the restore brings the
archive settings back). Then steps 3–7, and once the restored cluster runs,
`pg-pitr-install.sh <cipher-file>` again without the flag (stanza-create now matches,
the timers come back on).

## Data nodes

A data node (WP-EDGE-3, `docs/reviews/WP-EDGE-3.md`) is this binary with
`VAULTAIRE_ROLE=node` on a VPS: it moves customer bytes between S3 clients
and the backends on the links the control plane (the origin) mints, and
asks the control plane for every decision. **What a node never holds:** a
database, the master key, a tenant key version, an S3 secret, a backend
key, a job. What it holds: its own token (minted once by the registry) and,
for the seconds a request lasts, that request's presigned links and — for a
codec write — the tenant's codec PUBLIC key and the per-chunk random keys it
makes. A node is not the attested data plane (contract line 10).

**What it serves:** PutObject and UploadPart (the move, or the codec when
`edge_codec` is on for the tenant) and GetObject of a whole unsealed row
through `internal/dataplane`; every other signed S3 request (HEAD, DELETE,
listings, Create/Complete/Abort, any pass-through) is handed to the origin
untouched with the client's Host; the origin's web and API paths are 404.
Clients reach a node by the hostname they configure as their endpoint:
**no S3 client follows a 307 with a fresh signature** (the probe of
2026-10-28), so there is no redirect steering.

**Register it** (an admin JWT — the owner's TOTP):

```bash
curl -X PUT https://stored.ge/api/v1/admin/data-plane-hosts/node1.example.net \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"kind":"node","address":"203.0.113.10","weight":1}'
# → the row and "token": "<64 hex>" — ONCE. Only its sha256 is stored; a lost
#   token is a DELETE and a new PUT. A tenant-scoped (BYO) node adds
#   "tenant_id": it admits that tenant only, and nothing it reports is trusted
#   for integrity (every stored chunk opened at commit) or metering (its reads
#   metered at the admitted span).
```

The token is accepted only from `address` (the node's IP as HAProxy sees
it). **The control-plane URL is therefore a DNS-only origin** (RP74-1):
`https://s3.stored.ge` today — never `https://stored.ge`, which Cloudflare
proxies, so HAProxy's peer would be a Cloudflare edge, never the node, and
every admit, commit and heartbeat would be the route's 404 (the node then
logs one Error line: "the control plane answers 404 to this node"). The
four edge routes answer on every host and the S3 gate never captures them
(`TestEdgeRoutes_NeverCapturedByTheS3Gate` pins the `s3.` host). Do not
widen the gate to `CF-Connecting-IP`: a node behind the proxy would also
meet the 100 MB body cap and lose the direct-peer rule that makes a stolen
token useless off the registered box. **Install it** on the node, as root,
from a SIGNED release:

```bash
go build ./cmd/tools/release-sign ./cmd/tools/cf-range   # from the release's tagged source
RELEASE_VKEY_FILE=./release.vkey NODE_ACME_EMAIL=ops@example.net deploy/node/install.sh \
  https://github.com/FairForge/vaultaire/releases/download/<tag> https://s3.stored.ge node1.example.net
umask 077; printf %s "$TOKEN" > /etc/vaultaire/node_token   # the registry's token
systemctl daemon-reload && systemctl enable --now vaultaire-node
```

`install.sh` refuses a control-plane host that resolves into a Cloudflare
range (`cf-range`, the origin's own `internal/clientip` table).

**TLS** (RP74-3): the node terminates TLS itself on `:443`, one of two ways —
the recipe is what `install.sh` writes into `node.env`:

- **ACME** (Let's Encrypt): `NODE_ACME_EMAIL=ops@example.net` with
  `NODE_PUBLIC_HOST=node1.example.net` (its A record pointing at the node,
  DNS-only). The node answers the HTTP-01 challenge on `:80` and caches the
  account and certificates under `NODE_STATE_DIR` (the unit's
  `StateDirectory=vaultaire-node` → `/var/lib/vaultaire-node/acme`);
  renewals are automatic.
- **An operator's certificate:** `NODE_TLS_CERT=/etc/vaultaire/node.crt`
  and `NODE_TLS_KEY=/etc/vaultaire/node.key` (both or neither; read once at
  boot — restart after a renewal). Readable by the unit's dynamic user: put
  them in `/etc/vaultaire` with mode 0644 / 0640 and `LoadCredential=` if you
  prefer.

A node with neither refuses to boot unless `NODE_BIND` is loopback
(`127.0.0.1`: the lab, plain HTTP on 8100). `/metrics` is never on the
public port: it listens on `NODE_METRICS_ADDR` (default `127.0.0.1:9100`,
`off` = none; RP74-10).

**A key's IP allow-list and a node do not mix** (RP74-8): a request a node
hands on to the origin (HEAD, DELETE, a listing, Complete…) reaches HAProxy
from the node, so the origin evaluates it — the key's allow-list, the
per-IP limits, the access log's `client_ip` — under the NODE's address.
Until the control plane trusts a registered node's `X-Forwarded-For` as one
more hop, a tenant whose keys carry an IP allow-list must not be sent
through a node.

`install.sh` installs only what `release-sign verify` accepts under the
release verifier key (none exists until the owner's key ceremony —
docs/RELEASES.md — so no node can be installed before it). The unit
(`deploy/node/vaultaire-node.service`) runs as a dynamic user with the token
as the systemd credential `node_token`; `/etc/vaultaire/node.env` holds
`NODE_CONTROL_PLANE_URL`, `NODE_PUBLIC_HOST`, `PORT` (443) and the TLS
settings. The origin must
also accept the node's hostname on pass-throughs (HAProxy, Cloudflare) —
check with the first node.

**Health:** the node heartbeats `POST /api/v1/edge/heartbeat` every 30 s; a
node unseen for 90 s takes no new admit (503 `host_down`, Retry-After 30) and
the `edge_admit_reaper` marks it down; `vaultaire_data_plane_host_up{host,kind}`,
rule `DataPlaneHostDown` (`deploy/monitoring/vaultaire-dataplane.yml`), the
System page's Data-plane hosts card. Drain one with `"health":"draining"`
on its row (its commits are always accepted). The lab: `deploy/node/lab-proof.sh`.

**`host=slc`:** the flag `data_plane_slc` (per tenant, default OFF) sends a
tenant's edge-eligible PutObject / UploadPart / GetObject through the same
package on this box (registry row `slc`, weight 0) — the bytes still cross
SLC and are metered by the origin's own writer; the rows are the one shape.

## The second copy (`edge_mirror`) and the catalogue

**The mirror** (WP-EDGE-4, `docs/reviews/WP-EDGE-4.md`): `edge_mirror` ON for a tenant gives every blob it writes at the edge (the Worker, a node, host=slc) a copy on a `MIRROR_LEGS` leg (default `lyve,idrive`: an object on Lyve is copied to iDrive and the reverse) at the same address, recorded in `blob_copies`. Turning it on is also the **backfill**: the `blob_copies` job (every 10 min) enqueues a `pending` copy for every whole non-vault object of the tenant (1,000 a run) and copies at most 50 GiB a run from the primary through this box (the 2× lane). Set it for one tenant with the admin API (`PUT /api/v1/admin/flags/edge_mirror {"enabled":true,"tenant_id":"…"}`) — or, without the owner's TOTP, a `feature_flags` row plus its `flag.set` `audit_logs` row (event_type and result set), as `data_plane_slc` was. Watch it:

```sql
SELECT state, COUNT(*), pg_size_pretty(SUM(size)) FROM blob_copies WHERE tenant_id = '<tenant>' GROUP BY state;
SELECT last_outcome, result FROM job_runs WHERE job = 'blob_copies';
```

and `vaultaire_blob_copies_total{outcome}`, `vaultaire_blob_copies_pending`, `vaultaire_blob_copies_reads_total{outcome="served_mirror"}` (a read the primary did not serve). A row stuck `pending` past 20 attempts is the job's note; its `last_error` says why. `edge_rs` (RS 4+2) stays OFF until a third fast leg exists.

**The catalogue** (plan 10.3): seed a public artefact with `cmd/tools/catalogue-seed` (operator-run; `DATABASE_URL` and the blob backend's S3 pair in the environment, never a flag); the daily `catalogue_filter` job publishes the filter on `r2` (`t-_catalogue/catalogue/catalogue/<version>.bloom`, `…/latest`); the Worker reads `CATALOGUE_URL` (a `[vars]` entry: the public URL that serves those two files) — unset on prod until a public URL and the first artefacts are chosen.

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
  the dashboard sign-in counters and the TLS expiry gauge. **Denied from
  outside since 2026-10-22 (WP-R1-1)**: both public frontends (`http_front`,
  `https_front`) answer 404 to `/metrics`, `/health/backends` and `/health`
  with a `details` query, matched on the decoded, lower-cased path in a
  `txn` variable — the request itself is never rewritten (a SigV4 signature
  covers the path; keys may hold `//`), so `//metrics`, `/%6detrics` and
  `/METRICS` are refused too. Prometheus scrapes through `127.0.0.1:8010`
  (`frontend metrics_internal`, no deny) and every runbook line reads
  `localhost:8000/health/backends`; plain `/health`, `/health/ready` and
  `/version` stay public (the deploy check reads `/version`). The rules sit
  right under each frontend's `bind` line and survive `vaultaire-switch`,
  which rewrites only the `server` lines; backups
  `haproxy.cfg.bak-metrics-deny-*`, `haproxy.cfg.bak-health-deny-*`.
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
| Is the process up? | `curl -s https://stored.ge/health/live` (200 = serving); `sudo vaultaire-switch status` (both slots, what HAProxy routes to) |
| Is it ready / which backends are healthy? | `curl -s https://stored.ge/health` — JSON with per-backend probe state and counts; `curl -s localhost:$(cat /opt/vaultaire/ACTIVE_PORT)/health/backends` on the box for the detailed list (keep it off the public edge); `/health/ready` for the readiness verdict; `/status` is the HTML page |
| What is running? | `curl -s https://stored.ge/version` — the commit and its build stamp, which is the COMMIT time in UTC since RP37-9 (the reproducible recipe), not when it was built; the deploy job fails if it does not report the commit it just deployed |
| Logs | `journalctl -u 'vaultaire@*' -f` (both slots; `-u vaultaire@8001` for one) (Zap JSON; request ids in `X-Request-Id` / the S3 `RequestId`). Password-reset bodies are no longer logged (R14-01) |
| Metrics right now | `curl -s localhost:8010/metrics \| grep vaultaire_backend_health` on the box (HAProxy routes `:8010` to the active slot); `localhost:9090` for Prometheus, `curl -s localhost:9090/api/v1/rules` for loaded rules |
| Deploy did not run | `gh run list --workflow deploy.yml`; `gh workflow run deploy.yml --ref main` to re-trigger |
| Deploy failed | nothing was switched: the job log says which gate failed (not live, not the slot's own process, HAProxy config invalid, HAProxy never reported it UP, public `/version` mismatch, a migration hit `lock_timeout`); `journalctl -u vaultaire@<idle port> --since -10m` for the new binary's boot error (a missing `JWT_SECRET`, a migration that broke a query the new code needs) |
| Roll back by hand | `sudo vaultaire-switch rollback` — switches to the idle slot, which holds the previous build, with the same gates and no downtime |
| Backend key dead | `vaultaire_backend_health{backend="…"}` is 0 and `BackendProbeFailing` fires; rotate the key in the env file and `sudo vaultaire-switch restart` (env is read at boot only; restart boots the active build in the other slot and switches, no downtime) |
| Cert expiry alert | `sudo certbot renew --cert-name <sni> --force-renewal`; `--dry-run` is a false signal on this box (staging CA) |
| Change a feature flag | `PUT /api/v1/admin/flags/{key}` with an admin JWT, or the dashboard `/admin/flags`; ~15 s to take effect, no restart |

Never `pkill` by pattern on the host; the unit is the only supervisor.
