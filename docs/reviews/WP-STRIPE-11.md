# WP-STRIPE-11 — transfer in, on the origin's scheduler (Stage 1a Prompt 59; Part 0 = RP96)

*Stage 1a Prompt 59, 2026-11-04/05: WP-XFER-1 (plan § WP-STRIPE, its last unbuilt row) on the origin's job scheduler — prod has no data node — with Part 0 = the findings of RP96 (`docs/reviews/WP-STRIPE-10-REVIEW.md`). Migrations **126** (Part 0's: `scrub_findings` once per sighting) and **127** (`transfer_jobs`, `transfer_items`); next **128**. The post-merge review is **RP98**.*

## What shipped

| PR | What |
|---|---|
| owed first | **RB16-6 done:** `SCRUB_CYCLE=30d` on prod 2026-11-04 22:33 UTC (backup `.env.bak-scrub-cycle-30d-20261104T2233Z`, `vaultaire-switch restart` onto :8000, `e6cc8a27`). |
| #1155 | **The Worker 0.7.3** (RP96-11/12/21/22), first and alone: `partDisposition` reads a part's `Content-Disposition` only in the one shape the origin's `mime.ParseMediaType` reads the same (anything else → the origin whole); the admit answers `cors_origin` and the Worker sets it on its own 400 and every 503; a short pass-through body is 400 `IncompleteBody`; `package.json` names the version, `test/version.test.js` holds it to the README and the manifest. |
| #1157 | The Worker 0.7.3's client_build row + the edge proof. |
| #1156 | **Part 0, RP96-2/6/7/8/9/10:** the scrub reads a blob larger than one piece as one range per run, its offset, MD5 state and pinned ETag in `job_runs.result` (`resume`); a skipped backend's cycle is in progress from its first skip, and `ScrubCycleStale` sees the completed gauge too; ONE vendor ledger for the scrub and the copies (`jobsEgressLeft`: (mult − 1) × base without a vendor view, the full multiple when it reports); the plan PUT refuses an `idrive-<region>` (400 `plan_is_per_account`); a sealed object gets its mirror copy (by `backend_etag` and the stored length); a same-value flag re-assert writes nothing. |
| #1158 | **The P4s, RP96-14…20:** migration 126 (one finding per (tenant, address, verdict, file): `found_at` the last sighting, `first_found_at`, `times`); an unopenable codec chunk is `unreadable` "a key problem", never repaired; `sia` never scrubbed; `docs/API.md § Browser uploads` states every status; a per-network pre-limiter (600/min) on `/oauth/register`; the bucket line cached a minute. |
| #1159 | **PR A — the transfer job**, migration 127. |
| #1160 | **PR B — the dashboard page**, `docs/API.md § Transfers`, `docs/guides/migrate-from-b2.md`. |
| — | **PR C — the node seam: NOT built** (open question OQ-XFER-1). |
| this PR | PR D — this note, STATUS, the plan row, CLAUDE.md, Prompt 60. |

## Step 0 — answered before PR A

