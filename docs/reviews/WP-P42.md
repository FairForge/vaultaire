# Stage 1a — Prompt 42: plan 43.7, the two pre-launch holes (S3 secrets wrapped; the dump sealed before disk)

*2026-10-25. Two sessions. **Session A** built #969 + #970 (migration **108**, the wrap, the drop flag); its note is `docs/reviews/WP-P42-PARTA.md` (residuals RP61-1…20), folded in here by reference, not copied. **Session B** built PR A (#971, the age-sealed dump) and owns this docs PR. Session B went idle mid-mutation-run, and the reviewing session (RP62) finished PR A from its branch: folded in the pre-PR review, re-ran the tests and mutations, opened and installed it. Residuals here are **RP61-21…**; the review is `docs/reviews/WP-P42-REVIEW.md` (RP62-1…13). **Next migration: 109.***

Lane: Trust (contract rules 4 — keys only through the one hierarchy — and 5 — no global key; `docs/surfaces/TRUST.md` item 12).

## Step 0 (the RP60 checks owed by Prompt 41)

| Check | Result |
|---|---|
| (a) `pgbackrest --stanza=vaultaire info`: the first TIMER differential (Mon 2026-10-26 01:30 UTC) | **Not run yet** (checked 2026-10-25 22:45 UTC). Fulls: `20261024-223104F` (the install's) and `20261025-013001F` (the Sunday timer). Owed again → RP61-24 |
| (b) WP-P41's claimed tests exist by name; the same for WP-P42-PARTA's | Every `Test…` name in PARTA exists (`grep -rqE "func <name>\("`), 21 of 21, except: the two `TestP42RED_*`, which the note says were RED-run files and not kept; and `TestSSES3_GetAfterRotate`, the prefix of `TestSSES3_GetAfterRotate_UsesRecordedVersion` |
| (c) The rule hashes on the box = `deploy/monitoring/*.yml` | All 17 rule files in `/etc/prometheus/rules/` match main `bfea480b` (`sha256sum` both sides) |
| `docs/reviews/WP-P41-REVIEW.md` on main | Absent: no RP60 code findings to take first |
| Tenant zero's flag rows, bucket `tz-proof-1792873702` | Untouched |

## PR A — #971: the dump sealed with `age` before it reaches disk

| Piece | What | Why |
|---|---|---|
| The pipeline | `pg_dump \| count_ddl \| gzip \| count_bytes \| age -R configs/backup-age-recipients.txt` → `vaultaire_<ts>.sql.gz.age`, written under `.dump-*/` and renamed after both asserts | The asserts are counted on the stream (`tee /dev/fd/3` into counters), so the dump never has to exist in plaintext to be checked. The size counts the gzip stream, before age: an empty gzip seals to 1,679 B under one age1pq recipient, so a size check on the sealed file would pass it (RP62-2) |
| No recipients file | the plaintext `.sql.gz` of before, `WARNING - stage=encrypt … PLAINTEXT`, `vaultaire_backup_dump_encrypted 0` → `BackupDumpPlaintext` (warning, 26 h, absent too) | Dark-safe: prod has no recipients until the owner's ceremony (Prompt 43 B1) |
| An unusable file | `BACKUP FAILED - stage=encrypt` before pg_dump runs: a non-`age1` line, an `AGE-SECRET-KEY-` (refused, never logged), an `AGE-PLUGIN-` (an identities file), no recipient, a dangling symlink (RP62-4), no age, a file age refuses (mixed post-quantum + classic; `age1pq1…` under age < 1.3) | Never a silent plaintext: a broken setup costs a night (BackupStale pages), not the property |
| `deploy/scripts/age-install.sh` | the pinned upstream age v1.3.2 (`cbe24006…ac10`, the linux-amd64 tarball hashed from two downloads) into `/usr/local/bin`; a mismatch installs nothing | RP62-1: the box's apt candidate is 1.2.1, which has no `age1pq` recipients, and age ≥ 1.3 refuses to mix kinds; `age-plugin-yubikey` is classic and unpackaged on Ubuntu. Hence **decision F-36 (the owner's): recommended, two `age1pq` identities**, paper in the safe + one in the F-14 escrow |
| One run at a time | `flock -n` on `backups/.lock`, else `stage=lock` | RP62-5: a second run's sweep of stale `.dump-*` deleted the first run's work directory |
| Retention | on the box by mtime and off it by the name's date; both shapes | The off-box name sweeps already matched `.sql.gz.age`; `produce_test.go`'s anchor moved with the `find` line, facts unchanged (7 / 30 days) |
| The log | every `Backup completed` names the shape, the recipient count and the age version | What sealed a dump is in its own line |
| CI | `ci.yml` installs the pin; `pg-backup_test.sh` 5g (real age) fails, never skips, under `CI=true` | RP62-3: 5g was SKIPPED in CI before |

**Tests** (`pg-backup_test.sh`, 220 checks with GNU coreutils and the real age 1.3.2):
- 5a NoPlaintextDumpOnDisk
- 5b RecipientsAbsentIsLoudAndCounted
- 5c BrokenRecipientsFailsTheRun (9 shapes)
- 5d RetentionRetiresBothShapes
- 5e a failed pg_dump / no DDL
- 5h TooSmallDumpFailsWhenSealed
- 5i OneRunAtATime
- 5j the install pin refuses a wrong tarball
- 5g real age: classic and age1pq round trips, mixed refused

`TestProduceBackupsAreTheBackupScripts` and the promtool tests for `BackupDumpPlaintext` (`deploy/monitoring/tests/vaultaire-backup_test.yml`) are in the PR too. promtool is not on the Mac, so the rule tests were run on the box at install (below).

**Mutations: 10/10 caught.** M2 was the one that survived before the fold-in.

