# WP-STRIPE-10 — Stage 1a Prompt 58: WP-VAULT-2 Part 2, the blob scrub (Part 0 = RP94)

*Plan § WP-STRIPE, the **WP-VAULT-2** row's second half. Nothing read a hot blob whole: routing_truth reads the first 4 KiB of a sample, the parity scrub reads the Vault's second copy, the tape audit asks the tape. The `blob_scrub` job reads every hot blob — head rows with bytes, retained versions, ready chunks — WHOLE on its recorded backend once per `SCRUB_CYCLE`, checks it against its row, writes what is not ok to `scrub_findings` (migration 125), repairs from a ready mirror copy, and lets the bucket page say "Every file here was read back and checked on <date>". The page a base layer sells is `docs/DURABILITY.md`, every number held to the code by a test.*

## The PRs

| PR | What | Tests (red first) |
|---|---|---|
| #1144 Part 0 (1/3), the Worker 0.7.1, deployed (`1017f1c8`) | RP94-2: `fileStream` scans the file bytes for `\r\n--<boundary>` across chunk edges and gives the last held bytes only after the closing delimiter is proven (so a `FixedLengthStream` short of its count aborts the backend PUT on any refusal); the client gets the form's own 400; `describe()` names `post-tail`. **Plus a live bug the prod baseline found:** every form 0.7.0 handed to the origin (declined, unreadable, `post_tail_unproven`) was re-streamed with no length and answered **411 MissingContentLength** — now through a `FixedLengthStream` of the client's Content-Length | `post.test.js`: "a part after the file errors the stream before its last bytes", "a wrong tail errors before the last bytes under a FixedLengthStream double", "the host names post-tail", "a form handed to the origin keeps its Content-Length" |
| #1146 the client_build row of 0.7.1 + the edge proof | `edge-ingest 0.7.1` at the merge commit | `repro-check-clients` |
| #1145 Part 0 (2/3) | RP94-3 the edge's CORS headers (commit, refusal, GET admit); RP94-4 lock/checksum/redirect fields applied, other `x-amz-*` 501, a checksum form at the edge is the origin's; RP94-5 a refused form counts against its key; RP94-6 `/oauth/register` paces only new clients | `TestEdge_AnswersCarryTheBucketsCORSHeaders`, `TestPostPolicy_LockAndChecksumFieldsAreApplied`, `TestPostPolicy_EdgePassesAChecksumFormToTheOrigin`, `TestPostPolicy_ARefusedFormCountsAgainstItsKey`, `TestOAuth_ElevenIdenticalRegistrationsInAMinuteAreOneClient` |
| #1147 (its row #1150; the Worker 0.7.2 deployed `09063a2d`) Part 0 (3/3), the P4s (+ the Worker 0.7.2) | RP94-7 … 16 (RP94-13's delete half not built — below) | `TestPostPolicy_AnUnknownKeyIsInvalidAccessKeyId`, `_AWSChunkedContentEncodingIsRefused`, `_DataPlaneSLCLeavesFormsToTheHandler`, `_EdgeRefusesFieldsPastTheOriginsBound`, `TestEdge_DataPlaneBytesHelpNamesForms`, `TestInvitations_FamilyInvitationsDoNotFillTheMemberLimit`, `TestFamily_RevokeOfASponsoredMemberLapsesTheRowTouchesNoQuota`, `TestEntitlementLapse_DriftSkipsASponsoredMember`, `TestAdminPayments_InAFamilyIs409`, "a filename* form goes to the origin whole" |
| #1148 PR A, the scrub (migration 125) | `internal/api/scrub.go`, `scrub_findings`, `vaultaire-scrub.yml` | `TestScrub_ReadsEveryKindOnce`, `_VerifiesTheRowsDigest`, `_RepairsFromTheMirror`, `_NeverSpendsTheVendorsEgressAllowance`, `_CursorSurvivesARestart`, `_FindingsErasedWithTheAccount`, `_MetricsAreRegistered`, `TestBlobScrubCycleFromEnv`; promtool `vaultaire-scrub_test.yml` |
| #1149 PR B, the verified line | `handlers.BucketVerifiedLine`, `landing/durability.json` | `TestDashboard_BucketSaysVerifiedAfterACleanCycle` |
| this PR (C) | `docs/DURABILITY.md` + `TestDurabilityPage_ClaimsAreTheCodes`, this note, STATUS, the plan row, CLAUDE.md, Prompt 59 in both editions | |

## Step 0 answers

