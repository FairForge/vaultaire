# Stage 1a — Prompt 36: Part 0 (the WP-P35 review's RP48) + K4c-3b — client build entries

*2026-10-24. PRs: #931 (Part 0, **migration 107**), #932 (K4c-3b, no migration), and this docs PR. The next migration is **108**. No `sdk/`, `internal/recovery/`, `cmd/recovery-realm/` or `internal/api/recovery*` file was touched (K4c-3b builds `sdk/wasm` from source and reads `sdk/js/e2ee`; the loader-side refusal is K4c-3c, not here). Before Part 0 started, this session finished Prompt 35: #929 was DIRTY after #928's squash and was rebased (`git rebase --onto origin/main 44d2b92a stage1a-p35-rekor`, one docs conflict in `internal/translog/CLAUDE.md` resolved by keeping main's RP46-2 witness order and grafting #929's Client rows), force-pushed with lease and re-armed; Prompt 35's docs PR did not exist (the session ended first) and was written by this session from its records (#930, `docs/reviews/WP-P35.md` says so).*

Lane: Trust. Contract rule 7 ("every access-widening act in the transparency log" — the anchors must hold in behaviour, and a client build hash is the log's first promise to a person running our code) and rule 8 ("claim nothing we do not do": "at least two calendars" was configuration only, and a proof that could never complete sat as if it would).

## Step 0 (23:55 UTC 2026-10-23, after #929's deploy)

- `/version` `bc004600576e` (build 23:32:32Z); **0** `"level":"error"` lines in 2 h.
- `translog_entries` **33**: 29 `server_build` (max idx 32), 2 `admin_read` (23, 27), 2 `log_policy` (25, 26); `translog_anchors` **0 rows** (expected: no signing key); the `translog_anchor` job `ok` with `{"note": "no anchors configured (TRANSLOG_ANCHORS unset)"}`.
- Flags: `translog_admin_reads` + `translog_customer_view` ON for tenant zero only.
- Rule hashes vs `deploy/monitoring/`: 15 of 16 match. **`vaultaire-translog.yml` on the box is `bf955269…` — the FIVE-rule P34b file.** Prompt 35 promtool-tested its six-rule file in /tmp (`e8df784a…`, its notes) but never installed it, so `TranslogAnchorStale` was never live. Part 0 replaces that rule anyway; the seven-rule file was installed in this prompt's prod step (below).

## Part 0 (#931) — the WP-P35 review's RP48

| # | Sev | Fix | Red on `65de8a76`/`bc004600` (main) | Mutation → caught by |
|---|---|---|---|---|
| RP48-1a | P3 | `ots.Submit` (and every `Upgrade` tail) refuses an answer any of whose pending attestations names another URL after `TrimSuffix("/")`: `ErrBadAnswer` naming both URLs, nothing stored, `bad_answer` counted. A pool (`a.pool.opentimestamps.org`) hands out attestations naming alice/bob, which `Upgrade` would never ask — an anchor that could never complete | `TestAnchorJob_PoolAnswer_RefusedNothingStored`: on main the pool row was stored, outcome `ok`, `bad_answer` 0. `TestOTS_SubmitRefusesAPoolsAnswer` | M1 the check dropped → PoolAnswer red |
| RP48-1b | P3 | A per-service view on scrape, bounded to the configured services (≤ 8; `AnchorStateCollector` ∩ `ConfiguredAnchors()`): `vaultaire_translog_anchored_size_by_service{kind,service}`, `_anchor_timestamp_by_service_seconds`, `_anchor_oldest_pending_seconds`. **`TranslogAnchorServiceStale` replaces `TranslogAnchorStale`** (D-P36-1): one dead calendar of two fires by itself; none configured = no series = silent | `TestAnchorStateCollector_PerServiceView`: the families did not exist (0 series). `TestTranslogRules_ParseAndNameExportedSeries` (7 rules) | M2 the collector exports the table's rows instead of the configured services → PerServiceView red |
| RP48-2 | P3 | The upgrade pass walks a keyset cursor over pending AND resubmitted rows and wraps (`PendingAnchorsFrom`): every waiting row is asked within a bounded number of runs. The cursor lives in the job's own `job_runs.result` and in process (D-P36-3). A pending row past **48 h** with no complete row is **re-submitted once** to the same calendar: the fresh proof is a `resubmitted` row — migration 107 widens the state CHECK, and 106's unique index admits exactly one re-submission per (checkpoint, calendar) while staying the previous build's ON CONFLICT target (D-P36-2). `vaultaire_translog_anchor_oldest_pending_seconds` + `TranslogAnchorNotCompleting` (warn past the horizon) | `TestAnchorJob_LostCommitmentsNeverStarveNewerProofs`: on main 16 lost rows starved the 17th for ever (three runs, `upgraded=0`, no complete row). `TestAnchorJob_PendingPastHorizon_ResubmittedOnce`: does not compile on main | M3 the cursor ignored (main's ORDER BY LIMIT) → LostCommitments red; M4 the no-resubmission check dropped → ResubmittedOnce red |
| RP48-3 | P4 | `Anchors` returns EVERY row of (size, kind); `AnchorOTS` serves them all combined (`ots.Combine` dedups; `ots verify` judges each path): a wrong complete answer no longer replaces the same calendar's good pending path | `TestAnchorOTS_ServesPendingBesideComplete`: on main only the bogus complete path was served | M5 DISTINCT ON complete-first restored → ServesPendingBeside red |
| RP48-4 | note | § Carried below quotes RP46-1…4 and points at #927; RP46-5 is in the residual table | — | — |
| **Found in this prompt's lab** | **P2** | **The upgrade of a REAL calendar's pending proof never worked**: `Upgrade` parsed the calendar's answer through `fileOf`, whose file header carries exactly 32 digest bytes — but the commitment at a real pending attestation is the computed message, prepend/append-wrapped by the calendar (alice's is 44 bytes). Every real upgrade was `bad_answer: a commitment of 44 bytes, not a SHA-256`; #928's tests only ever met otstest's 32-byte commitments, and the P35 lab stopped at pending. Fixed: the answer (a timestamp with NO file header) is parsed structurally through the library's parser under a throwaway 32-byte digest, which is no part of the parsed sequences (`tailSequences`); the format code stays the library's (D-P35-1 held) | `TestOTS_UpgradeOfARealShapedCommitment` (otstest `RealShape`: the commitment wrapped like the real calendars') — on the pre-fix branch: `bad_answer: a commitment of 44 bytes, not a SHA-256`, the exact live failure | the red run IS the mutation (the fix reverted = the red) |

