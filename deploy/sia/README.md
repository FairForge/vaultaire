# deploy/sia — running Sia gateways (s3d) safely

The ops side of the `sia` backend: Stored's patched s3d build, the systemd
units that run it, continuous replication of each gateway's catalogue, metrics,
a read canary, log rotation and the self-hosted indexd lab's backups. The why
and the numbers are in `bench-results/SIA-S3D-2026-10.md` (SIA-BENCH-2..5).
Nothing here is sent upstream.

Everything runs as `user1` in its own systemd **user** instance (linger on), in
its own directories. Outside them there are three root-owned textfile symlinks
and the alert rules file.

## The build (`build-s3d.sh`, `patches/`)

Upstream s3d v0.2.1 with the siastorage SDK (`v0.2.2-0.20260831091740-f9961f7e5391`)
and the indexd v0.5.0 client patched, all versions pinned:

- **Funds-aware reads.** A host refusing a read for lack of account funds
  (`not enough funds`, the plan's read budget) no longer counts as a failed
  host (indexd client `isFailedRPC`) or as congestion (SDK limiter). A chunk
  that failed mostly on refusals waits for the 5-minute refill: 15 s, 30 s,
  then 60 s steps, about 10 min in all. Mainnet, free plan: 16/16 back-to-back
  1 GiB reads complete at the budget's rate, where stock s3d cut them.
- **Chunk retry.** Up to 5 re-attempts of a failed chunk, with 0.5–8 s
  backoff.
- **Overdrive switch.** `S3D_READ_OVERDRIVE=0` starts exactly MinShards
  reads, not 1.5×. It cuts refusals 28% and the worst stall from 268 s to
  118 s, with the same total time. Use it for bulk copies, not interactive
  reads.
- **Logging.**
  - `sector read failed` and `shard write failed` warnings, per host, with
    the error.
  - `chunk waiting for account refill` and `chunk recovery failed, giving
    up`.
- **Host timeout.** `S3D_DOWNLOAD_HOST_TIMEOUT` sets the per-host download
  timeout (default 60 s).

```bash
./build-s3d.sh                                    # app ID "s3d"        -> ~/gopath/bin/s3d-stored        (gateway 1)
APP_ID=stored-s3d-app2 OUT=~/gopath/bin/s3d-stored-app2 ./build-s3d.sh   #  -> gateway 2
```

An instance must always run the build of **its** app ID: another ID derives
another app key, and its objects become invisible. Rebuild after every patch
change, then restart the unit.

## Units (`systemd/` → `~/.config/systemd/user/`)

| Unit | What |
|---|---|
| `s3d@<instance>.service` | the gateway in `~/<instance>/` (its `s3d.yml` + `data/`); binary and extra env from `~/.config/stored-sia/<instance>.env` (`S3D_BIN=…`, optional `S3D_READ_OVERDRIVE=0`). `TimeoutStopSec=120`: graceful stop has no deadline upstream, and a SIGKILL is safe (buffered objects survive and re-upload whole, SIA-BENCH-2 Test B). **One unit per app key and data directory**: two instances on one key can unpin each other's data (s3d README) |
| `s3d-flush@<instance>.timer` | `s3d flush` every 10 min. A PUT's 200 is only on local disk until uploaded, and v0.2.1 has no `uploadDeadline` |
| `litestream.service` | replication of the catalogues (below) |
| `indexd@<instance>.service` | a self-hosted indexd in `~/indexd-<instance>/` (lab: `zen`, the Zen testnet); `INDEXD_BIN` from `~/.config/stored-sia/indexd-<instance>.env` |
| `indexd-backup@<instance>.timer` | daily 03:40 UTC: `pg_dump` (custom format, checked with `pg_restore -l`) plus `indexd.yml` (it holds the wallet's recovery phrase: 0600, never off the box unencrypted); 7 days kept |
| `sia-metrics.timer` | every minute: `sia-metrics.py` → `/opt/vaultaire/metrics/vaultaire_sia.prom` |
| `sia-canary.timer` | every 5 min: `sia-canary.py` reads a 4 MiB object (and once a day a 256 MiB one) back from each gateway and compares the MD5 → `vaultaire_sia_canary.prom` |
| `sia-logrotate.timer` | hourly: `logrotate.conf`, since s3d never rotates its `data/s3d.log` (a lab gateway reached 90 MB in a day) |

Running on SLC since 2026-10-21:
- **Gateways:** `s3d@s3d-lab` (gateway 1, `s3d-stored`), `s3d@s3d-lab2` (gateway 2, `s3d-stored-app2`), and the Zen lab's `s3d@s3d-self`, `s3d@s3d-funds` and `indexd@zen`.
- **Timers:** `s3d-flush@s3d-lab{,2}`, `sia-metrics`, `sia-canary`, `sia-logrotate` and `indexd-backup@zen`.

## The catalogue: Litestream (`litestream.yml`)

`s3d.db` is the **only** map from keys to Sia objects; a fresh s3d logged in
with the same seed recovers nothing (SIA-BENCH-2 DR). Litestream v0.5.17
replicates each mainnet gateway's catalogue every second:

- **Target:** R2 bucket `stored-sia-litestream` (production Cloudflare
  account), prefixes `s3d-lab/` and `s3d-lab2/`.
- **Encryption:** **SSE-C**. v0.5 dropped `age` and refuses that config.
- **The key:**
  - on the box: `~/.config/stored-sia/litestream-ssec.key` (0600, base64);
  - for restores: the owner's macOS Keychain, service `sia-litestream-ssec`,
    account `stored-sia`.
- **Credentials:** `~/.config/stored-sia/litestream.env` (0600): R2 keys
  derived from the zone token, plus `R2_ACCOUNT_ID`.
- **Metrics:** `127.0.0.1:9097/metrics`, folded into `vaultaire_sia.prom`.

**Restore drill** (passed 2026-10-21, off the box):
- A replica object read without the key is refused (`InvalidRequest`).
- Both catalogues restored on the Mac pass `pragma integrity_check`, with
  object counts equal to the live gateways (662, 2).

```bash
# key from the Keychain into a 0600 file; config = litestream.yml with that key path
litestream restore -config drill.yml -o restored.db /home/user1/s3d-lab/data/s3d.db
python3 -c "import sqlite3;c=sqlite3.connect('restored.db');print(c.execute('pragma integrity_check').fetchone(), c.execute('select count(*) from objects').fetchone())"
```

To bring a gateway back:
1. Stop its unit.
2. Restore to `~/<instance>/data/s3d.db`.
3. Start the unit, which must run **the same build** (same app ID).

Never run the restored copy beside the original.

## Metrics and alerts

`sia-metrics.py`, per gateway:
- the unit state and `s3d status` (pending, uploaded, failed, unpinned and
  orphaned objects; buffer used and limit);
- `vaultaire_sia_oldest_pending_seconds`;
- `vaultaire_sia_log_events_total{event}`: funds_refusal, refill_wait,
  chunk_giveup, body_cut, shard_write_failed;
- Litestream's sync and error counters.

The `.prom` files live in `/opt/vaultaire/metrics/` (user1). node_exporter
reads them through root-owned symlinks in `/var/lib/prometheus/node-exporter/`,
the backup-metrics layout:
`vaultaire_sia.prom`, `vaultaire_sia_canary.prom` and `vaultaire_sia_indexd_backup_zen.prom`.

Rules: `deploy/monitoring/vaultaire-sia.yml`, 10 rules, installed 2026-10-21
(sha256 = repo), promtool test `deploy/monitoring/tests/vaultaire-sia_test.yml`:
- **critical:** SiaCatalogueNotReplicating;
- **warning:** SiaGatewayDown, SiaPendingTooOld, SiaUploadsFailing,
  SiaBufferFilling, SiaReadsGivingUp, SiaCanaryFailing, SiaMetricsStale,
  SiaIndexdBackupStale;
- **info:** SiaReadBudgetSpent.

## Secrets

- **The sia.storage account seed** (12 words, partner@fairforge.io) is only
  needed at `s3d login`. It lives in the owner's macOS Keychain (service
  `sia-storage-seed`); it was shredded off SLC on 2026-10-21.
- **Zen lab seeds** are testnet only. The indexd one is inside
  `~/indexd-zen/indexd.yml` (0600), which indexd reads at start.
- **Each gateway's `s3d.db`** holds its app key, so a backup of it is a
  secret too (hence SSE-C).
- **Never expose s3d** beyond 127.0.0.1. A Cloudflare path to it goes through
  a named Tunnel plus an Access service token, never a quick tunnel.
