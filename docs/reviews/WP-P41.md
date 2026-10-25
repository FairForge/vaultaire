# Stage 1a — Prompt 41: Part 0 (the WP-P40 review's RP57/RP58) and Part A (WP-SUPPLY-1: SBOM + release signing)

*2026-10-25. One session, five product PRs then this docs PR. **No migration** (the next is still **108**; `ls internal/database/migrations | tail -1` = `107_translog_anchor_resubmit.sql` at Step 0). Residuals are **RP59-n**; the review of this prompt is RP60.*

Lane: Trust (contract rules 7 and 8 — the log and what a third party can check: the SBOM
and the signature; the sign-in surfaces' one lockout) and Engine (the metadata database's
drill on a timer).

## Step 0

- #958, #959, #960 merged before the first branch; **#961** (the review's second pass,
  RP57-2 → P2, RP58-5…9) was CLEAN and unarmed at Step 0 — armed by this session, merged
  `27441bf0` (03:39 UTC queue run) before the first product PR entered the queue.
- CI on main green (`87eeec45`, then `27441bf0`).
- Tenant zero's flag rows and `tz-proof-1792873702` untouched (RP57-4 is the owner's).
- The worktree `vaultaire-p41` (branch `stage1a-p41-login-lockout`) and the DB
  `vaultaire_test_p41` existed from the prompt's drafting session, with the RED test file
  for items 1–2 uncommitted; every other PR got its own worktree from `origin/main`.

## The PRs

| PR | Branch | Items | Merge |
|---|---|---|---|
| #962 | `stage1a-p41-login-lockout` | 1 + 2 (RP57-2 P2, RP57-3, RP58-8 a/b) + `ci.yml` `-timeout 20m` | `d595f42a` 06:05 UTC (after a CI `-timeout 20m` commit and an empty retrigger: see Prod) |
| #963 | `stage1a-p41-sigv4a` | 3 (RP58-1, RP58-8 c) | `bcedc263` 05:32 UTC |
| #964 | `stage1a-p41-reserved-names` | 5 (RP57-1) | `bcb2183e` 06:42 UTC (rebased after #962; the P40 round-trip test seeded, `025a5553`) |
| #965 | `stage1a-p41-pitr` | 4 + 6 (RP57-13, RP58-5/6/7, RP58-9 a/b/c) | `b3318b8e` 05:16 UTC |
| #966 | `stage1a-p41-supply` | Part A (WP-SUPPLY-1 A.1–A.4) | `4e577af6` 05:33 UTC |

## Part 0

### Item 1 — RP57-2 (P2): `/auth/login` shares the dashboard's per-account lockout, and every failure has a series

