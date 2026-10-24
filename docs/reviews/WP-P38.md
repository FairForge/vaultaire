# Stage 1a — Prompt 38: Part 0 (the realm erasure, the client-build warning, `make lint-wasm`) and 43.4 (the law-enforcement policy and "what we can produce")

*2026-10-24. Split into two sessions. **Session A** ran Part 0; its findings are in
[`WP-P38-PART0.md`](WP-P38-PART0.md), in its own PRs. **Session B** ran Part A (43.4) and
this docs PR: #938, then this. Part 0 was **in flight** when this PR opened (below). **No migration** (the next is still **108**).*

Lane: Trust (43.4, Trust lane charter item 2). Contract rule 7 (the trust host is its own
surface), rule 8 (claim nothing we do not do: every sentence about what we hold, keep
or can hand over is a code fact under test).

## Step 0

- **#937** (the WP-P37 review's RP52-1/2) merged before Part A started; #938 was rebased on it (`8d17d96f`).
- `recovery_vault` OFF and `RECOVERY_*` unset on prod; `translog_public` OFF. Both expected.

## Part 0 — see `WP-P38-PART0.md`

**In flight when this note was written (2026-10-24 ~13:50 UTC).** Session A's PRs
had not merged:

- **#939** (RP52-3: an account erasure reaches the recovery realms — a Delete per realm
  of `RECOVERY_REALMS_FILE` on its own token; a realm that does not confirm defers the
  tenant): open, unarmed, DIRTY after #938 (both appended to `internal/api/CLAUDE.md`).
- **#940** (RP51-3: the client repro check builds the checkout's own clients and warns):
  armed, in CI.
- `make lint-wasm`: no PR seen yet.
- `WP-P38-PART0.md` is not on main yet. The link above resolves once Session A's PR
  lands it.

**Owed when #939 merges** (by Session A, or by the RP54 review): the CLAUDE.md
`RECOVERY_*` rows saying the deletion runner now deletes at the realms, read from #939's
merged diff, never from this note; STATUS's Part 0 line; the ledger row's Part 0 facts.

## Part A — 43.4: "what we can produce" and the law-enforcement policy (#938)

The probe ([`43.4-PROBE.md`](43.4-PROBE.md)) quotes the shapes we take from Signal
(the answer is a data inventory), Proton (jurisdiction first, user notice, overbroad
requests refused), Ente (the contrast: our SSE-C names and metadata are not encrypted)
and Apple's Legal Process Guidelines (per service: what exists, how long it is kept;
end-to-end data stated as a key fact). It also inventories every fact with its code
source, and lists what is not stated and why.

### What was built

- **One source.** `internal/api/landing/produce.json`, parsed and validated by
  `internal/produce` (the modes are `modes.json`'s ids in order; every row is complete;
  no forbidden word and no unqualified "cannot read", through `modes.ForbiddenWord` /
  `modes.UnqualifiedCannotRead`; unknown fields are refused). Mode names and sentences
  are never in it: the pages render `modes.json`.
- **Each row held to its code** (`internal/api/produce_test.go`):

| Row | Code fact | Test |
|---|---|---|
| 21 account rows (section + fields) | `exportSections`; fields = each section's select-list output names (`exportSectionFields`, `produce_fields_test.go`) | `TestProduceAccountRowsAreTheExportsSections` |
| 9 retention rows (table, days, delete/scrub, scrubbed columns) | `defaultRetentionPolicies()` | `TestProduceRetentionRowsAreTheRetentionJobs` |
| export files 7 d | `exportTTL` = `retentionExportsTTL` | same |
| `audit_logs` kept for the life of the service | in no policy | same |
| backups 7 d on the box / 30 d off it | `pg-backup.sh` `-mtime +7`, `OFFBOX_KEEP_DAYS=30` | `TestProduceBackupsAreTheBackupScripts` |
| every table and column named | the migrated schema | `TestProduceTablesAndColumnsExist` |
| an SSE-C object's row: name, size, ETag = the plaintext's MD5, content type, metadata, the key's MD5, times; the key in no column, the plaintext in no stored byte | a real SSE-C PUT | `TestProducePrivateSSECRowHoldsWhatItSays` |
| admin-read and server-build sentences | `translog_admin_reads`, `translog_public` (registered) | `TestProduceGatedSentencesNameRegisteredFlags`, `TestLegalLEPages_GatedSentencesFollowTheirFlags` |

