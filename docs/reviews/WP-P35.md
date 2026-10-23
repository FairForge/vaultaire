# Stage 1a — Prompt 35: Part 0 (the WP-P34 review's RP46) + K4c-3a — checkpoints anchored in OpenTimestamps

*2026-10-23. PRs: #927 (Part 0), #928 (K4c-3a, **migration 106** `translog_anchors`), #929 (the Rekor probe note + `translog-verify -anchors`, stacked on #928), and this docs PR. The next migration is **107**. No `sdk/`, `internal/recovery/`, `cmd/recovery-realm/` or `internal/api/recovery*` file was touched.*

*This note was written by the Prompt 36 session from the Prompt 35 session's working notes, its PR bodies (#927–#929) and its lab database: that session ended after #929 was opened and before its docs PR existed (the WP-P35 review's RP48-4 found the same gap for the WP-P34 review). Everything below is sourced from those records; nothing is reconstructed from memory.*

Lane: Trust. This prompt serves `docs/ARCHITECTURE_CONTRACT.md` rule 7 ("the log has signed checkpoints and an external witness" — an anchor makes the log's history time-stamped outside us, so a rewritten log cannot be back-dated) and rule 8 ("claim nothing we do not do": nothing of ours verifies a proof against Bitcoin — that is `ots verify`'s job, and we say so; prod has no signing key, so nothing is signed or anchored there; Rekor is not built, and the tool and `TRANSLOG_ANCHORS` say so).

## Step 0 (the Prompt 35 session, ~18:30 UTC 2026-10-23)

- `/version` `7efa38bbed2e` (build 17:48:12Z), active slot 8001; 0 `"level":"error"` lines in 2 h.
- `translog_entries` 30: 26 `server_build` (max idx 29), 2 `admin_read` (23, 27), 2 `log_policy` (25, 26); 0 checkpoints; `translog_cosignatures` 0 rows; `translog_anchors` absent (migration 106 not applied yet); the `translog_checkpoint` job `ok`, `no signing key`, size 30.
- Flags: `translog_admin_reads` + `translog_customer_view` ON for tenant zero only, no global row. `TRANSLOG_WITNESSES` / `TRANSLOG_ANCHORS` not in `.env`.
- All 16 rule files on the box matched `deploy/monitoring/` hash for hash (`vaultaire-translog.yml` `bf955269…`).

## The specs, quoted where relied on