Also: a checkpoint completed in a run is not asked again through its other row (a pending and a resubmitted proof complete to the ONE row); promtool tests rewritten (one of two calendars silent → fires for that service alone; both anchored → silent; none configured → silent; pending past the horizon → NotCompleting fires, younger stays silent).

**Migration 107** is additive and idempotent: one DO block drops and re-adds the state CHECK (`'pending','complete','resubmitted'`); the previous build never writes `resubmitted` and its reads filter on pending/complete; applied twice to the private DB with the same result.

## § Carried — the WP-P34 review (RP48-4)

The WP-P34 review's findings RP46-1…5 were never written to a note (they lived in #927's body alone). As #927 records them: **RP46-1 (P3)** — `translog-witness serve` re-signed a replayed checkpoint on every request: 100 replays cost 100 signatures and 100 history lines; fixed with `-refresh-floor` (1 h), the existing cosignature answered, a re-sign past the floor adds no history line. **RP46-2 (P4)** — the witness wrote `cosigned` before its state, so a crash between them could cosign two roots at one size; fixed: state → cosigned → history. **RP46-3 (P4, rule 8)** — the customer's access page said "in the public log" and linked the checkpoint while nothing was published; fixed: the claim and the link only while `translog_public` is ON globally and a trust URL exists. **RP46-4** — "entry N, included from checkpoint M" with the `translog-verify` line, published only. **RP46-5** — a note-only finding whose text was lost with the unwritten docs PR; it is carried as lost in the residual table.

## K4c-3b (#932) — client build entries

