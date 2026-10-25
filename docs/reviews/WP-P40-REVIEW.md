# WP-P40 post-merge review (RP58)

*2026-10-25, ~00:30–01:00 UTC. Reviewed: #950 `bc74487b` (owed from RP56), #953
`9864016c`, #954 `9bbfb600`, #955 `cac9a31c` (PITR), #956 `5e5fc23c`, #957 `dec6941d`,
and the then-open #959 (RP57-5's `CDN-Cache-Control: no-store`). Worker notes read:
[`WP-P40-PART0.md`](WP-P40-PART0.md) (Session A) and `WP-P40.md` (Session B, from the
#958 branch). Prompt 41 handed out (worker residuals = RP59, its review = RP60).*

**Verdict: holds — no P0/P1/P2.**

## What was checked

- **Every claimed test exists by name** (the RP50-1 lesson): all 21 Go tests of both
  notes grep to real `func Test…(` declarations; the three shell `_test.sh` files run in
  CI (`.github/workflows/ci.yml` lines 117–120). CI and Deploy green on every merge
  commit `9864016c` → `dec6941d`.
- **The gate (#956), read adversarially.** `s3CredentialGate` is one `router.Use` after
  request-id / canonical / limits / version / logging, before site statistics and every
  route; `signedS3Request` is the one predicate (SigV4 or SigV2 `Authorization` scheme,
  case-insensitive, or a presigned query — `isPresignedRequest`, `X-Amz-Signature`,
  `X-Amz-Credential`, SigV2's `AWSAccessKeyId`+`Signature`). The exceptions are narrow
  and each is pinned by a test: OPTIONS (preflight), `GET /api/v1/whoami` (verifies SigV4
  itself), a public trust host (contract rule 11). `s3Handler()` is the catch-all's own
  `handleS3Request` with the `s3Fallback` test hook intact. The CDN and trust routers sit
  outside `s.router` and are accounted for. The routes that read `Authorization`
  themselves (requireJWT, recoveryAuth, whoami, the predicates) were enumerated by the
  worker and re-checked — a Bearer/Basic request keeps its route.
- **The login P2 fix (#956).** `handleLogin` fails closed on an unreadable MFA state,
  consumes a TOTP code once (`ConsumeTOTPCode`, the R12-38 guard) or a backup code, and
  writes an `auth.*` row for every outcome through the dashboard's `RecordLoginEvent`.
  The 401 texts don't leak which factor failed beyond `code_required`.
- **The PITR scripts (#955), read line by line.** The install script: passphrase from a
  0600 file read-then-deleted, never argv; a differing cipher refused (a repository's
  passphrase never changes); conf 0640 postgres, atomic install, idempotent re-run;
  `ALTER SYSTEM` only on change with `postgresql.auto.conf` backed up. The drill: the
  containment checks (`refuse_unsafe`) run **before** the cleanup trap is set; the
  restore and the scratch start both force `archive_mode=off` (nothing a drill does can
  push a timeline into the live stanza — the RP42-1 question asked and answered); the
  after-T canary commit guarantees recovery has a stop target, and `pgbackrest check`'s
  forced WAL switch (archiving is strictly ordered) guarantees the segment holding it is
  in the repository before the restore starts. The metrics script writes **every series
  on every run, at 0 when unknown**, with a `*_success` gauge per source (the counter
  lesson applied); the five rules fail closed on absence and on zeros.
- **Prod read (00:33 UTC).** `/version` `dec6941d5b3b`; archiver 136 archived / 0 failed,
  last segment 00:32:57; `info_success 1`, 1 full, 83.2 MB repo; the three timers armed
  (first weekly full: Sun 01:30 UTC, 56 min away); spool 20 K, `pg_wal` 225 M; active
  slot journal since the #957 deploy: 1,086 lines, **0** `"level":"error"`. **All 17 rule
  files on the box match the repo sha256-for-sha256** (`vaultaire-pitr.yml` `f69bf673…`
  among them).
- **#950** (owed from RP56): the proof script never mints a token (post-#956), refuses a
  group/other-readable JWT file, writes tenant-scoped flag rows only (`flag_body` refuses
  `'*'`), reads every reported number back from the database or `/metrics`, and deletes
  its temporary key on exit. Holds.
- **#953**: the singleflight re-reads the cache inside the flight function — the correct
  shape; the seam-based red test and the `-count=300` stress are in the diff. **#954**:
  `write_metric` with no value writes `date +%s` — the timestamp lands only after the leg
  was listed and every old file tried; the rule reads absence as not-running. **#959**:
  one header + one assertion per row; correct, armed.

## Findings

| # | Sev | What |
|---|---|---|
| RP58-1 | P4 | `signedS3Request` does not match the SigV4A header scheme (`AWS4-ECDSA-P256-SHA256`): such a request is routed to the web routes exactly as before RP56-1 (a presigned SigV4A URL IS caught — `q.Has("X-Amz-Signature")`). No SDK sends SigV4A to a vanity endpoint today (it is the multi-region access points' scheme), and our verifier would refuse it anyway, but the gate's claim is "a request carrying S3 credentials", and this is one. One line in the predicate + a matrix row → Prompt 41 Part 0 |
| RP58-2 | note | `PGArchiveStale` fires after ~12 min with no WAL written at all (archive_timeout switches only when something was written since the last switch). Today the app writes `job_runs` every 30 s and `indexd_zen` writes constantly, so this needs both slots stopped AND every other writer quiet — and during such a maintenance window the page is arguably wanted. Left as designed |
| RP58-3 | note | Both backup timers are `Persistent=true`: after an outage spanning 01:30 both can fire at boot; the loser of pgBackRest's stanza lock fails its oneshot unit silently. Covered only by `PGPITRBackupStale` (26 h). Acceptable; a `vaultaire_pg_pitr_drill_last_success` + unit-failure signal arrives with RP57-13's drill timer |
| RP58-4 | note | The install script's `$CONF.bak-<ts>` copies keep superseded S3 keys and the cipher at 0640 postgres — same trust domain as the live conf, but a key rotation (RP57-10's scoped token) should delete the old backups it leaves |

## Owed to the next review (RP60)

1. The **05:30 UTC 2026-10-25 `routing_truth`** run: the chunked sample over tenant
   zero's overnight bucket (`tz-proof-1792873702`) — read `job_runs.result` and the
   `vaultaire_routing_truth_*` gauges.
2. Prompt 39's **morning proof half** + the WP-P39 verdict table ([`WP-P39.md`](WP-P39.md)):
   blocked on the owner's MFA sign-in (RP57-4).
3. The first **weekly full** (Sun 01:30 UTC) and the first timer-driven **diff**
   (Mon 01:30): `pgbackrest info` should show full+diff and expire holding 4 fulls.

## Decisions

- **#958 (the docs PR) is unblocked and armed by this review**: the hold was Prompt 39's
  missing `WP-P39.md`, which this PR adds (written from the merged records; the P39
  Session B worktree held no work past #950). The tenant-zero verdict table stays owed
  there — the closeout says so and allows no flip.
- `/etc/pgbackrest.conf` (RP57-18, the 2023 dpkg conffile) is left in place: removing a
  dpkg conffile invites an apt prompt on the next package upgrade; pgBackRest provably
  reads `/etc/pgbackrest/pgbackrest.conf` first. Documented here instead.

## Owner items (carried, unchanged)

RP57-4 (the morning proof's MFA JWT); RP57-10 (a scoped R2 token for `stored-pg-pitr`,
then re-run the install script and delete the conf backups — RP58-4); RP57-15 (the
pgBackRest passphrase into the F-14 2-of-3 escrow); RP57-11 (Sync.com/Blomp as
sub-processors — counsel); RP57-19 (the legal pages' "Last updated" vs the 30-day notice
promise); the translog signing-key ceremony chain; F-36, F-37, F-28/RP55-4; RB-3.

## Second pass (2026-10-25 ~03:30 UTC, same review session — RP58-5…9)

The owner asked for a second look before Prompt 41 ("make sure we got everything").
Method: the gate predicate and its composition re-read by hand (SigV2 presigned IS
caught; `EqualFold` covers case; the CDN host router bypasses the gate but serves only
`/{slug}/{bucket}/*` public bytes — a scoping note, not a finding), plus two independent,
unanchored adversarial reviews — one on #956 (gate + login), one on #954/#955 (PITR) —
briefed with the diffs only, not the first pass's findings.

**Outcome: the PITR half still holds at P2+; the Part 0 half's known lockout gap
(RP57-2) is upgraded P3 → P2.** No new P2 was found that was not already filed.

| # | Sev | Finding (second pass) |
|---|-----|----------------------|
| RP57-2↑ | **P2** (was P3) | The JSON `/auth/login` second factor has NO per-account lockout and NO metric — only audit rows nothing alerts on. ~100 IPs at the 5/min per-IP limit reach ~50 % TOTP-guess success in ~8 h against a leaked password (skew ±1 ≈ 3 valid codes/instant), invisible to the auth rules; the same guesses on the dashboard lock the account at 10. Undercuts the P40 MFA fix's own threat model. Prompt 41 item 1 (now with the metric requirement) |
| RP58-5 | P3 | `pg-pitr-install.sh`'s ALTER SYSTEM idempotence guard compares `current_setting('archive_timeout')` to `60s`; Postgres normalizes to `1min`, so the guard NEVER matches on a real cluster — every re-run re-ALTERs and leaves a new `.bak-pitr-*`. Masked by the test fake echoing `60s` verbatim. Prompt 41 item 6 |
| RP58-6 | P3 | The drill's scratch resource limits (shared_buffers 256MB…) are silently overridden by the RESTORED prod `postgresql.auto.conf` (the 2026-10-21 tuning: 2GB etc.) — auto.conf is read after postgresql.conf. Safety settings stay protected (they ride `pg_ctl -o`, which outranks all files); the resource bound the script intends does not exist. Prompt 41 item 6 |
| RP58-7 | P3 | The "box lost too" DR path: on a fresh box `stanza-create` refuses the new cluster's system identifier and `set -e` kills the installer mid-script, during a disaster (steps 3–7 still work). Runbook line or a `--restore-box` flag. Prompt 41 item 6 |
| RP58-8 | P4 | Login/gate nits: (a) on MFA success + `GenerateJWT` failure the TOTP is burnt and `auth.mfa_succeeded` never written; (b) a DB error from `ValidatePassword` is audited `bad_password` + 401; (c) the verifier's `HasPrefix` accepts `AWS4-HMAC-SHA256,Credential=…` (no space) which the gate's space-`Cut` misses — predicates must agree (no SDK emits it, signature can never verify). Prompt 41 items 1–3 |
| RP58-9 | P4 | PITR nits: (a) the drill traps `EXIT` only — an untrapped SIGINT leaves the scratch postmaster on 5499 (live repo safe); (b) `jq` is a metrics dependency the installer never ensures; (c) timers are enabled before stanza-create (one failed unit if the install straddles 01:30, self-heals); (d) the cipher file is deleted before its format validation (cosmetic). Prompt 41 items 4/6 |

**What the second pass verified holds** (beyond the first): the gate predicate is a
strict superset of every credential shape our verifier accepts (header and presigned,
SigV2 included) except RP58-1/RP58-8c; the three gate exceptions are safe in both
directions; TOTP replay and backup-code single-use are correct across both surfaces (one
shared `AuthService`); the realm-outcome split (RP56-6) is right; the drill cannot reach
the live repo by any path found (refuse-before-trap, archive-off twice, `-o` outranking
every file); the async spool acks a segment only once it is in R2 (the stated RPO is
honest); every PITR series fails closed (`or absent(…)` everywhere probed); no secret in
argv (pinned by test), journal secrets redacted.

**Verified live on this pass:** the first SCHEDULED weekly full ran — `20261025-013001F`
(Sun 01:30:01–:07 UTC, 480.7 MB database → 80.3 MB encrypted in R2, expire clean,
`status=0/SUCCESS`); `pgbackrest info` shows both fulls, stanza ok, cipher aes-256-cbc.
RP60's owed backup item narrows to the first timer DIFF (Mon 2026-10-26 01:30).
