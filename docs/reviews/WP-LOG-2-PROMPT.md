Stage 1a side prompt (outside the Prompt 60 chain): WP-LOG-2 — a data-plane host keeps no log; the
control plane's rows are the record — Opus edition

Continue from WP-LOG-1 (the request log, migration 115) and WP-EDGE-3/4 (`data_plane_hosts`, the node,
the mirror). Most recent merged PR: #1163 (RP98, docs). The Worker `stored-s3-edge` is 0.7.3 (manifest
row `5b7162f9…`, deployed 2026-11-04 23:51 UTC). In flight elsewhere: Prompt 60 (WP-EGRESS-1) — its Part
0 ships the Worker 0.7.4 and claims migration 128. Open PRs #1078, #1124, #1025 do not collide.

Read first, before any edit: "About the Opus editions" in docs/reviews/WP-EDGE-PROMPTS.md (its rules
are yours: TDD red first, a step that cannot go as written is a STOP into the closeout's open
questions, work in `git worktree add ../vaultaire-wp-log-2 origin/main`, never `git checkout` in the
shared checkout, merge with `gh pr merge --auto`, never `--admin`, secrets never printed, never `pkill`
by pattern); CLAUDE.md (the `edge_admits` and `job_runs` bullets, Architecture Decisions 7 and 8);
internal/api/CLAUDE.md; plan § 40.10 (docs/IMPLEMENTATION_PLAN.md ~3054) and § 43.1; docs/reviews/
WP-EDGE-1A.md (RE-5, RB-9) and WP-EDGE-3.md; edge/s3-ingest/README.md (Files, Configuration, Deploy);
docs/ARCHITECTURE_CONTRACT.md rules 2, 12, 13 and docs/surfaces/ENGINE.md (cite both in every PR body).

Lane: Engine (with one Trust file, produce.json). Migration: the lowest number neither on main nor
claimed by an open PR when PR 2 opens — 129 if 128 is still Prompt 60's (latest on main:
127_transfer_jobs.sql). Five PRs; PRs 1–3 touch no Worker source and may run beside Prompt 60.

Scope: Cloudflare's log store stops holding customer requests, the Worker and the node send their
failures to the origin, the origin alerts on its own admit/commit rows and on Cloudflare's free Worker
metrics, the canary takes the edge path, and the customer's logs gain the two things they lack (a row
for a transfer whose commit never came; how long a request took).

Why (measured 2026-11-05 on the production account — carry these, do not re-measure):
- Deployed settings of stored-s3-edge: `observability {enabled:true, head_sampling_rate:1, logs:
  {invocation_logs:true}, redact_query_string:false}`. stored-status and stored-mcp log nothing.
- Every request writes Cloudflare's invocation event (avg 5,459 B: the URL with bucket and key in
  clear, cf-connecting-ip, postal code, coordinates, user agent), kept 7 days; a data request also
  writes one of our lines (829 B), and Cloudflare attaches the request URL to that line too.
  internal/api/landing/produce.json names Cloudflare only as the R2 vendor.
- From 2026-12-01 Workers Logs is billed by GB ingested (50 GB included, $0.25/GB, shared with traces
  and R2 access logs — developers.cloudflare.com/observability/pricing/).
- The synthetic check runs through s3.stored.cloud and never takes the edge path (its lines are `pass
  get flag_off` and `pass put small`). The Worker sends no heartbeat; `DataPlaneHostDown` is the only
  data-plane rule; no counter counts commit outcomes; an admit the reaper expires writes no customer
  row although an expired GET is metered; neither customer log has a duration.

Decisions (made; do not reopen):
- D-LOG-1 Workers Logs stays enabled with invocation logs OFF and the Worker silent by default
  (`EDGE_LOG = "report"`). Verbose logging is a deploy-time var (`wrangler deploy --var EDGE_LOG:all`),
  never committed. No Tail Worker, no Logpush, no OTLP export.