The probe first (`docs/reviews/K4c-3b-PROBE.md`): two fresh copies of `bc004600`, own GOCACHE/GOPATH each, `GOTOOLCHAIN=go1.26.10 CGO_ENABLED=0 GOOS=js GOARCH=wasm go build -trimpath -buildvcs=false -ldflags=-s` → **byte-identical**: `07685b5f6dba6a5419a9c64ef3d524839f71a1452ef79ee21193628163dcc911`. What would change the bytes (toolchain, embedded paths, ldflags; `wasm_exec.js` is the toolchain's shim, not the artefact) is in the probe.

| Item | What was built | Red on `bc004600` | Mutation → caught by |
|---|---|---|---|
| The entry | `client_build` (`entries.go`): `client` from the CLOSED list `sdk-wasm`/`sdk-js`, `version`, `source_commit` (40 hex), `sha256` as published, `toolchain` (or `none` for a file published as-is, D-P36-5), `recipe` (a `scripts/*.sh` path); one entry per (client, sha256) by dedup key; covered by `TestTranslog_EntriesNeverNameATenant` | `TestClientBuildEntry_ClosedListAndShape`: does not compile | C1 the closed-list check dropped → red |
| The manifest | `internal/translog/clientbuilds/manifest.json`, embedded, reviewed in its PR; `Load` refuses unknown fields, bad shapes, duplicates. First rows: `sdk-wasm` 0.1.0 (`07685b5f…`) and `sdk-js` 0.1.0 (`4766be3d…`, the source file as published), both at `bc004600` | `TestClientBuildManifest_Valid`, `_RefusesFreeTextAndDuplicates` | — |
| Who appends | the server at boot, like `server_build` (`internal/api/translog_clients.go`): every row not yet in the log, in the background; a row the frozen log refuses stays pending and every `translog_checkpoint` run retries it. **No new credential reaches CI or the box** | `TestClientBuildEntry_AppendedOncePerHash`, `TestClientBuild_FrozenLogRetriedNotLost`: do not compile | C2 `DedupKey` empty (every boot a new entry) → red; C5 a refused row dropped → red |
| The Deploy gate | `scripts/repro-check-clients.sh` in deploy.yml's build job: every manifest row rebuilt from its OWN source commit (fresh tree, own GOCACHE/GOPATH, the row's recipe `scripts/build-client.sh`), the deploy FAILS on a different hash. The first release's commit predates the recipe script, so the gate then uses the checkout's reviewed copy on the commit's sources (D-P36-6) | `TestClientBuildManifest_ReproCheckFailsOnADifferentHash`: fails on main (no script); doctors the cheap `sdk-js` row, so CI's run builds no WASM | C3 the script compares nothing → red |
| The verifier side | `translog-verify -client <file> [-client-name sdk-wasm]`: hashes the file, finds its `client_build` entry among what the verified checkpoint commits to (bundles authenticated by `TileHashReader`), checks the inclusion proof, prints "listed at entry N" — or FAILS "not in the log … refuse this build". The operator-side refusal; the loader-side one is K4c-3c | `TestVerify_Client_ListedAndUnlisted`: does not compile | C4 any `client_build` entry matches whatever the hash → red |

All 10 Part 0 + K4c-3b mutations turned their tests red. `make lint` 0 issues on both branches; gosec per the CI Security job on each PR.

## Lab

**Part 0 / RP47-1, the first pending → complete observed through the JOB, against the real calendars.** The Prompt 35 lab database (`vaultaire_p35lab`) still held its 3 pending rows of checkpoint 3 (submitted 20:17 UTC, digest `19668629…6b44df5`), by then committed to Bitcoin (the upstream client proved blocks 972833/972835 at 23:24 UTC — recorded in WP-P35.md). Migration 107 applied; the Part 0 build booted on it (0 error lines, 4 anchor Info lines).