- **The pages, dark.** `GET`/`HEAD` `/legal/law-enforcement` and
  `/legal/what-we-can-produce` use the existing legal layout (pixel style, no redesign),
  served by `dashboard.LegalLE` (`internal/dashboard/legal_le.go`). The same handlers
  serve the canonical origin (the dashboard's public group) and the trust host
  (`newTrustRouter`). Flag **`legal_le_pages`**: default OFF, `GlobalOnly`. OFF = a plain
  `no-store` 404 on every host (before, the canonical paths fell through to the S3 API).
  The pages override the `nav` block and a new `footer` block of `base.html` (its default
  is the old footer, byte for byte) so their links point at the canonical origin from
  the trust host.
- **The trust host's list stays closed.** It gains exactly the two pages and the six
  files their layout loads: `style.css`, `htmx.min.js`, `dashboard.js` and three fonts.
  `TestLegalLEAssets_AreWhatTheLayoutLoads` derives that list from `base.html`'s
  `{{asset}}` URLs and the stylesheet's `url()`s. The assets follow the flag too: while it
  is OFF the trust host is exactly what it was. `TestTrustHost_RouteListIsClosed` walks
  the trust router and lists all 24 method+path pairs.
- **The policy's procedure is counsel's.** Every procedural sentence renders as a marked
  **DRAFT** under a banner saying the policy is not in force (§ F-36 below).

### Findings

| # | Finding | Sev | Fix | RED test on main, and what it failed with | Mutation → caught by |
|---|---|---|---|---|---|
| RP53-10 | **modes.json's Private "what we hold" left out what an SSE-C row holds in the clear.** It said "names, sizes, counts, timestamps and when they are read". The row also holds the content type, the `x-amz-meta-*` metadata, the MD5 of the customer's key (`ssec_key_md5`), and, for a single-request upload, an ETag that is the MD5 of the plaintext (probe: `md5("hello plaintext")` = the stored ETag `db7f6a75…`). Every page that renders the sentence (the privacy table, the bucket settings card, the OpenAPI text, the produce page) said less than we hold. | P3 | Private's `server_holds` names them: "content types, the metadata you attach … with SSE-C, also the MD5 of your key and, for a file uploaded in one request, the MD5 of its contents (its ETag)" | `TestPrivateModeSentenceNamesWhatTheRowHolds`: `"…we still see object names, sizes, counts, timestamps and when they are read." does not contain "content types"` (and "metadata", "md5") | M12 (revert the clause) → that test |
| RP53-11 | **No preservation mechanism exists.** A preservation request (18 U.S.C. § 2703(f), Apple's § II.D) cannot stop the retention job; `legal_holds` has no writer (D-12). | P3 (policy) | Not built: the policy's Preservation line says so, marked "DRAFT (not yet possible)". Engineering item + F-36 | — (nothing to test until it exists) | — |
| RP53-12 | **An SSE-C object's ETag is the plaintext's MD5** (`sse_stream.go`: "the MD5 tee stays before it"). Anyone holding our database can confirm whether a known file is stored in a Private bucket; AWS's SSE-C ETag is not an MD5 of the data. | P3 (design) | **Stated, not changed** (an S3-compatibility decision: owner). The produce page and Private's sentence now say it | `TestProducePrivateSSECRowHoldsWhatItSays` pins the current behaviour, so a change has to update the page | M8 (an unproven column on the page) → that test |
| A1 | `/legal/law-enforcement` and `/legal/what-we-can-produce` did not exist: on the canonical origin they were the S3 API's | — | the pages, dark | `TestLegalLEPages_FlagOff404OnEveryHost`: `expected: 404 actual: 503` (`GET stored.cloud/legal/law-enforcement`; the S3 API with no database) | M15 (drop the canonical mount) → `…FlagOff404OnEveryHost`, `…FlagOnServedOnEveryHost`, `…GatedSentencesFollowTheirFlags` |
| A2 | `legal_le_pages` not registered | — | `registerFlags` + `GlobalOnly` | `TestProduceGatedSentencesNameRegisteredFlags`: `Should be true` (`legal_le_pages`); `TestLegalLEPagesFlag_DefaultOffGlobalOnly`: no `ErrGlobalOnly` | M11 (drop GlobalOnly) → `TestLegalLEPagesFlag_DefaultOffGlobalOnly` |

### Mutations (every one killed; scratchpad `mut.sh` on a detached copy of the branch)

| # | Mutation | Caught by |
|---|---|---|
| M1 | export section `user` → `account` | `TestProduceAccountRowsAreTheExportsSections` |
| M2 | drop `company` from the user section's SQL | `TestProduceAccountRowsAreTheExportsSections` |
| M3 | `retentionAccessLog` 30 → 14 days | `TestProduceRetentionRowsAreTheRetentionJobs` |
| M4 | `exportTTL` 7 → 3 days | `TestProduceRetentionRowsAreTheRetentionJobs` |
| M5 | `OFFBOX_KEEP_DAYS` 30 → 14 | `TestProduceBackupsAreTheBackupScripts` |
| M6 | mode id `standard` → `normal` (modes.json) | every produce test (`modes.json` refuses to load: a panic) |
| M7 | produce row `s3_access_log` → `s3_access_logs` | `…RetentionRows…`, `TestProduceTablesAndColumnsExist` |
| M8 | the SSE-C row names `encryption_key_version` (unproven) | `TestProducePrivateSSECRowHoldsWhatItSays` |
| M9 | the trust host's assets served with the flag OFF | `TestTrustHost_LegalAssetsFollowTheFlag` |
| M10 | one more trust route (`/legal/privacy`) | `TestTrustHost_RouteListIsClosed`, `…LegalAssetsFollowTheFlag` |
| M11 | `legal_le_pages` not `GlobalOnly` | `TestLegalLEPagesFlag_DefaultOffGlobalOnly` |
| M12 | Private's sentence without content types / metadata | `TestPrivateModeSentenceNamesWhatTheRowHolds` |
| M13 | gated sentences rendered whatever their flag | `TestLegalLEPages_GatedSentencesFollowTheirFlags` |
| M14 | the waitlist scrub drops `user_agent` | `TestProduceRetentionRowsAreTheRetentionJobs` |
| M15 | the canonical origin's mount removed | `…FlagOff404OnEveryHost`, `…FlagOnServedOnEveryHost`, `…GatedSentences…` |
| M16 | a font dropped from `LegalLEAssets` | `TestLegalLEAssets_AreWhatTheLayoutLoads` |

Gates: `-race` on `internal/api`, `internal/dashboard/...`, `internal/produce`,
`internal/modes`, `internal/flags` and `internal/database` (incl.
`TestSQLLiteralsMatchSchema`) green on `vaultaire_test_p38b`; `make lint` 0; gosec@latest
(the Security job's command) 0 issues on 506 files; `make landing` re-stamped
`landing.html` and `generated/house.html` (stamp only: modes.json and produce.json are
landing sources).

### Lab (private DB `vaultaire_lab_p38b`, the branch's binary, prod host settings)

`VAULTAIRE_PUBLIC_HOST=https://stored.cloud`, aliases `stored.ge,www.stored.ge`, port
8790, each request sent with a `Host:` header.

Flag OFF (default):

```
stored.cloud         /legal/law-enforcement       404 text/plain; charset=utf-8 cc=no-store
stored.cloud         /legal/what-we-can-produce   404 text/plain; charset=utf-8 cc=no-store
stored.ge            /legal/law-enforcement       404 text/plain; charset=utf-8 cc=no-store
trust.stored.cloud   /legal/law-enforcement       404 text/plain; charset=utf-8 cc=no-store
trust.stored.cloud   /legal/what-we-can-produce   404 text/plain; charset=utf-8 cc=no-store
trust.stored.cloud   /static/css/style.css        404 text/plain; charset=utf-8 cc=no-store
trust.stored.ge      /legal/what-we-can-produce   404 text/plain; charset=utf-8 cc=no-store
```

Flag ON (a `'*'` row in the lab's own database):

```
stored.cloud         /legal/law-enforcement       200 text/html; charset=utf-8 5219B
stored.cloud         /legal/what-we-can-produce   200 text/html; charset=utf-8 13631B
stored.ge            /legal/what-we-can-produce   200 text/html; charset=utf-8 13631B
trust.stored.cloud   /legal/law-enforcement       200 text/html; charset=utf-8 5219B
trust.stored.cloud   /legal/what-we-can-produce   200 text/html; charset=utf-8 13631B
trust.stored.cloud   /static/css/style.css        200 text/css; charset=utf-8 27942B
trust.stored.cloud   /static/fonts/silk400.woff2  200 font/woff2 3528B
trust.stored.cloud   /static/js/house.js          404 text/plain; charset=utf-8 143B
trust.stored.cloud   /legal/privacy               404 text/plain; charset=utf-8 143B
trust.stored.cloud   /                            404 text/plain; charset=utf-8 143B
(trust.stored.ge identical)
```

The pages from `stored.cloud` and `trust.stored.cloud` are byte-identical (`cmp`).
Headless Chrome, which resolved `trust.stored.cloud` to the lab, rendered both pages in
the pixel style with fonts and stylesheet loaded from the trust host itself. `make
dash-shots` renders handler fixtures only and does not cover the legal pages.

### Prod

#938 merged as `3d16ae43` (CI, Security, Deploy green on the merge commit). Prod `/version`
`3d16ae43e54a` (build 2026-10-24T13:27:03Z), active slot 8001 since 13:42:30 UTC. No
`legal_le_pages` row, so the flag is OFF by default.

Public, through Cloudflare:

```
stored.ge/legal/law-enforcement        404 text/plain; charset=utf-8 cc=no-store
stored.ge/legal/what-we-can-produce    404 text/plain; charset=utf-8 cc=no-store
stored.ge/legal/privacy                200 text/html  (unchanged)
stored.cloud/legal/law-enforcement     404 text/plain; charset=utf-8
stored.cloud/legal/what-we-can-produce 404 text/plain; charset=utf-8
```

At the origin (`127.0.0.1:8001` with a `Host:` header; trust DNS is still the owner's):

```
trust.stored.cloud   /legal/law-enforcement      404 text/plain cc=no-store
trust.stored.cloud   /legal/what-we-can-produce  404 text/plain cc=no-store
trust.stored.cloud   /static/css/style.css       404 text/plain cc=no-store   (the trust host as before)
trust.stored.cloud   /.well-known/security.txt   200
trust.stored.ge      (the same four answers)
stored.ge            /legal/…                    404 no-store; /static/css/style.css 200
```

Journal of `vaultaire@8001` since the switch: **0** `"level":"error"` lines of 145 (the
positive control).

## F-36 — the policy's procedural sentences (owner + counsel)

Each renders on `/legal/law-enforcement` as **DRAFT** and commits to nothing:

1. The entity and jurisdiction: FairForge LLC, State of Utah, United States; records disclosed only in answer to valid legal process served on FairForge LLC in the United States.
2. The named account only; a request for every customer's data, or for a key that protects every customer's data, is contested as overbroad.
3. Foreign requests through an MLAT or another process a US court recognises.
4. User notice before disclosure, its exceptions (a court order or law forbids it; risk to life or safety), and notice when a prohibition ends.
5. Emergency disclosure without process when US law permits (18 U.S.C. § 2702), notice afterwards.
6. Preservation: "not yet possible" (RP53-11). Counsel decides what we promise once a mechanism exists.
7. The contact: `legal@<canonical domain>`. The mailbox does not exist yet.

`legal_le_pages` stays OFF until counsel's words replace every DRAFT line.

## Owner items

Carried unchanged: the signing-key ceremony; `TRANSLOG_ANCHORS` / `TRANSLOG_WITNESSES`;
trust DNS + the origin certificate's SAN; the Rekor anchor key; F-33; RB-3; the witness
applications; the global translog flips. **New: F-36** (above), plus the RP53-12
decision (keep the plaintext-MD5 ETag for SSE-C, or not).

## Still open on purpose

- K4c-3c, the loader-side build refusal: no app serves the WASM yet. For the same reason
  the produce page states nothing about K2 spaces.
- K5's attested realm (41.4) and its second realm under different control (F-19). The
  recovery PIN is not stated while `recovery_vault` is OFF.
- RP52-4 and RP52-5 (notes in `docs/reviews/WP-P37.md` § Post-merge review).
- The yearly transparency report and "every production is a log entry" (43.4): not built.

## Residuals (Session B: RP53-10 onward; Session A: RP53-1…9; the review after both: RP54)

| # | Item | Sev |
|---|---|---|
| RP53-10 | fixed (above) | — |
| RP53-11 | a preservation mechanism: a per-tenant hold the retention job, the erasure and the collectors honour, logged | P3, engineering + F-36 |
| RP53-12 | the SSE-C ETag is the plaintext's MD5 (a known-file confirmation oracle) | P3, owner |
| RP53-13 | session records (24 h, `session_cleanup`) and `bandwidth_usage_daily` (the export reads 90 days; the table's own life is not a retention policy) are not on the produce page: no row until a code fact states their lives | P4 |
| RP53-14 | the privacy page's retention paragraph is still typed prose stating the same periods: render it from `produce.json` behind `modes_copy`, or test it against the same constants | P4 |
