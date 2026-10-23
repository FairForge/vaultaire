# Stage 1a — Prompt 32: Part 0 (RP40-1, RP38-1) + K4b, the witness side of the transparency log

*2026-10-23. PRs: #910 (Part 0), #911 (the witness), #912 (the restore), #913 (the trust host), and this docs PR. **No migration.** **Next migration: 104** (`ls internal/database/migrations | tail -1` = `103_translog.sql` at the start, and still at the end). No `sdk/`, `internal/recovery` or `cmd/recovery-realm` file was touched: K5 (#894–#897) and K2b (#905–#907) are other sessions' stacks.*

Lane: Trust (Engine-lane library). It serves `docs/ARCHITECTURE_CONTRACT.md` rule 7 ("the log has signed checkpoints and an external witness") and rule 11 (`trust.` on stored.cloud), and `docs/surfaces/TRUST.md` item 3.

Three sentences carry this prompt:
- **A second machine holds every byte the log ever signed and says so with its own signature** (#911).
- **A database restore no longer has to cost the log its origin** (#912). The lost entries go back byte for byte, or nothing is written.
- **The log lives at trust.stored.cloud, and a customer bucket named `translog` is a bucket again** (#913).

**Not in this prompt (K4c):** OpenTimestamps anchoring, the Rekor mirror, admin-read entries, client build hashes, and the fTPM quote in `server_build`. The ledger's line for Prompt 32 named OpenTimestamps, Rekor and admin-read entries; the prompt as issued moved them to K4c.

## Step 0

- At the start, main was `6bf724fa` (#905), deployed, and the latest migration was `103_translog.sql`. #909 (the WP-P30 post-merge review, docs) was queued.
- Prod `translog_entries` = 16, all `server_build`, with no checkpoint, no `translog_public` row and no signing key. `vaultaire_payment_renewal_stops_pending` was 0.

## Part 0 (#910) — `158868c4`, deployed with #911 as `eef950a3`

| Item | Sev | Fix | Red test on origin/main `6bf724fa` | Mutation → caught by |
|---|---|---|---|---|
| RP40-1 | P4 | `stopOne`: a pending row whose subscription the entitlement renews by again (`auto`, the same provider and ref) is **withdrawn** in the same transaction, under the same lock: `payments.WithdrawRenewalStop`, which is the un-cancel's own write done late. It is audited `billing.renewal_stop_withdrawn` (actor `renewal-stops`), gets one Info line and is counted in `SkippedWithdrawn`. A row withdrawn or closed meanwhile keeps the plain skip | `TestStopPendingRenewals_RenewsAgain_WithdrawsTheStop`: `a stop the house no longer owes is withdrawn, not left pending for ever`; `SkippedWithdrawn` was 1 again on the second pass | the withdraw call removed (the skip back) → the same test |
| RP38-1 | P4 | `BackendHealthState.Probed`, set by `UpdateHealth`. Until it is set the collector exports **no** `vaultaire_backend_health` sample and no `_probe_latency_seconds` sample; `_probe_failures_total` exports 0. `/health` is untouched (RP9-9) | `TestBackendHealth_NoSampleBeforeFirstProbe`: `vaultaire_backend_health{backend="idrive"} 1` at registration | the `continue` removed → the same test |

`TestBackendCollector_NilEngineStillExportsHealth` now probes once before it scrapes; its intent is unchanged. `promtool test rules vaultaire-backends_test.yml` on the box: **SUCCESS**. That file only changed a comment, and its rules match `== 0`, never an absent series.

## The witness (#911) — `eef950a3`

`internal/translog/witness` and `cmd/tools/translog-witness`. The library additions in `internal/translog/client.go` are:
- `Client.Tiles`: a cache of full tiles, fed by tlog's own `SaveTiles` hook and authenticated again on every read;
- `Client.EntryBundle`: every entry's leaf hash proven against the checkpoint.

A pass:
1. checks the signature;
2. checks consistency with the accepted checkpoint, through a **read-only** view of the cache;
3. mirrors every full entry bundle and full level-0 tile, plus the partial bundle at the checkpoint's size, under `<dir>/cache/` in the tlog-tiles layout;
4. co-signs (both signature lines) into `<dir>/cosigned`, and appends a line to `cosigned.history` for each NEW note;
5. writes the state last.

| Test | What it proves |
|---|---|
| `TestWitness_CosignedNoteCarriesBothKeys` | `note.Open` with both verifiers gives 2 verified signatures, and a reader with only the log key opens it too. A second pass at the same checkpoint adds no history line |
| `TestWitness_ForkRefused_StateAndCacheUntouched` | a same-size fork errors with `a fork`, and a grown rewritten prefix with `does not extend`. The state, cosigned note and cache are byte-identical afterwards |
| `TestWitness_CachesEntryBundlesByteExact` | cache bytes = served bytes. After growth the full bundle and tile `0/000` get 0 GETs and the narrower partial is dropped. A pass with nothing new fetches only `checkpoint` |
| `TestWitness_ForgedEntryBundleRefused_NothingKept` | a one-byte change in a served bundle, with honest tiles, is refused and nothing is kept |
| `TestWitness_RefusesTheLogsOwnKey`, `TestTranslogWitness_OnePass_KeysFromFiles`, `TestProductBinaryDoesNotLinkTheWitness` | |
| `TestTranslogClient_FullTilesFromTheCache`, `_EntryBundleProvesEveryEntry` | |

| Mutation | Caught by |
|---|---|
| M1 consistency skipped | ForkRefused |
| M2 leaf check off | ForgedEntryBundleRefused |
| M3 co-sign drops the log's signature | CosignedNoteCarriesBothKeys |
| M4 the cache never read | **survived the witness tests** → `TestTranslogClient_FullTilesFromTheCache` added |
| M5 full bundle always re-fetched | CachesEntryBundlesByteExact |
| M6 read-write cache during consistency | ForkRefused (the cache snapshot) |
| M7 state written before the checks | all four witness tests |
| M8 narrower partial not dropped | CachesEntryBundlesByteExact |
| M9 a history line on every pass | CosignedNoteCarriesBothKeys |

## The restore (#912) — `c6b7faf0`

`logstore.Store.ReAppend(ctx, bodies, expect)` runs ONE transaction under `lockLog`:
- it appends the raw bodies from `MAX(idx)+1` exactly as `Append` writes them, with their `translog_hashes`;
- kind and dedup key come from `translog.Decode` of each body, and an undecodable body is refused;
- the root recomputed at `expect.Size` must be `expect.Root`, or everything rolls back (`ErrReAppendRoot`). A wrong count, or M ≥ N, is `ErrReAppendSize`;
- nothing is re-encoded and nothing is signed.

`logstore.ReadHighwater` is the job's parser, now shared. `cmd/tools/translog-restore` takes `-from <witness dir>`, `-highwater <file>` and `DATABASE_URL`. `docs/TRANSPARENCY.md` § After a database restore case 2 is now the re-append recipe, with the new origin as the fallback; case 3 uses the witness's `cosigned` note as the high-water.

| Test | What it proves |
|---|---|
| `TestReAppend_RestoresTheExactTree` | a log of 9 (signed at 5 and 9), dumped at 5. Before the re-append the job refuses `behind_highwater`. After it, the root at 9 = the high-water's, the bodies alone give that root, the next `Checkpoint()` signs with no refusal, and the server_build's dedup key comes back from its body |
| `TestReAppend_WrongBytes_RollsBack` | one body still a valid entry (`participants` 1 → 2) gives `ErrReAppendRoot`; size 5, stored hashes still `StoredHashCount(5)` |
| `TestReAppend_RefusesPastCheckpoint`, `_RefusesUndecodableBody` | |
| `TestTranslogRestore_FromTheWitnessCache_TheJobPassesAgain` | the round trip: served `logstore` → two witness passes → a dump at 5 → `run` → the job passes → a second `run` is refused |
| `TestTranslogRestore_MissingBundleIsNamed`, `TestProductBinaryDoesNotLinkTheRestoreTool` | |

| Mutation | Caught by |
|---|---|
| R1 no root check | WrongBytes_RollsBack |
| R2 no size check | RefusesPastCheckpoint (+ RestoresTheExactTree) |
| R3 no decode | RefusesUndecodableBody |
| R4 dedup key always empty | RestoresTheExactTree |
| R5 commit before the root | WrongBytes + the round trip |
| R6 off-by-one in `Cache.Entries` | the round trip |
| R7 a too-narrow bundle accepted | MissingBundleIsNamed |

## The trust host (#913) — `9a9eb747`

`hosts.Trust` comes from `TRUST_PUBLIC_HOST`, else `https://trust.<canonical host>`, else the canonical origin. `IsTrustHost` matches the configured host, `trust.<canonical | alias>`, and `trust.stored.cloud` / `trust.stored.ge` always. `TrustPublic()` is true when the canonical origin is a public name. With a public canonical origin, `/translog/…` is the log's on a trust host only:
- everywhere else it is the S3 API's, signed or not;
- on the trust host a signed request is 404;
- a development origin keeps K4's rule.

`translog-verify` and (in #912) `translog-witness` default to `https://trust.stored.cloud/translog`.

| Test | Red before | Mutation → caught by |
|---|---|---|
| `TestTranslogRead_PublicHost_OnlyTrustHostServes` | an anonymous GET on `stored.cloud` / `s3.stored.cloud` / `stored.ge` reached the log handler (404 for the S3 stand-in's 418); a signed request on the trust host reached the S3 API | T1 any host serves, T2 signed on the trust host → S3 |
| `TestHosts_Trust` | absent (`h.Trust undefined`) | T3 no legacy `trust.` name (survived until the no-alias assertion), T4 dev origin gated too |
| `TestTranslogRead_DevHost_PathsStillServe` | — | T4 |

## Live proof — a local server, a lab signing key (combined build of #910–#913)

The lab used a private database `vaultaire_p32live` (all 103 migrations, dropped afterwards), `TRANSLOG_SIGNING_KEY_FILE` from `translog-keygen` (key ID `0ce43c92`), a witness key from `translog-keygen -name witness.lab/mac` (`ecb6bfba`), `translog_public` ON, and a lab appender that calls `logstore.Append` + `Checkpoint` (never committed).

1. Boot: entry 0 is `server_build`. 5 ceremonies are appended and signed at **6** (root `ad6c1bf2…`, `note: first run`, high-water written). Witness pass: `first pass, 1 bundle(s) mirrored, co-signed by witness.lab/mac`.
2. **`pg_dump` taken at 6.** 4 more entries are appended and signed at **10** (root `478c24b6…`); the high-water file reads 10. Witness pass: `consistent with 6`. `cosigned` carries two lines, `— stored.cloud/translog/v1 DOQ8kg…` and `— witness.lab/mac 7La/ur…`. The cache holds `tile/entries/000.p/10` (the 6-wide copy is gone).
3. **The disaster:** the database is dropped and `pg_restore`d from the dump (6 entries), and the server is started. The job run is `error` with `the database holds 6 entries but this box signed a tree of 10 … restored behind the high-water: refusing to sign`, and `vaultaire_translog_checkpoint_refusals_total{reason="behind_highwater"} 1`.
4. `translog-restore -from witness -highwater hw/highwater` prints `database: 6 entries; high-water … size 10 root 478c24b6…` → `re-appended entries 6…9: the root at 10 is 478c24b6… = the high-water's`. Run again: `FAIL: the database already holds 10 entries … nothing to re-append`, exit 1.
5. Server restarted: the job run is `ok`, `{"signed": true, "size": 10, "root": "478c24b6…", "highwater_size": 10}`. That is the SAME root at the size the key signed, so no equivocation. Refusals are all 0 in the new process. Witness pass: `consistent with 10, 0 bundle(s) mirrored`.
6. **The gate, `VAULTAIRE_PUBLIC_HOST=https://stored.cloud`**, origin/main build against the combined build (`/translog/checkpoint`):

   | Host | main: anonymous / signed | combined: anonymous / signed |
   |---|---|---|
   | `stored.cloud` | **200** (the log) / 403 | **403 S3 XML** / 403 |
   | `s3.stored.cloud` | **200** / 403 | **403 S3 XML** / 403 |
   | `trust.stored.cloud` | 200 / 403 (the S3 API) | 200 / **404** |

   On the combined build, aws-cli `mb s3://translog` + `cp f.txt s3://translog/checkpoint` + `cp` back gave `hello`.

**A slip, recorded:** step 4 ran while the step-3 server was still up. The `pkill` pattern missed its relative path; the next start failed on the port, and the process was found by `lsof`. Nothing appended in that window (the server's `server_build` was already in the dump), and the re-append's own count check would have refused if anything had: `n` is read inside the transaction, under the lock. The runbook says stop the slots first, and that stands.

## Prod proof (read-only; no key on prod, flag OFF)

- **`eef950a3`** (#910 + #911): 0 error lines in 15 min. Every registered backend exports `vaultaire_backend_health` 1 (the first probe runs at boot, before the slot is switched in). `vaultaire_payment_renewal_stops_pending 0`, the refusal counters are at 0, `vaultaire_translog_size 17` (entry 16 = this build).
- **`9a9eb747`** (#913), on the box with `curl --resolve <host>:443:127.0.0.1 https://<host>/translog/checkpoint`:

  | Host | Before (`eef950a3`) | After (`9a9eb747`) |
  |---|---|---|
  | `stored.ge` | 404 `text/plain` (the log handler, flag OFF) | **403 `application/xml` AccessDenied** (the S3 API) |
  | `stored.cloud` | 404 text/plain | **403 S3 XML** |
  | `s3.stored.cloud` | 404 text/plain | **403 S3 XML** |
  | `trust.stored.cloud` | 404 text/plain | **404** text/plain (flag OFF); signed → **404** |
  | `trust.stored.ge` | — | **404** |

- **`c6b7faf0`** (#912): tools only, nothing visible. 0 error lines in 20 min across the three deploys.
- **The origin certificate does not cover `trust.stored.cloud`.** The SNI answer's SAN is `s3.stored.cloud, stored.cloud, www.stored.cloud`, and Cloudflare SSL is strict. HAProxy's `default_backend` already routes the name to the app.

## Departures (D-P32-n)

- **D-P32-1.** The witness is its own package, `internal/translog/witness`, so `cmd/vaultaire` never links it (a test checks `go list -deps`). The two client additions live in `internal/translog`, which the product already links; its package-level dependencies are unchanged.
- **D-P32-2.** `translog-keygen -name` exists. The prompt said to reuse the keygen, but its keys are always named for the log's origin. A witness key named `stored.cloud/translog/v1` would read as a second log key, so the witness gets its own name (`witness.<host>`). The witness refuses the log's own key (`ErrSameKey`).
- **D-P32-3.** The cache is the tlog-tiles layout itself, so the directory could be served as a static mirror. A partial bundle keeps its real name `<N>.p/<W>`; a wider copy, or the full one, replaces it and the narrower files are deleted ("overwrite as it widens").
- **D-P32-4.** `cosigned.history` gains a line only when the cosigned note changes, not on every pass. A 10-minute cron would otherwise write 144 identical lines a day.
- **D-P32-5.** `-once` defaults to true; `-once=false` loops every `-interval` (10 min).
- **D-P32-6.** `ReAppend` checks a body with `translog.Decode` (strict) and not with the kinds' `validate()`. The recomputed root is the authority, and an entry valid when it was signed must stay restorable even if a later rule would refuse it.
- **D-P32-7.** A re-appended ceremony has no dedup key: the `Idempotency-Key` hash lives in `translog_entries.dedup_key`, never in the body (RP37-14). This is documented in TRANSPARENCY.md and filed as RP41-1.
- **D-P32-8.** RP40-1's late withdraw deletes only the stop row; it does not clear `pending_provider`, as the prompt specifies. The audit row is written after the commit with `audit.Record`, as `AuditRenewal` does, not with `RecordTx`.
- **D-P32-9.** RP38-1 also drops `_probe_latency_seconds` before the first probe; a 0 there was the same false reading.
- **D-P32-10.** `TrustPublic()` keys on the canonical origin, as the prompt says, not on `TRUST_PUBLIC_HOST`. `IsOwn` counts the trust host.
- **D-P32-11.** The witness's default `-url` changed in #912, because #911 was already in the merge queue when #913 set the trust host.
- **D-P32-12.** The restore's case 3 takes the witness's `cosigned` note body (`sed '/^$/,$d'`) as the high-water file. `-highwater` keeps the job's strict parser, so the tool and the job read one format.

## Residuals (RP41-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP41-1 | note | A re-appended ceremony entry has no dedup key, so a retry of that exact ceremony call after a restore would append it twice (D-P32-7) | translog |
| RP41-2 | P4 (owner) | The origin certificate lacks the `trust.stored.cloud` SAN (strict SSL), and `TLS_CERT_PROBE_TARGETS` does not list it. Until both are done, `trust.stored.cloud` cannot be served through Cloudflare | ops |
| RP41-3 | note | The witness is ours, under our key, on our machine. An independent party's co-signature (the c2sp `tlog-cosignature` form) is K4c | K4c |
| RP41-4 | note | A refused checkpoint result reports `checkpoint_size: 0` when the refusal is `behind_highwater`: the high-water check returns before the DB checkpoint's size is recorded (seen in the lab: the restored DB held the checkpoint at 6) | translog/logstore |
| RP41-5 | note | `translog-restore` does not itself check that no slot is running. Its count check inside the lock refuses if one appended, but a slot loading the key before the re-append would sign nothing (it refuses) and page | tools |
| RP41-6 | note | A HEAD on `/translog/…` goes to the S3 catch-all (chi routes GET only); this predates this prompt | api |

**Carried:**
- RP38-2…4, RP38-7…9; RP34-2, -5, -8, -10, -12; RP35-2…5; RP37-16; RP39-* (Prompt 31's); RP36-* (Prompt 29's).
- RP38-5 is closed **once a witness runs**: with no witness, losing the box still loses the high-water. RP38-6 is closed. RP35-1 is closed. RP40-1 is closed. RP38-1 is closed.

## Owner items (note only)

- **DNS:** `trust.stored.cloud`, proxied, on the stored.cloud zone (`docs/CLOUDFLARE.md`'s row). Add the SAN `trust.stored.cloud` (and `trust.stored.ge` if wanted) to the origin certificate with certbot, and add it to `TLS_CERT_PROBE_TARGETS`.
- **The witness cron, off the box (the Mac), once the signing-key ceremony has run:**
  1. run `translog-keygen -name witness.<host> -out ~/.translog-witness/witness.key > witness.vkey`;
  2. add the cron line in `docs/TRANSPARENCY.md` § The witness, with the published verifier key pinned;
  3. copy the witness directory to a second place once in a while, because it is now the restore source.
- **F-33** (the restore policy) and the **signing-key ceremony** remain from WP-P30. F-33's question is now narrower: "new origin only when no witness cache reaches N".
- `translog_public` stays OFF until the key is loaded and the trust host resolves.

## Post-merge review (2026-10-23)

Reviewed on `c6b7faf0` (main = prod, `/version` `c6b7faf0d606`, built 02:39:24Z), the four product diffs read against the code paths they touch. Main's CI, Security and Deploy are green for all three merge commits. **Verdict: K4b holds as a witness and as a restore of a frozen database. No P0, P1 or P2. One P3, proven: the restore only works if nothing has been appended to the restored database first, and the boot of any slot appends. One P4, proven live: the trust host serves the whole app.**

### Findings (RP42-n)

| # | Sev | What | Proof | Where |
|---|---|---|---|---|
| RP42-1 | **P3** proven | **Any append to a restored database before the re-append makes the re-append impossible.** `logstore.Append` is not gated by the high-water file; only `Checkpoint` reads it. Every slot's boot appends a `server_build` entry, deduplicated on (git sha, binary sha). The daily dump is from 03:00 UTC and prod deploys several times a day, so the running build's entry is usually NOT in the dump. Restore the database, start a slot (the runbook's fallback does exactly this: "comment out the credential line … the job then appends and signs nothing"; the deploy pipeline does it on any push to `main`), and entry M is a new `server_build` body. `translog-restore` then reads M+1 and re-appends M+1…N−1 on top of a body the key never signed: `ErrReAppendRoot`, every time. The tables refuse DELETE, so the only ways back are a second `pg_restore` (losing every customer write since the first) or the new origin. The same holds for the ceremony route today and for every future producer (K4c's admin-read entries). RP41-5 says the count check "refuses if one appended". It does, once; the rerun then fails on the root. | Throwaway test on `signedLog` (log of 9, signed at 5 and 9; restore at 5): `Append(uniqueBuild)` → appended at 5; `ReAppend(lost[1:], at9)` → `the recomputed root is not the checkpoint's: 895ad66b… at 9, the checkpoint signed 16443178…`; size stays 6 | `internal/translog/logstore` (Append), `internal/api/translog_read.go` (boot entry, ceremony route), `docs/TRANSPARENCY.md` § After a database restore |
| RP42-2 | P4 proven live | **The trust host serves the whole app.** #913 gates `/translog/…` by host, but nothing gates the trust host by path the way `CDNHostRouter` gates the CDN host. On the box, `curl --resolve trust.stored.cloud:443:127.0.0.1`: `/` → **200 text/html** (the landing page), `/dashboard/login` → **303**. A signed S3 request to any path other than `/translog/…` is the S3 API's. Contract rule 11 lists `trust.` as its own surface. A login form on the trust origin is the wrong thing to teach readers to trust, and K4c's client build hashes will be published from that origin. | box curl above | `internal/api/server.go` (host routers), `internal/hosts` |
| RP42-3 | P4 (reasoned, not proven) | **"Renewal back on at Stripe" is read from live state, which cannot tell a portal un-cancel from a stop that never landed.** `grantStripe` treats `cancel_at_period_end = false` on the entitlement's own subscription, after a switch, as the customer re-enabling it. It then withdraws the switch and its pending stop, and invalidates the open bitcoin invoices. But `SwitchToPrepaid` keeps the switch on a 5xx or a missing answer (the row stays `pending`), and in that case Stripe still says `false` because nothing was sent. Any event for that subscription arriving before the job's next pass (10 min) undoes the customer's switch, with only a Warn line. Stripe redelivers events that failed during the same network outage that failed the stop. #910's RP40-1 branch reads the same signal in the job. The event itself carries the evidence: a real un-cancel is a `customer.subscription.updated` whose `data.previous_attributes.cancel_at_period_end` is `true` | reading `internal/billing/webhook_switch.go` `grantStripe`, `internal/payments/switch.go` `SwitchToPrepaid` | billing |
| RP42-4 | test | **`TestChunkClaim_AStoppingSlotWaitsForTheDetachedChunkPuts` flakes:** `cutUpload` samples `inFlight` after it reaches ≥ 2 but before `cancel()`. A third chunk worker that was already dispatched enters the gated driver afterwards, so the result is `expected: int32(2) actual: int(3)` (#909's CI run 39884707012, a docs-only PR). Sample after the handler answered (`<-first`), or wait for the count to be stable | CI log | `internal/api/chunk_put_detached_test.go` |
| RP42-5 | note | The witness takes no lock on its directory. Two passes at once (a cron plus a `-once=false` loop) can each append the same history line. Every file is replaced atomically, so nothing tears; the history only gains a duplicate | reading | `internal/translog/witness` |
| RP42-6 | note | A high-water file of another origin (after F-33's v2 fallback) fails `ReadHighwater` (it requires `translog.Origin`), so the job would error and not refuse. The v2 runbook must move the v1 file aside, or the parser must treat another origin as "archived". Any gate built for RP42-1 inherits this | reading | `logstore.ReadHighwater` |

### What holds (checked, not just read)

- **The witness.** `Pass` opens the accepted state with the pinned log key, proves consistency against a read-only view of the cache, then mirrors. Every cached tile goes back through `tlog.TileHashReader` and is re-authenticated against the new checkpoint's root, so a poisoned cache is an error, never a wrong hash. `EntryBundle` proves each leaf. The state file is written last. Shrink and same-size fork are both refused before any write (`VerifyConsistency`). The co-signature is `note.Sign` over the note `note.Open` verified with the log key, and the witness refuses the log's own key.
- **The restore when nothing was appended.** `ReAppend` reads `n` inside `lockLog` and checks `stored = StoredHashCount(n)`. It recomputes the root at `expect.Size` over the database's own prefix plus the new bodies, so the prefix the dump kept is proven too. It rolls back on any mismatch and never signs. `ReadHighwater` is now the job's parser and the tool's, so they read one format.
- **The trust-host gate for `/translog/…`.** On the box, `stored.ge`, `stored.cloud` and `s3.stored.cloud` → **403 `application/xml`** (the S3 API), and `trust.stored.cloud` → 404 (flag OFF), as #913's table says.
- **RP38-1.** The probes run under `defaultProbeTimeout`, so an absent series cannot outlive the first probe. No rule uses `absent(vaultaire_backend_health)`, and every rule on the series reads `== 0` or puts it on the left of `unless`. Every backend scraped on prod exports 1.
- **RP40-1.** `renewsAgain` needs `renewal = 'auto'` on the same provider and ref. `SwitchToPrepaid` writes `prepaid` and the pending row in one transaction, so a normal switch never matches it; only the un-cancel's grant (or a lost withdraw) does. RP42-3 is about the signal, not this branch.
- **Prod read at ~03:05 UTC.** 0 `"level":"error"` lines in 60 min; `translog_entries` 19 (max idx 18), 0 checkpoints, no key; `vaultaire_payment_renewal_stops_pending 0`.
- **Rule files.** `vaultaire-backends.yml` on the box was the pre-#910 file (a comment change only). `promtool test rules vaultaire-backends_test.yml` **SUCCESS** on the box, then installed and Prometheus reloaded: `2785d8a9…` = the repository's. The other 15 rule files already matched hash for hash.

### Process

- #914 (this note's docs PR) was CLEAN, armed and not in the merge queue. It was re-armed and queued.
- #909 (the WP-P30 post-merge review, docs only) was red on RP42-4's flake; its failed job was re-run.
- Carried: RP41-1…6 as filed (RP41-5 is superseded by RP42-1).