- The first run — still on the PRE-fix binary — is what found the P2 above: every upgrade `bad_answer: … a commitment of 44 bytes, not a SHA-256`, plus one real artefact of the run: bob's first GET timed out at 15 s (`unreachable`, retried next run — the designed path).
- On the fixed binary (boot 00:07:48 UTC, job at +3 min): **every calendar `outcome ok, upgraded 1`** — checkpoint 3's three pending proofs completed into NEW `complete` rows (1216/1216/1275 B) at 00:10:50, the pending rows kept beside them, each service's `cursor` in the result. `GET /translog/anchor/3.ots` → 200, 3,743 B, **six paths**: `BitcoinBlockHeaderAttestation(972833)` ×1 + `(972835)` ×2 AND the three pending paths beside them (RP48-3 live). `ots --no-bitcoin verify -d 19668629… 3-complete.ots`: "check that Bitcoin block 972833 has merkleroot 1f63a590…" / "972835 … f24c1784…" — the digest is in two Bitcoin blocks, through the job's own rows. **RP47-1 closed.**
- The per-service gauges on `/metrics`: one series per CONFIGURED service — alice/bob/finney `anchored_size_by_service 5` with fresh timestamps, the down `https://127.0.0.1:9` at zeros (exactly what `TranslogAnchorServiceStale` fires on), `oldest_pending_seconds` ≈ 417 (checkpoint 4/5's young rows), nothing for any unconfigured service.
- The down calendar `https://127.0.0.1:9` stayed `unreachable` on every run and never failed a run.

**K4c-3b** (private DB `vaultaire_p36lab`, lab log key `7794b422`, `translog_public` ON): the boot appended `client_build` idx 0 (`sdk-wasm`) and idx 2 (`sdk-js`) beside its `server_build` (idx 1), 0 error lines; the job signed checkpoint 3.

```
$ translog-verify -url http://localhost:18036/translog -vkey @log.vkey -client e2ee.wasm -client-name sdk-wasm
checkpoint: stored.cloud/translog/v1 size 3 root 02c73763…45e110 — signature OK
client sdk-wasm 0.1.0: sha256 07685b5f…3dcc911 listed at entry 0 — inclusion OK
  source commit bc004600576eaeebc96acdd8bb85a9530febb34d, toolchain go1.26.10, recipe scripts/build-client.sh
$ # one flipped byte:
FAIL: client build not in the log: no client_build entry lists sha256 07403b09… in the 3 entries the checkpoint commits to — refuse this build   (exit 1)
```

`sdk-js` the same ("listed at entry 2 — inclusion OK"). The repro gate ran clean on the Mac against the real manifest (both rows OK) and red against a doctored row (`REFUSED`, exit 1).

## Prod proof

- #931 merged as `cc5f3645` (Deploy run 40043951409, success), #932 as `1c826108` (Deploy run 40045154494, success — its build job ran **"Reproducible client builds": success**, the manifest's two rows rebuilt from `bc004600` on the runner and matched).
- `/version` `1c826108bcaa` (build 00:16:26Z); **0** `"level":"error"` lines across both deploys.
- **Two `client_build` entries appended at the new build's boot** (00:32:30Z): idx **34** — `{"client":"sdk-wasm","version":"0.1.0","source_commit":"bc004600…b34d","sha256":"07685b5f…3dcc911","toolchain":"go1.26.10","recipe":"scripts/build-client.sh"}` — and idx **36** (`sdk-js`, `4766be3d…f9d7d0`, `toolchain none`); two `client build appended` Info lines, nothing appended twice (the dedup key). The log: 37 entries — 31 `server_build` (max 35), 2 `client_build`, 2 `admin_read`, 2 `log_policy`.
- **No signing key on prod** (owner ceremony): the entries are in the log, no checkpoint commits to them yet, nothing is signed and nothing can be anchored — `translog_anchors` 0 rows, the `translog_anchor` job `ok` `{"note": "no anchors configured (TRANSLOG_ANCHORS unset)"}` (RP49-5).
- **Migration 107 applied**: `translog_anchors_state_check` reads `('pending','complete','resubmitted')`.
- **Rules installed**: `promtool test rules` on the box **SUCCESS** (7 rules), `install -m 0644` into `/etc/prometheus/rules/`, `systemctl reload prometheus`; Prometheus serves `TranslogAnchorServiceStale` and `TranslogAnchorNotCompleting`; sha256 `46546ee4a7b84cca…` = the repo's. (Step 0 found the box still on P34b's five-rule `bf955269…`: Prompt 35's six-rule file was promtool-tested in /tmp and never installed, so `TranslogAnchorStale` was never live — WP-P35.md's prod section is corrected in this docs PR, and the install above supersedes it.)
- **The release**: `sdk-e2ee-v0.1.0` on the repository — `e2ee.wasm` (`07685b5f…`) and `index.mjs` (`4766be3d…`), the entries' exact hashes (D-P36-7).

## Departures (D-P36-n)

