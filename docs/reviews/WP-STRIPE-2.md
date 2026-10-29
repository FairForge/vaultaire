# WP-STRIPE-2 — test mode and the local server (Stage 1a Prompt 50), after Part 0 = RP78's fixes

*Plan § WP-STRIPE, the **WP-DX-1** row. It asks for:*
- *"`VLT_TEST_` keys on a sandbox tenant paired to every tenant … every write routed to the `local` driver … never billed, never metered … purged … nightly past 7 days";*
- *"`livemode: false`" webhooks and "`stored listen`";*
- *"the Docker image … the 'stripe-mock' of this product … listed in the transparency log as a `server_build`".*

*Part 0 is the RP78 review's findings (`docs/reviews/WP-STRIPE-1-REVIEW.md`).*

*Prompt: `docs/reviews/WP-STRIPE-PROMPTS.md` § Stage 1a Prompt 50 (the session ran the Opus edition). Session 2026-10-29. **Migration 116** (`116_sandbox.sql`, PR A). Next: 117. The post-merge review is RP80.*

Contract lines cited (`docs/ARCHITECTURE_CONTRACT.md`):
- **4** (keys only through the one hierarchy):
  - A sandbox is a tenant, so its S3 secrets are wrapped under its own tenant key like any tenant's.
  - It is erased with its parent, and the crypto-shred covers it.
  - The local image's first-boot account is an ordinary registration.