- **The OpenTimestamps calendar protocol**, as `github.com/nbd-wtf/opentimestamps` and the upstream `opentimestamps-client` implement it: `POST <calendar>/digest` with the raw digest answers a timestamp ending in a *pending attestation* naming the calendar (its promise to put the aggregate in a Bitcoin block); `GET <calendar>/timestamp/<commitment hex>` answers 404 while pending, then the path from that commitment to a *Bitcoin block header attestation*. The `.ots` file, the operations, the commitment computed at a pending attestation and the serialisation are the library's (D-P35-1); nothing of ours writes that format by hand.
- **Rekor v2 (rekor-tiles v2.3.0)**: `hashedrekord` only ("Rekor v2 will not support a DSSE entry type"), and its `verifySignature` is `LoadVerifierWithOpts(…, options.WithED25519ph())` + `WithDigest` — always Ed25519ph over the submitted digest, never pure Ed25519 over an artefact. Quoted at length in `docs/reviews/K4c-3-REKOR-PROBE.md` (#929).
- c2sp.org/tlog-checkpoint / signed-note: what is anchored is SHA-256 of the checkpoint note exactly as the log signed it — the body and the log's signature line, **no cosignature lines** (`logstore.NoteDigest`; `translog.LogNote` recovers it from a served checkpoint).

## Part 0 (#927, `c5a90bdc`) — the WP-P34 review's RP46-1…4

| # | Sev | Fix | Red on `aaf33d14` (main) | Mutation → caught by |
|---|---|---|---|---|
| RP46-1 | P3 | `translog-witness serve` answers a request for the checkpoint it already holds from the existing cosignature — no signature, no write — while that cosignature is younger than `-refresh-floor` (default 1 h, in (0, 6 h), below the log's 6 h refresh); past the floor it re-signs (the log's refresh) with no history line. The lock is taken only after the log's signature verifies | `TestWitnessServe_ReplayedCheckpoint_NoNewWrites` alone on main: history **expected 1, actual 101**; 100 signatures for 100 replays; state, cosigned and history all changed | P1 never reuse → Replayed + PastTheFloor; P2 a history line on every signature → `_ReplayPastTheRefreshFloor_ResignsWithoutAHistoryLine`; P4 serve ignores the floor → PastTheFloor |
| RP46-2 | P4 | `accept` writes **state → `cosigned` → history**: a crash can leave only the state ahead, which is safe (409 names N, only the same root is cosigned). Seam `afterWrite` | `TestWitness_CrashAfterCosign_ForkAtSameSizeRefused` on main's order + the seam: a fork B at 9 from 5 → **200** (want 409 "9"), from 9 → **200** (want 422): the witness cosigned two roots at 9 | P3 `cosigned` before the state → Crash |
| RP46-3 | P4 | The access page claims the public log only while it is published (`translog_public` ON globally and a trust URL): unpublished it says "In our transparency log (not published yet)" and links nowhere | `TestAccessPage_Unpublished_NoPublicClaim`: does not compile on main; main's body carried "in the public log" + the checkpoint link with nothing published | A1 ignore `published` → Unpublished; A2 unpublished words say "public log" → Unpublished; A3 the template always links → Unpublished; A4 the router never published → Route |
| RP46-4 | P4 | "entry N, included from checkpoint M", the link at the newest served checkpoint, plus the `translog-verify … -entry N` line, published only; words in `adminread.Where`, one table | `TestAccessPage_Published_LinksTheCheckpoint`: does not compile on main | A5 "at" instead of "included from" → ListsOnly + Published |

All 9 mutations turned their tests red (P1 first as a non-compiling deletion, re-run compile-safe). Side effect found by the full suite: `TestCheckpointJob_RefreshesAnIdleCosignature` had moved only the job's clock, so the real-clock witness answered the same line — the cosign lab's witness now runs on the test's clock.

## K4c-3a (#928, `65de8a76`) — checkpoints anchored in OpenTimestamps

- **Migration 106 `translog_anchors`** (`checkpoint_size → translog_checkpoints`, `kind ∈ {ots, rekor}`, `service`, `state ∈ {pending, complete}`, `proof ≤ 64 KiB`, 32-byte `digest`, `received_at`): unique on (size, kind, service, state); append-only by migration 103's trigger function; infrastructure — never erased, never exported (`TestTranslogTablesAreInfrastructure`).
- **`internal/translog/ots`**: the format is the library's (D-P35-1); ours are the HTTP (no redirects, a 64 KiB cap, status → unreachable / refused / bad_answer), the rule that a pending attestation is completed only by asking the calendar it names when that is the configured one, and a `recover` around the library (it panics on operations it does not implement).
- **The `translog_anchor` job** (every 10 min, boot +3 min, 5 min ceiling; its own job, never inside `translog_checkpoint`): submits unanchored signed checkpoints newest first (≤ 8 per calendar per run), then asks pending proofs older than 30 min to complete (≤ 16 per run). An upgrade is a **new row** `complete`; the pending row stays. A calendar that is down never fails the job: `job_runs.result.anchors`, the run's note, a Warn line, `vaultaire_translog_anchor_total{kind,service,outcome}`.
- **`TRANSLOG_ANCHORS`**: `ots <calendar URL>` separated by `;`/newlines; https only; at least 2 calendars (one calendar is one point of trust), at most 8; all or none; a `rekor` line refused naming D-P35-2; unset = one Info line and a no-op job.
- **Read side:** `GET /translog/anchor/<size>.ots` on the trust host behind `translog_public` — every calendar's proof combined, `application/vnd.opentimestamps.v1`, `no-store`; 404 for a size never signed, no anchor, a row of another digest, a signed request, or the flag OFF.
- **Metrics and the rule:** `vaultaire_translog_anchor_total` pre-created per configured service at boot (the D-P34b-4 pattern), `vaultaire_translog_anchors_configured{kind}`; on scrape `vaultaire_translog_anchored_size{kind}`, `_anchor_timestamp_seconds{kind}`, `_anchor_pending{kind}`. `TranslogAnchorStale` (warn, 2 h + 10 m); `translog_anchor` in `PeriodicJobStale`.

| Test (red on `aaf33d14`: none compiles; `translog_anchors` absent from main's schema) | Mutation → caught |
|---|---|
| `TestAnchorJob_SubmitsEachSignedCheckpointOnce` | O1 `Unanchored` ignores the service; O5 the digest is not the note's |
| `TestAnchorJob_ServiceDown_JobOK_Counted` | O2 a calendar down fails the job |
| `TestAnchorJob_OTSUpgrade_NewRowNeverUpdate` | O3 the upgrade is not a new row; O4 a completed proof asked again |
| `TestTranslogAnchors_AppendOnly` (logstore + 106's checks) | — |
| `TestParseTranslogAnchors_AllOrNone` | O7 one calendar accepted; O8 rekor accepted; O12 http accepted |
| `TestTrustHost_ServesOTSProofOnlyWhenPublic` | O6 the route ungated; O9 a row of another digest served |
| `TestOTS_UpgradeAsksOnlyTheCalendarItNames`, `_LibraryPanicIsABadAnswer`, `_CalendarFailuresAreClassified`, `_SubmitUpgradeCombine`, `_RealCalendarsProof` (golden: the three real calendars' answers from the lab) | O10 an upgrade asks any URL; O11 no `recover` |
| `TestAnchorJob_Unset_NoOp`, `TestTranslogRules_ParseAndNameExportedSeries` (6 rules) | — |

All 12 mutations turned their tests red (O6, O9 first non-compiling, re-run compile-safe).

**Cost:** `cmd/vaultaire` linux/amd64 grew 51,644,611 → 53,426,543 B (+1.78 MB, +3.4 %): `nbd-wtf/opentimestamps` v0.4.0 pulls btcd (bumped 0.23.4 → 0.24.2 for CVE-2024-38365 / CVE-2024-34478), btcec, btcutil, chainhash, decred blake256/secp256k1.

## #929 — the Rekor probe (D-P35-2) and `translog-verify -anchors`

- **Rekor stopped at the probe note** (`docs/reviews/K4c-3-REKOR-PROBE.md`). The probe ran rekor-tiles v2.3.0's own `hashedrekord.ToLogEntry` offline against a real lab checkpoint and its signature: `PKIX_ED25519` **panics** ("crypto: Size of unknown hash function"; rekor-tiles `main` `a7ec779`, 2026-10-23, now refuses it in words: "hashedrekord only permits signing algorithms that prehash the data"); `PKIX_ED25519_PH` is **refused** ("ed25519: invalid signature") — the log's key signs pure Ed25519 over the note text and nothing else. Anchoring in Rekor needs a **dedicated anchor key** (an owner ceremony; ECDSA P-256 or Ed25519ph, published like the log's verifier key). Migration 106 already admits `kind = 'rekor'`. Nothing was sent to any Rekor instance.
- **`translog-verify -anchors`**: fetches `anchor/<size>.ots` for the checkpoint it just verified, checks the proof is of SHA-256 of the note as the log signed it (`translog.LogNote`; a proof of another digest fails the run, the state does not move), keeps it as `checkpoint-<N>.ots` beside `-state`, prints each attestation's state and the `ots verify -d <digest> <file>` command, and says "anchor rekor: not built". A 404 is "none served yet", not a failure. Red test `TestVerify_Anchors_PrintsStateAndChecksTheDigest` (does not compile on #928); mutations R1 `LogNote` keeps cosignature lines, R2 no digest check, R3 a 404 an error — all 3 caught.
- #929 went DIRTY after #928's squash (the stacked-PR rule); the Prompt 36 session rebased it with `git rebase --onto origin/main 44d2b92a stage1a-p35-rekor` (one docs conflict in `internal/translog/CLAUDE.md`: main's RP46-2 witness write order kept, #929's `Client.Anchor`/`LogNote` rows grafted in), force-pushed with lease and re-armed the queue.

## Lab (private DB `vaultaire_p35lab`, :18035, lab log key `ed69882b`, `translog_public` ON, built under `GOTOOLCHAIN=go1.26.10`)

`TRANSLOG_ANCHORS` = the three real public calendars (alice, bob, finney) + `https://127.0.0.1:9` (down). Boot: 4 `translog anchor` Info lines.

1. `server_build` idx 0 + 2 lab ceremonies → checkpoint 3 signed 20:17:18Z. The anchor run: `ok`, note `anchor ots https://127.0.0.1:9: unreachable (… connection refused)`; alice/bob/finney each `submitted 1, outcome ok`. 3 pending rows (proofs 307/270/256 B), all of digest `19668629f6804000310c6443fbceb7e9c9ce36df12888c9d9cf30097d6b44df5` = `sha256(note)`.
2. `GET /translog/anchor/3.ots` → 200 `application/vnd.opentimestamps.v1`; `ots info` (opentimestamps-client v0.7.2): the same file hash, 3 `PendingAttestation`s (bob, finney, alice).
3. `translog-verify -anchors` (#929's build): 3 pending lines, the kept `checkpoint-3.ots`, the `ots verify -d 19668629…` command printed, "anchor rekor: not built", entry 2 included OK. Upstream `ots --no-bitcoin verify -d 19668629… checkpoint-3.ots`: "Pending confirmation in Bitcoin blockchain" from each calendar; a wrong digest → "Digest provided does not match digest in timestamp".
4. `/metrics`: `anchor_pending{ots} 3`, `anchored_size{ots} 3`, `anchors_configured{ots} 4`, `anchor_total` ok=1 ×3 + unreachable=1, 20 series pre-created; 0 error lines.
5. **Pending → complete, observed** (the Prompt 36 session, 23:24 UTC the same day, ~3 h after submission): `ots upgrade checkpoint-3.ots` (the upstream client, against the same three calendars) answered "Got 1 attestation(s)" from each and "Success! Timestamp complete" — the digest reached **Bitcoin block 972833** (merkle root `1f63a590…`, alice's path) and **972835** (`f24c1784…`, bob's path). `ots --no-bitcoin verify -d 19668629… ` names both blocks to check manually. The `translog_anchor` job's own upgrade of the stored pending rows (the new-complete-row path against a real calendar) is proven in `docs/reviews/WP-P36.md`'s lab (RP47-1).

**promtool on the box** (/tmp): `vaultaire-translog_test.yml` **SUCCESS** (6 rules; TranslogAnchorStale silent at 110 m, firing at 135 m, silent idle, silent unconfigured), `vaultaire-jobs_test.yml` **SUCCESS** (9 rules). Hashes at install: `vaultaire-translog.yml` `e8df784a…`, `vaultaire-translog_test.yml` `b07a3dcc…`, `vaultaire-jobs.yml` `46c31602…`, `vaultaire-jobs_test.yml` `aaf1a0d6…`.

## Prod proof

- #927 merged as `c5a90bdc` (Deploy run 40022371231), #928 as `65de8a76` (Deploy run 40030880018), both through the merge queue, both `/version`-checked by the Deploy job.
- `TRANSLOG_ANCHORS` stays unset — a no-op proven: the boot Info line `translog anchors: none — signed checkpoints are not anchored in OpenTimestamps`; the `translog_anchor` job `ok` with result `{"note": "no anchors configured (TRANSLOG_ANCHORS unset)"}` and no `anchors` key; migration 106 applied with both triggers; 0 error lines; the rules installed after promtool SUCCESS.
- No signing key on prod (owner ceremony): nothing is signed, so nothing can be anchored; `translog_anchors` has 0 rows by design.

## Departures (D-P35-n)

- **D-P35-1.** **None of the OpenTimestamps format is ours.** The `.ots` file, the operations, the commitment and the serialisation are `github.com/nbd-wtf/opentimestamps`'s; ours are the HTTP, the only-the-named-calendar rule and a `recover` around the library. If the library cannot do something we need, that is a new departure first, never hand-written format code.
- **D-P35-2.** **Rekor is not built; the probe note is the deliverable.** Rekor v2 verifies Ed25519ph over the submitted digest; the log's key signs checkpoints only, and re-using it under a second signature domain is exactly what a signing key must never do. The way forward is a dedicated anchor key — an owner ceremony — whose signatures over the note's digest go to Rekor; `translog_anchors` already admits `kind = 'rekor'`, and `parseTranslogAnchors` refuses a `rekor` line naming this departure.

## Residuals (RP47-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP47-1 | note | The `translog_anchor` job's own pending → complete upgrade (the new-complete-row path against a real calendar) was not yet observed when #928 merged: the lab's three proofs were pending, and only the upstream client's upgrade was seen (block 972833/972835). Proven in WP-P36's lab | closed by WP-P36 |
| RP47-2 | note | `cmd/vaultaire` +1.78 MB (+3.4 %): btcd/btcec/decred via `nbd-wtf/opentimestamps`. btcd rides dependabot from 0.24.2 on | cost |
| RP47-3 | note | Only the newest checkpoint is served, so `translog-verify -anchors` checks an anchor of an older checkpoint only against a note the reader kept in `-state` | verify |
| RP47-4 | note | A pending proof completes only if the calendar keeps the commitment; nothing yet re-submits one the calendar lost, and nothing bounds how long a proof may stay pending | fixed by WP-P36 Part 0 (RP48-2) |
| RP47-5 | note | The two-calendar floor is configuration only: nothing refuses a calendar whose pending attestations name another URL (an aggregator pool), and the stale rule fires only when every calendar stops | fixed by WP-P36 Part 0 (RP48-1) |

**Carried:** RP45a-1…6; RP45b-1…6; RP43-2, RP43-3, RP43-5, RP43-7…9, RP43-11/12; RP44-5; RP41-1; RP41-3 (in part, RP45b-1); RP38-2…4, RP38-7…9; RP34-2/-5/-8/-10/-12; RP35-2…5; RP37-16; RP39-* (Prompt 31's); RP36-* (Prompt 29's). RP46-5 (the WP-P34 review): its text was never written down durably — see WP-P36 § Carried.

## Owner items (note only)

- **The signing-key ceremony**, the verifier key published outside our infrastructure and on a `stored.cloud` page (`docs/TRANSPARENCY.md` § The signing key).
- **After the ceremony:** `TRANSLOG_ANCHORS` set to the three **calendars** — `https://alice.btc.calendar.opentimestamps.org`, `https://bob.btc.calendar.opentimestamps.org`, `https://finney.calendar.eternitywall.com` — **never the aggregator pools** (`a.pool.opentimestamps.org` hands out attestations naming alice/bob); `TRANSLOG_WITNESSES`; the witness on the Mac.
- **The dedicated Rekor anchor key** (D-P35-2): a ceremony, ECDSA P-256 or Ed25519ph, published like the log's verifier key.
- `trust.stored.cloud` DNS (proxied), the origin certificate's SAN, `TLS_CERT_PROBE_TARGETS` (RP41-2).
- Third-party witness applications (Witness Network, Glasklar — `docs/reviews/WP-P34b.md` § Owner items).
- F-33; RB-3 (`BLOMP_ALLOW_UNSEALED` off before customers).
- `translog_admin_reads` / `translog_customer_view` global ON per `docs/TRANSPARENCY.md` § Before the global flip (`translog_customer_view` only after `translog_public`).