- D-LOG-2 A host's failure goes to the origin as a report: closed kinds, six fields, never a URL, path,
  key or header. The origin writes one Warn line and one counter. No table.
- D-LOG-3 An admit that expires without a commit writes the customer's row: status 0, code
  `EdgeNoCommit`.
- D-LOG-4 `duration_ms` is nullable on both logs; in the delivered access-log line it is appended as
  the LAST field (`-` when unknown). No existing field moves.
- D-LOG-5 Worker health is PULLED from Cloudflare's GraphQL dataset `workersInvocationsAdaptive` by a
  job; its result lives in `job_runs.result`; a failed call is the run's error.
- D-LOG-6 The canary's edge leg is judged by the admit counter, never by a response header (the edge's
  response headers stay the origin's).
- D-LOG-7 No new Worker module (each module is a `client_build` client and that list is closed).

Step 0 — before the first edit, answers into the closeout note
1. `git fetch -q && git log --oneline -5 origin/main && gh pr list --state open`. Record the newest
   `edge-ingest` row of internal/translog/clientbuilds/manifest.json and whether any open PR touches
   edge/s3-ingest/src.
2. Deployed settings: `curl -s -H "Authorization: Bearer $CLOUDFLARE_ZONE_TOKEN"
   https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_PROD_ACCOUNT_ID/workers/scripts/stored-s3-edge/settings
   | jq .result.observability` (both from ~/fairforge/.cloudflare-creds.env, sourced, never echoed).
3. Deployed bytes: download `…/workers/scripts/stored-s3-edge/content/v2`, sha256 its index.js, name
   the manifest row it equals. This is the DEPLOYED version D.
4. On the box (`ssh vaultaire-slc`): `grep -c '^SYNTHETIC_CHECK_URL=https://s3.stored.cloud'
   /opt/vaultaire/configs/.env` (expect 1); the synthetic key's tenant id (`api_keys` by key id), its
   quota and egress allowance; `curl -s localhost:$(cat /opt/vaultaire/ACTIVE_PORT)/metrics | grep
   vaultaire_data_plane_admits_total` as the baseline.

PR 1 — invocation logs off (config only). Branch `wp-log-2-config`.
1. edge/s3-ingest/test/observability.test.js (new, red first): wrangler.toml has `[observability]`
   `enabled = true` and `[observability.logs]` `invocation_logs = false`. Then add to wrangler.toml,
   below `[observability]`: `[observability.logs]` / `invocation_logs = false`. Verify: `make test-edge`.
2. Deploy ONLY IF `shasum -a 256 edge/s3-ingest/src/index.js` on main equals D's row (Step 0.3); if main
   is ahead of the deployed script, STOP: the setting rides that version's own deploy — say so in the
   note. Deploy: `make edge-wasm && cd edge/s3-ingest && npx -y wrangler@4.151.3 deploy` with
   CLOUDFLARE_API_TOKEN = the zone user token, CLOUDFLARE_ACCOUNT_ID = the production account.
3. Verify: Step 0.2's command answers `logs.invocation_logs: false`; `aws --profile stored
   --endpoint-url https://s3.stored.cloud s3 ls` still lists; `vaultaire_synthetic_check_success 1` on
   the box.