- **5** (convergence within a space only): a sandbox is never chunked and never in the public catalogue.
- **6** (the chunker is frozen): untouched.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1047** Part 0 (merged `7728ab8f`, deployed 04:52 UTC) | **RP78-1:** a mirrored PUT takes its buffer (16 + 2 MiB) from `Host.Budget` within 200 ms, else it goes unmirrored (`mirror_unbudgeted` → `{outcome="unbudgeted"}`).<br>**RP78-2:** `putTwo` returns at the primary's answer. The commit carries `mirror_settle`, and **`POST /api/v1/edge/mirror`** settles the copy after. It goes `pending → ready` under the admit row's lock, then the key's, only while the admit committed, a row names the address and the ETag is the body's MD5. Otherwise the leg's address is queued on the leg as `orphan_write`. Idempotent; `{outcome="settled"}`.<br>A codec chunk's mirror answers inside its write (`putTwoInline`; RP78-7's ETag is kept).<br>**RP78-3:** one request id per S3 request (the middleware's `X-Request-Id`, stamped as `x-amz-request-id` at header time, the error body's `RequestId`, a long op's, the row's); RB7-10's SSE-C clause.<br>**RP78-4:** `drivers.RegionJurisdiction` (`eu-west-3` = `uk`): the mirror rule, `IsEURegion`, `DataResidency`, the UK label and sentence, the picker's UK group.<br>**RP78-5:** `(multiple − 1) × stored`.<br>**RP78-8:** `engine.StrictTargets()`.<br>**RP78-10:** the manifest's `withdrawn` list (0.5.1's `edge-codec`); `translog-verify -client` and the Worker's `predeploy` refuse it. | `TestPutTwo_NoBudgetNoMirror`, `TestPutTwo_ThePrimarysAnswerDoesNotWaitForTheMirror`, `TestEdgeMirror_SettleAfterCommit`, `…AnUnbudgetedMirrorIsCountedAndTheCopyOwed`, `…TheGoHostSettlesAfterTheAnswer`, `TestEdgeRoutes_NeverCapturedByTheS3Gate` (+ mirror), `TestS3_EveryResponseCarriesTheLoggedRequestID`, `TestIsEURegion_London`, `TestMirrorLegFor_LondonIsNotTheEU`, `TestBucketSettings_AUKBucketSaysTheUK`, `TestBlobCopiesJob_LeavesTheCustomersTheirEgress`, `TestStrictTargets_EveryStrictTargetSorted`, `TestClientBuildManifest_WithdrawnIsRefused`, `TestVerify_Client_WithdrawnIsRefused`, `test/withdrawn.test.js` |
| **#1048** Worker 0.6.0 (merged `dbcc420e`; rows #1052 `c8c4b1f1`; deployed 06:33:04 UTC, version `b89a12b2`) | **RP78-1/2/9 at the edge:**<br>• `Semaphore.acquireWithin(MIRROR_BUFFER, 200 ms)` from the isolate's one budget, else unmirrored;<br>• `putTwo` resolves at the primary's answer; `settleMirror` posts the settle under `ctx.waitUntil`, cut 25 s after the primary;<br>• `putTwoInline` for codec chunks;<br>• the dead `cancel` gone.<br>Proved in workerd first (`lab/mirror-settle/run.sh`). | `putTwo: a mirrored PUT the isolate budget cannot cover is sent unmirrored`, `putTwo: the client is answered at the primary's pace; the mirror settles after`, `putTwoInline: …`, `settleMirror: …` |
| **#1049** PR A (merged `41a4195d`, deployed) | **The sandbox** (migration 116):<br>• `tenants.sandbox_of` (one per tenant); `VLT_TEST_` keys minted on it in one transaction (`POST /api/v1/manage/keys {test: true}`, `livemode: false`, a cap of 10 of their own);<br>• every write on `local` (`placeOn`'s and `decidePlacement`'s first branches); the edge passes it through (`sandbox`); never metered; `x-stored-mode: test`; vault parity and the mirror backfill skip it;<br>• `POST /api/v1/manage/sandbox/reset`; the `sandbox_purge` job (10:30 UTC, 7 days);<br>• erased with its parent (the runner's stage b4, then `EraseRows` sandbox-first); the overview's Test data card;<br>• `events.livemode`.<br>**RP78-6:** management requests in the request log. | `TestTestKey_ResolvesToTheSandbox`, `TestTestKey_NeverReachesAPaidBackend`, `TestTestKey_NeverBilled`, `TestSandbox_NoJobCopiesItToAPaidBackend`, `TestMgmtCreateKey_TestKeyOnTheSandbox`, `TestSandbox_ResetPurgesEverything`, `TestSandbox_NightlyPurgeAt7Days`, `TestEraseRows_ErasesTheSandboxWithItsParent`, `TestOverview_TestDataCard`, `TestRequestLog_ManagementRequestsToo` |
| **#1050** PR B (merged `f55a0ab6`, deployed 06:0x UTC) | **`livemode` and `stored listen`:**<br>• a sandbox's event is recorded under its parent with `events.livemode = false`, and the body says `"livemode": false` on every attempt (the retry reads the row);<br>• `GET /api/v1/events?after=<id>&wait=≤25` is the forward long-poll (`after=now` starts at the newest; an unknown cursor is 400 `unknown_cursor`);<br>• `internal/webhooksig`, the one signature;<br>• `cmd/stored listen --forward-to` (localhost only; signs with the listen secret; the cursor file resumes). | `TestWebhook_LivemodeFalseFromASandboxTenant`, `TestEvents_AfterIsAForwardLongPoll`, `TestSign_VerifiesAndRefusesTamperAndAge`, `TestListen_ReplaysToLocalhost`, `TestListen_ForwardsOnlyToLocalhost` |
| **#1051** PR C (merged `edc335e2`; the image job's first run passed) | **The image:**<br>• `Dockerfile` (the release binary copied as built onto distroless `static-debian12:nonroot` by digest; `/data` owned by nonroot); `make docker`;<br>• `deploy/docker/compose.yml` (Postgres 18 by digest, the checkout's migrations by an init step, 127.0.0.1 only);<br>• the first-boot account (`VAULTAIRE_BOOTSTRAP_EMAIL`: made once, its pair printed once);<br>• `deploy.yml`'s `image` job after the prod deploy (`ghcr.io/fairforge/vaultaire:<sha12>` / `latest`, refused unless `/vaultaire` hashes to the release binary);<br>• `docs/guides/local.md`, `docs/API.md` § Test mode. | `TestBootstrapTenant_MadeOnceKeysPrintedOnce`, `TestDockerImage_BootsAndAnswersTheOpenAPI` (`-tags docker`: healthy 5 s after `up` on the Mac, OpenAPI, PUT/GET with the printed keys) |

## Decisions (D-STRIPE-7…)

- **D-STRIPE-7:** the settle is a whole PUT's only. A codec chunk's mirror still answers inside its write (`putTwoInline`, 5 s after its primary). No admit-level route names a chunk, and the chunk's copy row is the commit's.
- **D-STRIPE-8:** the budget wait is 200 ms on both hosts. The code has no "windowed reader's short wait": its acquire waits unbounded.
  - The Worker's settle cut is **25 s after the primary's answer**: inside `waitUntil`'s 30 s, with the settle call's time left.
  - The Go host's bound is `mirrorDeadline`: 30 s plus the body at 2 MiB/s, detached from the request.
- **D-STRIPE-9:** a settle is `ready` only under all four conditions: the admit committed, a head or version row names `storage_key` (`blobNamedBy` under `lockHeadKey`), the copy row is pending, and the ETag is the body's MD5.
  - A cut, or no ETag, answers `pending` (counted `cut`).
  - Anything else is `retired`: `orphan_write` on the leg. The collector's own re-check keeps a blob that a row names again.
- **D-STRIPE-10:** `x-amz-request-id` is the request-id middleware's uuid, stamped by a writer at header time over a handler's own value. `WriteS3Error*` takes it from the writer chain, and a long op's buffered recorder carries it. A request handled without the middleware gets a fresh uuid.
- **D-STRIPE-11:** the jurisdiction table is us / eu / uk / jp. `DataResidency` is eu / uk / us, so an ap bucket keeps its pre-existing "us" label (RB8-4). The picker gains "United Kingdom", and the display name is "UK (London)".
- **D-STRIPE-12:** a withdrawal is a top-level `withdrawn` list keyed `(client, sha256)`, never a changed row.
  - Only 0.5.1's `edge-codec` is withdrawn. Its `edge-ingest` bytes are 0.5.2's, which is live, so withdrawing them would refuse prod.
  - `predeploy` hashes every file of `src/`.
- **D-STRIPE-13 (deviation, open question 1):** the prompt says "no reservation". The sandbox's **own** `tenant_quotas` row counts its bytes (the free tier's 5 GiB), and that is **the cap on `local` disk**. It is never a bill: the parent's row and every bandwidth row are untouched (`TestTestKey_NeverBilled`). Skipping the ledger on every write path would leave SLC's disk unbounded for test data.
- **D-STRIPE-14:** a sandbox has no `users` row. The four-table rule's users step is the parent user's row under `FOR SHARE`, and the test keys are that user's. `tenants.email` is unique, so a sandbox's email is `sandbox+<parent>@sandbox.invalid`.
- **D-STRIPE-15:** the event log is `GET /api/v1/events`; the prompt's `/api/v1/manage/events` does not exist. Its existing `cursor` pages backwards, so the forward cursor is a new `after` (+ `wait`). `stored listen` forwards the event as the list returns it: `{object, id, type, data, created_at, livemode}`, without `tenant_id`.
- **D-STRIPE-16:** the image's migrations are the checkout's, mounted by the compose file's init step. The image has no shell and no copy, and the guide starts from a checkout. The published image is linux/amd64; `make docker` builds the host's architecture.
- **D-STRIPE-17:** a sandbox's request-log rows are the sandbox's (the chain edition's rule), so the parent's Developers page does not show them (RB8-2).

## The workerd lab (RP78-2, before 0.6.0's deploy)

`edge/s3-ingest/lab/mirror-settle/run.sh` runs `wrangler dev --local` (4.151.3), with the Worker's own modules copied unchanged and `no_bundle` as in production. A Node sink answers **411 to any PUT without a Content-Length**.

| Case | Client answered | Primary | Mirror | Settle |
|---|---|---|---|---|
| 8 MiB, 2 MB/s mirror | 105 ms | 71 ms | 200 at 4,280 ms | `{etag}` 4.2 s after the answer (= the primary's MD5) |
| 4 MiB, mirror answering at 40 s | 68 ms | — | — | `{cut: true}` 25.0 s after the primary |

No PUT lost its length (0 refusals).

## Residuals (RB8-n)

- **RB8-1:** a Go node's shutdown does not call `Host.WaitMirrors()`. A settle in flight at a stop is lost, which leaves the copy pending: the job's, as designed.
- **RB8-2:** the parent's request log does not list its sandbox's requests (D-STRIPE-17). The sandbox's rows exist, but no route reads them. A `?mode=test` on `/manage/requests` is the obvious follow-up.
- **RB8-3:** admin views count sandbox tenants as tenants: the admin tenant list, the totals, the routing-truth samples.
- **RB8-4:** an `ap-northeast-1` bucket's `data_residency` label stays `us`, as before. The jurisdiction says `jp`, but the label set has no `jp`.
- **RB8-5:** Object Lock is not honoured by a sandbox reset (test data).
- **RB8-6:** `stored listen` forwards the list item, not the byte-identical webhook body: it has no `tenant_id`.
- **RB8-7:** the image publishes linux/amd64 only.

## The prod proof

All on prod, 2026-10-29 (UTC).

- **Part 0** (`7728ab8f`, 04:52):
  - The active slot logs `edge ingest routes mounted: … /api/v1/edge/mirror`.
  - A refused anonymous request answers `x-amz-request-id`.
  - The `/verify` run before the push (private DB, `local`) found every response's id equal to its `request_log` row's, including a DeleteObject 204 (28 of 44 such rows were empty on prod) and a 404's `RequestId`.
- **The image** (the first Deploy after #1051):
  - the `image` job checked `release binary sha256 15d6204a…` = the image's `/vaultaire`, and pushed `ghcr.io/fairforge/vaultaire:<sha12>` and `:latest` (digest `0e4cbba2…`);
  - **the package is private** (GHCR's default for a new package), and this session's `gh` token has no `read:packages`, so pulling the published image on the Mac is the owner's step: make the package public, or `gh auth refresh -s read:packages`;
  - the same Dockerfile built locally (`make docker`, arm64) is **healthy 5–7 s after `up`** and round-trips an object with the first-boot keys (`TestDockerImage_BootsAndAnswersTheOpenAPI`).
- **Test mode, tenant zero** (`41a4195d`): migration 116 live.
  - **Making the key:** the sandbox and one `VLT_TEST_` key were made on the box by `psql` in one transaction, as `GenerateTestKey` makes them: the parent user `FOR SHARE`, tenants, api_keys, tenant_quotas, and an `audit_logs` `key.created` row (`mode: test`). The management API needs the owner's TOTP. The secret was generated on the box, kept in a 0600 file, and never printed.
  - **The round trip:** through **`s3.stored.cloud` (the edge route)**: CreateBucket 200, a 3 MB PutObject 200 with **`x-stored-mode: test`**, GetObject bytes identical, DeleteObject 204.
  - **The server log:** `artifact stored … "backend":"local"`.
  - **`request_log`** for the sandbox: four rows, `CreateBucket` / `PutObject` / `GetObject` / `DeleteObject`, every one `host = origin` with a request id.
  - **No admit, nothing metered:** `edge_admits` for the sandbox: 0; `bandwidth_usage_daily`: 0 rows; the sandbox's counter 0 after the delete.
  - **Tidied:** the bucket was deleted, the key revoked, and the secret file removed. Sandbox `sandbox-691e66a0c436a457` stays, empty.
- **Worker 0.6.0:** the rows `edge-ingest` / `edge-codec` 0.6.0 are log entries 134–135, appended by the boot of `c8c4b1f1`.
  - **The deploy:** `npm run deploy`, its `predeploy` check passed ("no module of src/ is a withdrawn build"), at 06:33:04 UTC, version `b89a12b2`. `index.js` and `codec.js` download back as `3c8b2b70…` and `5d1c7c11…`, their rows.
  - **The mirror** (the same script, tenant zero, 32 MiB whole PUTs through `s3.stored.cloud`, mirror leg `lyve`):

    | Worker | Client time ×3 | Copies |
    |---|---|---|
    | 0.5.2 (minutes before) | 2.40 / 2.48 / 2.32 s | `ready` at the commit |
    | **0.6.0** | **2.24 / 2.23 / 2.27 s** | `ready` through the **settle** |

  - **The counters after the 0.6.0 run:** the server logged `edge mirror settled` three times (`state ready`, `leg lyve`): each mirror was still running at the primary's answer. `vaultaire_blob_copies_total` reads `settled` 3, `cut` 0, `unbudgeted` 0.
  - A healthy Lyve leg made a 32 MiB PUT about 6 % slower under 0.5.2. RP78's 64 MiB measurement found 2.3×, mostly the slower leg's last 48 MiB.
  - The test bucket and its objects were deleted.
  - **Not proved on prod:** `stored listen` against the live event log, and the sandbox reset route (both need a management JWT, which needs the owner's TOTP; their tests cover them).

## Open questions

1. **D-STRIPE-13:** keep the sandbox's own 5 GiB counter as the disk cap? The alternative is no reservation at all, as the prompt wrote.
2. **RB8-2:** show test requests on the parent's Developers page (a mode filter)?
