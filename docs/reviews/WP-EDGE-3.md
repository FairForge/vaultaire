# WP-EDGE-3 — the data node (`VAULTAIRE_ROLE=node`), `data_plane_hosts`, `host=slc` (Stage 1a Prompt 47)

*Plan 40.10: "the control plane picks a host per request — the first hosts are the client and SLC itself, which exist"; "unmetered data nodes running this binary stateless".*
- *Part 0 = the RP72 review's fixes (`docs/reviews/WP-EDGE-2C-REVIEW.md`).*
- *Then the node: the Worker's pipeline as a Go package, the binary as a second kind of host, the hosts in a table.*
- *Then `host=slc`: the origin's own edge-eligible requests through the same package, in-process.*

*Prompt: `docs/reviews/WP-EDGE-PROMPTS.md` § Stage 1a Prompt 47 (the Opus edition was the session's). Session 2026-10-27/28. **Migration 112** (`112_data_plane_hosts.sql`, in PR 0 — see D-EDGE-41; next: 113). The post-merge review is RP74.*

**The node is not a speed project** (the SPEED bench: the Worker reaches 840–980 MB/s per client). Its case is Cloudflare-independence (the undue-burden clause), RS and GLACIER writes off SLC (Prompt 48), and tenant-scoped BYO hosts.

Contract lines cited:
- **4** (keys only through the one hierarchy). A node holds its own token (minted by the registry, stored as its sha256, shown once) and the request's presigned or header-signed links — never the master key, a tenant key version or a stored secret. Its codec seals under the admit's PUBLIC key with random per-chunk keys that die with the request.
- **5** (no global convergent key). Unchanged: per-tenant, per-version claims and codec keys.
- **6** (dedup by a keyed index). A node and `host=slc` send plain sha256 per chunk to `/api/v1/edge/index`, which keys it in memory as for the Worker. The node logs no hash (Step 0.3).
- **10** (the attested data plane): **a node is NOT it.** A node claims nothing about the code it runs to anyone but its operator; nothing here is attestation.
- **13** (the tenant is the unit of disclosure): a tenant-scoped node admits one tenant (D-EDGE-38).

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1018** Part 0 (merged `048ee7f1`, deployed 00:21 UTC) | RP72-1 `backend_etag` (migration 112) + the dated single-PUT cutoff; RP72-2 `EnforcesDecodedLength` (probed) + the size HEAD at commit / Complete; RP72-3 a 401/403 commit is the Worker's 503; RP72-4 `EntityTooSmall` relayed; RP72-5 the window stops at the client's disconnect; RP72-6 one isolate budget | `TestS3Upload_ReportsTheBackendsETag`, `TestEdgeGetIfMatch_TheCutoffInForceWhenTheRowWasWritten`, `TestBackendETag_RecordedByMoverAndOriginWritesAndSigned`, `TestEdgeCommit_AWSChunkedLengthIsCheckedWhereTheBackendDoesNotEnforceIt`, `TestEdgeComplete_AssembledSizeIsCheckedWhereTheBackendDoesNotEnforceIt`, `TestEdgeComplete_BackendEntityTooSmallIsTheClients400`, node: the 401/403 commit, the disconnect, the parent semaphore |
| **#1019** PR A (merged `f0ec185b`, deployed) | `internal/dataplane` (wire types moved, `ControlPlane`, `Host.Write`/`Read`, the aws-chunked decoder, the HTTP control plane, the backend deadline client); `Server.DataPlane()`; `VAULTAIRE_ROLE=node` + `internal/node`; the registry routes; per-host tokens from the registered address; the tenant-scoped rules; `edge_admits.host`; the heartbeat; `deploy/node/` | the harness unchanged through `Host.Write`; `TestDataHost_*`, `TestHTTPControlPlane`, `TestNodeRole_*`, `TestDataPlaneHosts_*`, `TestEdgeCalls_*`, `TestEdgeCodecCommit_ATenantScopedNodeHasEveryStoredChunkOpened`, `TestEdgeCommitGet_ATenantScopedNodeIsMeteredAtTheAdmittedSpan`, `TestEdgeAdmitRow_CarriesItsHost`, `TestEdgeHeartbeat_BringsADownNodeUp`, `TestDataNode_EndToEndThroughTheControlPlanesRoutes`, `TestHost_ABackendThatNeverAnswersEndsAtTheDeadline` |
| **#1020** PR B (merged `0d2f1cc2`, deployed 01:45 UTC) | `host=slc` (`data_plane_slc`); the 307 probe (steering by configuration); `vaultaire_data_plane_host_up`, the 90 s sweep, `DataPlaneHostDown`, the System card; `deploy/node/lab-proof.sh` | `TestDataPlaneSLC_*`, `TestDataPlaneHostUp_Collector`, `TestEdgeReaper_MarksNodesUnseenFor90sDown`, `TestDataPlaneRuleFile`, `TestHandleAdminSystem_ShowsTheDataPlaneHosts`; promtool SUCCESS on SLC |
| PR C (this) | this note, DEPLOY.md § Data nodes, STATUS, plan 40.10, CLAUDE.md, the Worker 0.4.1 manifest rows, Prompt 48 refreshed, 47 DONE in the chain | — |

## Step 0 — the probes

### 0.1 — do S3 clients follow a 307 to another host, re-signed?

`deploy/node/lab/probe307.go`: origin A (:8099) answers PutObject and GetObject with `307 Location: http://127.0.0.1:8100<uri>`; origin B (:8100) records every request and checks its signature against B's host with the lab key. `sdk307.go` is aws-sdk-go-v2's two calls. Run 2026-10-28 on the Mac.

| Client | PutObject → 307 | GetObject → 307 |
|---|---|---|
| aws-cli 2.28.19 (botocore, Python 3.13.7) | error `(307) Temporary Redirect`; never reaches B (it HEADs the bucket for its region redirector and stops) | the same |
| rclone 1.71.0 | error `StatusCode: 307 … TemporaryRedirect` | **followed** (Go's net/http) with A's `Authorization` byte for byte — **INVALID for B's host** (a real node or backend: 403) |
| aws-sdk-go-v2 (the go.mod's) | error `StatusCode: 307` | **followed**, not re-signed — **INVALID for B** |
| boto3 1.43.120 | error `(307)` | error `(307)` |

**No client follows a 307 with a fresh signature, on PUT or GET.** Steering is by configuration: the customer's endpoint names the node (D-EDGE-36). `edge_redirect` is not built (step 12's own rule).

### 0.2 — a node's throughput and CPU

The Mac (M1, the home uplink), the lab with `LAB_BACKEND=lyve` (the bench pair, us-west-1). A 1 GiB golden body (seed 20261027) by aws-cli 2.28.19 (multipart, 8 MiB parts — every part the node's codec) through the node: **33 s ≈ 31 MiB/s** — the uplink's (Part B's Worker codec measured 26.2 on the same line). **Node CPU 8.55 s per GiB** (chunk + sha256 + 0x06 seal + MD5). Row `chunked AES256-EW 1073741824`. The Hostbrr box was not used: on the Mac the uplink, not the node, is the limit, and a VPS number is the first node's own measurement (RB5-3).

### 0.3 — what a node logs

The node's log after the 100 MiB case: two lines — `data node: serving` (its port, its public host, the control plane URL, the version) and `data plane: codec` (op, admit_id, bytes, chunks, misses, hits, ms). **The bucket, the key, the tenant id and a body hash appear 0 times.** A pass-through is proxied and logged by the origin, never by the node.

## Decisions

- **D-EDGE-34 — the registry's GET is an admin read where it names a customer.** The prompt lists the three routes as admin writes; a GET cannot be (`TestAdminRoutes_EveryMethodClassified` puts every GET through the admin-read gate). A tenant-scoped (BYO) host's row is that tenant's record (`ScopeTenantDetail`); a platform host's names no customer — the gate's resolver may now answer an empty scope, which records nothing (an error still refuses). PUT and DELETE are in `apiAdminWrites`.
- **D-EDGE-35 — a node serves three requests itself and hands every other SIGNED S3 request to the origin untouched.** PutObject, UploadPart and GetObject go through `dataplane.Host`; HEAD, DELETE, listings, Create/Complete/Abort and every pass-through the admit answers go to `NODE_ORIGIN_URL` (default the control plane's) with the client's Host kept, so the client's signature verifies there. The origin's web and API paths (`/api/`, `/dashboard`, `/translog/`, …) are 404 on a node, signed or not. Keys hold slashes, so the routes are `PUT|GET /{bucket}/*` (the prompt's `{key}`).
- **D-EDGE-36 — steering is by configuration** (Step 0.1).
- **D-EDGE-37 — `host=slc` meters through the origin's own writer.** The bytes cross SLC on that path, and `handleS3Request` already counts every byte written and writes the access-log row; an `slc` commit records neither, nor does the reaper meter an expired `slc` read. The GET hook sits inside the egress guard (pacing applies). `data_plane_slc` needs `edge_ingest` too (the admit's gate).
- **D-EDGE-38 — a tenant-scoped host is trusted for nothing.** It admits its tenant only (the relayed 403 AccessDenied otherwise, counted `refused`); every stored chunk of its codec commit is OPENED (RP68-3 at 100 %); every GET commit — served, cut OR fallback — meters the admitted span (it holds the presigned link whatever it reports; a fallback the origin really served is then metered twice, the trade).
- **D-EDGE-39 — `backend_etag` and the dated cutoff (RP72-1).** The cutoff is `s3SinglePutCutoffBefore` = 2026-10-27 19:58 UTC, the minute after #1011's Deploy ended (19:52:34 → 19:57:38): a row the old slot wrote during the switch reads under the old 16 MiB rule, and one the new slot wrote then only loses its If-Match. A row's `updated_at` dates it (every whole write upserts it). Statements that re-point a head row's blob (the demotion flip, a version delete's re-point, the whole re-encrypt move, the chunked upsert) clear `backend_etag`.
- **D-EDGE-40 — RP72-2's probe and the part case.** iDrive refuses a wrong decoded length (+1: 400 IncompleteBody; −1: 503 SlowDown); Lyve stores the bytes sent either way, PutObject and UploadPart. A part cannot be HEADed: the edge Complete on a non-enforcing backend HEADs the assembled object when one of its committed parts was `check_length`, and another size ends the upload (holding released), retires the blob, answers `IncompleteBody`. A HEAD that fails there is logged and the commit goes on (the backend already assembled; refusing would lose the client's bytes for a check that only failed to run).
- **D-EDGE-41 — migration 112 landed whole in PR 0.** The prompt put `backend_etag` in PR 0 and the table in PR A, both in 112; a migration on main is never edited, so the one file (table, seeds, `edge_admits.host`, `backend_etag`, and `idx_edge_admits_upload` for RP72-2's Complete query) went in with PR 0. The table and the column sat unused until PR A.
- **D-EDGE-42 — a node is up only with a heartbeat within 90 s, at the gate too.** The reaper's sweep runs every 5 minutes; `edgeCaller` treats a node whose row says up but whose last heartbeat is older than 90 s as down at once. The heartbeat endpoint landed with PR A (a registered node starts `down` and only a heartbeat brings it up; never a draining one).
- **D-EDGE-43 — a client that leaves a windowed read is `served` (RP72-5).** The commit says `served` with `bytes_sent` and `backend_code client_disconnected`: what went out is metered and the backend's breaker is not charged (before: `error`, status 0, a charge).
- **D-EDGE-44 — a host's backend client answers within 60 s.** `dataplane.NewBackendClient(BackendAnswerTimeout)`: the driver's per-PUT deadline as a response-header timeout, so a backend that takes a body and never answers (Lyve us-east-1, 1–3 % of PUTs on 2026-10-17) ends the write as `edge_error` (the client's 503) — the origin's driver would have retried it once (RB5-5).

## The lab proof (`deploy/node/lab-proof.sh`, 2026-10-28, the Mac)

The control plane on :8000 (a private migrated DB `vaultaire_nodelab`, a lab master key, `EDGE_SERVICE_TOKEN`, `STORAGE_MODE=idrive` on the bench us-central-1 pair), a node on :8100 registered through `PUT /api/v1/admin/data-plane-hosts/node.lab.test` with address 127.0.0.1 (the token answered once, kept 0600). The node's heartbeat brought it up.

| Case | Result |
|---|---|
| node | 100 MiB golden (seed 20261026) by aws-cli → the node, one PutObject: **6 s**; row `chunked AES256-EW 104857600 e9aff7ea…`; **51 chunks** (the golden count — the harness's); admit `put host=node.lab.test committed`; read back THROUGH THE NODE MD5-equal (the GET of a chunked row is a pass-through: the node handed it and its HEAD to the origin); **control-plane PUT lines for the object: 0**; `admits{host="node.lab.test",op="put",outcome="admitted"} 1`, `bytes{put} 104857600` |
| slc | `data_plane_slc` ON; 12 MiB golden (seed 4701) through the control plane itself: admit `put host=slc committed`, row `chunked AES256-EW 12582912 9cbdb9e0…`, read back MD5-equal |
| leaks | Step 0.3 above |
| down | the node killed → `vaultaire_data_plane_host_up{host="node.lab.test",kind="node"} 0` at the 90 s poll; an admit with its token from 127.0.0.1 → **503, Retry-After 30** |
| big | Step 0.2 above (Lyve) |

Teardown: both labs purged and verified empty (iDrive and Lyve, `t-<lab tenant>/` and `t-_global/_global/_chunks/<lab tenant>/`: 0 objects, 0 open uploads); the DB dropped; the keys and tokens removed.

## The prod proof

2026-10-28, tenant zero, `data_plane_slc` ON (02:00:19 UTC — a `feature_flags` row by psql with the flag service's own `flag.set` audit row, `updated_by sql:prompt-47-session`: no admin JWT without the owner's TOTP, the RE-5 route), aws-cli 2.28.19's DEFAULTS through `stored.ge` (the origin itself, not the Worker), the Lightroom zip `lightroom-backups/old-lightroom-catalogs/LRViera23_2025-04-03 2051.zip` (242,911,992 B, on Lyve):

| | Result |
|---|---|
| download | 97 s, MD5 `26c0d8ca…` — every one of aws-cli's ranged GETs admitted `host=slc` on `lyve` and committed |
| upload to `p47-proof/LRViera23.zip` | 37 s; **29 of 29 aws-chunked parts admitted `host=slc`**, header-signed to Lyve, each `check_length` (RP72-2: Lyve does not enforce the decoded length), the Complete's HEAD found the parts' sum, committed. Head row `242911992 347535c4f06bb1feff0bfef3eecbd707-29 lyve`, **`backend_etag` = the same `-29`** (RP72-1's column written on prod) — the ETag 46c's proof got through the Worker for the same bytes |
| download again | 32 s, **MD5-equal** |
| the rows | `edge_admits` since 02:00:40: `slc get committed lyve` ×58 = 485,823,984 bytes (exactly 2 × the object), `slc part committed lyve` ×29 = 242,911,992 |
| metered once | the tenant's `bandwidth_usage_daily` for 2026-10-28: 485,824,443 egress bytes = the two downloads + 459 bytes of small answers (a double meter would be ~971 MB) |
| errors | 0 `"level":"error"` lines on the slots since 02:00:40; no host=slc fallback line |

The proof object was deleted after. **`data_plane_slc` stays ON for tenant zero.** The `slc` row's weight stays 0; no node is registered. `deploy/monitoring/vaultaire-dataplane.yml` installed on SLC (sha256 `e6d59af9…` = repo, promtool SUCCESS, `DataPlaneHostDown` loaded; `vaultaire_data_plane_host_up` exports the two seeded hosts at 1). The access log has no rows for it: `s3_access_log` records only buckets with `logging_enabled`, and `lightroom-backups` has none.

## Residuals

- **RB5-1 (P3) — the Worker 0.4.1 is built, not deployed.** RP72-3/5/6 change `codec.js` and `index.js`; their `client_build` rows (`edge-ingest`, `edge-codec` 0.4.1 at `048ee7f1`) are in this PR. The deploy (wrangler, RB-9's capture armed) is the next session's, after the server boots with the rows; until then prod runs 0.4.0.
- **RB5-2 (P3) — a BYO node reports the client's IP**, which the admit uses for the key's IP allow-list: a tenant-scoped node can lie past its OWN tenant's allow-list (no other tenant's). Platform nodes are ours.
- **RB5-3 (P3) — no node can be installed until the release-key ceremony.** `install.sh` refuses without the release verifier key (docs/RELEASES.md: none exists yet); the first node's VPS number (Step 0.2 on a VPS) comes with it.
- **RB5-4 (P3) — a node's pass-throughs reach the origin under the node's Host.** In the lab the origin is 127.0.0.1; on prod HAProxy and Cloudflare must accept the node's hostname at the origin (a config item with the first node).
- **RB5-5 (P3) — the Go host's PUT is not retried.** The driver's uploader retries a stalled PUT once on a fresh connection (`FIXED_BUCKET_PUT_TIMEOUT`); a host's single PUT to a presigned link ends at 60 s as the client's 503. With `data_plane_slc` ON, tenant zero's PUTs lose that retry.
- **RB5-6 (P4) — the isolate budget couples requests.** A codec write whose client stalls mid-body holds its unflushed batch of the isolate's 96 MiB until the client sends more; a window waits behind it. Dark with `edge_codec`.
- **RB5-7 (P4) — a node reads no window** (it names no feature): a large GET through a node is one stream; a chunked row is the origin's.
- **RB5-8 (P4) — the page for "a tenant's only node is down" is not built**: metrics carry no tenant label by policy; `DataPlaneHostDown` warns per host.
- **RB5-9 (P4) — the edge routes mount only with `EDGE_SERVICE_TOKEN`**: a deployment with nodes and no Worker still needs the service token set.
- **RB5-10 (P4) — a node keeps no access log of its own** (Step 0.3's rule); its metrics and the control plane's rows are the record.

## Open questions (for the owner)

1. **RB5-5**: keep host=slc's single PUT (the client retries a 503), or teach the Go host the driver's one retry on a fresh connection? Today: single.
2. **D-EDGE-38's double meter**: a BYO host's fallback is metered at the admitted span even when the origin then serves it (and meters it). Acceptable, or charge only the origin's? Today: both.