PR 2 — the origin: the customer's log is whole, the commit is counted, hosts can report. Branch
`wp-log-2-origin`. Migration NNN_request_duration.sql: `ALTER TABLE request_log ADD COLUMN IF NOT EXISTS
duration_ms INTEGER; ALTER TABLE s3_access_log ADD COLUMN IF NOT EXISTS duration_ms INTEGER;` (NULL =
not measured; the previous build's inserts leave it NULL).
4. Duration. `s3AccessEvent` (access_log.go:20) gains `durationMS int64` (-1 = unknown → NULL). The
   origin: `started := time.Now()` at the top of `handleS3Request`, passed to the deferred
   `recordS3Access` (s3.go:474, :796); `logManagementRequest` (request_log.go:272) the same. The edge:
   `edgeAdmitRow` gains an unscanned field `HostDurationMS`, set from `req.DurationMS` at every commit
   entrance right after the row is loaded (`grep -n "DurationMS\|lockEdgeAdmit(" internal/api/edge_*.go`),
   read by `recordEdgeAccess` (edge_ingest.go:1780). Writers: `Flush` (access_log.go:220) and
   `writeRequestLog` (request_log.go:109, `requestLogInsertSQL`). Readers: `requestlog.Row` gains
   `DurationMS *int64 json:"duration_ms,omitempty"` (requestlog.go:19, `Read` :95), the Developers
   page a "ms" column, `formatAccessLogLine` (access_log.go:233) the last field. Run
   `go test ./internal/database/ ./internal/api/ ./internal/account/ -run
   'SQLLiteralsMatchSchema|ExportSectionsCoverSchema|ErasedTablesCoverSchema|Produce'` and do what their
   messages demand for the two columns.
5. `EdgeNoCommit`. In `EdgeAdmitReaper.RunOnce` (edge_reaper.go:89), after `expireOne` returned
   expired = true (never inside its transaction) and `row.Host != dataplane.HostSLC`: record one event
   — op as `recordEdgeAccess` maps it, status 0, errorCode `EdgeNoCommit`, bytesSent = `row.ContentLength`
   for a GET (what `expireEdgeAdmit` metered) else 0, the row's client IP, user agent, key id, request
   id, host, durationMS -1. `recordEdgeAccess` turns status 0 into 200: add a sibling
   `recordEdgeNoCommit(row)`, do not change that rule. `shouldRecord` (access_log.go:133): an event
   with an error code is an error row whatever its status. `requestSuggestions` gains `EdgeNoCommit`:
   "The transfer was admitted at the edge and its result never reached us: the client may have
   disconnected, or the edge host failed. A download is metered for its admitted size."
6. `vaultaire_data_plane_commits_total{host,op,outcome}` beside `dataPlaneAdmits` (edge_ingest.go:93),
   registered in prom_metrics.go:103: one increment per accepted commit call; outcome = the request's
   `outcome` when it is one of ok, backend_error, edge_error, checksum_mismatch, served, fallback,
   error — else `other`; a commit of a row already terminal counts `replay`.
   `vaultaire_data_plane_pass_through_total{host,op,reason}` incremented in `countEdgeAdmit` for both
   pass-through cases, reason = `resp.Reason` (`unknown` when empty). Every reason is built from
   constants (the three concatenations take `placement.Kind.String()` or a method name): assert it in
   a test that walks the call sites' literals.
7. `POST /api/v1/edge/report` — internal/api/edge_report.go, mounted in `registerEdgeRoutes`
   (edge_ingest.go:245; extend its Info line), auth `s.edgeCaller(r)` exactly as
   `handleEdgeHeartbeat` (data_plane_heartbeat.go:25): not ok = `s.edgeNotFound`. Body ≤ 16 KiB:
   `dataplane.HostReport{Reports []HostReportItem; Suppressed int}`, item = `{kind, request_id,
   admit_id, op, status, detail}` (JSON names), at most 32 items (more = 400 `too_many`). Per item:
   kind outside `dataplane.ReportKinds` (admit_failed, admit_unavailable, commit_failed,
   codec_unavailable, codec_failed, mirror_settle_failed, post_short_body, post_unread, refused,
   catalogue_unavailable, exception) becomes `other`; detail cut to 512 bytes, control characters
   removed; one Warn line `edge host report` (host, kind, request_id, admit_id, op, status, detail);
   `vaultaire_data_plane_host_reports_total{host,kind}` +1. `Suppressed` adds to kind `suppressed`.
   Answer 200 `{}`; needs no database. Add the route to `TestEdgeRoutes_NeverCapturedByTheS3Gate`.
   `dataplane.ControlPlane` (controlplane.go:32) gains `Report(ctx, HostReport) error`; httpcp.go
   implements it; the node (internal/dataplane/host.go, internal/node/node.go) sends `admit_failed` /
   `commit_failed` where it logs them today — detached, 5 s, at most 20 a minute, never on the
   request's path.
   Verify PR 2: `make test-db TEST_DB=vaultaire_wplog2`, then the package tests on that DATABASE_URL,
   `make lint`, `make gosec`. Merge; wait for the Deploy job; `/version` on prod names the commit.

PR 3 — the origin: Cloudflare's metrics, the rules, the canary's edge leg. Branch `wp-log-2-watch`.
8. Job `edge_worker_metrics` (internal/api/edge_worker_metrics.go; `jobSpec` jobs.go:98; Every 5 min,
   BootDelay 90 s, MaxRunTime 30 s; the result-in-`job_runs.result` pattern of backend_capacity.go +
   metrics_capacity.go). Registered only with BOTH `CLOUDFLARE_ANALYTICS_TOKEN` (read with
   `secret.Load(getenv, readFile, "cloudflare_analytics_token", "CLOUDFLARE_ANALYTICS_TOKEN")`,
   internal/recovery/secret/source.go:43, wired like btcpay_wiring.go) and `CLOUDFLARE_ACCOUNT_ID` (32
   hex): neither = one Info line; one = one Error line, not registered. One POST to
   https://api.cloudflare.com/client/v4/graphql (the URL a struct field so the test uses httptest),
   `Authorization: Bearer`, 20 s timeout, body:
   `{ viewer { accounts(filter:{accountTag:"<id>"}) { workersInvocationsAdaptive(limit:200,
   filter:{datetime_geq:"<now-15m RFC3339>", datetime_leq:"<now>"}) { sum { requests errors subrequests }
   quantiles { cpuTimeP99 wallTimeP99 } dimensions { scriptName status } } } } }` (verified live
   2026-11-05; cpuTimeP99 is microseconds; statuses seen: success, clientDisconnected). A non-200, a
   GraphQL `errors` array or a body that does not parse is the run's error (status and the first 200
   bytes, never the token). Gauges read from the last result on scrape:
   `vaultaire_edge_worker_requests{script,status}` (the last 15 minutes),
   `vaultaire_edge_worker_cpu_p99_seconds{script,status}`. Add the job to `PeriodicJobStale`'s
   alternation (vaultaire-jobs.yml:55) and its description.
