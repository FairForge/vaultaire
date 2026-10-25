# Stage 1a — Prompt 42 Session A: plan 43.7, S3 secrets wrapped under the tenant's key

*2026-10-25. Session A of Prompt 42. Session B runs PR A (the age-encrypted dump), the Step 0 owed checks and the ONE docs PR (WP-P42.md, STATUS, CLAUDE.md, the plan) that folds this note in. **Migration 108** is this session's (`108_credential_secrets_wrapped.sql`; the next is **109**). Residuals here are **RP61-1…20**; Session B's start at RP61-21. The review of this prompt is RP62.*

Lane: Trust (contract rules 4 — keys only through the one hierarchy — and 5 — no global key; `docs/surfaces/TRUST.md` item 12), with Engine's crypto package.

## Step 0

- `git fetch`: main at `4bd79ccc` (#968, the Geyser LA2 docs, DEPLOY.md's `ip_local_reserved_ports` row) — #968 had already landed, nothing to rebase on.
- `docs/reviews/WP-P41-REVIEW.md` did not exist on origin/main: no RP60 code findings in `internal/auth` / `internal/crypto` to take first.
- Tenant zero's flag rows and bucket `tz-proof-1792873702` untouched.
- Worktrees `vaultaire-p42a` (PR B), `vaultaire-p42c` (PR C, stacked), `vaultaire-p42m` (mutations); DB `vaultaire_test_p42a` (`make test-db TEST_DB=vaultaire_test_p42a`); a second DB `vaultaire_test_p42a_main` for the RED runs on main.

## The PRs

| PR | Branch | What | Merge |
|---|---|---|---|
| #969 | `feat/p42-credential-wrap` | **PR B** — migration 108, `SealCredential`/`OpenCredential`, wrap on write, ONE reader rule, the `key_reencrypt` backfill + re-wrap; dark without a master key | `5d9c2f03` 18:14 UTC; deployed by run 40220306035 (success), `/version` `5d9c2f03ef30` |
| #970 | `feat/p42-credential-drop` | **PR C** — flag `credential_plaintext_drop` (OFF, global only): writers drop the plaintext, the job nulls it where the wrap opens back to it, 409 refusals, `vaultaire-switch` refuses a pre-108 build; this note | merge sha and deploy run: Session B's docs PR (this note ships inside #970) |

## PR B — the wrap (dark until the master key)

| Piece | What | Decision / why |
|---|---|---|
| 108 | `api_keys` / `sts_tokens`: `secret_wrapped BYTEA`, `secret_key_version INT`; partial index `idx_api_keys_secret_unwrapped (tenant_id) WHERE secret_wrapped IS NULL AND secret_key IS NOT NULL AND revoked_at IS NULL`; **`sts_tokens.secret_key DROP NOT NULL`** | Additive; the NOT NULL drop is here so PR C needs no migration (the previous build always writes the column). `tenants.secret_key` gets no wrapped column: the mirror is read by no lookup, nulled by PR C and dropped with D-12 |
| `crypto.TenantKeys.SealCredential` / `OpenCredential` | HKDF-SHA256(the version's chunk key, no salt, `stored/credential/v1`) → AES-256-GCM; `[0x01][nonce 12][ct+tag]`; AAD `"vaultaire-credential" ‖0‖ tenant ‖0‖ key id ‖0‖ version u32 BE`; empty names and a NUL in a name refused (`"a\0b"‖"c"` = `"a"‖"b\0c"` otherwise) | The ONE seam the F-20 broker replaces. A derivation of the existing chunk key, no new key and no `tenant_key_versions` column (contract rule 4). A master re-key leaves the wraps as they are (the chunk key's bytes do not change); a tenant key rotation moves them (the job). The golden vector was computed with Python `cryptography` 45 (independent of our code) |
| Writers | registration's primary (`persistNewAccount`), a new key and a rotation's successor (`persistAPIKey`), an STS mint (`GenerateSTSToken(ctx, db, keys, …)`) wrap when the server has a master key; a seal that fails refuses the write | Still write the plaintext (expand). `AuthService.SetCredentialKeys` is called where the master key loads and in `SetAuthService` |
| Reader | `auth.openStoredSecret`, in `LookupCredential` (both tables) and `(*Auth).PrimaryPair` (now a method, so the opener travels with the Auth): wrapped present → opened, and only it; failure = Error line + `vaultaire_credential_unwrap_failures_total{table}` (both series pre-created) + `ErrCredentialUnavailable` → **500 `InternalError`** (header path, presign verifier; the presign route 500, no longer "credentials not found" 404) — **never the plaintext beside it**; wrapped absent → plaintext | `NewAuth(db, logger, keys)` is called in ONE place, `Server.s3Auth` (the three per-request call sites, `whoami`, the export service's signer) |
| Memory | `LoadFromDB` loads no secret; the maps keep secretless copies of a key the process creates (the returned key shows it once) | Traced: nothing on a serving path read `Tenant.SecretKey` / `APIKey.Secret` after boot (listings blanked it, `ValidateS3Request` has no product caller, `ValidateAPIKey` reads the hash). Side effect: a caller's `*APIKey` no longer aliases the map's (WP-R5-6's "what leaves is a copy" now holds for `GenerateAPIKey` too) — one old test read the revocation through the alias and now asks the service |
| Backfill | a pass of `key_reencrypt` (`credential_backfill.go`), first after the master re-wrap: ≤ 1,000 live rows a run (api_keys not revoked with a tenant, sts_tokens not expired) — unwrapped ones sealed, wrapped ones below the tenant's newest version re-sealed; each seal opened back and compared before the write; the write guarded on the value read (`IS NOT DISTINCT FROM` the old wrap or NULL): a lost race is skipped and counted (`credentials_raced`). Result `credentials_wrapped`, `credentials_rewrapped`, `credential_failures`; `vaultaire_key_rotation_pending{kind="credential"}` | **Not at lookup** (the plan said "lazy backfill at lookup"): a write + seal + failure mode on the SigV4 hot path, and a key nobody presents would never be wrapped — PR C's precondition "every live row is wrapped" could never be met. The job wraps everything within one or two runs of the master key's arrival |
| Export / schema | the four columns `credential` in `exportNeverColumns`; `TestSQLLiteralsMatchSchema`, `TestErasedTablesCoverSchema`, `TestExportSectionsCoverSchema`, produce tests green | — |

### Tests (red first, handlers, own DB)

| Test | RED before (main `87eeec45` / `4bd79ccc`) | Green after |
|---|---|---|
| `TestP42RED_TheApiKeysRowIsTheSecretInTheClear` (main only, not committed) | the dumped `api_keys` row **is** the secret ("signs as tenant tenant-9732a92eba413042"), so is the `tenants` mirror, `pq: column "secret_wrapped" does not exist` | — (PR C closes the first two, PR B the third) |
| `TestCredentialSeal_GoldenAndRefusals` | `undefined: sealCredential` | Python-computed vector; another tenant / tenant prefix / key id / version / tenant's key, flipped format / nonce / body / tag, truncated, empty, NUL collision → `ErrCredentialOpen`; fresh nonce each seal |
| `TestTenantKeys_CredentialRoundTripAndShred` | build | v1 → rotate → v2, v1 still opens as v1 only; another tenant refused; master re-key keeps the wrap; version rows deleted → `ErrNoTenantKey` |
| `TestLookupCredential_WrappedRowSignsAWSRequest` | build (`NewAuth` arity, no columns) | registration / new key / rotation successor wrapped; with **every plaintext column NULLed**: aws-sdk SigV4 PUT + GET, the presign route + its presigned GET; rotated pair still `InvalidAccessKeyId` |
| `TestLookupCredential_UnwrapFailureIsOursNeverPlaintext` | build | damaged wrap, another row's wrap with that row's plaintext beside it, a server without the key, the presign route → 500 each, counted; a row with no wrap still signs from the plaintext |
| `TestLookupCredential_STSWrappedAndParentChecks` | build | STS route wraps; signs with no plaintext; damaged → 500 on `sts_tokens`; a rotated parent's token `InvalidAccessKeyId` |
| `TestCredentialWriters_AllWrapWhenKeysExist` | — (new structure) | AST walk of `internal/` + `cmd/`: every SQL writer of a secret column of the three tables is listed in `credentialWriters`; every INSERT carries `secret_wrapped` + `secret_key_version` |
| `TestNewAuth_EveryCallSiteHasTheOpener` | — | `auth.NewAuth` only in `Server.s3Auth`; `PrimaryPair` on `s.s3Auth()` / `s.signer`; `NewAccountExportService(…, s.s3Auth(), …)`; STS with `s.credentialKeys()`; `SetCredentialKeys` in `NewServer` + `SetAuthService` |
| `TestKeyReencrypt_BackfillWrapsVerifiesAndRaces` | build | a sabotaged seal is caught by the re-open (3 failures, nothing written); budget of 1; a lost race skipped and counted; revoked and expired rows never wrapped; the job's wraps sign alone |
| `TestKeyReencrypt_RotationRewrapsCredentials` | build | pending `credential` 2 → 0, rows at v2, sign alone; a damaged wrap after a second rotation counted, untouched |
| `TestCredentialWrap_ErasureShredsTheWrappedSecret` | build | the real `account.Service.EraseRows`: a wrap dumped before the erasure opens under no key after (`ErrNoTenantKey`) |

### Mutations (PR B) — 19/19 killed

| # | Mutation | Caught by |
|---|---|---|
| B1 | AAD without the key id | `GoldenAndRefusals` |
| B2 | AAD with a fixed version | `GoldenAndRefusals` (another version) |
| B3 | another HKDF label | `GoldenAndRefusals` |
| B4 | a NUL in a name accepted | `GoldenAndRefusals` |
| B5 | **fallback to the plaintext on an open failure** | `UnwrapFailureIsOursNeverPlaintext`, `STSWrappedAndParentChecks` |
| B6 | no keys = the plaintext | `UnwrapFailureIsOursNeverPlaintext` |
| B7 | the failure not counted | both |
| B8 | an unwrap failure answers AccessDenied | both |
| B9 | **GenerateAPIKey skips the wrap** | `WrappedRowSignsAWSRequest` ("GenerateAPIKey wraps"), `RotationRewrapsCredentials` |
| B10 | registration skips the wrap | four tests |
| B11 | the STS route mints without the keys | `STSWrappedAndParentChecks`, `EveryCallSiteHasTheOpener` |
| B12 | the auth service never gets the keys | `EveryCallSiteHasTheOpener` |
| B13 | the presign verifier builds its own Auth | `EveryCallSiteHasTheOpener`, `WrappedRowSignsAWSRequest` |
| B14 | backfill writes without the re-open check | `BackfillWrapsVerifiesAndRaces` |
| B15 | backfill write unguarded | `BackfillWrapsVerifiesAndRaces` |
| B16 | backfill wraps revoked keys | `BackfillWrapsVerifiesAndRaces` |
| B17 | no re-wrap after a rotation | `RotationRewrapsCredentials` |
| B18 | the pending gauge ignores credentials | `RotationRewrapsCredentials` |
| B19 | secrets loaded into memory at boot | `TestPrimaryKey_RotationKillsTheOldPairEverywhere` |

### Lab (PR B)

- `go test -race ./...` on `vaultaire_test_p42a`: green (after the fallout below). `make lint` 0; gosec (pinned command via `go run`) 0.
- Fallout fixed in the PR: the WP-R2-6 allocation audit named `sealCredential` (row added: < 100 bytes per credential write); sqlmock rows of the presign tests grew the two columns; `TestSSES3_GetAfterRotate` counts three `vaultaire_key_rotation_pending` series; two auth tests read the secret or the revocation through the in-memory alias (above).
- **Live** (`/verify`; local binary, `ENCRYPTION_MASTER_KEY_FILE`, DB `vaultaire_test_p42a`): `master key: file`; registration → `secret_wrapped` 63 B, `secret_key_version` 1, plaintext kept; aws-cli `mb` / `cp` / `cp -` ✓; plaintext NULLed in `api_keys` and the `tenants` mirror → `cp` / `cp -` / `ls` ✓ (the wrapped form alone signs); restart **without** the key → `InternalError` (500) — also after the correct plaintext was put back beside the wrap — with `vaultaire_credential_unwrap_failures_total{table="api_keys"}` 3 → 6 and the Error line "a wrapped S3 secret did not open — the request is refused (500), never served from the plaintext column"; a pre-108 plaintext-only key → the job's first run (4 min after boot) `credentials_wrapped=1 credential_failures=0` → `secret_wrapped` 69 B v1 → signs with its plaintext NULLed. The boot's `tenant keys checked … unwrap_failures=3` were the concurrent test suite's key rows under the tests' master keys on the shared test DB.

### Prod (PR B)

- Deploy run `40220306035` green; `https://stored.ge/version` → `5d9c2f03ef30`; active slot `:8001`.
- **Migration 108 applied**: `api_keys.secret_wrapped`, `.secret_key_version`, `sts_tokens.secret_wrapped`, `.secret_key_version` present; `sts_tokens.secret_key` now nullable; `idx_api_keys_secret_unwrapped` present.
- **0 wrapped rows** in both tables (no master key — expected); 3 live `api_keys` rows hold their plaintext; **no `credential_plaintext_drop` row**.
- `vaultaire_credential_unwrap_failures_total{table="api_keys"} 0`, `{table="sts_tokens"} 0` (pre-created). `vaultaire_key_rotation_pending` is not exported without a master key (as before).
- **0 `"level":"error"` lines** on the active slot in the 20 min after the switch.
- A real S3 round trip through `stored.ge` with the bench tenant's key (`~/vaultaire-bench/.env.bench`): `mb` / `cp` / `cp -` (`after 108`) / `rm` / `rb` ✓, the counters still 0.

## PR C — the drop (behind `credential_plaintext_drop`)

| Piece | What | Decision / why |
|---|---|---|
| Flag | `credential_plaintext_drop`, default OFF, `GlobalOnly` (a tenant row 400 `flag_global_only`) | Global: the dump is global |
| Writers | `sealSecret` → `storedSecret{plaintext, wrapped, version}`; the plaintext is SQL NULL when the keys implement `auth.PlaintextDropper` and say so — the api's `credentialKeyring` (`credentialKeys()` = key store + the flag) — in `api_keys`, `sts_tokens` and the tenants mirror (registration; `rotateRows` mirrors what `persistAPIKey` wrote) | No call site changes (the wrapper rides the existing seam). **No keys = the plaintext, flag or not**: a slot booted without its credential never stores a secret nowhere |
| The job | with the flag on, after the wrap pass: `dropPlaintextPass` nulls the plaintext of every wrapped row whose wrap opens back to **exactly** it (write guarded on the plaintext and the wrap seen), of every **dead** row (revoked key, key with no tenant, expired token) with or without a wrap, then the tenants mirror where the live primary has no plaintext left; ≤ 1,000 rows a run. `credentials_plaintext_dropped`, `mirrors_plaintext_dropped`; a wrap that does not open keeps its plaintext (a `credential_failures` note) | Dead rows: the prompt's rule is "only after its wrapped form opens"; a dead row is never wrapped (it never authenticates — the lookup refuses it before reading a secret), so it would keep a plaintext in every dump for nothing. Said here so the review can disagree |
| Refusals | the server's write guard (`guardFlagWrite`: the credential check, then the translog `log_policy` append — `flags.Service` has one guard slot): turning it ON → **409 `master_key_absent`** without a key store, **409 `credentials_not_wrapped`** + `unwrapped` count while a live row holds only the plaintext; nothing written, nothing logged. Off / clear: always allowed. `auth.PlaintextDropRefusal`, mapped by the admin API (`flagWriteError`) and the dashboard (`flagRefused`); `flag.set` audited as today | Turning it off brings nothing back (the wrap is the credential); it only restarts plaintext writes |
| Rollback | **`vaultaire-switch` refuses** a build older than PR B while the flag is on or unreadable: `start_and_switch` (deploy, restart, rollback) greps the binary for `stored/credential/v1` (the HKDF label; every build since PR B has it, main's had 0 — checked on two linux/amd64 builds) and only then asks the flag (`runuser -u postgres -- psql` on `DB_NAME`); fail closed. `docs/DEPLOY.md` § Zero-downtime deploys, "Rollback past the wrapped S3 secrets"; `TestCredentialWrap_TheSwitchMarkerIsTheHKDFLabel` binds the script's string to the constant | Chosen over a runbook line: a one-way door, and a refusal cannot be forgotten during an incident. The script is `deploy/**`: **installed on the box by hand** (below) |

### Tests (PR C)

| Test | RED before (PR B `a86465a6`) | Green after |
|---|---|---|
| `TestP42RED_AfterPRBADumpStillHoldsTheSecret` (PR B only, not committed) | wrapped and backfilled, `api_keys` and `tenants` still hold the secret in the clear | — |
| `TestCredentialPlaintextDrop_RefusedWithoutKeyOrWithUnwrappedRows` | `undefined: flagCredentialPlaintextDrop` | handler 409 `master_key_absent`, no row; 409 `credentials_not_wrapped` `unwrapped: 1`, no row; the same through `flags.Set`; tenant row `ErrGlobalOnly`; after a job run the guard passes; off / clear never refused |
| `TestCredentialPlaintextDrop_WritersStopWritingPlaintext` | build | registration (row + mirror), new key, rotation (+ mirror), STS: NULL plaintext, all sign; flag off → plaintext again |
| `TestCredentialPlaintextDrop_NoKeysStillWritesThePlaintext` | build | flag on, no key store → plaintext written, signs |
| `TestCredentialPlaintextDrop_NullsOnlyRowsThatOpen` | build | flag off: wraps, nulls nothing; on: the primary, a revoked key, an expired token nulled, the mirror nulled, a damaged wrap keeps its plaintext; a plaintext differing from its wrap is kept |
| **`TestDump_RestoredWithoutTheMasterKeyHasNoUsableSecret`** (43.7's own) | build | after the drop, every row of every table (`t::text`, > 50 tables) and `pg_dump --data-only` of `api_keys`, `sts_tokens`, `tenants`, `audit_logs`: none of the primary, the extra key or the STS secret; with the key the request signs, without it 500 (key and token) |
| `TestFlagRefused_CredentialPlaintextDrop` (dashboard) | build | both codes → 409 with the reason, `no-store` |
| `vaultaire-switch_test.sh` (+9) | the marker absent | marker = the label; new build passes; old build passes only with the flag `f`; refused when on, unreadable or empty; the refusal says why; `DB_NAME` read; `start_and_switch` runs it |

### Mutations (PR C) — 17 + 3 shell, all killed

| # | Mutation | Caught by |
|---|---|---|
| C1 | the guard never refuses without a key | `Refused…` |
| C2 | the guard ignores unwrapped rows | `Refused…` |
| C3 | the count misses STS tokens | `Refused…` |
| C4 | the credential guard not installed (translog guard only) | `Refused…` |
| C5 | the 409 becomes the generic 503 | `Refused…` |
| C6 | the flag not global-only | `Refused…` |
| C7 | writers ignore the flag | `WritersStop…` |
| C8 | the rotation's mirror keeps the plaintext | `WritersStop…` |
| C9 | registration's mirror keeps the plaintext | `WritersStop…` |
| C10 | flag on, no keys → nothing stored | `NoKeysStill…` |
| C11 | the drop nulls without opening | `NullsOnly…` |
| C12 | the drop does not compare | `NullsOnly…` |
| C13 | the drop pass runs with the flag off | `NullsOnly…` |
| C14 | the mirror never nulled | `NullsOnly…` |
| C15 | NewServer never hands the job the flag | `EveryCallSiteHasTheOpener` (an assertion added after the first run: it **survived** behind a test-hygiene failure, below) |
| C16 | the keyring never drops | `WritersStop…` |
| C17 | the dashboard maps the refusal to 503 | `TestFlagRefused_…` |
| S1 | `start_and_switch` skips the guard | shell: "runs the guard", shellcheck |
| S2 | an unreadable flag passes | shell: "fail closed", "empty answer" |
| S3 | the marker check passes everything | shell: five checks |

**Caught by the mutation run:** C1 let the handler write a GLOBAL `credential_plaintext_drop = t` row on the shared test DB, which then failed every later run's "nothing written" checks (C7, C9, C15, C16 looked killed for the wrong reason). The row was deleted, the refusal test now deletes the flag's rows in its cleanup, C15 got its own assertion, and the four were re-run clean.

### Lab (PR C)

- `go test -race ./...` on `vaultaire_test_p42a`: green. `make lint` 0; gosec 0; `bash deploy/scripts/vaultaire-switch_test.sh` PASS (shellcheck clean).
- Marker check: `grep -aFc stored/credential/v1` = **0** on a linux/amd64 build of main, **1** on this branch.
- **Live** (`/verify` flow; local binary, DB `vaultaire_test_p42a`; an account registered WITHOUT a master key, made `role = admin`, signed in through `/auth/login`): `PUT /api/v1/admin/flags/credential_plaintext_drop {"enabled":true}` → **409 `master_key_absent`**, no row; restarted with `ENCRYPTION_MASTER_KEY_FILE` → **409 `credentials_not_wrapped` `"unwrapped":2`**, no row; `POST /api/v1/admin/jobs/key_reencrypt/run` (202) → `credentials_wrapped=2 credential_failures=0`; the PUT → **200**, `enabled: true`; the next run → `credentials_plaintext_dropped=2 mirrors_plaintext_dropped=2`; the row's plaintext NULL, wrapped present, the mirror NULL; aws-cli `mb` / `cp` / `cp -` ✓; **`pg_dump` of the whole test database (2.8 MB): the secret 0 times**; an STS token minted through the API while ON: plaintext NULL, wrapped, signs ✓. Error lines: the boot's test-DB key rows and three `read egress status for alert` of other packages' fixture tenants (no quota row) — none of this change. The flag row and the tenant were deleted after.

### Prod (PR C)

- **`vaultaire-switch` installed on the box by hand before this PR's deploy** (2026-10-25 18:21 UTC; `deploy/**` never ships through the pipeline): the installed script was byte-identical to main's first (`cmp`), backup `/usr/local/sbin/vaultaire-switch.bak-p42-20261025T1821`, new sha256 `72a74241bfa8e093…` = the branch's. Before installing, the guard's own functions were run as root against prod: the flag read through `runuser -u postgres -- psql` on `DB_NAME` = `f`; `vaultaire-8001` (`5d9c2f03`, marker 1) pass; `vaultaire-8000` (`bcb2183e`, the idle pre-108 build, marker 0) **pass only because the flag is off** — with it on, a `vaultaire-switch rollback` now would be refused.
- **After #970's deploy** (measured by Session A after the merge and handed to Session B with the merge sha — this note ships inside the PR): `/version` = the merge; the deploy ran through the NEW `vaultaire-switch` (the guard passed: the new build carries the marker); 0 `"level":"error"`; no `credential_plaintext_drop` row and the flag OFF in `GET /api/v1/admin/flags`; `vaultaire_credential_unwrap_failures_total` both series 0; a bench-key S3 round trip through `stored.ge`.

## For the owner ([YOU])

1. **F-14, the master-key ceremony** (`docs/ENCRYPTION.md` § Setting the master key), then one `sudo vaultaire-switch restart`.
2. Within ~15 min the `key_reencrypt` job wraps every live S3 secret: `GET /api/v1/admin/jobs` → `key_reencrypt.result.credentials_wrapped` > 0 and `credential_failures` 0, then 0 on a later run.
3. Flip `credential_plaintext_drop` ON (`/admin/flags` or `PUT /api/v1/admin/flags/credential_plaintext_drop {"enabled":true}`). A 409 tells you why not yet (`master_key_absent` | `credentials_not_wrapped` + count). The next run nulls the plaintext (`credentials_plaintext_dropped`, `mirrors_plaintext_dropped`). From then on a build before #969 cannot be deployed or rolled back to (the switch refuses it).

## Residuals (RP61-1…20)

| # | Sev | What | Where / why left |
|---|---|---|---|
| RP61-1 | P3 | **Old dumps keep the plaintext.** The drop is not retroactive: the on-box dumps (7 d), the off-box copies (30 d) and the PITR archive (≤ 28 d) taken before the flip hold every secret in the clear until they age out. The only way to make them useless sooner is to rotate every key after the flip (a customer-visible event) | Owner decision: rotate-all after the flip, or wait out the longest retention (30 d) and say so on the produce page |
| RP61-2 | P4 | The seal on a registration creates the tenant's key v1 OUTSIDE the registration transaction (`TenantKeys.Current`): a registration that then fails leaves a `tenant_key_versions` row for a tenant id that never existed | Harmless (random id, no data), never swept; a sweep of key rows with no tenant would close it |
| RP61-3 | P4 | `LookupCredential` now runs an HKDF + AES-GCM open per wrapped request (the version cached 1 h): microseconds, not benchmarked against the SigV4 path | A `BenchmarkLookupCredential_Wrapped` when the broker (F-20) moves the unwrap out of process |
| RP61-4 | P4 | Revoked keys, keys with no tenant and expired tokens are never wrapped; while the flag is OFF they keep their plaintext in every dump (none authenticates) | Closed by the flag (the drop nulls them); a retention rule for revoked rows would close it earlier |
| RP61-5 | P4 | A wrap after a rotation on the OTHER slot records the old version for ≤ 60 s (`currentKeyTTL`), like chunks; the job re-wraps it | By design (WP-R8-1) |
| RP61-6 | note | **The plan's text says "lazy backfill at lookup"**; this session built a job pass (why in PR B's table) | Session B: plan 43.7's row to say "the `key_reencrypt` job's credential pass" |
| RP61-7 | P4 | A tenant whose key rows open under no master key (`unwrap_failures`) can never have its credentials wrapped, so `credentials_not_wrapped` blocks the flag for everyone | Intended (the drop must never strand a key); the 409's count says how many — find the tenant in the job's `errors` |
| RP61-8 | note | The `vaultaire-switch` refusal reads the flag with `runuser -u postgres -- psql` only for a binary WITHOUT the marker; a future rename of the HKDF label must keep `stored/credential/v1` in the binary (the test binds the two) | — |
| RP61-9 | note | Canary credentials (43.8), the produce page claiming encrypted dumps (after the recipient file exists on the box — Session B's PR A), and dropping the mirror columns (D-12) are not in this scope | Next prompts |
| RP61-10 | P4 | The 409 counts live unwrapped rows on the whole database; on prod after the ceremony that is every key until the job's first run (~4 min after a restart, then every 10 min). No progress gauge of "unwrapped live rows" | A `vaultaire_credentials_unwrapped` gauge if the owner wants to watch it rather than ask the 409 |
| RP61-11 | note | The ENCRYPTION.md shredding caveat now covers wrapped secrets: a dump taken before an erasure still holds the tenant's key rows, so the old master key + that dump open them until the dump ages out (as for data) | Disclosed, unchanged |
| RP61-12 | note | `GenerateAPIKey` / `CreateUserWithTenant` no longer return the map's own pointer: a caller holding the returned `*APIKey` does not see a later revocation on it (one test read through the alias) | WP-R5-6's rule, now true for these two |
