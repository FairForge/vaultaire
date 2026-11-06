# WP-LOG-2 — a data-plane host keeps no log; the control plane's rows are the record

*Stage 1a side prompt, outside the Prompt 60 chain (`docs/reviews/WP-LOG-2-PROMPT.md`, the Opus edition). Session 2026-11-05. Lane: Engine (with one Trust file, `produce.json`). **Migration 129** (`129_request_duration.sql`; 128 is Prompt 60's claim). The Worker becomes **0.8.0**.*

Contract lines cited:
- **2** (every feature declares its mode): a host's report has closed kinds and six fields, never a URL, path, key or header; a duration is a number the client already saw.
- **12** (metadata has clocks): Cloudflare's per-request invocation event — the URL with bucket and key, the client's IP, postal code, coordinates, user agent, kept 7 days — was a copy of request metadata with an unstated clock. It is gone; the one switch that brings a trace back has its clock stated on `/legal/what-we-can-produce`.
- **13** (the tenant is the unit of disclosure): the customer's own log carries its own rows (`EdgeNoCommit`, `duration_ms`); a report names no tenant.
- Engine charter (`docs/surfaces/ENGINE.md`): the synthetic check's edge leg extends "synthetic check success" to the edge path.

## Why (measured 2026-11-05 on the production account, carried from the prompt)

- Deployed settings of `stored-s3-edge`: `observability {enabled:true, head_sampling_rate:1, logs:{invocation_logs:true}, redact_query_string:false}`. `stored-status` and `stored-mcp` log nothing.
- Every request wrote Cloudflare's invocation event (avg 5,459 B: the URL with bucket and key in clear, `cf-connecting-ip`, postal code, coordinates, user agent), kept 7 days; a data request also wrote one of our lines (829 B), with the request URL attached. `produce.json` named Cloudflare only as the R2 vendor.
- From 2026-12-01 Workers Logs is billed by GB ingested (50 GB included, $0.25/GB, shared with traces and R2 access logs).
- The synthetic check ran through `s3.stored.cloud` and never took the edge path (`pass get flag_off`, `pass put small`). No heartbeat from the Worker; `DataPlaneHostDown` the only data-plane rule; no counter of commit outcomes; an expired admit wrote no customer row; neither customer log had a duration.

## Step 0 — the answers

| # | Question | Answer |
|---|---|---|
| 0.1 | main, open PRs, the newest `edge-ingest` row | main `acc00561` (#1163, RP98). Open: #1124, #1078, #1025 — none touches `edge/s3-ingest/src`. Newest row: `edge-ingest` **0.7.3** `5b7162f9c3716a23d2fffe563c3d8d2848e59330149a7fad9ad5febfa31a37d7` at `d9204eed`. No Prompt 60 branch on the remote. |
| 0.2 | deployed settings | `{"enabled":true,"head_sampling_rate":1,"redact_query_string":false,"logs":{"enabled":true,"head_sampling_rate":1,"persist":true,"invocation_logs":true},"traces":{"enabled":false,"persist":true,"head_sampling_rate":1}}` |
| 0.3 | deployed bytes | `…/content/v2` parts: `index.js` 51,062 B sha256 `5b7162f9…` = the **0.7.3** row (= main's `src/index.js`); `codec.js` `5d1c7c11…` (edge-codec 0.6.0), `seal.js` `b211fcd4…`, `awschunked.js` `2ff68eb5…`, `wasm/chunker.wasm` `c48b60ca…`, `wasm/e2ee.wasm` `4d2d94d8…`, `wasm/wasm_exec.js` `0c949f49…`. **D = 0.7.3.** |
| 0.4 | the box | `SYNTHETIC_CHECK_URL=https://s3.stored.cloud` (1 line). The synthetic key's tenant `tenant-6cf6b63b58b95ca5`: `storage_limit_bytes` 5,368,709,120, no floors, `bandwidth_limit_bytes` NULL → egress allowance 0.5 × 5 GiB = **2.5 GiB** (under 10 GiB: the override is owed); a per-tenant `egress_throttle` OFF row (the 2026-11-02 exemption); no `edge_ingest` row. Active slot :8001 `30f44a5e8cd0`. Baseline: `admits{host="edge",op="get",outcome="pass_through"} 427`, `{op="put",outcome="pass_through_small"} 209`, every `admitted` 0; reaper `deleted 320`, `expired 0`; `vaultaire_synthetic_check_success 1`. `data_plane_hosts`: `s3.stored.cloud` (edge) and `slc`, both up. |

## What was built

| PR | What |
|---|---|
| **#1164** PR 1 (merged `b0c7e7af`) | `wrangler.toml` `[observability.logs] invocation_logs = false`; `test/observability.test.js` |
| **#1165** PR 2 (merged `55bbc883`, live 23:36 UTC) | migration 129 `duration_ms` (both logs); `EdgeNoCommit`; `vaultaire_data_plane_commits_total`, `_pass_through_total`; `POST /api/v1/edge/report` + `dataplane.ControlPlane.Report` + the node's `Reporter` |
| **#1170** PR 3 (merged `96035449`, live 00:45 UTC 2026-11-06) | the `edge_worker_metrics` job; the canary's edge leg; eight rules + promtool tests (17/17); the allocation row for the leg's body (`TestDataPathBounds_…` caught it) |
| **#1166** PR 4 (merged `3f32d17d`) | the Worker 0.8.0: `emit`, `EDGE_LOG`, reports, `guarded` |
| **#1171** PR 4b (merged `0b283cd6`; its Deploy run was superseded by #1170's, which carries it) | the 0.8.0 `client_build` rows: `edge-ingest` `92c68c75…`, `edge-codec` `a27ff837…` (log entries 225, 227) |
| PR 5 (this) | `produce.json` `edge_logs` + its render and test; docs; this note; the prompt |

## Decisions (made by the prompt; recorded)

- **D-LOG-1** Workers Logs stays enabled with invocation logs OFF and the Worker silent by default (`EDGE_LOG = "report"`). Verbose logging is a deploy-time var (`wrangler deploy --var EDGE_LOG:all`), never committed. No Tail Worker, no Logpush, no OTLP export.
- **D-LOG-2** A host's failure goes to the origin as a report: closed kinds, six fields, never a URL, path, key or header. The origin writes one Warn line and one counter. No table.
- **D-LOG-3** An admit that expires without a commit writes the customer's row: status 0, code `EdgeNoCommit`.
- **D-LOG-4** `duration_ms` is nullable on both logs; in the delivered access-log line it is the LAST field (`-` when unknown). No existing field moves.
- **D-LOG-5** Worker health is PULLED from Cloudflare's GraphQL dataset `workersInvocationsAdaptive` by a job; its result lives in `job_runs.result`; a failed call is the run's error.
- **D-LOG-6** The canary's edge leg is judged by the admit counter, never by a response header.
- **D-LOG-7** No new Worker module (each module is a `client_build` client and that list is closed).

Decisions of this session:

- **D-LOG-8 — the pass-through counter labels a reason by its family.** The prompt says every pass-through reason is built from constants; three call sites concatenate a backend NAME (`"backend:" + backend`, `"backend:" + row.backend`, `"backend:" + res.Upload.Backend`). The label is the reason cut at its first `:` (`backend:lyve` → `backend`), so the series stays bounded whatever a row names; the admit's wire `reason` is unchanged. `TestEdgePassThroughReasonsAreConstants` walks every call site (go/ast) and allows exactly: a literal, a package constant, a literal + `Kind.String()`, a literal + `strings.ToLower(<method>)`, a literal ending in `:`, and `edgeStreamingOf`'s reason (its returns checked to be literals).
- **D-LOG-9 — `edge_logs.days` is a JSON number** (`7`), the `Backups` shape the prompt names (a vendor, days > 0, a what); the prompt's text quoted it as a string.
- **D-LOG-10 — the codec's own log objects map onto the closed kinds**: `commit_unauthorized` and `commit_refused` report as `commit_failed`, `codec_checksum_mismatch` as `codec_failed` (the original name in `detail`); `codec` and `codec_index` are normal lines.
- **D-LOG-11 — a Go host reports `commit_failed` once**, after its second attempt (the Worker emits per attempt, as its lines did).

## Proofs

### PR 1 — invocation logs off (deployed 2026-11-05 ~23:00 UTC)

Main's `src/index.js` was the deployed row (Step 0.3), so the config rode its own deploy: `make edge-wasm` (every module rebuilt byte-identical to the deployed ones: `chunker.wasm` `c48b60ca…`, `e2ee.wasm` `4d2d94d8…`, `wasm_exec.js` `0c949f49…`), `check-withdrawn` clean, `npx -y wrangler@4.151.3 deploy` from `b0c7e7af` (#1164) with the zone user token and the production account → version **`1bc47613-af50-448c-82b5-613b08e969ee`**.

| Check | Result |
|---|---|
| settings API `.result.observability` | `{"enabled":true,"head_sampling_rate":1,"redact_query_string":false,"logs":{"enabled":true,"head_sampling_rate":1,"persist":true,"invocation_logs":false},…}` |
| deployed bytes (`content/v2`) | `index.js` `5b7162f9…` (the 0.7.3 row, unchanged), every other module unchanged — an identical-bytes deploy (RB-9 does not strike one) |
| `aws --profile stored --endpoint-url https://s3.stored.cloud s3 ls` | lists (`lightroom-backups`, `mac-offload`, …) |
| `vaultaire_synthetic_check_success` on the box | 1 |

### The report path in the lab (PR 4 step 15, 2026-11-05 23:59 UTC)

The origin: PR 2's build (`wp-log-2-origin`) on :8099, the private DB `vaultaire_wplog2`, `EDGE_SERVICE_TOKEN_FILE` (a fresh 48-hex token, 0600, deleted after), `EDGE_TRUST_ANY_PEER=true`, `STORAGE_MODE=local`; boot lines `edge service token: file` and `edge ingest routes mounted: … /api/v1/edge/report`. Between the Worker and it a stub on :8098 answering every admit **503** and forwarding every other call to the origin (the prompt's "make its admit answer non-200"). The Worker: PR 4's `src/` under `wrangler dev --local --port 8787 --local-upstream 127.0.0.1:8099` (see RL-5 for the entry shim), `.dev.vars` `VAULTAIRE_ORIGIN=http://127.0.0.1:8098`, `EDGE_LOG` = the committed `report`.

| Step | Result |
|---|---|
| a SigV4-shaped PutObject to the Worker | the admit answered 503 → the request passed through to the origin (its own 403 for the lab's fake signature) |
| the stub | `POST /api/v1/edge/admit` 503; `forward /api/v1/edge/report -> 200` |
| the origin's log | ONE line: `{"level":"warn","msg":"edge host report","host":"edge","kind":"admit_unavailable","request_id":"1706da67-…","admit_id":"","op":"put","status":503,"detail":""}` — the seven fields, nothing else |
| `vaultaire_data_plane_host_reports_total{host="edge",kind="admit_unavailable"}` | 0 → **1** |
| the Worker's own output (`report` mode) | no `admit_unavailable` line written |

Teardown: the three processes stopped by PID (the wrangler's respawned workerd too), no listener on 8787/8098/8099, the token and `.dev.vars` deleted.

### PR 2 on prod (`55bbc883`, active :8001 23:36 UTC)

Both columns `integer`; the canary's origin rows already carried durations (`GetObject 404 1 ms`, `DeleteObject 204 112 ms`, `GetObject 200 212 ms`); the boot line names `/api/v1/edge/report`; an unauthenticated `POST /api/v1/edge/report` 404; every `vaultaire_data_plane_commits_total{host="edge",…}` series exported at 0; 0 `"level":"error"` lines.

### The operator items (step 11, 2026-11-06 00:47–00:50 UTC, after #1170 was the active slot)

| Item | Done |
|---|---|
| `.env` | backup `.env.bak-wp-log-2-20261106T004714Z`; appended `CLOUDFLARE_ANALYTICS_TOKEN` (the value of that name in the creds file: written to a 0600 file, `scp -p`, appended by a script whose stdin carried only the sudo password, both copies shredded — never on a command line or a pipe into a heredoc), `CLOUDFLARE_ACCOUNT_ID` (the production account), `SYNTHETIC_CHECK_EDGE_EVERY=30m` |
| restart | one `sudo vaultaire-switch restart` → active blue :8000 `9603544982df`; boot lines `edge worker metrics on: Cloudflare's workersInvocationsAdaptive every 5 minutes` (`token_source: env`) and `synthetic customer check ON … edge_every 1800 edge_bytes 4194304`; 0 error lines |
| the flag | `feature_flags` `edge_ingest` / `tenant-6cf6b63b58b95ca5` / TRUE / `sql:wp-log-2-session` + its `audit_logs` `flag.set` row (`via: operator_sql`, the RE-5 route) |
| the egress override | the allowance was 2.5 GiB (Step 0.4) < 10 GiB: `tenant_quotas.bandwidth_limit_bytes = 53687091200` + the `admin.tenant_bandwidth_set` audit row `HandleUpdateBandwidthLimit` writes (`via: operator_sql`) |
| the rules | `vaultaire-dataplane.yml` `dd303984…`, `vaultaire-synthetic.yml` `160656c2…`, `vaultaire-jobs.yml` `b1001837…` (= main), each old file kept as `.bak-wp-log-2-<ts>`; `promtool check rules` SUCCESS (8, 4, 19 rules); Prometheus reloaded: the eight new rules load (148 rules in all) |

### The Worker 0.8.0 on prod (deployed 2026-11-06 00:48 UTC)

From main `96035449` (after #1171's rows were in the active slot's log): `make edge-wasm`; every module = its row (`index.js` `92c68c75…`, `codec.js` `a27ff837…`, `seal.js` `b211fcd4…`, `awschunked.js` `2ff68eb5…`, `chunker.wasm` `c48b60ca…`, `e2ee.wasm` `4d2d94d8…`, `wasm_exec.js` `0c949f49…`); `check-withdrawn` clean; `npx -y wrangler@4.151.3 deploy` → version **`e00e5b6c-6327-4395-a1e8-098d2dc7f02b`**, bindings `VAULTAIRE_ORIGIN`, `ENVIRONMENT`, `EDGE_LOG ("report")`.

| Check | Result |
|---|---|
| downloaded back (`content/v2`) | every part = its row (`index.js` `92c68c75…`) |
| settings | `logs.invocation_logs: false`; binding `EDGE_LOG` = `report` |
| RB-9 (one signed request after a content deploy, discarded) | `s3 ls s3://lightroom-backups/` through `s3.stored.cloud` answered normally — no 403 this time |
| tenant zero, aws-cli defaults, a 16 MiB random file to `lightroom-backups/wp-log-2-proof/16m.bin` and back | MD5 `7e04788d…` both ways |
| `edge_admits` since 00:48:40 | `part edge committed lyve 8388608` ×2, `get edge committed lyve 8388608` ×2 |
| `request_log` (the customer's) | `UploadPart edge 200` **412 ms** / **443 ms**, `GetObject edge 206` **50 ms** / **200 ms** (the host's own `duration_ms`); `InitiateMultipartUpload` / `CompleteMultipartUpload` / `HeadObject` `origin` 5 / 761 / 0 ms |
| commits | `vaultaire_data_plane_commits_total{host="edge",op="part",outcome="ok"} 2`, `{op="get",outcome="served"} 2` |
| reports / errors | no `vaultaire_data_plane_host_reports_total` above 0; 0 error lines |
| the proof object | deleted; the prefix lists empty |
| `edge_worker_metrics` | first run ok (00:48:57 UTC): `vaultaire_edge_worker_requests{script="stored-s3-edge",status="success"} 45`, `{script="stored-status",…} 14`; CPU p99 2.1 ms |

### The canary's edge leg (step 11's verify, 2026-11-06 01:17 UTC)

The leg at boot (00:47:29) ran before the flag row existed and took the origin path, as it should have; the next, 30 minutes later, took the edge:

| Series / row | Value |
|---|---|
| `vaultaire_edge_worker_requests{script="stored-s3-edge",status="success"}` | 44 (> 0) |
| `vaultaire_synthetic_check_edge_success` / `_edge_enabled` | 1 / 1 (last success 01:17:32) |
| `vaultaire_data_plane_admits_total{host="edge",op="put",outcome="admitted"}` | 1 (Step 0 baseline 0) |
| `vaultaire_data_plane_admits_total{host="edge",op="get",outcome="admitted"}` | 3 (baseline 0; 2 are the tenant-zero proof's) |
| `vaultaire_data_plane_commits_total{host="edge",…}` | `put ok` 1, `get served` 3, `part ok` 2 |
| `request_log` of `tenant-6cf6b63b58b95ca5`, `host = 'edge'` (read-only) | `PutObject edge 200 1049 ms` 01:17:31, `GetObject edge 200 76 ms` 01:17:32 |
| Prometheus alerts of the eight rules | none firing or pending (`DataPlaneEdgePathUnused`'s boot-time pending cleared by the leg) |
| `"level":"error"` lines, 35 min | 0 |

[YOU], optional: re-assert the canary tenant's `edge_ingest` flag once from `/admin/flags` (it writes the admin's own audit row, RE-5).

## Residuals

- **RL-1 (by design)** A request Cloudflare refuses before the Worker runs (its 100 MB 413) is seen by no one of ours.
- **RL-2 (by design)** A host failure while the origin is unreachable: the report cannot arrive. The canary's edge leg and `EdgeWorkerErrors` (Cloudflare's own counts) are the signal.
- **RL-3** `catalogue_unavailable` is emitted from `codec.js` through a sink with no `ctx`: its report is an unawaited fetch inside a request that is still running (a codec write), not a `ctx.waitUntil`.
- **RL-4** The Mac's `gosec` cannot type-check Go 1.26 (`export data version 5`): CI's gosec is the check of record (green on #1165).
- **RL-5 (found, not caused)** `wrangler dev` (4.151.3's workerd) refuses to start the Worker — main's 0.7.3 as well as 0.8.0 — because the entry module exports non-function values (`FORM_PREFIX_MAX` since 0.7.0, `WINDOW_ISOLATE_BYTES`, and 0.8.0's `REPORT_KINDS`): `Incorrect type for map entry 'FORM_PREFIX_MAX': the provided value is not of type 'function or ExportedHandler'`. Production accepts the same script. The lab ran a throwaway copy whose `entry.js` re-exports only `default` (`index.js` byte-identical); the README's lab section does not say so yet. A fix (the tests' pure parts in a module of their own) would add a module, which D-LOG-7 forbids here.

## Open questions (for the owner)

1. **`redact_query_string`** is a field of the settings API (`false` today) with no documented `wrangler.toml` key. With invocation logs off it applies only to the diagnostic mode; setting it by the API would be overwritten by the next `wrangler deploy` unless wrangler learns the key. Leave it, or set it by API after each verbose deploy?
2. D-LOG-8 (above): the family label hides WHICH backend a `backend:` pass-through named. The Debug line of the admit still names it. Enough?
