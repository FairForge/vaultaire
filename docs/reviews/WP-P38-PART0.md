# Stage 1a — Prompt 38 Session A: Part 0 — RP52-3 (an account erasure reaches the recovery realms), RP51-3 (a Deploy warning for unlisted client builds), RP51-2 (`make lint-wasm` in CI)

*2026-10-24. Session A of Prompt 38 (split 2026-10-24; Session B runs Part A and writes the docs PR — STATUS.md, CLAUDE.md, the plan rows and `docs/reviews/WP-P38.md` fold this note in). Merged: #940 (RP51-3, `2e772d2e`), #939 (RP52-3, `a2dfce6b`), and this PR, #943 (RP51-2 + this note). **No migration**: the next is still **108**. Residuals RP53-1…RP53-7 (Session B numbers from RP53-10).*

Lane: Engine (K5's erasure), Trust (the client build entries), Developer (the sdk's lint). Contract line 1 (least access: an erased user's record leaves every realm), line 7 (the log lists what we ship), rule 8 (claim nothing we do not do: "erased" must hold at the realms too).

## Step 0

- **#937** (RP52-1/2) was in the merge queue at the start (CI green, `isInMergeQueue: true`); merged as `8d17d96f`. CI and Deploy green on the merge commit; `/version` `8d17d96f4cb4` (build 12:54:37Z); active slot 8000 since 13:12:54 UTC: 398 journal lines, **0** `"level":"error"`.
- `recovery_vault` OFF on prod (no `feature_flags` row: the default; `POST /api/v1/recovery/tokens` → **404**), `RECOVERY_*` unset (0 `RECOVERY_` lines in `/opt/vaultaire/configs/.env`); `translog_public` OFF (no row). Both expected.

## 1. RP52-3 — an account erasure reaches the recovery realms (#939)

| # | Finding | Sev | Fix | RED test on main (the fields stubbed) | Mutation → caught by |
|---|---|---|---|---|---|
| 1a | **The runner never talked to a realm**: an erased user's key shares and encrypted secret stayed at every realm for ever, under the HMAC subject | P3 (the gate before `recovery_vault` can be turned on) | Stage **b3** of `deletion_runner.go` (`recovery_erasure.go`), after the sweep and before the row erase (the user row still exists): for the due user and each realm of `RECOVERY_REALMS_FILE`, a token with `mint`'s claims (`recoveryRealm.token`, factored out of `mint`: `iss` = the kid's tenant, `aud` = the realm, `scope` user, **10 min — never longer**) and a `client.Delete` through `HTTPTransport`, which rides the Noise handshake (a Delete needs no forward secrecy). 30 s per realm, the transport's retries included | `TestAccountDeletionRunner_ErasesTheUsersRecordAtEveryRealm`: `realm a holds nothing for the subject — Should be false` (and realm b; 0 Delete requests at each) | M1 the stage call removed → all 5 tests red; M5 `realms_deleted` dropped from the audit row → red; M4b the token's life doubled → red (`the erasure's token has the minter's life, never longer`); M4a a token minted 23 h ago (expired) → 3 red |
| 1b | NotRegistered | — | A realm answers a Delete of nothing with `OK` (`realm.go`: `OpDelete` of a `NotRegistered` record changes nothing), so a realm that never held a record, or one an earlier run reached, counts as done — no special case | `…RealmDownDefersTheTenantAndRetries` second run: `realms_deleted 2` with realm a already empty | — |
| 1c | **A realm that does not confirm** (unreachable, 5xx, `Unavailable`, a refused token) | — | **defers the tenant** (`defer_` stage `recovery realms`, the run fails, the hourly check retries), exactly as a backend the sweep cannot list: never `account.erased` while a realm may hold the record. The other realms are still asked (the reachable one's record goes now) | `…RealmDownDefersTheTenantAndRetries`, `…UnreachableRealmDefers` (red on main: `An error is expected but got nil`) | M2 deferral ignored → 2 red; M6 a failed realm skipped silently → 2 red; **M8 the stage moved after the row erase → 3 red** |
| 1d | **A realm token minted within its life could still Register after the Delete** (found writing the fix: a user can mint until the rows go, and a token lives 10 min) | P4 | A `recovery.tokens_issued` audit row younger than the token life + 1 min (the realms accept `exp` + 5 s skew) defers the tenant: `a realm token minted at … may still register until … — the realms are asked at the next run`. The seconds-wide window between this check and the row erase stays (RP53-3) | `…RecentRealmTokenDefers` (red on main: no error) | M3 the check removed → red |
| 1e | Not configured | — | `s.recovery == nil`: nothing asked, `realms_not_configured: true` on `account.erased`, one note on the run (below) | `…RecoveryNotConfiguredIsANote` | M7 the flag dropped → red |
| 1f | Counts | — | `account.erased` (and `account.erasure_settling`) carry `realms_deleted` / `realms_deferred`; the run's tenant entry too. In an `account.erased` row `realms_deferred` is always 0 by construction (a deferral writes no such row); a deferred tenant's count is in the run's `job_runs` error and tenant entry. Never the subject (`NotContains(meta, sub)`) | `…ErasesTheUsersRecordAtEveryRealm` | M5 |

**One user per erasure.** The prompt says "each user of the tenant": `account.Due` is one user (the tenant is joined by e-mail) and `EraseRows` erases that user only, so the realm stage asks for `d.UserID`. A second user of the same tenant is erased by its own due row.

**Tests:** `internal/api/deletion_runner_recovery_test.go`. Two `internal/recovery/realm` `MemoryStore`s behind a minimal Noise NK `/req` (the handshake path, `cmd/recovery-realm`'s token rule in short), so the runner's real `HTTPTransport`, token minting and `loadRecoveryVault` run end to end. The realm package is linked into the test only — `go list -deps ./cmd/vaultaire` has none (`TestRecoveryRealm_NotLinkedIntoVaultaire` green). The runner is scoped (`onlyDue`). `go test -race ./internal/api/ ./internal/recovery/... ./cmd/recovery-realm/` green; `make lint` 0; gosec `@latest` (the CI command) 0.

**Not configured — what it means later.** The run's note says: this process minted no realm token, so no realm holds a record of its minting. A record minted while `RECOVERY_*` was set and since unset stays at its realm; the user id is in `account.erased`'s metadata, and with the same subject key it derives the same subject, so it can still be deleted once the vault is configured again. **Nothing runs that replay** (the user row is gone, so the runner no longer sees the user): RP53-1. On prod nothing is owed — `RECOVERY_*` has never been set, so no token was ever minted.

**A realm removed from `RECOVERY_REALMS_FILE` while it still holds records** is not asked again: every record there stays. A residual, not a fix (RP53-2).

### Lab (two `recovery-realm` on BoltStore + `bin/vaultaire` `0cb7a14e` with `RECOVERY_*`, private DB `vaultaire_lab_p38a`)

Realms `4d90e749…` (`:18780`) and `e13567e7…` (`:18790`), each with its own tenant key (`stored:1`); `vaultaire` with `RECOVERY_SUBJECT_KEY_FILE`, `RECOVERY_TOKEN_KEYS_FILE`, `RECOVERY_REALMS_FILE` (boot: `recovery vault: token minter ready` naming both, 38 lines, 0 errors); `recovery_vault` ON by a global row **in the lab DB only**.

```
alice POST /api/v1/recovery/tokens -> 200
registered at 2 realms under sub vR0xwG-I…; recover = "lab recovery key"      # our Go client, HTTPTransport, Standard2019
bob   POST /api/v1/recovery/tokens -> 200
registered at 2 realms under sub P7Umrk60…; recover = "lab recovery key"
each realm: recovery_realm_requests_total{op="register2",outcome="ok"} 2

DELETE /api/v1/user (alice) -> 200; deletion_scheduled_at back-dated 1 h
POST /api/v1/admin/account-deletion -> 202
job_runs: error | tenant … deferred: recovery realms: a realm token minted at 2026-10-24T13:09:21Z
          may still register until 2026-10-24T13:20:21Z — the realms are asked at the next run
# (1d live: the tokens were 13 s old. The lab back-dated that audit row 12 min instead of waiting.)
POST /api/v1/admin/account-deletion -> 202
job_runs: ok | rows 1
account.erased: user_id 52866fed-… realms_deleted 2 realms_deferred 0   (no realms_not_configured)
users row: 0
realm :18781  delete ok 1   records_destroyed_total 0
realm :18791  delete ok 1   records_destroyed_total 0
# both realms stopped, the BoltStores loaded:
[realm a] alice: state=NotRegistered revision=0 holds_secret_material=false
[realm a] bob:   state=Registered revision=3 holds_secret_material=true      # the control
[realm b] alice: state=NotRegistered revision=0 holds_secret_material=false
[realm b] bob:   state=Registered revision=3 holds_secret_material=true

# one realm down: bob scheduled + back-dated (and his token row), realm b stopped
realm b: connection refused (stopped)
POST /api/v1/admin/account-deletion -> 202
job_runs: error | tenant … deferred: recovery realms: 1 of 2 realms did not confirm the delete:
          e13567e7bb5b4e8e7abb5994c2c3012b: recovery: not enough realms answered: realm e13567e7…:
          Post "http://127.0.0.1:18790/req": dial tcp 127.0.0.1:18790: connect: connection refused
bob's users row: 1                     # nothing of the rows went
realm :18781  delete ok 1              # realm a's record went at once
# realm b restarted
POST /api/v1/admin/account-deletion -> 202
job_runs: ok | rows 1
account.erased: user_id ce944382-… realms_deleted 2 realms_deferred 0
bob's users row: 0
realm :18781  delete ok 2 (bob's second Delete: NotRegistered = OK)   records_destroyed_total 0
realm :18791  delete ok 1                                             records_destroyed_total 0
[realm a] bob: state=NotRegistered revision=0 holds_secret_material=false
[realm b] bob: state=NotRegistered revision=0 holds_secret_material=false
vaultaire log: 72 lines, 4 "level":"error" — the two deliberate deferrals (each "tenant deferred" + "job run failed")
```

`records_destroyed_total` counts records that ran out of guesses (NoGuesses erasure); an account erasure is a `delete ok`, so it stays 0.

## 2. RP51-3 — main can ship client bytes the log does not list (#940)

| # | Finding | Sev | Fix | RED test on main | Mutation → caught by |
|---|---|---|---|---|---|
| 2a | Between an sdk merge and its manifest row, main builds client bytes no row lists; `translog-verify -client` refuses them and nothing says so. No CI check can demand the row (it names the MERGE commit, which no sdk PR knows — WP-P37 § A, guard withdrawn) | P4 | `scripts/repro-check-clients.sh` (the Deploy job's step) builds **the checkout's own clients** after the rows (`scripts/build-client.sh`, default list = translog's closed list) and looks each sha256 up in the manifest: listed → `this checkout's <client> <hash> is listed (N s)`; not listed → `::warning::repro-check-clients: this checkout builds <client> <hash>, which no row of the client manifest lists — translog-verify -client refuses it until a row names it`; a build that fails → a `::warning::` too. **Never a failure**: the exit code is the rows' alone | `TestClientBuildManifest_ReproCheckWarnsOnAnUnlistedCheckoutBuild` (the real 0.1.0 sdk-js row alone, `REPRO_CHECKOUT_CLIENTS=sdk-js`): `does not contain "::warning::"` | R1 no `::warning::` prefix → red; R2 an unlisted build fails the deploy → red (exit 1); R3 the lookup inverted → 2 red; R5 the hash not named → red |
| 2b | The list drifting from the closed list | — | `checkout_clients=${REPRO_CHECKOUT_CLIENTS:-sdk-wasm sdk-js}`, held to `translog.BuildClients()` | `TestReproCheckClients_DefaultsToTheClosedClientList` | R4 sdk-wasm dropped → red |
| 2c | A listed build is quiet | — | — | `…ListedCheckoutBuildIsQuiet` (a row at HEAD with its own hash: no warning) | R3 |

**Proof on a doctored manifest** (the real one minus the 0.2.0 rows = "sdk merged, row not yet added"):

```
repro-check-clients: sdk-wasm 0.1.0 @ bc004600576e: 07685b5f…c911 OK
repro-check-clients: sdk-js 0.1.0 @ bc004600576e: 4766be3d…d7d0 OK
::warning::repro-check-clients: this checkout builds sdk-wasm bc06f7a614c864f9eb1c0de690be55c997a66826ff891eb3e2eadc4b1fee9dcd, which no row of the client manifest lists — translog-verify -client refuses it until a row names it (0 s)
::warning::repro-check-clients: this checkout builds sdk-js beea90d70611834c07aa00fca19be68ff14cbd713d30d164f7cbd68ec5b7b151, which no row of the client manifest lists — translog-verify -client refuses it until a row names it (0 s)
script exit: 0
```

On the real manifest: both of the checkout's builds listed (`bc06f7a6…` = 0.2.0's sdk-wasm, `beea90d7…` = 0.2.0's sdk-js). **Runner time** (Deploy run 40100487800, #940's merge commit `2e772d2e`): both of the checkout's builds listed, sdk-wasm **4 s** and sdk-js **0 s** on top of the four rows; the whole "Reproducible client builds" step 14 s (14:01:29 → 14:01:43 UTC; the four rows alone took 13 s in WP-P37).

## 3. RP51-2 — `make lint-wasm` (this PR)

| # | Finding | Sev | Fix | Proof | Mutation |
|---|---|---|---|---|---|
| 3a | The GOOS=js lint of `sdk/wasm` was manual: `make lint` skips the `js && wasm` build tag, and `go run` under `GOOS=js` builds the linter itself for js (`sdk/CLAUDE.md`) | note | `make lint-wasm`: the pinned linter (`$(GOLANGCI_LINT_VERSION)`, behind `lint-version-check`) built for the HOST into a temporary `GOBIN`, then `GOOS=js GOARCH=wasm <bin> run ./sdk/wasm/`. `ci.yml`'s build-and-test job runs it right after the Lint step | local: `0 issues.` (10.8 s, the linter build included); CI on this PR (run 40100813464, build-and-test): step `Lint (sdk/wasm, js/wasm)` success, `0 issues.`, 50 s (14:16:42 → 14:17:32 UTC, the linter build included) | a probe `x := 1; x = 2` added to `sdk/wasm/main.go`: `make lint-wasm` → `declared and not used: x (typecheck)`, `1 issues`, exit 2; `make lint` on the same tree → `0 issues.` |

## Prod proof

- **#940** `2e772d2e`: CI and Deploy green; the Deploy's client step as above.
- **#939** `a2dfce6b`: ejected once from the queue with `merge_conflict` (Session B's #938 had landed a bullet at the same spot of `internal/api/CLAUDE.md`), rebased keeping both bullets, the api package re-run green, re-armed; merged. CI and Deploy (run 40101589320) green.
- `/version` `a2dfce6b790b` (build 14:01:28Z). Active slot 8001 since 14:17:36 UTC: 131 journal lines, **0** `"level":"error"`; boot line `recovery vault: not configured — POST /api/v1/recovery/tokens answers 503 if recovery_vault is on`.
- `recovery_vault` still OFF (no `feature_flags` row for it or `translog_public`; `POST /api/v1/recovery/tokens` → **404**), `RECOVERY_*` unset (0 lines in `.env`).
- `account_deletion`: last run `ok` at 04:34:30 UTC today (the build before #939). **0 accounts are pending deletion on prod** (`users.status = 'pending_deletion'`: 0), so the next run (04:30 UTC tomorrow) erases nobody and exercises nothing new; the session did not trigger a prod run. With no vault configured, the first prod erasure will carry `realms_not_configured: true` and the run's note. The lab above is the proof of the realm path.

## Residuals (RP53-1…RP53-7)

| # | Sev | Residual |
|---|---|---|
| RP53-1 | P4 | An erasure while the vault is **not configured** asks no realm. A record minted while `RECOVERY_*` was set and later unset stays at its realm; the user id is in that `account.erased` row and the same subject key derives its subject, but no tool replays the Delete (the runner no longer sees the user). On prod nothing is owed: `RECOVERY_*` has never been set. Owed only if the vault is ever configured, used, then unset |
| RP53-2 | P3 | **A realm removed from `RECOVERY_REALMS_FILE` while it holds records** is never asked again: every record there stays, for every user erased after the removal. Rule until a tool exists: remove a realm only after it is wiped, or keep it listed (a listed realm that cannot be reached defers erasures, which is the honest failure) |
| RP53-3 | P4 | The token window is closed only up to the seconds between the runner's recent-token check and the row erase: a token minted in them can Register after the Delete, leaving a record under a subject no user has. Closing it needs the mint to read the user row under `FOR SHARE` in the transaction that writes its audit row, or a second realm pass a token life after the erase (an `erasure_settle`-style owed walk) |
| RP53-4 | P4 | The recent-token check reads the `recovery.tokens_issued` audit row, which `audit.Record` writes best effort (a failed insert is an Error line, the tokens are still served). A token whose row was not written is not seen by the check |
| RP53-5 | P4 | The unlisted-client warning lives in the Deploy log only (a `::warning::` annotation on a green run): no metric, no alert. Whoever cuts the next client release must still read it |
| RP53-6 | note | A realm's BoltStore erasure is logical (bbolt frees pages without zeroing them; `internal/recovery/CLAUDE.md`): an account erasure is as strong as the realm volume's encryption, like a NoGuesses erasure |
| RP53-7 | P4 (CI) | `TestStream_BudgetIsReturnedAfterClose` (`internal/parfetch`) failed #940's first CI run (`"0" is not positive`), passes 200/200 locally under `-race`: it reads `Budget.InUse()` right after the first `Next()`, before the prefetch of parts 1–5 has necessarily reserved — a timing assumption, unrelated to this session's changes; re-run green |

## Facts for Session B (CLAUDE.md rows)

- **`RECOVERY_SUBJECT_KEY`** (credential `recovery_subject_key` → `RECOVERY_SUBJECT_KEY_FILE` → the variable), **`RECOVERY_TOKEN_KEYS`** (credential `recovery_token_keys` → `_FILE` → the variable; JSON `{realm id: hex HS256 key ≥ 32 bytes}`), **`RECOVERY_REALMS_FILE`** (JSON array `{id, address, public_key, kid}`): all three or none — some set, or any unusable, refuses the boot. None = the minter is nil: `POST /api/v1/recovery/tokens` answers 503 when `recovery_vault` is on, and **an account erasure asks no realm** (`realms_not_configured: true` on `account.erased`, a note on the run). Set = the deletion runner's stage b3 sends every listed realm a Delete for the user's subject before the row erase; a realm that does not confirm, or a realm token minted within 10 min + 1 min, defers the tenant. Removing a realm from the file while it holds records leaves them there (RP53-2). Prod: unset.
- **`account.erased` audit fields:** `realms_deleted` (int: realms that confirmed the Delete; a realm holding nothing confirms too), `realms_deferred` (int: always 0 in this row — a deferral writes none; the deferred count is in the run's tenant entry and `job_runs` error), `realms_not_configured` (true, present only when no vault is configured). Also on `account.erasure_settling`.
- `make lint-wasm` — the pinned golangci-lint built for the host, run with `GOOS=js GOARCH=wasm` on `./sdk/wasm/`; in CI after Lint; 0 issues.
- `scripts/repro-check-clients.sh` also builds the checkout's clients and warns (never fails) when one is unlisted; `REPRO_CHECKOUT_CLIENTS` narrows the list (tests).
- Migrations: none. The next is still **108**.
