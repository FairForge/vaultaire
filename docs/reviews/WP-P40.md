# Stage 1a — Prompt 40: Part 0 (the S3-credentials gate, the DNS cache race, robots/sitemap/favicon) and Part A (point-in-time recovery of the metadata database)

*2026-10-24. Split into two sessions. **Session A** ran Part 0; its findings are in
[`WP-P40-PART0.md`](WP-P40-PART0.md), in its own PRs (#953 `9864016c`, #956 `5e5fc23c`, #957 `dec6941d`). **Session B** ran Part A
and this docs PR: #954 `9bbfb600` (RP55-1), #955 `cac9a31c` (PITR), then this docs PR, opened
after every Session A PR had merged (Prompt 39's `WP-P39.md` had not: Step 0). **No migration** (the next is still **108**).*

Lane: Engine (the metadata database's durability; plan-gaps item 5 "PITR") and Trust
(contract rule 8: the produce page and the legal pages state the new backup leg from code
facts under test).

## Step 0

- PR #952 (`WP-P39-REVIEW.md`) and #950 (`tenant-zero-proof.sh`) merged before Part A's
  first PR; `/version` = main's tip `bc74487b` (21:55 UTC build), active slot :8000.
- The 16 rule files on the box matched main by sha256 (checked 22:36 UTC); `vaultaire-pitr.yml` was the 17th, installed from #955's branch before its merge and equal to main after it.
- Prompt 39's docs PR (`WP-P39.md`) had **not** landed when this PR opened (Prompt 39 Session B's worktree held no work past #950); the prompt's rule is to wait for it, so this PR was held unarmed until the owner decided — see the PR body — its residual RP55-1 (the Blomp
  retention listing) was carried here as the prompt asked (#954).

## Part 0 — merged; the findings are [`WP-P40-PART0.md`](WP-P40-PART0.md)

| PR | Merge | Items |
|---|---|---|
| #953 | `9864016c` (22:41 UTC) | item 2 — RP56-2, the DNS cache re-reads its entry inside the singleflight (the main CI flake) |
| #956 | `5e5fc23c` (22:59 UTC) | item 1 — RP56-1 (P3), **a request carrying S3 credentials is the S3 API's on every host before any route** (`s3CredentialGate`, one predicate `signedS3Request`; exceptions Bearer/Basic, OPTIONS, `GET /api/v1/whoami`, a public trust host; a `chi.Walk` matrix); item 4 — `/register` closed → 303 `/#waitlist`; item 5 — the tenant-zero proof takes a real MFA sign-in's JWT, never one minted from `JWT_SECRET`; item 6 — our realm-token mint failure is not the realm's `invalid_auth`; **found doing item 5 (P2, pre-existing): `POST /auth/login` minted a 24 h admin JWT on the password alone for an account with MFA on** — now it needs `"code"` and audits every outcome |
| #957 | `dec6941d` (00:14 UTC) | item 3 — RP56-3, `robots.txt` / `sitemap.xml` / `favicon.ico` to an anonymous fetch on the web host; the note |

Prod of #956 is in Session A's note (fake-signature probe 26/26 `403 application/xml`, the bench tenant's real round trip on `docs` / `legal` / `dashboard` / `status`). **Prod of #957, run by this session** (deployed 00:16:29 UTC, `/version` `dec6941d5b3b`; before = 00:15:00 on `5e5fc23c`):

| | before (`5e5fc23c`) | after (`dec6941d`) |
|---|---|---|
| `stored.ge` / `stored.cloud` `robots.txt`, `sitemap.xml`, `favicon.ico` (`Accept: */*`, Googlebot UA) | 403 `application/xml` × 6 | 200 `text/plain` / `application/xml` / `image/x-icon`; cache `max-age=3600` / `300` / `86400` (stored.cloud's zone rewrites robots to 14400 — RP57-16) |
| `s3.stored.cloud` the three | 403 XML | 403 XML (unchanged) |
| `robots.txt` | — | `Disallow: /dashboard`, `/api/`, `/auth/`, `/cdn/`; `Sitemap: https://stored.ge/sitemap.xml` (the canonical origin today) |
| `sitemap.xml` | — | 16 URLs: `/`, five `/docs…`, `/status`, `/changelog`, eight `/legal/…`; no refunds, dmca, law-enforcement or what-we-can-produce (flags off) |
| `favicon.ico` | — | 561 B, sha256 `98563531…` = main's `internal/api/favicon.ico` |
| a fake-SigV4 GET of the three on `stored.ge` | — | 403 XML × 3 (the S3 API) |
| the same on `stored.cloud` | — | `sitemap.xml` 403 XML; **`robots.txt` and `favicon.ico` 200 from Cloudflare's cache (`cf-cache-status: HIT`)** — RP57-5 confirmed, wider (RP57-16) |
| journal of slot 8000 since 00:16:29 | — | 180 lines, **0** `"level":"error"`, 3 warn (`BLOMP_ALLOW_UNSEALED`, two bench-tenant HEAD cache misses) |

Session A's residuals are RP57-1…6 in its note.

## Part A — point-in-time recovery (#955) and RP55-1 (#954)

### The gap

Nightly `pg_dump` only (`pg-backup.sh`, 03:00 UTC): an RPO of up to 24 h. A lost day loses
the head rows of that day's objects (bytes on a backend, no row), brings back rows naming
blobs `retired_blobs` already deleted, and — the one that cannot be repaired — loses every
`tenant_key_versions` row created that day: whatever was sealed under it never opens again.

### What was built

| Piece | What it does | Held by |
|---|---|---|
| `deploy/scripts/pg-pitr-install.sh` | pgBackRest 2.58 (Ubuntu package); `/etc/pgbackrest/pgbackrest.conf` postgres 0640 — repo1 = private R2 bucket `stored-pg-pitr` path `/vaultaire`, `aes-256-cbc`, `retention-full=4` (count), bundles, zstd 3, `process-max` 4 (archive-push/get 2), `archive-async` spooled on the NVMe; the S3 key from `R2_*` in `.env`, the passphrase from a 0600 file read then deleted — never argv; a passphrase that differs from the installed one is refused; units `vaultaire-pitr-backup@.service` + full (Sun 01:30) / diff (Mon–Sat 01:30) timers + the metrics timer (every minute); helpers in `/usr/local/lib/vaultaire-pitr/` (root 0755: postgres runs what root installed); `/var/lib/vaultaire-pitr/` + the textfile symlink; `stanza-create`; `ALTER SYSTEM` archive_mode / archive_command / `archive_timeout = 60s` (auto.conf backed up first, only on change, reload). The restart stays the operator's quiet-minute step | `pg-pitr_test.sh` §2–3 |
| `pg-pitr-metrics.sh` | every series every minute, at 0 when unknown, `*_success` per source — `pg_stat_archiver` and `pgbackrest info` (status 0/2 = answered; 1 = missing stanza = not) | §4 |
| `pg-pitr-drill.sh` | canary rows around T in `pitr_canary`, `pgbackrest check` (the WAL past T archived), `restore --type=time --target=T --target-action=promote --archive-mode=off` into `/var/lib/postgresql/18/pitr-drill` (refused if it exists, is the live dir, is inside it or contains it), its own `postgresql.conf` (port 5499, socket in its own dir, no TCP, archive off, the live hot-standby maxima), `-c archive_mode=off` on the command line too; before row present, after row absent, `object_head_cache` = live at T ± 25; RTO; stop + delete pass or fail | §5 |
| `vaultaire-pitr.yml` | PGArchiveFailing (critical, 5 m), PGArchiveStale (critical, > 10 m or absent), PGPITRBackupStale (no full 8 d / no diff-or-full 26 h), PGPITRRetentionTooLong (> 31 d), PGPITRMetricsStale | `tests/vaultaire-pitr_test.yml` (promtool 2.53.5 = the box's) |
| produce + legal pages | `produce.json` `backups.pitr` {28 d, 60 s, Cloudflare R2, AES-256, what it holds} + `Backups.UpToDays()` = 30; the page's sentence; the five legal pages' backup sentences | `TestProducePITRIsTheInstallScript`, `TestPITRRetentionAlarmIsThePagesNumber`, `TestLegalBackupSentencesAreTheFacts`, `TestLegalLE_ProducePageStatesTheBackupFacts`, `TestBackups_UpToDaysIsTheLongest` |
| `docs/DEPLOY.md` § Point-in-time recovery | install, the drill, **the real restore** in order: both slots stopped first, the cluster's other writers, merges paused (`gh workflow disable deploy.yml`), the damaged dir kept, latest point by default (`tenant_key_versions`), an earlier point's costs (orphans, resurrected rows → `routing_truth` missing), the translog high-water | — |
| RP55-1 (#954) | each leg's retention pass writes `vaultaire_backup_offbox{,2}_retention_last_run_timestamp_seconds` after it listed the leg; `BackupOffboxRetentionNotRunning` per leg, > 26 h or absent | `pg-backup_test.sh` 4j (red first), promtool |

### Findings

- **F1 — the legal pages said "database backups age out within 7 days"** (privacy, terms,
  GDPR, DPA, BAA), and the DPA/GDPR "off-host copies planned before launch". False since
  the Sync off-box leg (30 days, 2026-10-07) — and the produce page and the backup rule's
  own description already said 30. Now 30 (`UpToDays`), the DPA/GDPR measure states the
  dump's 7/30 days and the archive's 28, Cloudflare's listed purpose names the archive,
  "Last updated" October 24; a test fails if any of them moves.
- **F2 — the cluster is shared**: `vaultaire`, `vaultaire_prod`, the Sia `indexd_zen`, the
  lab databases. PITR archives all of them, a restore rewinds all of them; the runbook stops
  their writers too (RP57-12).
- **F3 — the prod R2 key is account-admin**: it created `stored-pg-pitr` (so no F-38), and
  it is also the app's key — RP57-10.
- **F4 — Sync.com and Blomp are not listed as sub-processors** although they hold encrypted
  dumps — RP57-11 (owner/counsel).

### Mutations (all killed)

Shell, on a fresh copy each (`~/.cache/mut.sh`, Ubuntu 24.04 container, non-root):
| # | Mutation | Killed by |
|---|---|---|
| 1 | restore without `--archive-mode=off` | 5a restore has `--archive-mode=off` |
| 2 | scratch start without `-c archive_mode=off` | 5a started with archive_mode=off |
| 3 | `refuse_unsafe` skipped | 5g nothing run for the live dir (5 FAIL) |
| 4 | the after-T row check made true | 5b a restore past T must fail |
| 5 | the scratch dir never deleted | 5a/5b/5e (3 FAIL) |
| 6 | `pgbackrest info` status not checked | 4d missing stanza = info_success 0 |
| 7 | a different cipher accepted | 3c |
| 8 | the cipher file kept | 3a |
| 9 | `PITR_RETENTION_FULL=5` | 3a conf has `repo1-retention-full=4` |
| 10 | no `pgbackrest check` before the restore | 5a check, then restore (3 FAIL) |
| 11 | "inside the live dir" not refused | 5g `…/main/sub` |
| 12 | an existing dir not refused | 5g leaves it alone |
| 13 | a failed info keeps a non-zero full time | 4c |
| 14 | the conf 0644 | 3a |
| 15 | the scratch cluster with a TCP listener | 5a no TCP listener |

Go (`git checkout --` after each; the branch committed first):
| # | Mutation | Killed by |
|---|---|---|
| 1 | `PITR_RETENTION_FULL=5` | `TestProducePITRIsTheInstallScript` (35 ≠ 28) |
| 2 | `PITR_CIPHER_TYPE=none` | same (no cipher name) |
| 3 | `PITR_PG_ARCHIVE_TIMEOUT=300s` | same |
| 4 | a monthly full calendar | same (the full must be weekly) |
| 5 | `repo1-retention-full-type=time` | same |
| 6 | the endpoint moved to Lyve | same (vendor) |
| 7 | the alarm at 32 d | `TestPITRRetentionAlarmIsThePagesNumber` |
| 8 | the DPA back to "7 days" | `TestLegalBackupSentencesAreTheFacts` |
| 9 | the GDPR page's archive reach 14 d | same |
| 10 | the privacy sentence reworded away | same (5 sentences expected) |
| 11 | the produce page's "up to" bound hard-coded | `TestLegalLE_ProducePageStatesTheBackupFacts` |

### Prod (2026-10-24, from #955's bytes, sha256-checked before the run)

| Step | UTC | Result |
|---|---|---|
| R2 bucket | 22:07:53 | `stored-pg-pitr` created with ACL private by the prod R2 key (rclone; the key lists `snapshelter-r2-check`, `vaultaire-public`) — no F-38 |
| Keychain | 22:27 | `vaultaire-pgbackrest-cipher` (64 chars, `openssl rand -base64 48`), added through `security -i` on stdin |
| install | 22:28:19 | pgbackrest 2.58.0-1 installed; conf 0640 postgres; 3 timers enabled; `stanza-create` 1.2 s; `postgresql.auto.conf.bak-pitr-20261024T222820`; ALTER SYSTEM; `archive_mode` pending_restart |
| restart window | **22:29:34.916 → 22:29:37.310** | 2.4 s (accepting at 2.41 s); app journal 22:29:04–22:30:37: **66 lines, 0 error, 0 warn**; PostgreSQL ended 23 connections (7 `vaultaire`, 15 `indexd_zen`, 1 `s3dlab`), all back; no deploy ran (#953/#954 in CI) |
| check | 22:30:49 | segment `…9F` archived (0.53 s push, 0.78 s check) |
| first full | 22:31:04–22:31:11 | `20261024-223104F`: 473.1 MB, 5,932 files, 7.6 s (copy window 5 s ≈ 95 MB/s logical), **79.4 MB in R2**; expire clean |
| latency | 22:32:30–22:32:49 | 10 forced switches, switch → `pg_stat_archiver`: 1145, 830, 651, 699, 693, 1244, 682, 638, 640, 667 ms (**median 0.68 s, max 1.24 s**) |
| rules | 22:33 | promtool test SUCCESS on the box; installed sha256 `f69bf673…` = repo; 116 rules; 5/5 health ok |
| drill | 22:33:44 | transcript below — **PASS, RTO 12.8 s** |
| after | 22:35 | no timeline-2 file in `archive/vaultaire/18-1`; live timeline 1; app journal since 22:28: 275 lines, 0 errors |
| #954 on the box | 23:06 | `pg-backup.sh` sha256 `38544f7f…` and `vaultaire-backup.yml` `5b80cd61…` = main (`9bbfb600`); promtool test on the box SUCCESS; two new textfile symlinks; a hand run 23:06:44 → 23:07:20 completed, both legs' `retention_last_run` written, undeleted 0 / 0; 6/6 backup rules health ok, `node_textfile_scrape_error` 0 |
| #955 merged | 22:54 (deploy) | the box's `pg-pitr-metrics.sh` `9279939e…`, `pg-pitr-drill.sh` `2dfa9d11…`, `vaultaire-pitr.yml` `f69bf673…` = main (`cac9a31c`); 58 segments archived, 0 failed (23:42) |

```
22:33:44 drill 20261024T223344-2497507: stanza vaultaire, live /var/lib/postgresql/18/main, scratch /var/lib/postgresql/18/pitr-drill on port 5499
22:33:46 T = 2026-10-24 22:33:46.946318+00 (object_head_cache at T: 507 rows)
22:33:49 WAL past T archived (pgbackrest check)
2026-10-24 22:33:50.096 P00   INFO: repo1: restore backup set 20261024-223104F, recovery will start at 2026-10-24 22:31:04
2026-10-24 22:33:50.096 P00   INFO: remap data directory to '/var/lib/postgresql/18/pitr-drill'
2026-10-24 22:33:56.658 P00   INFO: restore size = 473.1MB, file total = 5932
2026-10-24 22:33:56.658 P00   INFO: restore command end: completed successfully (6904ms)
22:33:56 restored into /var/lib/postgresql/18/pitr-drill in 6.910 s
22:34:02 accepting connections 3.315 s after the restore, promoted 5.932 s after it
22:34:02 canary: before-T row present, after-T row absent
22:34:02 object_head_cache: 507 rows restored, 507 live at T
22:34:02 PITR DRILL PASS target=2026-10-24 22:33:46.946318+00 restore_s=6.910 accept_s=10.225 rto_s=12.842 ohc_restored=507 ohc_live_at_t=507
22:34:02 scratch cluster stopped
22:34:02 scratch cluster deleted (/var/lib/postgresql/18/pitr-drill)
```

**RPO ≤ ~61 s** (`archive_timeout` 60 s + the push, max 1.24 s measured); **RTO 12.8 s**
for the 473 MB cluster (restore 6.9 s, accepting 10.2 s, promoted 12.8 s). Expected
repository size: ~80 MB per full, diffs, ~WAL of a day per day kept — a few GB at four
weeks. **The restart window was 22:29:34–22:29:38 UTC**; Session A's prod proofs ran from 23:19 UTC (its note) — no overlap.

## Owner items

- **No F-38.** The prod R2 key created `stored-pg-pitr` itself. A token scoped to that bucket is still better (RP57-10).
- **The Keychain item `vaultaire-pgbackrest-cipher` exists** (64 characters). It is the only copy off the box: put it in the F-14 2-of-3 escrow with the master key (RP57-15).
- Carried, unchanged: the translog signing-key ceremony; `TRANSLOG_ANCHORS` / `TRANSLOG_WITNESSES`; trust DNS + the origin-cert SAN; the Rekor anchor key (D-P35-2); F-33; RB-3 (`BLOMP_ALLOW_UNSEALED` before customers); F-36 (the LE policy's sentences); F-37 (the DMCA agent); F-28's refund words (RP55-4: the Terms say 30 days, F-28 recommends 14); the global flips Prompt 39's tenant-zero table allows; RP57-4 (the next tenant-zero proof needs the admin's password **and** MFA code).
- **Sub-processors** (RP57-11): Sync.com and Blomp hold encrypted dumps and are listed nowhere — counsel's call.

## Still open on purpose

K4c-3c; K5's attested realm (41.4) and the second realm (F-19); RP52-4/5, RP53-1/2/5/11/12; the yearly transparency report; SBOM + release signing (→ Prompt 41); the pen test (owner). The Postgres replica on the F-18 box (PITR is not a standby).

## Residuals (Session B: RP57-10 onward; Session A: RP57-1…9; the review after both: RP58)

| # | Sev | What | Owner / where |
|---|---|---|---|
| RP57-10 | P3 | The R2 key that writes the PITR repository is the app's own `R2_*` key, and it is account-admin (it created the bucket): anyone holding `.env` can delete the archive. An R2 token with Object Read & Write on `stored-pg-pitr` only, put in `.env` under its own names (the install script reads `R2_*` today), then re-run `pg-pitr-install.sh` | owner (Cloudflare dashboard) + Stage 1a |
| RP57-11 | note | Sync.com (since 2026-10-07) and Blomp (since 2026-10-21) hold encrypted dumps of the database and are not in the privacy policy's, DPA's or GDPR page's sub-processor lists. This PR lists Cloudflare's new purpose only (R2 was already listed) | owner + counsel |
| RP57-12 | note | The cluster is shared: `vaultaire`, `vaultaire_prod`, the Sia `indexd_zen`, the lab databases, `pitr_canary`. PITR archives all of them and a restore rewinds all of them (the runbook stops their writers). A cluster of its own for `vaultaire` would let a restore touch only it | Stage 1a / F-18 box |
| RP57-13 | P4 | The drill is run by hand; nothing records when it last passed. A monthly timer + `vaultaire_pg_pitr_drill_last_success_timestamp_seconds` and a rule | Stage 1a |
| RP57-14 | note | While archiving fails, `pg_wal` grows without bound — `archive-push-queue-max` is unset on purpose (it drops WAL, breaking PITR silently). `PGArchiveFailing` pages within 5 min; 3.4 TB free; `DiskSpaceLow` at 10 % | — |
| RP57-15 | P3 | The repository passphrase exists on the box (`pgbackrest.conf`) and in ONE Keychain. A lost box plus a lost Mac = an unreadable archive (the dumps on Sync/Blomp remain). Add it to the F-14 2-of-3 `age` escrow | owner |
| RP57-16 | P4 | RP57-5 confirmed and wider on prod: on `stored.cloud`, a signed GET of `/robots.txt` or `/favicon.ico` is answered by Cloudflare's cache (`HIT`, 200) — `stored.ge` and `/sitemap.xml` reach the S3 API. Only a bucket named `robots.txt` / `favicon.ico` addressed through `stored.cloud` (not a documented endpoint) is affected. The stored.cloud zone also rewrites `robots.txt`'s `max-age=3600` to 14400 (Browser Cache TTL) | WP-DOMAIN-1 / CF rules |
| RP57-17 | note | The hand backup run of 23:06 UTC (installing #954): the rules tar on Blomp read `remote 0` bytes right after its upload (`OFFBOX2 FAILED … size mismatch`, non-fatal by design; the dump itself verified). Blomp's listing lag (RB-1's shape); `BackupSecondOffboxStale` watches the dump only | — |
| RP57-18 | note | The package left `/etc/pgbackrest.conf` (the old default path, a stock `[demo]` stanza) beside ours; pgBackRest reads `/etc/pgbackrest/pgbackrest.conf` first (proven: `stanza-create` reached R2). Removing it avoids a reader's confusion | — |
| RP57-19 | note | The privacy policy, terms, GDPR page, DPA and BAA changed (the backup sentences) and say "Last updated: October 24, 2026"; the terms and DPA promise registered users 30 days' notice of material changes. Pre-launch (no paying customers); the owner decides whether this counts | owner |
| RP57-20 | note | The on-box dump (`/opt/vaultaire/backups/`) is still plaintext gzip (TRUST item 12: `age` before it exists on disk); the PITR archive is encrypted before it leaves the box but WAL in `pg_wal` and the spool is plaintext on the box like the database itself | Trust lane |