1. **The source key (D-XFER-1, the recommendation kept).** Migration 108's rule, as `api_keys` has it: with a master key the secret is sealed with `crypto.TenantKeys.SealCredential` (HKDF of the tenant's newest chunk key, AES-256-GCM, AAD tenant ‖ the job's id ‖ version) into `secret_wrapped` + `secret_key_version` and the plaintext column stays NULL; without one (prod today, F-14) the plaintext `secret_key`. Either way the access key id and both secret columns are NULLED in the statement that moves the job to `completed`, `failed` or `cancelled`; the rows go with the account (`account.EraseRows`); the export carries the jobs WITHOUT any key column; no log line, error, audit row or webhook body carries the key (the source's own error text is quoted only after the key id is scrubbed from it). **No reason found to refuse without a master key:** the source key is a key the customer made for this, read-only by its own vendor's rule, held for the job's life only — the same exposure as every `api_keys` secret on prod today, for less time; refusing would make the feature dark until F-14 for no gain the customer can see. With `credential_plaintext_drop` ON (which requires a master key) the plaintext column is never written.
2. **The source endpoint cannot reach the box or a private network.** At create: `https://` only (no `http`, no other scheme), a host and an optional port, no userinfo, no path beyond `/`, no query, no fragment; a literal IP is checked at once. At DIAL time — every connection, the only check that matters: the transport's `net.Dialer.Control` sees the address actually being connected and refuses it when `isPrivateOrSpecialIP` (loopback, RFC 1918, link-local, CGNAT, ULA, unspecified, multicast, the metadata ranges), so a DNS answer that changes between the create-time check and the dial (rebinding) cannot get round it. Redirects are refused (`CheckRedirect` → the 3xx is an error), the environment's proxy is never used (`Proxy: nil` — a proxy would make the dial check see the proxy's address), TLS verified. Tests reach an `httptest` fake through `transferAllowPrivateTargets`, `webhookAllowPrivateTargets`' test-only escape.
3. **"A capped share of bandwidth" on SLC.** The link is 10 Gbps (1.25 GB/s) with a 660 TB monthly budget, and a transferred byte crosses it twice (source → SLC, SLC → backend). `TRANSFER_ORIGIN_SHARE` — one token bucket for every transfer on the process — defaults to **50 MiB/s** (≈ 420 Mb/s each way, 4 % of the link); running flat out for a whole month that is 136 TB moved and 272 TB of the budget (41 %), and transfers are bursts, not a constant. `TRANSFER_RATE_MAX` — the per-job default AND ceiling of `rate_bytes_per_sec` — defaults to **25 MiB/s**, so two jobs at full rate fill the share. Both are sizes per second, a rejected value logged at Warn and the default kept.
4. **A Geyser destination.** No: Geyser's cloud integrations and `cloudSync` are pinned to real AWS / Wasabi / Oracle — the endpoint field is accepted and silently dropped (the functional verdict of 2026-07-30, WP-Geyser-Admin: a `cloudSync` create with Lyve credentials was refused "insufficient access to list objects" by AWS). So a destination whose bucket places on the vault floor is **501 `NotImplemented`** at create (**D-XFER-2**), and `TestTransfer_GeyserDestinationUsesCloudSync` is an open question, not a test.

## Decisions

- **D-XFER-1** — the source key follows migration 108's rule (Step 0 answer 1).
- **D-XFER-2** — a vault-floor destination is 501 `NotImplemented` (Step 0 answer 4); a Private (`ssec_required`) destination is 409 `destination_private` (the job holds no customer key).
- **D-XFER-3** — the copy goes through the S3 API's own handlers (`handlePutObject`; initiate / UploadPart / Complete / Abort), called on a request carrying the tenant as `handleS3Request` puts it there — not through `generatedObjectWriter`, which skips the placement decision, the net-of-overwrite reservation, metadata, the bucket's default Object Lock and the events. `TestTransfer_WritesThroughThePUTPath` compares the head row, the version rows and the event with a PUT of the same bytes.
- **D-XFER-4** — the destination key is the source key, prefix included; the object's `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` and `x-amz-meta-*` are carried (from the first range's answer). Tags, ACLs, the source's Object Lock and versions are not.
- **D-XFER-5** — "idempotent on (source ETag, size)": the destination's head row has the source's ETag and size, OR an earlier transfer of this tenant from the same endpoint and bucket copied this source key at this ETag and size and the head row still has the ETag that copy gave it (`transfer_items.dest_etag`). The second rule is what makes a multipart source object (ETag `…-N`, which our own parts never reproduce) skip on a second create.
- **D-XFER-6** — every ranged GET carries `If-Match` of the listing's ETag: a source object changed under the copy is a 412 → the item fails (never spliced bytes); a body under the multipart size whose ETag is an MD5 is PUT with its `Content-MD5`, so the PUT path checks the bytes end to end.
- **D-XFER-7** — a cancel or pause during a copy cuts that copy (its context is cancelled within 2 s, the PUT's body errors, nothing commits); the item stays pending. A destination bucket deleted under the job fails the job.
- **D-XFER-8** — RP96-2's ranged read is ONE open-ended range per run, checkpointed per `LARGE_GET_PART_BYTES` piece, not one request per piece: a request per 16 MiB would cost Lyve's ~107 MB/s single stream its per-request latency. RP96-15's dedupe makes `found_at` the LAST sighting (the line's query unchanged) and keeps the first as `first_found_at` — a `last_seen` column beside a frozen `found_at` would have hidden a recurring finding from the line.
- **D-XFER-9** — migration numbers: 126 went to Part 0 (RP96-15 needs a unique index), so the transfer tables are 127.

## Residuals (RB17-n)