- **Is RP94 on main?** Yes (`764d77bb`, #1143) — the gate held.
- **What does a whole read cost per backend?** The footprint × Every ÷ Cycle bytes per 10-minute run (A1). Tenant zero's ~106 GB over a 30-day cycle is ≈ 25 MB a run per backend share; over the proof's 1-day cycle ≈ 0.74 GB a run.
- **Is the vendor allowance ever passed?** No: a backend's slice is bounded by `egressFreeBytes` less max(the vendor's month-to-date, this job's + the blob copies' month). A metered account with no capacity run reads nothing.
- **What is unverifiable, and how much of prod?** A multipart ETag with no `body_md5` (objects completed before Prompt 14), a sealed whole object with no `backend_etag`, every sealed retained version (no column records its stored MD5). On prod, tenant zero (the one tenant in the scrub): **none** — 0 of its 14,262 head rows with bytes are a multipart ETag without `body_md5` or sealed, and its 22 retained versions are unsealed (3 objects are chunked, verified chunk by chunk).

## Decisions (D-SCRUB-n)

- **D-SCRUB-1 — `unverifiable` is counted, never written.** A row per multipart object of every cycle would grow `scrub_findings` by the tenant's multipart count each month and say nothing wrong. The blob is still read, so `unreadable`/`missing` are still found.
- **D-SCRUB-2 — a chunk is walked once, by its index row; its finding is written per referencing object** (`tenant_chunk_refs`, ≤ 1000 rows a chunk), so every finding names a tenant, a bucket and a key (the bucket page, the erasure). A chunk no object references writes nothing (counted).
- **D-SCRUB-3 — the walk per backend is whole → version → chunk, in key order; the cursor is in `job_runs.result`; a finished cycle waits until its start + the cycle.** A run reads at least one blob; a blob with no answer (a deadline) is no verdict — the slice stops, the blob is read again next run.
- **D-SCRUB-4 — the vendor budget is the full free multiple less real egress** (the prompt's rule), not the blob copies' (multiple − 1): the customers' own reads are in the vendor's month-to-date.
- **D-SCRUB-5 — tape is not scrubbed** (`geyser` is skipped; a tape object read answers `ErrArchived`, no verdict); the vault parity is no repair source here (a vault-floor blob is on tape — the parity scrub and the tape audit read it).
- **D-SCRUB-6 — a non-ok verdict re-reads its row first:** a write or delete that moved it while it was read is `changed`, no finding.
- **D-SCRUB-7 — repair streams the copy through a reader that fails WITH the last byte when its MD5 or size is not the copy row's,** so the backend write never completes with wrong bytes (no buffer, no temp file); the same address is rewritten (an address is never reused for other bytes).
- **D-SCRUB-8 — the bucket line claims only what is true of every byte:** the tenant in the scrub since before the counted cycles began (`feature_flags.updated_at`), every backend with scrubbed data past its first cycle, nothing found in the bucket left without a later `repaired`. The date is the earliest finished cycle's (UTC).
- **D-SCRUB-9 — `ScrubCycleStale` compares with `scalar(vaultaire_scrub_cycle_seconds)`;** the promtool test samples every minute (a 10-minute series is stale at half the 1-minute evaluations and resets `for`).
- **D-SCRUB-10 — the Worker's pass-through Content-Length fix went into 0.7.1** with RP94-2 (one release, one deploy): it was found by this prompt's prod baseline and is the path every form takes until 0.7.1.

## Residuals (RB16-n)

- **RB16-1** — a form's synthetic answers at the edge (the Worker's own 400 for a malformed form, its 503 SlowDown) carry no CORS headers (RP94-3 covers the relayed answers).
- **RB16-2** — a SigV4 PUT at the edge carrying a plain `x-amz-checksum-*` header (not an aws-chunked trailer) is admitted with no checksum signed — pre-existing; a form naming one is now the origin's (RP94-4).
- **RB16-3** — the PUT path never applies `x-amz-object-lock-legal-hold` (pre-existing): a form naming it is 501 rather than silently accepted.
- **RB16-4** — (found and FIXED inside PR A, before it merged) the chunk walk is by `backend_id`, so a chunk still at a legacy (uploader-prefix) address read as `missing` at the one address; the scrub now reads such a chunk where it is (`legacyChunk`, routing truth's `legacy` rule; test `TestScrub_ALegacyChunkIsReadWhereItIs`). Counted here because the six named tests did not catch it.
- **RB16-6** — prod runs `SCRUB_CYCLE=1d` for the proof (every ~101 GB of tenant zero's, read once a day from `lyve`, unmetered); after the first clean cycle, set it to `30d` (the design's default) with a `.env` backup and `vaultaire-switch restart`.
- **RB16-5** — PR B's helper was written before its test (TDD order slipped; the test exercises every branch).

## Open questions

- **RP94-13's delete half** cannot be built as written: migration 120's `idx_tenant_invitations_one_pending` allows one pending invitation per (tenant, address) of ANY kind, so `AND kind = …` on the two DELETEs makes the second invitation a 23505 → 500. Building it needs the index to name the kind — a migration (the next free number) and the owner's word on whether one address may hold a member and a family invitation at once. The count half is built.
- **A client_build row "in the same PR"** cannot be: the row names the MERGE commit (`scripts/repro-check-clients.sh`'s header); it lands in the follow-up PR, as #1139 → #1141.
- __PROOF_OPEN__

## The prod proof

__PROOF__