| # | Mutation | Checks failed |
|---|---|---|
| M1 | a broken file falls back to plaintext | 64 |
| M2 | size counted on the sealed file | 8 |
| M3 | retention drops `.age` | 6 |
| M5 | rename before the asserts | 14 |
| M6 | no lock | 8 |
| M7 | dangling symlink read as absent | 10 |
| M8 | no age dry run | 10 |
| M9 | identity lines accepted | 4 |
| M10 | gauge always 1 | 5 |
| M11 | DDL check dropped | 8 |

**Lab** (Session B, before the fold-in): a throwaway identity and a run of the script with the real age 1.3.2 against a migrated test DB. The file was decrypted and restored into a scratch DB, with every table's row count equal (`docs/DEPLOY.md` § Backups).

## Prod

Merged: **#971 `8386cca1`** (23:07 UTC, deployed `8386cca16c9b`), **#973 `a122539e`** (the cron PATH fix, RP61-26), and **#972 `c82a1c42`** (Prompt 43 Part A, `launch-readiness.sh`; its note is WP-P43's).

Merging #972 triggered a Deploy run that failed in **SBOMs** after all three SBOMs were written (`rm: cannot remove …/src/.git: Directory not empty`, a detached auto-gc in `sbom.sh`'s throwaway repository). That was 1 of the last 15 runs; fixed by **#974**. `a122539e` got no Deploy run at all, the dropped-trigger shape. No binary is behind: #972 and #973 change only hand-installed scripts and CI.

**Box install, 2026-10-25 23:41 UTC**, root, from main `a122539e`'s files:

| What | sha256 / result |
|---|---|
| `age-install.sh /usr/local/bin` | `age v1.3.2` installed (tarball pin `cbe24006…ac10` checked) |
| `promtool check rules` / `test rules` (on the box, before install) | 7 rules; unit tests SUCCESS (`BackupDumpPlaintext` included) |
| `/opt/vaultaire/bin/pg-backup.sh` | `38544f7f…` → **`f479a813…`** = repo; the old one kept as `pg-backup.sh.bak-p42-20261025T2341Z` |
| `/etc/prometheus/rules/vaultaire-backup.yml` | `5b80cd61…` → **`0a9ed933…`** = repo; prometheus reloaded |
| `/opt/vaultaire/bin/launch-readiness.sh` | **`0895e94e…`** = repo |
| Symlinks | `vaultaire_backup_dump_encrypted.prom`, `vaultaire_launch_gate.prom` → node_exporter (`node_textfile_scrape_error 0`; 15 `vaultaire_launch_gate` series scraped) |

**The hand run**, as user1 under cron's own environment (`env -i … PATH=/usr/bin:/bin`), 3 h 18 min before 03:00 UTC: exit 0.
```
WARNING - stage=encrypt: /opt/vaultaire/configs/backup-age-recipients.txt not present: the dump is written in PLAINTEXT (plan 43.7; alert BackupDumpPlaintext)
Backup completed - vaultaire_20261025_234139.sql.gz (16393996 bytes, 110 tables, PLAINTEXT)
Off-box copy completed - … → Sync (… 16393996 bytes verified)
Off-box copy 2 completed - … → Blomp (… encrypted, 16393996 bytes verified)
Run completed - configs + rules archived, rules → Sync, retention applied
```
`vaultaire_backup_dump_encrypted 0`, so `BackupDumpPlaintext` is pending and fires after 26 h unless B1 lands first. **That is expected: the dump stays plaintext until the owner's recipients ceremony** (RP61-21). CI ran the sealed paths for real: 5g (classic and age1pq round trips) and 5k (cron's PATH finds `/usr/local/bin/age`) on the runner with the pin installed.

`launch-readiness.sh` baseline at 22:43 UTC: **2/15 GREEN** (sia_sealed, tenant_zero_objects), 9 launch gates not GREEN; the table is in #972 and WP-P43.

## Residuals (Session B; Session A's are RP61-1…20 in PARTA; the review's RP62-1…13)

| # | Sev | What | Owner / when |
|---|---|---|---|
| RP61-21 | P2 (the property) | The dump is plaintext on the box until the recipients exist: `BackupDumpPlaintext` fires from the first run after install. | Owner, Prompt 43 B1: decision F-36, `backup-age-recipients.txt` (public lines only, root 0644), a hand run, the restore drill on the Mac |
| RP61-22 | P3 | The produce page and the legal pages must not claim sealed dumps before B1, and then only in RP62-7's words: the dumps sealed to offline keys, the PITR archive under a key on the box. | The Prompt 43 docs PR |
| RP61-23 | P3 | The PITR archive's passphrase is on the box, so a box compromise reads ≤ 28 days of base backups + WAL, with plaintext S3 secrets until the drop + rotation. | Prompt 43 B3 kills the secrets; the broker (43.7 F-20) is the structural fix |
| RP61-24 | P4 | The first timer differential (Mon 01:30 UTC) is unchecked. | Prompt 43 Step 0 |
| RP61-25 | P4 | `pg-backup_test.sh` needs GNU stat/date, so on a Mac it runs only with coreutils on PATH (`PATH=/opt/homebrew/opt/coreutils/libexec/gnubin:$PATH`). | — (CI runs it on Linux) |
| RP61-26 | P2 → **fixed, #973** | Found at install: cron runs the script with `PATH=/usr/bin:/bin`, and the pin lives in `/usr/local/bin`. With recipients present, the 03:00 run would have failed at `stage=encrypt` ("age binary not installed"), with no dump that night. `/usr/local/bin` is now appended to PATH (never prepended). Test 5k CronPathFindsTheInstalledAge runs with cron's PATH where the pin is installed (CI). #971 was already in the merge queue, so this is its own PR | Done; must be on the box before B1 |