- **RB17-1** — the prod transfer proof's create needs tenant zero's management access: the owner's sign-in (password + authenticator code) or a management key; no operator script mints a JWT. The commands are in § Prod proof.
- **RB17-2** — a transfer item that is cut mid-copy (a run's end, a cancel) is re-copied whole next run; no part-level resume inside an object.
- **RB17-3** — the rate caps are per process: a slot switch mid-run starts a new token bucket (a burst of at most one second's share).
- **RB17-4** — the 404 for a vanished source object fails the item (`the source object is gone or changed`); a re-listing that would notice the delete is not done.
- **RB17-5** — TDD order slipped on PR A (the tests were written after the first draft of the job) and on RP96-20; each test was run against the change.

## Open questions

- **OQ-XFER-1 (PR C, the node seam):** STOPPED as the prompt allows. The node relays only a request the CUSTOMER signed: the admit verifies that signature (`auth.ValidateDescribed`). A transfer item has no customer-signed request, so the node would need (a) a new admit kind authorised by the node's token for a tenant's job — a node able to write into any tenant's bucket on the control plane's word — and (b) the source key handed to the node to read the source. Both widen access (contract rule 7: the transparency log); neither is the admit "as written". The origin's path stays; the design is a decision for the plan (who may hold a tenant's source key, and a job-bound admit with its own log entry).
- **OQ-XFER-2:** `TestTransfer_GeyserDestinationUsesCloudSync` stays a question (D-XFER-2): Geyser's cloudSync is AWS-pinned.
- **D-SCRUB-11** (RP96-13) and **D-SCRUB-12** (RP94-13's delete half) remain the owner's.
- Prompt 58's owed capacity line (the card's egress line after the 2026-11-05 08:30 `backend_capacity` run) — not yet: the run is in the future at this writing.

## Prod proof

**Done by the session:**
- **The Worker 0.7.3** (`a8011109` from `d9204eed`, 2026-11-04 23:51 UTC; every module byte-identical to its manifest row): a 5 MiB form committed at the edge (`edge_admits` `via = post`, `committed`, `lyve`; head row `p59/edge.bin`, 5,242,880 bytes, ETag `16bd5907…` = the file's MD5); an unquoted `filename=a b.txt` form → the origin's 400 `MalformedPOSTRequest`, NO `edge_admits` row; a bad-tail form from `Origin: https://page.example` → the Worker's own 400 `MalformedPOSTRequest` WITH `Access-Control-Allow-Origin: https://page.example`, its admit `failed`, nothing stored. (The proof bucket `tz-proof-1792873702` now has a CORS rule allowing only `https://page.example`, set by `PutBucketCors` with tenant zero's own key.)
- **Part 0 and the transfer code deployed** (the merge commits of #1156, #1158, #1159, #1160); migrations 126 and 127 applied by the deploy (checked on prod 2026-11-05: `transfer_jobs`, `transfer_items`, `uq_scrub_findings_once` and `scrub_findings.first_found_at` / `times` present; `/version` `9e118d9e` after #1159; the `transfer` job's first run `ok` at 03:54 UTC, nothing due).

**Owed — RB17-1, the owner's sign-in.** Creating a transfer needs tenant zero's management access (the owner's password + authenticator code, or a management key); no operator script mints a JWT. After signing in (`POST /auth/login` with `"code"`, the token in `tz.jwt`):

```sh
T="Authorization: Bearer $(cat tz.jwt)"
# a read-only source key of tenant zero (WP-KEY-1), scoped to the source bucket
curl -s https://stored.cloud/api/v1/manage/keys -H "$T" -H 'Content-Type: application/json' \
  -d '{"name":"p59-transfer-source","permissions":["ListObjects","GetObject","HeadObject"],"bucket_scope":["mac-offload"]}'
# a fresh destination bucket
curl -s https://stored.cloud/api/v1/manage/buckets -H "$T" -H 'Content-Type: application/json' -d '{"name":"p59-transfer-in"}'
# the transfer: >= 100 objects incl. one >= 64 MiB
curl -s https://stored.cloud/api/v1/manage/transfers -H "$T" -H 'Content-Type: application/json' \
  -d '{"bucket":"p59-transfer-in","source":{"endpoint":"https://s3.stored.cloud","region":"us-east-1","bucket":"mac-offload","prefix":"<a prefix of >= 100 objects>","access_key":"<id>","secret_key":"<secret>"}}'
```

Then: `GET /api/v1/manage/transfers/{id}` until `completed`; every item's destination ETag against the source's listing (a GET of the large one hashed); a second create of the same source = every item `skipped`; `access_key_id`, `secret_key`, `secret_wrapped` of the row all NULL; the `transfer.completed` event in `webhook_outbox` (or none: tenant zero has no endpoint — say which); `vaultaire_transfer_bytes_total` = the copied bytes; then delete the destination's objects and the source key.