9. The canary's edge leg (synthetic_check.go: constants :43, `cycle` :177, `send` :268, collector
   :315). `SYNTHETIC_CHECK_EDGE_EVERY` (a duration ≥ 5m; unset or `off` = no leg) and
   `SYNTHETIC_CHECK_EDGE_BYTES` (default 4194304, 1 MiB–64 MiB); a rejected value is a Warn and the
   default. After a small cycle, when the last leg is older than EVERY: PUT a random body, GET it whole
   comparing sha256 as it streams (never two copies in memory), DELETE; 60 s per op; its failure never
   changes `vaultaire_synthetic_check_success`. Series: `vaultaire_synthetic_check_edge_enabled`,
   `_edge_success`, `_edge_last_success_timestamp_seconds`; ops `edge_put`, `edge_get`, `edge_delete`
   in the existing latency and failure vecs.
10. Rules, each with a promtool test (`bash deploy/monitoring/promtool-test.sh`; follow the existing
   file pairing). vaultaire-dataplane.yml, all `severity: warning`:
   - DataPlaneAdmitsExpiring, for 15m: `sum(increase(vaultaire_edge_admit_reaper_total{outcome="expired"}[1h]))
     >= 5 and sum(increase(vaultaire_edge_admit_reaper_total{outcome="expired"}[1h])) /
     clamp_min(sum(increase(vaultaire_data_plane_admits_total{outcome=~"admitted.*"}[1h])), 1) > 0.02`
   - DataPlaneCommitsFailing, for 15m, by host: the share of outcomes
     `backend_error|edge_error|checksum_mismatch|error` in `vaultaire_data_plane_commits_total` over 30m
     above 0.05 with at least 5 such commits.
   - DataPlaneHostReporting, for 10m: `sum by (host, kind)
     (increase(vaultaire_data_plane_host_reports_total{kind!="exception"}[15m])) > 10`
   - DataPlaneHostException: `sum by (host) (increase(vaultaire_data_plane_host_reports_total{kind="exception"}[15m])) > 0`
   - EdgeWorkerErrors, for 10m: `vaultaire_edge_worker_requests{status!~"success|clientDisconnected"} > 0`
   - EdgeWorkerCPUNearLimit, for 15m: `vaultaire_edge_worker_cpu_p99_seconds{script="stored-s3-edge"} > 240`
     (80 % of wrangler.toml's `cpu_ms = 300000`)
   - DataPlaneEdgePathUnused, for 30m: `vaultaire_synthetic_check_edge_enabled == 1 and on()
     sum(increase(vaultaire_data_plane_admits_total{host="edge",outcome=~"admitted.*"}[2h])) < 1`
   vaultaire-synthetic.yml: SyntheticEdgeLegFailing, for 65m: `vaultaire_synthetic_check_edge_enabled == 1
   and vaultaire_synthetic_check_edge_success == 0`.
11. Operator items, after PR 3 is the active slot (yours; each with a `.env` backup
   `.env.bak-wp-log-2-<UTC>` and one `sudo vaultaire-switch restart` for all three lines):
   `CLOUDFLARE_ANALYTICS_TOKEN` (the value of that name in ~/fairforge/.cloudflare-creds.env: write it
   to a 0600 temp file, scp, append on the box, delete both — never a pipe into `ssh … <<EOF`),
   `CLOUDFLARE_ACCOUNT_ID`, `SYNTHETIC_CHECK_EDGE_EVERY=30m`. The synthetic tenant gets `edge_ingest`
   by RE-5's route (a `feature_flags` row plus the `flag.set` audit row with `via: operator_sql`). Its
   leg downloads 4 MiB × 48 a day ≈ 6 GiB a month: if its egress allowance (Step 0.4) is under 10 GiB
   set `tenant_quotas.bandwidth_limit_bytes = 53687091200` with the audit row the admin route writes
   (`HandleUpdateBandwidthLimit`), `via: operator_sql`. Install the three rule files per
   deploy/monitoring/README.md after `promtool check rules`.
   Verify (one table in the note): `vaultaire_edge_worker_requests{script="stored-s3-edge",
   status="success"}` > 0; 35 minutes later `vaultaire_synthetic_check_edge_success 1`, admits
   `{host="edge",op="put",outcome="admitted"}` and commits `{outcome="ok"}` / `{outcome="served"}` above
   the baseline; on the box, read-only, `SELECT op, host, status, duration_ms FROM request_log WHERE
   tenant_id = '<the synthetic tenant>' AND host = 'edge' ORDER BY id DESC LIMIT 3` shows the leg's
   rows with a duration. (sudo on the box: the Keychain item `vaultaire-slc-sudo` piped to `sudo -S`.)

PR 4 — the Worker: silent by default, failures reported. Branch `wp-log-2-worker`. START ONLY when no
open PR touches edge/s3-ingest/src and main's newest `edge-ingest` row is deployed (Step 0.1/0.3 again);
otherwise finish PRs 2–3 and wait — never two Worker versions in flight. Version: the next minor after
main's newest (0.8.0 after 0.7.x) in package.json and a new README heading (test/version.test.js).
12. src/index.js: `emit(env, ctx, kind, fields)` is the ONE place a line or a report leaves the Worker;
   replace all 19 logging sites with it (the `log` helper :546 and its two uses, :693, :696, :715, :759,
   :847, :925, :971, :982, :1001, :1009, :1024, :1050, :1065, :1078, :1083, :1088, :1097). Normal kinds:
   pass, get, put, part, post, mirror_settled. Failure kinds: `dataplane.ReportKinds` above.
   `env.EDGE_LOG`: `report` (also when unset) — failures are reported, nothing is written; `errors` —
   failures reported and written; `all` — every kind written, failures also reported; `off` — nothing.
   A report is `{reports:[{kind, request_id, admit_id, op, status, detail}]}` — exactly these six
   fields, detail = the error's message cut to 300 characters, never the URL, the path or a header —
   sent `ctx.waitUntil(controlPlane(env, "/api/v1/edge/report", body, 5000).then(r => r.body?.cancel(),
   () => {}))`; thread `ctx` to the sites that lack it. A module-scope bucket allows 20 reports a
   minute per isolate; the rest are counted and sent as `suppressed` on the next report.
   src/codec.js:493: no `console`; `export function setEventSink(f)` and index.js sets it to `emit`.
13. The top-level catch: today's `fetch` body becomes `async function handle(request, env, ctx)`;
   `fetch` awaits it in try/catch. On a throw: `emit` kind `exception` (detail = message plus the first
   stack line); then `passThrough(request)` when `request.bodyUsed` is false, else `slowDown(path, a new
   request id, null)` (:587; `withCORS` already takes no admit). Today a throw is Cloudflare's HTML
   1101 page.
14. wrangler.toml `[vars]` gains `EDGE_LOG = "report"`. Tests (node --test): test/emit.test.js — the
   mode × kind table, the six-field shape, the cap and `suppressed`, the sink from codec.js;
   test/observability.test.js — `EDGE_LOG = "report"` is committed, and `console.` occurs exactly once
   in src/index.js (inside `emit`) and nowhere in codec.js, seal.js, awschunked.js; the catch — a
   throwing handler with an unused body passes through, with a used body answers 503 XML with
   `x-amz-request-id`, and reports one `exception`. Verify: `make test-edge`.
15. Release, in the README's order: merge; a follow-up PR adds the `edge-ingest` and `edge-codec` rows
   from the merge commit; deploy as PR 1 step 2 once PR 2's origin is the active slot; the deployed
   index.js downloads back equal to its row. One signed request after a content-changing deploy may be
   refused 403 (RB-9): send one and discard it before the proof. Proof on prod: the Step 0.2 command
   shows `invocation_logs: false`; an `aws s3 cp` of a 16 MiB file up and down through
   https://s3.stored.cloud by tenant zero is MD5-equal; the canary's next edge leg is green. The report
   path is proven in the lab (a local origin with `EDGE_TRUST_ANY_PEER=true` on the private DB and
   `npm run dev`; make its admit answer non-200 → one `admit_unavailable` Warn line and the counter).

PR 5 — the page, the docs, the closeout. Branch `wp-log-2-docs`.
16. internal/api/landing/produce.json gains `"edge_logs": {"vendor": "Cloudflare", "days": "7",
   "what": "Cloudflare runs the program that moves large uploads and downloads on s3.stored.cloud. In
   normal operation that program writes no log of a request at Cloudflare. If we switch on its
   diagnostic log during an incident, Cloudflare keeps for up to 7 days the address of each request
   made meanwhile, as sent (it names the bucket and the object, and for a presigned link its query),
   and the program's own line about it; never the content."}`, loaded and validated by
   internal/produce/produce.go (the `Backups` struct :59 is the shape: a vendor, days > 0, a non-empty
   what) and rendered beside the backups rows (`grep -rn "PITR" internal/api internal/produce` finds
   the template). internal/api/produce_test.go:
   `TestProduceEdgeLogsAreTheWorkerConfig` fails unless edge/s3-ingest/wrangler.toml holds
   `invocation_logs = false` and `EDGE_LOG = "report"`.
17. Docs: docs/CLOUDFLARE.md (a "Worker logging" section: the settings, the debugging switch and its
   7-day trace, the pricing change, the Why numbers above); plan § 40.10 gains the sentence "A
   data-plane host keeps no record of a request after it ends; the control plane's rows are the
   record."; docs/API.md (`duration_ms`, the access-log line's last field, `EdgeNoCommit`); CLAUDE.md
   (the env rows `CLOUDFLARE_ANALYTICS_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `SYNTHETIC_CHECK_EDGE_EVERY`,
   `SYNTHETIC_CHECK_EDGE_BYTES`; the `edge_admits` bullet; the migration count); internal/api/CLAUDE.md;
   deploy/monitoring/README.md; edge/s3-ingest/README.md (Configuration: `EDGE_LOG`); docs/STATUS.md.
   Closeout docs/reviews/WP-LOG-2.md: Step 0 answers, D-LOG-1…7, the proof tables, residuals, open
   questions. Commit this prompt as docs/reviews/WP-LOG-2-PROMPT.md (it is untracked in the shared
   checkout).

Tests that must exist (red first, at the handlers, private DB)
- TestRequestLog_DurationIsRecorded: an origin PUT and a management call each leave `duration_ms` ≥ 0;
  a row written without it reads back with the field absent.
- TestAccessLogLine_DurationIsTheLastField: every earlier field is byte-identical to today's line.
- TestEdgeCommit_RecordsTheHostsDuration: a commit with `duration_ms: 1234` writes 1234.
- TestEdgeReaper_ExpiredAdmitWritesEdgeNoCommit: a GET admit past its expiry → one request-log row
  (status 0, `EdgeNoCommit`, bytes_out = the admitted size, its suggestion); a `host=slc` row writes none.
- TestEdgeCommit_CountsOutcome: ok, backend_error, an unknown outcome (`other`), a replay.
- TestEdgeAdmit_PassThroughReasonIsCounted and TestEdgePassThroughReasonsAreConstants.
- TestEdgeReport_*: not the edge caller → 404; an unknown kind → `other`; detail cut and cleaned; 33
  items → 400; `suppressed` counted; the Warn line holds no field beyond the seven.
- TestEdgeWorkerMetrics_*: unregistered without both settings; a good answer → the gauges; a non-200
  and a GraphQL error → the run's error, the token in no log line.
- TestSyntheticCheck_EdgeLeg*: off by default; runs once per EVERY; a body mismatch fails the leg and
  not the small cycle.
- Structural: TestEdgeRoutes_NeverCapturedByTheS3Gate, TestSQLLiteralsMatchSchema,
  TestExportSectionsCoverSchema, TestErasedTablesCoverSchema, the produce tests, `make test-edge`.

Architecture notes
- Never change: a D-EDGE decision, the edge's response headers, the admit or commit wire shapes beyond
  what is written here, a migration on main, `recordEdgeAccess`'s status rule, a format byte.
- Two-slot deploy: the previous build must serve the new schema (the columns are nullable) and a
  Worker that sends a report to a build without the route gets a 404 it ignores.
- What stays unseen by design: a request Cloudflare refuses before the Worker runs (its 413), and a
  failure while the origin is unreachable (the report cannot arrive; the canary and
  `EdgeWorkerErrors` are the signal). Record both as residuals, do not build for them.
- Not in scope: counting `/cdn` cache hits, a Tail Worker, per-colo latency, turning
  `[observability] enabled` off, query-string redaction (no documented wrangler key — record the API's
  `redact_query_string` field as an open question).
- [YOU], optional: re-assert the canary tenant's `edge_ingest` flag once from /admin/flags (it writes
  the admin's own audit row, RE-5).
- Commit: type(scope): description [WP-LOG-2]