- **D-P36-1.** **`TranslogAnchorStale` is replaced, not kept beside `TranslogAnchorServiceStale`.** The per-service rule fires for every dead service, so the all-services case is covered per service; two rules would warn twice for one fact. The by-service series exist only for configured services, which is what keeps "none configured = silent" without a `_configured` conjunct.
- **D-P36-2.** **The re-submission is a new state `resubmitted`, not an attempt column in the unique key.** Migration 107 only widens the state CHECK: the previous build's `INSERT … ON CONFLICT (checkpoint_size, kind, service, state)` infers migration 106's unique index, which an attempt column in the key would have had to drop — breaking the old build between migrate and switch. The unique index then admits exactly one re-submission per (checkpoint, calendar): "once" is schema, not bookkeeping.
- **D-P36-3.** **The upgrade cursor lives in process and in `job_runs.result`** (the vault_parity reconcile pattern): read back once per process from the job's own last result, carried in each service's outcome (`cursor`), so a deploy resumes where the old slot stopped and a lab run needs no `job_runs` row.
- **D-P36-4.** **An `Upgrade` tail is held to the same names-only rule as `Submit`** (not asked by the review): a tail whose pending attestation names another URL would re-create exactly the RP48-1 dead end one hop later.
- **D-P36-5.** **`toolchain: "none"`** is the manifest's word for an artefact published as-is (`sdk-js` is its own source file); recording a Go version for a byte copy would claim a build that never ran (rule 8).
- **D-P36-6.** **The first release's source commit predates the recipe script**, so `repro-check-clients.sh` uses the checkout's reviewed copy of the recipe on that commit's sources and says so; every later release's commit carries the script and is rebuilt entirely from itself.
- **D-P36-7.** **The artefacts are published as a GitHub release** (`sdk-e2ee-v0.1.0`: `e2ee.wasm`, `index.mjs`) — readable with no credential of ours, and rebuildable from the entry alone (commit + recipe + toolchain).

## Residuals (RP49-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP49-1 | note | The upgrade asks every waiting row within ⌈N/16⌉ runs, but a calendar with many lost rows still spends its whole budget on them between wraps; the re-submission (48 h) is what actually drains them | anchors |
| RP49-2 | note | `tailSequences` accepts a tail that carries MORE paths than the one asked (the library's parser is lenient; each must still end in an attestation and name only the configured calendar) | ots |
| RP49-3 | note | The repro gate builds the manifest's rows serially in the Deploy job (~1 min per WASM row); a manifest of many releases will want a prune-or-parallelise decision | deploy |
| RP49-4 | note | `translog-verify -client` scans every entry bundle (fine at today's sizes; a log of millions wants an index or a sidecar map) | verify |
| RP49-5 | note | The `client_build` entries are appended with no signing key on prod: they are in the log and nothing is signed, so a verifier cannot check them against prod until the owner's ceremony | owner |
| RP49-6 | note | RP46-5's text is lost (the WP-P34 review was never written down; #927 says only that it was a note for the docs PR). Reviews now land in the worker's note the same day (RP48-4's rule) | process |

**Carried:** RP47-2/3 (RP47-1 closed by this lab, RP47-4/5 fixed by Part 0); RP45a-1…6; RP45b-1…6; RP43-2, RP43-3, RP43-5, RP43-7…9, RP43-11/12; RP44-5; RP41-1; RP41-3 (in part); RP38-2…4, RP38-7…9; RP34-2/-5/-8/-10/-12; RP35-2…5; RP37-16; RP39-* (Prompt 31's); RP36-* (Prompt 29's); RP46-5 (lost, above).

## Owner items (note only)

- The signing-key ceremony; the verifier key published outside our infrastructure and on a `stored.cloud` page.
- After the ceremony: `TRANSLOG_ANCHORS` = the three calendars (never the pools); `TRANSLOG_WITNESSES`; the witness on the Mac.
- The dedicated Rekor anchor key (D-P35-2).
- `trust.stored.cloud` DNS (proxied), the origin certificate's SAN, `TLS_CERT_PROBE_TARGETS` (RP41-2).
- Third-party witness applications (Witness Network, Glasklar).
- F-33; RB-3 (`BLOMP_ALLOW_UNSEALED` off before customers).
- `translog_admin_reads` / `translog_customer_view` global ON per `docs/TRANSPARENCY.md` § Before the global flip.