| Finding | Sev | Fix | RED on main (87eeec45) | Mutation → caught by | Lab | Prod |
|---|---|---|---|---|---|---|
| `POST /auth/login` had the 5/min per-IP limiter and no per-ACCOUNT lockout; its second factor had no metric — ~100 IPs at the limit reach ~50 % TOTP success in ~8 h against a leaked password, invisible to the auth rules | **P2** | ONE `middleware.AccountLockout` for both surfaces: `Server.loginLockout` → `dashboard.Deps.Lockout` (nil = the router makes its own); `handleLogin` checks the lock first (a locked account answers exactly as a wrong password, `auth.login_failed` reason `locked`, never evaluated), counts every failure in `vaultaire_dashboard_login_failures_total{surface="api",reason}` (new `surface` label; every surface × reason pair pre-created at 0 in `dashboard.metrics.go`'s `init`), `Fail`s the account on a password or second-factor failure (not on `locked`, not on `code_required` — the dashboard's empty-code rule), writes `auth.login_locked` with the factor's via, and resets the count only when a token is minted | `TestP41RED_Item1_ElevenBadPasswordsFromElevenIPs`: `expected: 10 actual: 11` bad_password rows, no `auth.login_locked`, the right password then `200`; `TestP41RED_Item1_TenBadCodesAfterARightPassword`: the right code `200` after ten bad ones | 1 `Fail` never called → `TenFailuresLock…`, `ARightPasswordAndTenBadCodes…`, `TheLockoutIsTheDashboards`; 2 lock never checked → same; 3 count reset at the password step → `ARightPasswordAndTenBadCodes…`; 7 locked refusals not counted → `TenFailuresLock…`; 8 no pre-creation → `EverySeriesExistsFromBoot…`; 9 `Deps.Lockout` ignored → `TheLockoutIsTheDashboards`, `DashboardLogin_AFreshPasswordStep…`; 10 the dashboard resets at the password step → `DashboardLogin_AFreshPasswordStep…`; 11 dashboard failures counted on the api surface → `TheLockoutIsTheDashboards` (survived first: the surface assertion was added) | `go test ./internal/api -run 'TestAuthLogin_\|TestLoginFailures_\|TestDashboardLogin_'` + the dashboard packages, on `vaultaire_test_p41` | `d595f42a` deployed 06:10 UTC (slot :8001), 0 app errors; the active slot exports **16** `vaultaire_dashboard_login_failures_total{surface,reason}` series at 0 (8 reasons × 2 surfaces) |
| **The dashboard had the same hole**: `handleLogin` RESET the count at a valid password, before the 2FA challenge — a known password plus a fresh `/login` before every code guess never locked. The prompt asked to say so rather than invent a stricter rule; the fix is the one rule for both: a sign-in resets the count only when it completes (the 2FA handler's reset, or the no-MFA session) | P2 (same class) | `router.go`: no `Reset` after the password step; `Reset` at the no-MFA session create | `TestDashboardLogin_AFreshPasswordStepBeforeEveryBadCodeStillLocks` (10 × (`POST /login` 303 + bad code) — on main the 10th code guess is still 401, no lock) | 10 above | same | same |
| The metric: `surface` label or a twin series? **`surface` label on the existing series** — the two existing rules (`DashboardLoginFailuresElevated`, `DashboardAccountLockouts`) sum over all labels and now cover both doors with no new rule; their comments and descriptions say so; names kept for the alert history. promtool test `deploy/monitoring/tests/vaultaire-auth_test.yml` (new): 1 bad_code/s on the api surface alone fires the elevated rule after its 10 min; 3 lockouts in 15 min fire the lockouts rule; the pre-created zeros fire nothing | — | `vaultaire-auth.yml` | — | — | — | installed 05:17 UTC, sha256 `d206c2db93c59642…`, `promtool test` SUCCESS on the box, Prometheus reloaded, both rules listed by `/api/v1/rules` |

### Item 2 — RP57-3 (P4) + RP58-8 a/b: a store failure is ours

| Finding | Sev | Fix | RED on main | Mutation → caught by | Lab | Prod |
|---|---|---|---|---|---|---|
| The password validated and the user row could not be loaded → `404 User not found`, no audit row; a DB error out of `ValidatePassword` → `401` audited `bad_password` (or `unknown_user`) | P4 | `ours(reason, …)`: 500 with the generic text, one Error line, `auth.login_failed` with reason `store_error` (the users read, the password check, the MFA state) or `user_load_failed` (the row after a valid password; seam `loginUserLoader`), never a `Fail` against the account | `TestP41RED_Item2_AStoreFailureIs500`: `expected: 500 actual: 401 "Invalid credentials"`, audited `unknown_user` | 4 store error audited bad_password + 401 → `AStoreFailureIsOursNotTheCallers`; 5 the old 404 → `AUserRowThatCannotBeLoadedIs500AndAudited` | same | — |
| MFA passed, `GenerateJWT` failed → the TOTP code burnt and no `auth.mfa_succeeded` row | P4 | the row is written for the factor that passed, the mint is the Error line (seam `loginTokenMinter`) | — (new) | 6 → `ATokenMintFailureStillRecordsTheFactorThatPassed` | same | — |

### Item 3 — RP58-1 (P4) + RP58-8 c: SigV4A, and the two predicates agree

| Finding | Sev | Fix | RED on main | Mutation → caught by | Lab | Prod |
|---|---|---|---|---|---|---|
| `AWS4-ECDSA-P256-SHA256` (SigV4A) was not in the gate's predicate: a SigV4A-signed GET of `stored.ge/legal/privacy` got the page | P4 | `auth.IsAWSAuthorization` — the ONE scheme predicate (SigV4, SigV4A, SigV2 by the token before the first space, case-insensitive); `signedS3Request` calls it; the S3 API answers a SigV4A header `403 SignatureDoesNotMatch` in XML | `TestP41RED_Item3_SigV4AHeaderGetsThePage`: `answered 200 text/html` | 1 predicate without SigV4A → `TestIsAWSAuthorization`, `TestS3Gate_ASigV4ARequest…`, the matrix, `TestSignedS3Request`; 4 the old space-Cut gate → the same; 5 case-sensitive → three tests | `go test ./internal/auth ./internal/api -run 'TestS3Gate_\|TestSignedS3Request\|TestIsAWS\|TestValidateRequest_TheScheme'` | — |
| The verifier's bare `HasPrefix(h, "AWS4-HMAC-SHA256")` attempted `AWS4-HMAC-SHA256,Credential=…` and a tab after the scheme, which the gate's space-Cut missed | P4 | **Tightened the verifier, not widened the gate:** `ValidateRequest` and `HeaderAccessKey` need `scheme + " "` (RFC 9110 § 11.4 — every SDK sends it; a comma-shape header could never verify, and a gate that matched prefixes would also match nonsense schemes). `auth.AttemptsAWSHeader` is the verifier's witness; `TestS3Gate_EveryHeaderTheVerifierAttemptsIsTheGates` generates 12 schemes × 6 separators × 4 payloads and demands attempts ⇒ gate and gate ⇔ predicate | the same RED test's second half (`AWS4-HMAC-SHA256,Credential=…` reached the page) | 2 verifier back to the bare prefix → `TestValidateRequest_TheSchemeNeedsItsSpace`; 3 the witness lies → the agreement test + the verifier test | same | — |

### Item 5 — RP57-1 (P4, the one-way door): a NEW bucket may not be named after a web path

| Finding | Sev | Fix | RED on main | Mutation → caught by | Lab | Prod |
|---|---|---|---|---|---|---|
| A bucket could be named `legal`, `docs`, `login`, `robots.txt`, `favicon.ico`, … — the reverse shadow left after the gate (RP55-3, RP57-5's residue) | P4 | `bucket_reserved.go`: the list is DERIVED from the router once (`routeFirstSegments`: every route ahead of the catch-all, any method, kept when a valid bucket name — 30 names today), never typed; `createBucketRegistryWith` answers `bucketCreateReservedName` after the owned-already short-circuit (an existing bucket keeps every operation and re-creates idempotently): S3 `400 InvalidBucketName` + "is reserved: it is a path of this service's web host", management `400 invalid_bucket_name` (the existing code family; the prompt's `invalid_name` was a paraphrase), dashboard `BucketCreateReservedName`. `docs/API.md § Reserved bucket names` states the deviation from AWS and the `cdn.*` scoping | `TestP41RED_Item5_ABucketNamedAfterAWebPathIsCreated`: six names created, `200` | 1 no check → `EveryWebPathIsRefused…`; 2 the check ahead of the short-circuit → `AnExistingBucketKeepsWorking`; 3 the walk skips wildcard routes → `EveryWebPathIsRefused…` (the must-list); 4 the generic message → same; 5 another management code → same; 6 dashboard state unmapped → same; 7 names not lower-cased / non-bucket segments kept → same | real SigV4 through `handleS3Request` on every derived name × three surfaces | prod holds 0 buckets of the 31 names (SQL read 04:44 UTC, before the deploy); #964 `bcb2183e` deployed ~06:50 UTC, 0 app errors in 3 h; **live proof with the bench tenant's key through stored.ge:** `aws s3api create-bucket --bucket docs` → `InvalidBucketName: The bucket name docs is reserved: it is a path of this service's web host (docs/API.md § Reserved bucket names). Choose another name.`; `--bucket robots.txt` the same; a control bucket `p41-proof-1792910830` created and deleted normally; 0 reserved-name buckets after the deploy |

### Items 4 and 6 — the drill on a timer; RP58-5/6/7; RP58-9 a/b/c

| Finding | Sev | Fix | RED on main | Mutation → caught by | Lab | Prod |
|---|---|---|---|---|---|---|
| RP57-13: the drill ran by hand, nothing recorded a pass | P4 | `vaultaire-pitr-drill.service` (root, `Nice=10`, `IOSchedulingClass=idle`) + `.timer` `*-*-02 02:10:00` UTC (Persistent); every run writes `/var/lib/vaultaire-pitr/vaultaire_pitr_drill.prom` (own file + own symlink; `_drill_success`, `_drill_last_success_timestamp_seconds` kept across a FAIL, `_drill_last_run_…`, `_drill_rto_seconds`); rules `PGPITRDrillStale` (> 35 d or absent, 1 h) and `PGPITRDrillFailed` (10 min) with promtool tests | the new `pg-pitr_test.sh` against main's scripts: 23 FAIL lines (`3a: the drill service runs…`, `… the drill's textfile has its own symlink`, `5a: the metrics file is written`, …) | see the shell mutations below | 159 checks ok on the box (Linux; `shellcheck` absent there is the one environmental FAIL — CI has it) | see Prod: timer armed for Mon 2026-11-02 02:10 UTC, drill PASS `rto_s=49.524`, metrics in node_exporter |
| RP58-5: the ALTER SYSTEM guard compared `current_setting('archive_timeout')` to `60s` — PostgreSQL prints `1min`; every re-run re-ALTERed and left a `.bak-pitr-*`; the fake echoed `60s` | P3 | the guard reads `pg_settings.setting` (`archive_command=…\|archive_mode=on\|archive_timeout=60`, `archive_timeout_seconds` from the fact); the fake answers the pretty form to the old query and the raw rows to the new | `3b: no ALTER SYSTEM when the settings are already set (the faithful fake: current_setting says 1min)` FAIL on main | — | same | run 2 on the box: 0 `ALTER SYSTEM`, `.bak-pitr-*` 1 → 1 |
| RP58-6: the restored `postgresql.auto.conf` (prod's tuning, read after `postgresql.conf`) made the scratch cluster prod-sized | P3 | the scratch overrides are APPENDED to the restored auto.conf (last assignment wins; pgbackrest's `restore_command` / `recovery_target_*` kept), `max_wal_size 1GB` added; `-o` still outranks every file | `5a: the effective shared_buffers is the drill's (last assignment)` FAIL on main (the fake restore writes `2GB`) | — | same | the drill on the box (see Prod: timer armed for Mon 2026-11-02 02:10 UTC, drill PASS `rto_s=49.524`, metrics in node_exporter) |
| RP58-7: "the box lost too" — `stanza-create` refuses the fresh cluster's identifier and `set -e` kills the installer mid-script | P3 | **`--restore-box`** (chosen over a runbook line: a flag cannot be forgotten in a disaster): conf, helpers and units only; no `stanza-create`, no timers, no ALTER; the next step named; `docs/DEPLOY.md` "box lost too" rewritten | `3g: --restore-box failed: usage: … <cipher-file>` on main | — | same | — |
| RP58-9 a: `trap cleanup EXIT` only — an interrupted drill left the scratch postmaster on 5499 | P4 | `trap 'on_signal INT' INT; trap 'on_signal TERM' TERM` → `cleanup_work` (stop, delete, metrics) then `trap - SIG; kill -s SIG $$` | `5h` is new (a background drill killed with SIGTERM mid-wait: stopped, deleted, metrics FAIL, exit 143) | — | same | — |
| RP58-9 b: `jq` never ensured | P4 | installed like `pgbackrest`; `die` if still absent | `3a: jq is ensured` FAIL on main | — | — | — |
| RP58-9 c: timers enabled before `stanza-create` | P4 | enabled after it (the four, the drill's included) | `3a: the timers are enabled AFTER stanza-create` FAIL on main | — | — | — |

Shell mutations (hand-run, each restored): the drill's `write_metrics` removed → `5a: the metrics file is written` / `5b: the metrics say FAIL`; `write_metrics` not keeping the previous PASS → `5b: … keep the previous PASS's time and RTO`; the overrides written to `postgresql.conf` only (the old shape) → `5a: the effective shared_buffers is the drill's`; the guard back on `current_setting` → `3b`; the timers' `enable` moved back before `stanza-create` → `3a: the timers are enabled AFTER stanza-create`; `--restore-box` running `stanza-create` → `3g: no stanza-create`; `on_signal` calling `cleanup` (which `exit`s with the last status — the first shape, caught on the box: rc 1, not 143) → `5h: the drill died of SIGTERM (143)`.

**Notes carried, no code (the prompt's list):** the cipher file is deleted before its format validation (a typo'd passphrase = re-send from the Keychain — cosmetic, unchanged); `BackupOffboxRetentionNotRunning{leg="blomp"}` fires for ever on a future box without the Blomp leg (matches `BackupSecondOffboxStale`'s design; prod has the leg); signed requests on `cdn.*` hosts bypass the gate by design — the CDN router serves `/{slug}/{bucket}/*` public bytes only, nothing web-paged (written into `docs/API.md § Reserved bucket names` as the scoping of RP57-1).

## Part A — WP-SUPPLY-1

`docs/RELEASES.md` is the operator page; `internal/release/CLAUDE.md` and `cmd/tools/README.md` the directory docs.

| Piece | What | Decision / why | Tests |
|---|---|---|---|
| A.1 `scripts/sbom.sh` (`make sbom`) | CycloneDX 1.6 JSON: `vaultaire.cdx.json` (the server), `sdk-wasm.cdx.json`, `sdk-js.cdx.json` into `bin/sbom/` | **cyclonedx-gomod v1.12.0** (2026-08-22, > 2 weeks old at Step 0) pinned and built for the HOST into a temporary `GOBIN` — `go run` under `GOOS=linux` built the tool itself for linux (`exec format error`). The server document is **`bin` mode over the release binary**: the module list and go.sum hashes as linked (`debug/buildinfo`), the binary's own hashes in the document (`app` mode needs git for the main module's version and go-git failed on a worktree, `reference not found`; `bin` mode needs `GOOS`/`GOARCH` in its environment or it finds 0 components — a tool quirk, set to the binary's target). sdk-wasm is `app` mode for js/wasm (`debug/buildinfo` cannot read a wasm file) in a throwaway `git init` repository of `git archive HEAD`, the version then set to HEAD's commit. sdk-js: one file component. | the repro gate and the check below; `TestClientBuildManifest…` untouched |
| reproducible | `scripts/repro-check-sbom.sh` (`make repro-check-sbom`): twice, seconds apart, byte-identical; with a directory, the shipped ones too | no serial, no timestamp; the main component's version = HEAD's commit EVERYWHERE it appears (purl, bom-ref, the dependency graph's refs — a first version normalized only the component and the refs still carried the throwaway repo's version: the first repro run caught it); the tool's host-dependent `metadata.tools[].hashes` dropped. Locally: `repro-check-sbom: reproducible` (three runs); the Deploy job runs it with the shipped binary as `SBOM_SERVER_BINARY` and uploads the `sbom` artifact | mutation 13 (the timestamp back) → the gate fails; 15 (the tool hashes kept) → the gate fails even on one host (the temporary GOBIN rebuilds the tool: its hash moves) |
| go.sum's truth | `scripts/sbom-check.sh` (`make sbom-check`): every module component = a `<module> <version> h1:` line of `go.sum` whose h1 decodes to the SBOM's SHA-256; `std` = go.mod's toolchain; an SBOM with no module fails | 73 components pass at HEAD. `scripts/sbom_test.sh` (CI "Deploy scripts") on fixtures (`SBOM_GO_SUM` / `SBOM_GO_MOD`): a moved hash, a removed module, a dependency bump (go.sum's version moved), a toolchain change, an empty SBOM each fail — the prompt's "RED by a dependency bump" | mutation 14 (the hash compare dropped) → `sbom_test.sh` fails |
| A.2 `internal/release` + `cmd/tools/release-sign` | `keygen -out` (0600, must not exist; the verifier key on stdout), `sign <file>…` → `<file>.sig` (all or none), `verify -vkey … <file> <sig>` | **Format: a detached signed note** (`golang.org/x/mod/sumdb/note`, Ed25519, key `stored.cloud/release/v1`; text = name, sha256, size) — over minisign/signify because the note format is already the project's (translog keys, verifier, published checkpoint keys) and the key hash in the signature names the key. The name is reported, the BYTES are checked (a renamed download verifies). The key is read like `TRANSLOG_SIGNING_KEY` (credential `release_signing_key` → `RELEASE_SIGNING_KEY_FILE` → env): set but unusable = refusal, none = `ErrNoSigningKey` — dark until the owner's ceremony; never in CI (`docs/RELEASES.md` says why) | `release_test.go`: round trip; one bit, one appended byte, wrong key, truncated (4 cuts) and flipped signature, seven malformed statements under a VALID signature, key names, loader precedence/refusals. `main_test.go`: the verbs; dark; all-or-none; `-log`. Mutations 1 (hash ignored), 3, 4, 5, 7, 8, 11, 12 killed; **2 (size ignored) and 6 (lenient Open) are equivalent mutants**: a size that differs with an equal sha256 does not exist, and `note.Open` already fails with no verifying signature — both checks are defence in depth and stay |
| A.3 the wiring | `verify -log <url> -log-vkey … [-client sdk-wasm]` = `translog.FindBuild` (moved out of `translog-verify`'s `clientListed`, which now calls it; a `server_build` by `binary_sha256` or the named client's `client_build`, inclusion proven); `deploy.yml` "SBOMs" step (build + check + repro, keyless) + the `sbom` artifact; `ci.yml` runs `sbom_test.sh`; **sdk-e2ee-v0.2.0** got `sdk-wasm.cdx.json` + `sdk-js.cdx.json` regenerated from `4a8aa618` (`SBOM_TARGETS=sdk-wasm,sdk-js SBOM_MANIFEST=<HEAD's>`; the sdk-js hash = the manifest's `beea90d7…`) and attached 2026-10-25 ~05:35 UTC; signatures wait for the key | `TestReleaseSign_VerifyAgainstTheLog` (server + client listed, an unlisted build refused, wrong client, no `-log-vkey`, another log key), `TestFindBuild_FoundWithInclusionAndRefusedWithout` (a mirror serving the honest checkpoint over another history's tiles is refused). Mutation 9 (client name ignored) killed; **10 (VerifyInclusion skipped) is equivalent**: the entry bundle is read through the tile reader, which authenticates the tiles against the root, so the mirror fails before the proof — `VerifyInclusion` is the second check and stays |
| A.4 the public key on the trust host | not wired | a residual (RP59-2): the trust host's route list is closed by `TestTrustHost_RouteListIsClosed` and no key exists; `docs/RELEASES.md` names both publication places |


## The promtool tests, on the box

There is no `promtool` on the Mac; the two new test files were first run on the box
(`/usr/bin/promtool`) and both had one wrong expectation: the auth lockouts case lacked the
`instance`/`job` labels `increase()` keeps, and the drill-stale case sampled hourly, which
flips between the series and `absent()` and resets `for` (the file's own header says
samples must be ≤ 4 min apart). Both corrected (a per-minute series with a negative
timestamp: `time()` starts at 0 in a unit test) — **after** #962 and #965 were in the
merge queue, whose branches cannot be pushed; the corrected `tests/vaultaire-auth_test.yml`
and `tests/vaultaire-pitr_test.yml` ride in this docs PR. `promtool test rules` on the box:
`SUCCESS` for both, before the install below.

## Prod

Read-only before any deploy (04:44 UTC): `/version` `55490359`, 0 app errors in 60 min, the
rules `vaultaire-auth.yml` `3c1e8870…` / `vaultaire-pitr.yml` `f69bf673…` (= main), **0 buckets
named after a web path** (`SELECT count(*) FROM buckets WHERE name = ANY(<the 31 derived names>)`
— item 5's one-way door closes on an empty set), one `postgresql.auto.conf.bak-pitr-*` from the
first install.

**#965 merged `b3318b8e` 05:16:42 UTC → the box (05:17–05:19 UTC, the sudo password and the
cipher on stdin, nothing in argv):**

| Step | Result |
|---|---|
| `promtool test rules` on the box (both files, in one directory with their rules) | `SUCCESS` / `SUCCESS`; `promtool check rules`: 5 + 7 rules |
| `install -m 0644` + `systemctl reload prometheus` | `vaultaire-auth.yml` sha256 `d206c2db93c59642…`, `vaultaire-pitr.yml` `7f203da55840030b…`; `/api/v1/rules` lists `DashboardAccountLockouts`, `DashboardLoginFailuresElevated`, `PGPITRDrillFailed`, `PGPITRDrillStale` |
| `pg-pitr-install.sh`, run 1 | `stanza-create … completed successfully`; `vaultaire-pitr-drill.timer` enabled (next **Mon 2026-11-02 02:10 UTC**); the drill unit `User=root`, `Nice=10`, `IOSchedulingClass=idle`; the second symlink `vaultaire_pitr_drill.prom` in node_exporter's directory |
| run 2 (idempotence, RP58-5, traced with `bash -x`) | exit 0, **0 `ALTER SYSTEM`**, `postgresql.auto.conf.bak-pitr-*` count 1 → 1, `pgbackrest.conf` sha unchanged (`95982fa7…`), 0 conf `.bak` files; the guard's read: `archive_command=pgbackrest --stanza=vaultaire archive-push %p\|archive_mode=on\|archive_timeout=60` both times |
| the drill as the timer runs it (`systemctl start vaultaire-pitr-drill.service`) | `PITR DRILL PASS target=2026-10-25 05:18:07.530782+00 restore_s=8.454 accept_s=12.389 rto_s=49.524 ohc_restored=507 ohc_live_at_t=507`; stopped and deleted. (RTO 49.5 s against 12.8 s on 2026-10-24: promotion took 41 s — 3 h 48 min of WAL since the Sunday full to replay, against minutes after the first full then; the restore itself 8.5 s) |
| the drill's textfile | `vaultaire_pg_pitr_drill_success 1`, `_last_success_timestamp_seconds 1792905540`, `_rto_seconds 49.524`, read back from node_exporter's `/metrics` |
| app errors (journal, 90 min) | 0 before, 0 after |

**#962 merged `d595f42a` 06:05 UTC → deployed 06:10 UTC** (slot :8001; `/version` `d595f42a685a`, 0 app errors in the hour): the active slot exports 16 `vaultaire_dashboard_login_failures_total{reason,surface}` series, every one at 0 — the pre-creation the auth rules need. The auth rules were installed at 05:17 (above) against the old shape and are label-agnostic sums; they read the new series from this deploy on.

**The merge queue, for the record:** `internal/api` takes ~470–570 s on a fast runner (main's own push run: 567 s); the merge-group runners were ~45 % slower this morning (the dashboard package 74 s → 110 s), so the queue builds of #962 and #963 hit `go test`'s 10-minute default twice with no failing test (the test running at the alarm had used 3 s). The fix is `ci.yml` `go test -race -timeout 20m ./...`, carried by #962 (RP59-5 says the package wants splitting, not a longer timeout for ever). Three queue quirks cost an hour and are worth knowing: a branch in the queue cannot be pushed (`GH006`; `dequeuePullRequest` through GraphQL, then push, then `--auto`); a `workflow_dispatch` CI run does not report `build-and-test` on the PR (an empty commit does); and a PR whose own check had timed out (#964) was never enqueued until the branch carried the fix. #964 also needed a rebase after #962 (both added fields to `Server`) and a test fix: the P40 round trip `TestS3Gate_ARealRoundTripOnWebPathBuckets` CREATED buckets named `legal`, `docs`, `dashboard`, `version` through S3 — item 5 refuses that now; the test seeds them as existing rows (the door's exemption) and asserts the 400 (`025a5553`).

**The owed P40 item — the 2026-10-25 `routing_truth` run:** it ran at **05:46:18 UTC** (the hourly check after the 05:33 deploy; the 05:30 minute itself fell inside a switch), outcome `ok`: `blomp: 2 of 2 rows sampled, 2 present; idrive: 300 of 502 rows sampled, 0 present, 300 missing, 0 unreadable; chunks: 3 object(s), 145 chunk(s), 145 present`. The 502 `idrive` head rows are the 501 rows of the 2026-10-04 account (known, 503 until deleted — CLAUDE.md § Entry Point) plus one new row; the summary does not say whether `tz-proof-1792873702`'s row was among the 300 sampled (RP59-8: the result should name the buckets of its missing rows, or the sample should cover a bucket the operator names).


## Residuals (RP59)

| # | Sev | Residual |
|---|---|---|
| RP59-1 | P4 | `vaultaire_dashboard_login_failures_total` keeps its R12 name with a `surface` label (`dashboard` \| `api`); a rename to `vaultaire_login_failures_total` would lose the alert history — do it with the next rule-file sweep, or never |
| RP59-2 | P4 | The release verifier key is published in the release body only; serving it on the trust host (behind `translog_public`, beside the log's key) needs the key to exist (the owner's ceremony) and a route added to the closed trust list |
| RP59-3 | P4 | `sdk-wasm.cdx.json` is `app` mode (module graph) because `debug/buildinfo` cannot read a wasm binary: it has no hash of the artefact itself; the manifest/`client_build` entry carries it. Revisit when Go's buildinfo reads wasm |
| RP59-4 | P4 | `cyclonedx-gomod bin` finds 0 components unless `GOOS`/`GOARCH` are set in its environment (set to the binary's target in `sbom.sh`); upstream quirk, worth an issue |
| RP59-5 | P4 | CI's `go test -timeout 20m`: `internal/api` takes ~470 s on a fast runner and the merge-group runners were ~45 % slower (pr-962/963 timed out at 10 min with no failing test); the package wants splitting or its DB-backed tests parallelized, not a longer timeout for ever |
| RP59-6 | P4 | The dashboard's `/login/verify-2fa` with an EMPTY code renders an error without counting toward the lockout (its R12 rule) and `/auth/login`'s `code_required` follows it; a client that never sends a code is bounded by the per-IP limiter only — by design, noted |
| RP59-7 | P4 | `pg-pitr_test.sh` on the box FAILs `shellcheck is not installed` (environmental); CI has it. A `shellcheck` on the box would let the operator run the suite green there |
| RP59-8 | P4 | `routing_truth`'s result names counts per backend, not the buckets of its missing rows: the owed tenant-zero check cannot be answered from `job_runs.result` (idrive: 300 of 502 sampled, 0 present — 501 are the dead account's). A per-bucket breakdown of `missing`, or `ROUTING_TRUTH_BUCKETS` to force-sample named buckets |
| RP59-9 | P4 | The drill's RTO was 49.5 s on the timer's first run (12.8 s on 2026-10-24): promotion replayed 3 h 48 min of WAL since the Sunday full. `PGPITRDrillFailed` watches pass/fail only; a rule on `_drill_rto_seconds` (> 120 s?) once a few runs give a baseline |


## Owner items (restated)

RP57-4 (the morning tenant-zero proof: password + MFA code → `TZP_ADMIN_JWT_FILE`; `WP-P39.md`'s verdict table); RP57-10 (a scoped R2 token for `stored-pg-pitr`, re-run the installer, delete the conf `.bak-*` files — RP58-4); RP57-15 (the pgBackRest passphrase → the F-14 escrow); RP57-11 (Sync.com / Blomp as sub-processors — counsel); **NEW: the release-signing key ceremony** (`release-sign keygen`; `docs/RELEASES.md`) and where its public key is published — every GitHub release body, and the trust host (RP59-n: serving it there); the translog signing-key ceremony chain (witnesses, anchors, trust DNS + cert SAN); F-36, F-37, F-28/RP55-4; RB-3.

## What the review (RP60) will check — the owed P40 items

- the 2026-10-25 `routing_truth` result over `tz-proof-1792873702`: ran 05:46 UTC, see Prod — the summary cannot single the bucket out (RP59-8).
- the first timer DIFF backup (Mon 2026-10-26 01:30 UTC) in `pgbackrest info` — after this note; the first SCHEDULED weekly full `20261025-013001F` was verified by the review's second pass.
- the morning proof's verdict table in `WP-P39.md` — the owner's (RP57-4).
