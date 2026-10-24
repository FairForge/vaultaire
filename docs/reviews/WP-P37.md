# Stage 1a — Prompt 37: land K5 recovery (#894–#897) and the K2b tail (#906/#907), cut client release 0.2.0

*2026-10-24. Merged: #934 (the WP-P36 review's RP50-1, armed by this session), #894 → #895 → #896 → #897 (K5, one at a time through the queue), #906 + #907 (K2b), #935 (the 0.2.0 manifest), and this docs PR. **No migration** (the next is still **108**); neither stack carried one.*

Lane: Engine (K5's realm), Developer (K2), Trust (the client build entries). Contract line 1 (least access: Vaultaire never sees the PIN or the secret), line 7 (every client build the log promises is the one we ship), line 12 (an opaque subject, never an e-mail).

## Step 0

- **#934 (RP50-1) was green and CLEAN but not armed** at 09:30 UTC. Armed by this session; merged as `44f936e8`, Deploy green, `/version` `44f936e877cf` (build 09:33:25Z), 0 `"level":"error"` lines. Nothing else owed from RP50.
- `TRANSLOG_ANCHORS` still unset on prod (owner ceremony), anchors dark — expected.
- **No WP-P29 note exists**: Prompt 29 left the K5 stack unreviewed (only `docs/reviews/K5-PROBE.md`, which landed with #894). This note records what was verified at merge time; the review after this prompt (RP52) is the stack's first review.

## Part A — the K5 recovery stack

### Rebase

Bottom-up, each branch `rebase --onto` its freshly rebased base. Conflicts:

- **#895 `go.sum`** (main grew btcd/ots/mldsa since 948b86c2). Resolved by keeping main's `go.mod`/`go.sum` and adding only the branch's three requires (`filippo.io/edwards25519 v1.1.0`, `github.com/gtank/ristretto255 v0.2.0`, `go.etcd.io/bbolt v1.4.3`) with `go get` — the `go.mod` diff is byte-for-byte the branch's original. Main itself is not `go mod tidy`-clean (`go mod tidy` on main rewrites 26 lines of go.mod/go.sum); left alone — nothing in CI checks it (RP51-1).
- **#897 `internal/api/flags_wiring.go`, `server.go`**: both sides kept (main's translog flags/fields + the branch's `recovery_vault` / `recovery`).
- #895's old CI red `TestSiaWriteBudget_PacingIsNeverAStalledS3d` passes on the rebased tree (3/3, `-count=3`), as #893's margin fix predicted.

### Re-checked against what main grew

| # | Finding | Sev | Fix | Red test on the stack as rebased | Mutation → caught by |
|---|---|---|---|---|---|
| A1 | **The realm's enrolment was the pre-Prompt-30 ceremony shape.** `recovery-realm enrolment` printed `{ceremony, key_fingerprint, participants, statement}`; Prompt 30 Part 0b (RP37-10) closed the ceremony to enumerated fields with no free text, so the ceremony route refuses it (live in the lab: `400 invalid JSON body`) and the stack's own test no longer compiled (`unknown field Ceremony/Statement in translog.KeyCeremony`). Main had already reserved the realm's values (`action: realm_enrolled`, `key_role: recovery_realm_key`) | P2 (the documented way to enrol a realm could not work; build-breaking test) | `enrolment` returns a `translog.KeyCeremony` (typed — no drift possible): `realm_enrolled`, `recovery_realm_key`, `sha256:` of the Noise public key, `participants 1`; the realm id / operator move to the `reference`d page. `docs/TRANSPARENCY.md` § Recovery realms rewritten (re-key = `key_retired` + `realm_enrolled`) | `TestRecoveryRealm_EnrolmentIsAValidCeremonyEntry` (now decodes the body exactly as the route does — `DisallowUnknownFields` into `KeyCeremony` — then `translog.Encode`): `json: unknown field "ceremony"` | M1 action `recovery_realm` → red (the log's validation); M2 fingerprint over `pub[1:]` → red ("a reader hashes the pinned Noise key"); M4 role `master_key` → red |
| A2 | **`recovery_vault` accepted a tenant row that could open nothing.** The gate is read before authentication (`Enabled(flag, "")`, so the route is a 404 to everyone), so a tenant row was written, shown on /admin/flags and ignored — RP44-2's class, which main fixed for `translog_public` with `GlobalOnly` | P4 | `fl.GlobalOnly(flagRecoveryVault)`: the admin API answers `400 flag_global_only` (live in the lab), the dashboard the same refusal | `TestRecoveryTokens_DarkWhenFlagOff_404`: `Expected error with "flags: this flag has a global row only" in chain but got nil` | M3 `GlobalOnly` removed → red |
| A3 | The stack's tests wrote a `'*'` row of a real flag on the shared test database (`recoveryFixture.open`) — `internal/flags/CLAUDE.md` forbids it (parallel packages share ONE database) | note | global ON modelled by the fixture's own default (`f.fl.Register(flagRecoveryVault, true)`) | — | — |
| A3b | **The Security job's gosec (`@latest`) failed #895 on G407** — `EncryptSecret` seals under a zero nonce. It is the Juicebox wire format (the MIT golden vectors in `vectors_test.go` pin it), and the key is single-use: `EncryptionKey(seed, scalar)` is derived per registration from a fresh OPRF scalar and seals exactly one message. The rule postdates Prompt 29's green run | — (CI gate) | `// #nosec G407 -- <reason>` on the Seal, the repo's annotation style; construction unchanged | the CI gosec job on #895 | — |
| A4 | Route classification (K4c-1/34a): `POST /api/v1/recovery/tokens` is a tenant route, outside `/api/v1/admin/`; the walk's default branch is the right verdict. The stack adds no admin route | — | — | `TestAdminReadRoutes_AllClassified` green on the merged tree | — |
| A5 | Flag on the /admin/flags list: `Resolved()` lists every registered flag; the write path is the one flags API (`PUT /api/v1/admin/flags/recovery_vault`, proven in the lab). Not a `translog_*` flag, so no `log_policy` entry | — | — | — | — |
| A6 | The realm is never linked into the product: Prompt 29 built the guard as `TestRecoveryRealm_NotLinkedIntoVaultaire` (bans `internal/recovery/realm`, `cmd/recovery-realm`, `go.etcd.io/bbolt` from `go list -deps ./cmd/vaultaire`); re-run green. `cmd/vaultaire` links only `internal/recovery/secret`. `make deadcode` on the stack tip: 276 unreachable functions, the same list as main (the only difference is a line shift in `ots.go` from #934): the stack adds none | — | — | — | — |

The prompt's premise "per tenant first" for `recovery_vault` does not match the merged design: Prompt 29 made it **global only, checked before authentication** on purpose (a per-tenant gate needs the caller authenticated first, so the route would answer 401 to the world instead of 404). Kept, and enforced (A2) — D-P37-1. The lab therefore opened it with the global row in the lab database only.

### Lab (Part A)

Two `recovery-realm` processes built from the stack tip (BoltStore files on disk, Noise keys from files, tenants `stored:1` and `storedcloud:3` with different keys), `bin/vaultaire` on the private migrated DB `vaultaire_lab_p37` with `RECOVERY_SUBJECT_KEY_FILE`, `RECOVERY_TOKEN_KEYS_FILE`, `RECOVERY_REALMS_FILE` (boot: `recovery vault: token minter ready`, 0 error lines).

- Flag OFF: `POST /api/v1/recovery/tokens` 404 with and without a JWT. `PUT /api/v1/admin/flags/recovery_vault {"tenant_id": …}` → **400 `flag_global_only`**, still 404. Global row → 200.
- Minted: two tokens, `kid stored:1` / `storedcloud:3`, `iss` = the kid's tenant, `aud` = the realm id, `exp − nbf = 600`, the same opaque `sub` for both, the tenant id in no claim. Audit row `recovery.tokens_issued {"realms": 2}`.
- **The MIT SDK's own demo client** (`juicebox-sdk` `2e0f544e`, `rust/cli/demo`, built for this lab) with those tokens against the two processes, Noise NK end to end: register (2 guesses) → wrong PIN → `teyla21` → two wrong PINs → correct PIN refused (self-destructed) → register → `presso42` → delete; exit 0. Each realm: `register1/2 ok 2`, `recover1 ok 5`, `recover1 no_guesses 1`, `recover2 ok 5`, `recover3 ok 2`, `delete ok 1`, `records_destroyed_total 1`; 0 error lines.
- **Crash/restart, our Go client** (`client.HTTPTransport`, Noise NK over HTTP; a lab-only test, not committed): register with 3 guesses → **SIGKILL both realms**, restart → recover ✓ (BoltStore kept the record) → wrong PIN (2 remaining) → **SIGKILL both again**, restart → wrong PIN: **1 remaining** (the count survived the crash) → wrong PIN: 0 → correct PIN: `no guesses remain, the secret was destroyed`, and again.
- **Enrolment, live**: `recovery-realm enrolment` (realm 1) → the closed-list body; `sha256` of the key the token endpoint pins (`c9cdcc88…`) = its `key_fingerprint` `4316d679…`; POSTed with a fresh TOTP to `/api/v1/admin/translog/ceremony` → **201, entry idx 3**. The stack's original body → 400.

### Prod (Part A)

Merge commits #894 `c338968c`, #895 `85292f93`, #896 `f4d37d59`, #897 `be2aec57`; every Deploy green. `/version` `be2aec5794e6` (build 11:29:26Z).

- `POST https://stored.ge/api/v1/recovery/tokens`: **404** with no auth and with a bearer token. The flag is OFF and there are 0 `recovery_vault` rows in `feature_flags`.
- Boot line: `recovery vault: not configured — POST /api/v1/recovery/tokens answers 503 if recovery_vault is on`. No `RECOVERY_*` is set on prod, and no realm runs: the attested box (41.4) is not built yet.
- Journal: 1,083 lines in 20 min, **0** `"level":"error"`.


## Part B — the K2b tail and client release 0.2.0

#906's only unique commit (`dea5460e`; the four below it were squashed into #901/#903/#905) rebased onto main with no conflict; the `sdk/` tree is identical to the reviewed branch. On the rebased tree: `go test ./sdk/...` (non-short, `TestE2EE_WASMMatchesNative` ran in node 24.6: every construction both ways), `GOOS=js GOARCH=wasm` lint of `sdk/wasm` with the pinned v2.14.0 built for the host — **0 issues** (the Makefile has no such target; the prompt assumed one), and `vectors_check.py` (independent libsodium/OpenSSL recompute): **49/49 ok**.

### Release 0.2.0 (#935)

- Built at #906's merge commit `4a8aa618`, from two fresh `git archive` trees (own GOCACHE/GOPATH each), by that commit's own `scripts/build-client.sh`. **Byte-identical**: `sdk-wasm` `bc06f7a614c864f9eb1c0de690be55c997a66826ff891eb3e2eadc4b1fee9dcd` (4,438,502 B) and `sdk-js` `beea90d70611834c07aa00fca19be68ff14cbd713d30d164f7cbd68ec5b7b151` (3,137 B).
- #935 added the two 0.2.0 rows (`toolchain` go1.26.10 / `none`, recipe `scripts/build-client.sh`). The 0.1.0 rows stay.
- `scripts/repro-check-clients.sh` passed all four rows locally (26 s). Unlike 0.1.0, the 0.2.0 rows need no recipe fallback (D-P36-6 no longer applies).
- **Deploy gate on `80d7d669`:** all four rows `OK`, in **13 s** on the runner (the step ran 10:49:51 → 10:50:04 UTC; well under RP49-3's ~1 min per WASM row).
- GitHub release **`sdk-e2ee-v0.2.0`**:
  - Tagged at #935's merge `80d7d669`, the 0.1.0 convention (that tag is on #932's merge, the commit that listed it); the body names the source commit.
  - Assets `e2ee.wasm` / `index.mjs`; GitHub's digests are the manifest's.

### Lab (Part B)

`vaultaire_lab_p37`, the #935 build, a lab log key (`8a322c6e`), `translog_public` ON in the lab DB.

- At boot: `client build already in the log` for 0.1.0 (idx 0, 2), **appended** for 0.2.0 (idx 5 `sdk-wasm`, 6 `sdk-js`).
- The job signed size 7. Then:

```
$ translog-verify -client e2ee-0.2.0.wasm -client-name sdk-wasm
client sdk-wasm 0.2.0: sha256 bc06f7a6…9dcd listed at entry 5 — inclusion OK
$ translog-verify -client index.mjs -client-name sdk-js
client sdk-js 0.2.0: sha256 beea90d7…b151 listed at entry 6 — inclusion OK
$ translog-verify -client flipped.wasm          # bit 0 of byte 1000 XORed
FAIL: client build not in the log: no sdk-wasm entry lists sha256 78709c2a… — refuse this build   (exit 1)
```

My first "flipped" copy wrote `0x00` over a byte that already was `0x00`, and verify said "listed". A flip has to change the byte; `cmp` now checks that every time.

### Prod (Part B)

- `80d7d669` deployed.
- `translog_entries` `client_build`:
  - idx 34 `sdk-wasm` 0.1.0, 36 `sdk-js` 0.1.0 (unchanged);
  - **41 `sdk-wasm` 0.2.0 `bc06f7a6…`** and **42 `sdk-js` 0.2.0 `beea90d7…`**, both `4a8aa618`, at 10:51:03Z.
- Dedup kept 0.1.0's rows: no second entry.
- Journal: 1,502 lines in 30 min, 0 `"level":"error"`, 2 `client build appended`.

### A guard withdrawn

I wrote `TestClientBuildManifest_ListsTheClientsThisTreeBuilds`: the current `sdk/js` file and the `sdk/wasm` build must be listed. It was red on main with exactly the two new hashes. It was **not committed**: a row's `source_commit` must be the merge commit, which no sdk PR can know before it merges, so the test would fail every future sdk change. RP51-3 records the gap.

K4c-3c (the loader-side refusal) stays open: #906 adds no surface that serves the WASM (it touches `sdk/` only) — Prompt 38.

## Departures

- **D-P37-1** `recovery_vault` stays global-only (above).
- **D-P37-2** the realm's enrolment carries no realm id or operator; the `reference` names the page that does (the log's no-free-text rule).
- **D-P37-3** `go.mod`/`go.sum` merged by `go get`, not `go mod tidy` (main is not tidy; no churn in a recovery PR).
- **D-P37-4** #907's STATUS line was dropped in its rebase (main's text kept, the prompt's rule); Prompt 31's closeout is folded into this prompt's STATUS entry. Its plan K2 row was grafted onto main's text, and its F-34/F-35 rows added.
- **D-P37-5** `sdk-e2ee-v0.2.0` is tagged at the manifest PR's merge (`80d7d669`), not at the source commit, matching 0.1.0.
- **D-P37-6** #895's zero-nonce `#nosec G407` (A3b): the wire format is the contract, so the construction is unchanged.

## Residuals (RP51-n)

| # | Sev | What |
|---|---|---|
| RP51-1 | note | main is not `go mod tidy`-clean (tidy rewrites 26 lines of go.mod/go.sum); nothing in CI checks it |
| RP51-2 | note | the GOOS=js lint of `sdk/wasm` is a manual step (no Makefile target, not in CI) |
| RP51-3 | P4 | Nothing flags main shipping unlisted client bytes between an sdk merge and its manifest row (the trap of this prompt; caught here by the prompt's text, not by a check). A CI test cannot do it (above); a non-failing warning in the Deploy step's `repro-check-clients.sh` (build the checkout's clients, warn when unlisted) would |
| RP51-4 | note | K4c-3c (the loader-side refusal) still open: #906 added no surface that serves the WASM |
| RP51-5 | note | The merged PR bodies of #896/#897 still describe the realm enrolment as `{ceremony, statement}` (written in Prompt 29); this note and `docs/TRANSPARENCY.md` § Recovery realms are the current word |
| RP51-6 | note | The local worktree `vaultaire-k5` (Prompt 29's) still holds the pre-rebase `feat/k5-recovery-tokens` branch; housekeeping only |
| RP51-7 | note | `recovery_vault` ON globally opens the minter for every tenant at once (by design, D-P37-1); a staged roll-out would need the gate moved after authentication and a 404-for-unauthenticated rule written down first |

## Post-merge review (2026-10-24, RP52)

The first review of the K5 stack (no WP-P29 note ever existed), plus an adversarial pass on this prompt's own fixes. Every "fixed" row was checked against the merged tree by test name: `TestRecoveryRealm_EnrolmentIsAValidCeremonyEntry`, `TestRecoveryTokens_DarkWhenFlagOff_404` (with `flags.ErrGlobalOnly`), `TestRecoveryRealm_NotLinkedIntoVaultaire` and `TestE2EE_WASMMatchesNative` are all present. `GlobalOnly(flagRecoveryVault)`, the `#nosec G407` and `realm_enrolled` are in the merged code, and both 0.2.0 rows are in the manifest. CI and Deploy are green on every merge commit.

Checked and holding:
- **The realm state machine.** The Recover2 guess is committed before the answer exists. Recover3 cannot be guessed without spending a guess (a 128-bit tag). The last guess is usable, `AttemptedGuesses` cannot overflow (it is bounded before the increment), and NoGuesses erases.
- **Realm tokens.** HS256 is pinned, there is a single `aud`, `iss` = the kid's tenant, `exp − nbf` ≤ 24 h and `scope` = `user`.
- **Sessions.** Keyed by (tenant, subject, id), so another user's session cannot be used. A request that needs forward secrecy never rides a handshake.
- **The token endpoint's session path.** It uses the same `sessionStore.Get` as the dashboard's `RequireSession`, and a session exists only after the MFA step.

| # | Sev | Finding | Fix | Red on main (`7f3a9ce2`) | Mutation → caught by |
|---|---|---|---|---|---|
| RP52-1 | **P3, proven** | **A deleted recovery record could come back.** Both stores counted revisions per record, and a Delete removed the key, so a re-registration started again at revision 1. A Recover2 that loaded the record before a concurrent Delete + Register2 then matched the new revision. Its compare-and-swap wrote the *deleted* record — its encrypted secret and key shares — over the new registration. A user who deletes recovery because they fear compromise, while another token holder has a Recover2 in flight, keeps the old secret. Only a token holder for the subject can trigger it | Revisions come from one store-wide counter that is never reused (bbolt's bucket `NextSequence`; a counter in `MemoryStore`). A file written before starts its sequence past the largest revision on disk. The `Store` contract now states the invariant | `TestRecovery_StaleWriteAfterDeleteAndReregister_Conflicts` (memory + bolt): `Expected error … record changed concurrently … but got nil`, and the version read back was the deleted record's `{0x1}`, not the new `{0x2}`; `TestBoltStore_SequenceStartsPastRevisionsOnDisk` | M1 seeding off → SequenceStarts red; M2 bolt `rev+1` → both red; M3 memory `rev+1` → StaleWrite red |
| RP52-2 | **P3** | **One user could fill the realm's session table.** The table was capped only in total (`maxSessions` 100,000). One signed-in user holding one realm token (it lives 10 minutes and can be reused) could open 100,000 handshakes under distinct session ids. Every other user then got `Unavailable` until those sessions idled out (5 min), and could be locked out again at once | `maxSessionsPerSubject` = 8 (the SDK keeps one session per realm). A subject's new session evicts that subject's least recently used one, so one user can only crowd out themselves. The `bySubject` index is kept in step by one `deleteLocked` | `TestRecoveryRealm_SessionsBoundedPerSubject`: `"1000" is not less than or equal to "8"` | M4 bound off → red; M5 evict the newest → red (the test was tightened after M5 first survived: it now requires exactly the 8 most recent ids to remain) |
| RP52-3 | P3 (gate) | **An account erasure never reaches the realms.** The deletion runner and `account.EraseRows` do not know about them, so an erased user's records (key shares + encrypted secret, keyed by the HMAC subject) would stay at every realm for ever. Dark today: no realm runs and `RECOVERY_*` is unset | → Prompt 38: before `recovery_vault` can be turned on, the runner sends a `Delete` to every configured realm for the user's subject (it can mint its own token). A failure defers the tenant, like the sweep | — | — |
| RP52-4 | note | A handshake runs its riding request (Recover1 / Register1 / Delete) before `putSession`. When the table is full, the work is done and the answer is lost. No guess can be lost this way (Recover2/3 need forward secrecy) | — | — | — |
| RP52-5 | note | The token limiter (10 per user per hour) lives in the process. During a `vaultaire-switch` both slots briefly serve, so a user can get up to 20 in the overlap hour | — | — | — |

The K2b WASM surface (#906) was reviewed by Prompt 31 (M1–M35) and its sdk tree is identical to that review's. This review only confirmed the cross-test and vectors at merge.
