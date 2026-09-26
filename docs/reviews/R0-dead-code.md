# R0 Dead code & scaffolding triage — 2026-09-26

**Repo state reviewed:** `main` @ `3ad2be1` (#472). Branch: `review/R0-dead-code`.
**Tooling:** `go list -deps ./cmd/vaultaire`, `golang.org/x/tools/cmd/deadcode@v0.50.0`
(runs under go1.26.8 toolchain auto-download; results identical in shape to the 09-25 survey).

## Scope reviewed

| What | Measured |
|------|----------|
| `internal/` packages | **72** total, **23** linked into `cmd/vaultaire`, **49** never linked |
| `internal/` source LOC (non-test / test) | 131,819 / 103,392 |
| `deadcode ./cmd/vaultaire` | **924** unreachable functions across the 23 linked packages (plan said 928; 4 fewer after #469-#472) |
| 100%-dead files inside linked packages | **96** files, 18,524 LOC (every `func` in the file unreachable) |
| `cmd/*` directories | 23; only `cmd/vaultaire` is product, 22 are benches/probes/tools (table below) |
| Untracked root artifacts | ~35 build outputs, all gitignored, none tracked (verified with `git ls-files`) |

**Regenerated numbers vs. the 09-25 survey.** Same 49 unlinked packages. Per-package dead
function counts: drivers 219, cache 132, auth 130, crypto 104, engine 75, usage 63, rbac 62,
compliance 54, api 32, database 22, dashboard/handlers 12, billing 7, tenant 5, config 2,
dashboard/auth 2, dashboard/middleware 2, email 1.

**Skipped:** behaviour of live code (belongs to R1–R14). Partially-dead files (45, listed under
"Dead code noted") were inventoried but not edited — deleting individual functions from live files
is R-session work, not triage. `cmd/*` tools were classified but not reviewed for correctness.

**Method for every classification below:** (1) `deadcode` says every function in the file is
unreachable; (2) `grep -rl "<import path>"` shows no importer outside the package's own tests
(exceptions called out); (3) `docs/IMPLEMENTATION_PLAN.md` grepped for the package path, the file
basename and the feature keyword — plan line numbers cited where a hit exists. Unlinked packages
were also grepped in `Makefile`, `.github/`, `scripts/`, `tools/`, `deploy/` — no references.

## Findings

| ID | Sev | file:line | What | Why it matters | Proposed fix |
|----|-----|-----------|------|----------------|--------------|
| R0-01 | P3 | `docs/CODE_REVIEW_PLAN.md:45`, `internal/engine/engine.go:34,93-98,193,452,494,591,612,710` | Plan says "every function in `internal/cache` unreachable". False: `engine.go` constructs and calls `cache.TieredCache` (`Get/Set/Delete/HealthCheck/GetMetrics`). 16 of 17 cache files are dead; `tiered_cache.go` (76 LOC, a mutex-guarded `map[string][]byte`, no size bound) is live and is the engine's read cache. | Later sessions would skip a live, unbounded in-memory cache believing it deleted. R6 must review it (no eviction, no cap — `Config.MemorySize`/`SSDSize`/`SSDPath` are ignored at `tiered_cache.go:11-30`). | Corrected in the plan tracker by this session. R6: review or remove the engine cache (see WP-R0-3). |
| R0-02 | P2 | `internal/api/quota_management.go:132,145-151` | `handleDeleteQuota`/`handleUpdateQuota` read `mux.Vars(r)["tenant_id"]` (gorilla/mux) but the server is chi; `mux.Vars` on a chi request is always empty. Unreachable today only because `setupQuotaManagementRoutes` (145-151) is an empty function with the routes commented out. | If anyone uncomments those routes, DELETE `/api/v1/admin/quotas/{tenant_id}` calls `DeleteQuota(ctx, "")`. This file is also the **only** reason `github.com/gorilla/mux` is in `go.mod`. | R10: either port to `chi.URLParam` and register under the admin group, or delete the 4 handlers + `quota_management_test.go`; then `go mod tidy` drops gorilla/mux. Not fixed here (behavioural decision). |
| R0-03 | P3 | `docs/IMPLEMENTATION_PLAN.md:1134`, `internal/crypto/postquantum.go`, `internal/crypto/sse_s3.go:7` | Plan 5.14.4 lists `postquantum.go` as the SSE-S3 implementation file. SSE-S3 uses Go stdlib `crypto/mlkem`; `postquantum.go` (cloudflare/circl) is 13/13 dead. `crypto/CLAUDE.md:12,35` also describes it as "pipeline encryption" — that pipeline (`crypto/pipeline.go`) is 10/10 dead too. | `github.com/cloudflare/circl` is kept alive only by dead code; two ML-KEM implementations in one package invites the wrong one being used. | Decision D-3 below (recommend delete postquantum.go + pipeline.go, drop circl, fix plan line 1134 and crypto/CLAUDE.md). |
| R0-04 | P3 | `docs/IMPLEMENTATION_PLAN.md:955`, `internal/api/metrics.go`, `internal/api/prom_metrics.go` | Plan 5.12.10 (SHIPPED) names `api/metrics.go` as a deliverable file. The shipped Prometheus registry is `prom_metrics.go`; `metrics.go` is a 6/6-dead legacy stub kept alive only by `metrics_test.go`. | Two `Metrics` registries in one package; R1 would read the wrong file. | Decision D-4 (recommend delete metrics.go + metrics_test.go; fix plan line 955). |
| R0-05 | P3 | `docs/IMPLEMENTATION_PLAN.md:720,1204`, `internal/api/middleware.go`, `internal/api/server.go` | Plan 5.11.1 and 5.14.11 (both COMPLETE) name `api/middleware.go` as the file. `X-Vaultaire-Version` and the security headers are set in `server.go`; `middleware.go` (`RateLimitMiddleware`, `ExtractTenant`) is 2/2 dead. `RateLimiter` itself is live (`server.go:212` CDN limiter). | Same as R0-04. | Decision D-5 (recommend delete middleware.go + middleware_test.go; fix plan lines 720/1204). |
| R0-06 | P3 | `docs/IMPLEMENTATION_PLAN.md:775`, `internal/webhooks/webhook.go`, `internal/api/events.go` | Plan 5.11.6 (COMPLETE) says `internal/webhooks/webhook.go` "already has the delivery engine ... this phase exposes it". The shipped phase implemented its own HMAC dispatch in `api/events.go`; `internal/webhooks` is not imported anywhere. | 884 LOC unlinked package with a plan sentence that says it is the engine. | Decision D-6 (recommend delete; fix plan line 775). |
| R0-07 | P3 | `internal/database/test_config.go:1-16` | `//go:build !prod` file in the production `database` package hardcodes `User: "viera"`. `TestConfig()` has zero callers (deadcode + grep). Duplicates `GetTestConfig()` in `config.go`. | Personal dev username compiled into the product binary; misleading build tag nobody sets. | **Fixed here** (class D): deleted; test config moved to `internal/testutil`. |
| R0-08 | P3 | `internal/api/test_db_fix.go:5`, `internal/api/compat_test_helpers.go`, `tests/compatibility/s3_compat_test.go:108` | Non-`_test.go` file imports `"testing"` and defines `setupTestDBFixed(t *testing.T)`. `compat_test_helpers.go` exports `NewTestServer` + 5 `Handle*` wrappers; their only caller is the external `tests/compatibility` suite, so they cannot live in a `_test.go` file (deadcode flags them because `tests/` is not the binary). | Test scaffolding compiled into the product binary; exported `Handle*` wrappers on `Server` widen the API surface. | `test_db_fix.go` **fixed here** (→ `db_test_helpers_test.go` using `testutil`). `compat_test_helpers.go` **kept** — R15: either move `tests/compatibility` in-package or drive it through the router via `httptest` so the wrappers can go. |
| R0-09 | P3 | `internal/storage/CLAUDE.md:18-24` | Claims the package is "the foundation for Phase 8 (chunking+dedup), 9, 10, 11". Phase 8/9/10 shipped in `internal/crypto/{chunker,gci,compression,chunk_encryption}.go`; `internal/storage` is imported only by `internal/testing/coverage` (itself unlinked). | A per-directory CLAUDE.md that actively misdirects. | **Fixed here** (class A): package deleted with its CLAUDE.md. |
| R0-10 | P3 | `cmd/loadtest/main.go`, `internal/loadtest/*` (4,084 src + 3,167 test LOC) | A second load-test harness. The harness of record is `tests/load/` (plan 5.15.2, `bench-results/LOADTEST-2026-08-03.md`, `make test-load`). `cmd/loadtest` is the only importer of `internal/loadtest`, so the package is not "unimported" but it is tool-only. | 7k LOC of CI-built code duplicating a gate that already passed. | Decision D-7 (recommend delete both; `tests/load` stays). |
| R0-11 | P3 | `Makefile:59-61` | `make clean` removes only `bin/`. Root holds ~35 gitignored build outputs (`bench-*`, `permafrost-*`, `vaultaire-bin`, `*.bin`, `test*.txt`, `test_output.log`, `geyser-*-bin`, `loadtest*`, `lighthouse-bench`, `pixeldrain-bench*`, `quotaless-bench-v2*`, `vaultaire`, `vaultaire-linux`). None are tracked (`git ls-files` matches only `bench-results/*.md` and `tests/benchmarks/baseline_results.txt`, both intentional). | Clutter; a stale `./vaultaire` binary in root is easy to run by mistake. | R15 (Makefile item 8): extend `clean` — proposed target below. Not changed here (out of R0 scope by the prompt). |
| R0-12 | P3 | `internal/crypto/encryption.go` (26/27 dead), `internal/database/postgres.go` (15/17 dead), `internal/crypto/keymanager.go` (14/16), `internal/crypto/compression.go` (16/31), `internal/crypto/chunker.go` (10/14), `internal/rbac/*` (62 dead across 9 files, none 100%) | Live files that are mostly dead. `encryption.go`'s whole `Encryptor` interface family (AES-GCM/ChaCha/Noop) is unused — the product encrypts via `ChunkEncryptionService` and `SSEService`. `postgres.go`'s `Postgres` wrapper methods (`CreateTenant`, `GetArtifact`, `ListArtifacts`, ...) are unused. | Confuses which code path is real; `chunker.go`'s dead `FixedChunker`/`ChunkBytes` sit beside the live `FastCDCChunker.ChunkContext`. | Hand to R8 (crypto), R9 (database), R5 (rbac) — full list under "Dead code noted". |

No P0/P1 found: dead code by definition has no runtime behaviour. R0-02 is the only latent
behavioural hazard and is unreachable today.

## Classification

### Class A — scaffolding, no plan reference, no importer outside own tests → DELETED in this PR

41 packages, **35,555 src + 25,179 test LOC**. Every one was created from the old 1000-step
MASTER_PLAN ("[Step NNN]" commits, Aug–Dec 2025); later touches are lint/gosec/race sweeps only.

| Package | Src | Test | First commit | Notes |
|---------|----:|-----:|--------------|-------|
| alerting | 1,127 | 1,092 | 2025-12-04 Step 387 | 5.12.10 alerting shipped as Prometheus rules (`deploy/monitoring/`), not this |
| apikeys | 87 | 44 | 2025-10-01 | live key minting is `internal/auth/apikey.go` |
| apm | 389 | 289 | 2025-12-04 Step 384 | |
| audit | 2,679 | 1,742 | 2025-10-02 Step 331 | 3.6 audit viewer (#276) is `dashboard/handlers/admin_audit.go`, does not import this |
| devops | 8,781 | 6,179 | 2025-12-04 Step 391 | Dockerfile/CI/DNS/firewall generators; deploy is `.github/workflows/deploy.yml` |
| docs/{admin,architecture,developer,faq,guides,runbooks,support,troubleshooting,tutorials} | 5,094 | 3,259 | 2025-12-19 Step 463 | plan line 1285 (B3, DONE #355) already calls these "empty scaffolding, imported nowhere". Parent `internal/docs` (OpenAPI) is live and untouched |
| gateway, gateway/cache, gateway/metrics, gateway/validation | 2,117 | 1,680 | 2025-09-04 Step 131 | 13.1 names `internal/protocol/` (new), not gateway. Only importer of `gateway/validation` is `gateway` itself. Sole user of `xeipuuv/gojsonschema` |
| logger | 12 | 0 | 2025-08-20 | product uses Zap directly |
| logging | 508 | 395 | 2025-12-04 Step 382 | |
| metrics | 1,174 | 636 | 2025-12-04 Step 381 | live registry is `api/prom_metrics.go` |
| monitoring | 311 | 350 | 2025-12-04 Step 390 | |
| perf | 4,180 | 3,178 | 2025-12-14 Step 436 | |
| pipeline | 125 | 0 | 2025-08-11 | |
| queue | 459 | 381 | 2025-12-02 Step 377 | 22.1 explicitly reuses 5.10.12/5.11.6 event plumbing (line 2281) |
| ratelimit | 599 | 319 | 2025-09-03 | live limiters: `api/ratelimit.go`, `api/management_ratelimit.go`, `dashboard/middleware/ratelimit.go` |
| reporting | 482 | 348 | 2025-12-02 Step 379 | |
| retention | 645 | 186 | 2025-10-07 Step 342 | V18.5 (line 1025) extends `api/s3_handler.go` Object Lock, not this |
| storage | 902 | 279 | 2025-09-04 | see R0-09; importer `testing/coverage` is class A too |
| streaming | 484 | 340 | 2025-12-02 Step 376 | see queue |
| testing/{accessibility,api,browser,coverage,e2e,integration,load,performance,security,uat} | 4,932 | 4,161 | 2025-12-18 Step 458 | plan line 113 only mentions a flaky test here. Real integration/load tests live in `tests/` (imports only api, drivers, engine, tenant) |
| tracing | 468 | 321 | 2025-12-04 Step 383 | |

### Class B — plan phase ≥ 6 topical match → NOT deleted, decision needed

6 unlinked packages (**21,694 src + 17,611 test LOC**) plus 9 files inside live packages.
"Referenced" here means the phase's deliverable is what the package implements; none of these
are named by path in the plan.

| Item | LOC (src) | Plan reference | Recommendation |
|------|----------:|----------------|----------------|
| `internal/k8s` (10 files) | 8,337 | 15.3 Helm Chart, lines 1972-1974: "`deploy/helm/` (new) … Kubernetes deployment" | **Delete, rely on git.** 15.3 calls for a Helm chart under `deploy/`, not Go code that emits manifests/Istio/netpol YAML. Sole user (with devops) of `gopkg.in/yaml.v3` |
| `internal/container` (10 files) | 2,629 | 15.1 Docker Image, lines 1959-1960: "`Dockerfile` (new), `docker-compose.yml`" | **Delete.** 15.1 wants a Dockerfile, not a Dockerfile generator |
| `internal/global` (10 files) | 6,583 | 28.3 Multi-Region Hub, line 2371; 7.6 (COMPLETE #299, shipped in `engine/tiering.go`) | **Delete.** 28.3 is PG streaming replication + Cloudflare DNS failover (ops), not geo-routing code |
| `internal/ha` (8 files) | 3,364 | 28.3 / 28.4 DR drills, lines 2371, 2373 | **Delete.** Same reasoning; `DISASTER_RECOVERY.md` is the deliverable |
| `internal/slo` | 356 | 28.2 SLA Definition + Enforcement, line 2368 | **Delete.** Uptime accounting will come from the live `engine/health*.go` probes + Prometheus |
| `internal/integrations` | 425 | Phase 29 Ecosystem Integrations, line 2377 | **Delete.** Phase 29 lists Veeam/Nextcloud/Terraform/Zapier — none of which this generic HTTP-integration registry implements |
| `internal/engine/analytics.go` | 139 | 14.1 Access Pattern Analyzer, line 1926 names this file | **Quarantine or keep.** The plan names it explicitly; 139 LOC. Keep if 14.1 is still wanted |
| `internal/drivers/cost_advisor.go` | 233 | 14.2, line 1932: "`internal/engine/cost_advisor.go` (exists, expand)" — wrong package, it is in `drivers/` | **Keep, fix plan path.** Explicitly named |
| `internal/engine/capacity.go` | 211 | 5.12.9 Backend Capacity Planning, line 946 names this file (phase unstarted) | **Keep** if 5.12.9 stays on the roadmap; else delete |
| `internal/engine/replicator.go` | 189 | P3 Async Parity Replication, line 1826: "extends existing replicator" | **Keep.** Explicitly named as the base for P3 |
| `internal/compliance/soc2.go` | 813 | 28.1 SOC 2, line 2367 names it as evidence tracker; 5.14.3 lines 1125/1129 | **Keep.** Explicitly named twice |
| `internal/engine/load_balancer.go` | 186 | none — but imported by `internal/ha/lb_integration.go:24` (class B above), so it cannot go before `ha` does | **Delete with `ha` (D-1).** Was classified C; `go build ./...` proved the dependency |
| `internal/engine/sla.go` | 192 | 28.2, line 2368 | **Delete.** Not named; generic SLA monitor |
| `internal/engine/disaster_recovery.go` | 244 | 28.4, line 2373 | **Delete.** Not named; DR is runbooks |
| `internal/drivers/wasm.go` | 57 | 22.1 WASM Runtime, line 2281 ("Wasmtime embedded") | **Delete.** Plan says Wasmtime; this is a wazero stub. Sole user of `tetratelabs/wazero` |
| `internal/crypto/postquantum.go` | 239 | 5.14.4 (COMPLETE) line 1134 — stale, see R0-03 | **Delete + fix plan line.** Sole user of `cloudflare/circl` |

### Class C — 100% dead files inside live packages → DELETED in this PR (no plan reference)

Every file below: all functions unreachable, no plan hit for path/basename/feature, no
non-test importer. Tests that existed only to exercise these files were deleted with them;
tests of live code that borrowed a helper from a dead file were edited (listed in the PR body).

| Package | Files deleted | LOC |
|---------|---------------|----:|
| auth | `activedirectory.go`, `activity.go`, `auth_profile_support.go`, `db.go`, `db_postgres.go`, `db_support.go`, `ldap.go`, `oauth.go`, `saml.go`, `serviceaccount.go`, `sso.go` | 2,442 |
| cache | `advanced.go`, `backup.go`, `config_api.go`, `consistency.go`, `cost_optimizer.go`, `debug.go`, `geo_cache.go`, `lru.go`, `metrics.go`, `patterns.go`, `prefetch.go`, `recovery.go`, `sized_cache.go`, `ssd_cache.go`, `time_strategies.go`, `user_cache.go` (kept: `tiered_cache.go`, live) | 3,762 |
| compliance | `dashboard.go`, `deletion.go`, `iso27001.go` (kept: `soc2.go`, class B) | 1,694 |
| config | `env.go` | 36 |
| crypto | `backend_integration.go`, `pipeline.go`, `tls.go` (kept: `postquantum.go`, class B) | 1,018 |
| dashboard/handlers | `activity.go`, `upload.go` (kept: `dashboard.go`, `files.go`, `help.go` — see decisions D-8) | 125 |
| database | `history.go` | 58 |
| drivers | `bandwidth_quota.go`, `chunked_transfer.go`, `circuit_breaker.go`, `compression.go`, `conflict.go`, `driver.go`, `egress_predictor.go`, `fallback.go`, `health.go`, `parallel.go`, `parallel_chunks.go`, `parallel_stream.go`, `queue.go`, `reader_pool.go`, `regional_failover.go`, `resumable.go`, `retry.go`, `s3_auth.go`, `s3_iam.go`, `smart_cache.go`, `throttle.go`, `webhook.go` (kept: `geyser_admin.go` tooling, `cost_advisor.go`/`wasm.go` class B) | 2,822 |
| engine | `context.go`, `migration_progress.go`, `migrator.go`, `monitor.go`, `performance_monitor.go` (kept: `analytics.go`, `capacity.go`, `replicator.go`, `sla.go`, `disaster_recovery.go`, `load_balancer.go` — class B) | 430 |
| tenant | `store.go` (in-memory `Store`; product uses PostgreSQL) | 60 |
| usage | `auto_upgrade.go`, `billing_integration.go`, `cost_tracker.go`, `grace_period.go`, `overage.go`, `quotas.go`, `reporting.go`, `templates.go`, `tracker.go` (kept: `free_tier.go`, `quota_manager.go`, live) | 1,832 |

Specific checks worth recording:
- `drivers/circuit_breaker.go`, `retry.go`, `fallback.go`, `regional_failover.go`, `health.go`:
  the live equivalents are `engine/failover.go` + `engine/errors.go` (breaker), `engine/health*.go`
  and `api/backend_probes.go`. Plan line 899 (5.12.4 circuit breaker) is satisfied by the engine.
- `drivers/s3_iam.go`, `s3_auth.go`: Phase 19.1 (line 2086) names `internal/auth/iam.go` (new);
  live SigV4 is `internal/auth/sigv4.go`.
- `drivers/driver.go`: the `Driver` alias was used only by `fallback.go`/`parallel.go` (both dead);
  every live driver implements `engine.Driver` directly.
- `usage/overage.go`, `grace_period.go`: plan hits for "overage"/"grace period" (lines 124, 173,
  1109, 1341, 1359) are the quota-sold pricing decision and 5.14.1's 30-day account-deletion grace
  (`api/account_deletion.go`, live), not these types.
- `engine/monitor.go` (`BackendMonitor` → `backend_health` table): superseded by
  `engine/health*.go` (5.12.10).

### Class D — test helpers in non-test files → FIXED in this PR

| File | Action |
|------|--------|
| `internal/api/compat_test_helpers.go` | **Kept.** Its only caller is `tests/compatibility/s3_compat_test.go:108` (external test package, plan line 662 / 5.10.14). A first attempt to delete it was caught by `go vet ./tests/...`. Follow-up in R15 (see R0-08) |
| `internal/api/test_db_fix.go` | Renamed to `db_test_helpers_test.go`; uses `testutil.DBConfig()` |
| `internal/database/config.go` (`GetTestConfig`, `getEnv`) | Moved to new `internal/testutil/db.go` as `DBConfig()`; `database/postgres_test.go` gets a local copy (import cycle: `testutil` imports `database`) |
| `internal/database/test_helpers.go` (`GetTestDSN`) | Moved to `testutil.DSN()`; `usage/manager_test.go` updated |
| `internal/database/test_config.go` (`TestConfig`, `//go:build !prod`) | Deleted, no callers (R0-07) |
| `internal/drivers/test_helpers.go` | Renamed to `helpers_test.go` (used by 4 test files); `drivers/CLAUDE.md:127` updated |

`internal/testutil` is by construction never linked into `cmd/vaultaire`; `deadcode` will list it.
That is the accepted trade-off for cross-package test config.

### Tooling dependencies (not product-dead, kept)

| Item | Used by | Note |
|------|---------|------|
| `internal/drivers/geyser_admin.go` (1,282 LOC, 49 funcs) | `cmd/geyser-admin-test`, `geyser-cloudsync-probe`, `geyser-console-probe`, `geyser-smoke`, `geyser-test` — `deadcode ./cmd/...` shows 32/49 funcs reachable from the probes | Plan lines 204, 993 (V18.2 console auth). Not product code; R7 should treat it as a tool library. 17 funcs are dead even from the tools |
| `internal/loadtest` | `cmd/loadtest` only | See R0-10 / decision D-7 |

### cmd/* inventory

| Directory | LOC | Imports `internal/` | Kind |
|-----------|----:|---------------------|------|
| vaultaire | 373 | api, config, database, drivers, engine, usage | **product** |
| backend-matrix | 298 | common, drivers, engine | tool (pairwise interop) |
| erasure-bench | 968 | common, drivers, engine | bench |
| onedrive-bench | 171 | common, drivers, engine | bench |
| dedup-migrate | 481 | crypto, database, drivers, engine, tenant | tool (Phase 8.8, shipped #309) |
| geyser-admin-test, geyser-cloudsync-probe, geyser-console-probe, geyser-smoke, geyser-test | 56 / 285 / 200 / 59 / 144 | drivers | probes |
| quotaless-bench | 161 | drivers | bench |
| loadtest | 236 | loadtest | tool (duplicate harness, R0-10) |
| bench, bench-compare, lighthouse-bench, permafrost-v2, permafrost-v3, pixeldrain-bench, quotaless-bench-v2, quotaless-debug, uloz-bench, validate | 921 / 2,609 / 829 / 1,056 / 1,269 / 1,665 / 654 / 74 / 1,112 / 1,028 | none | standalone benches |
| quotaless-full-bench | 0 | — | contains only `bench.sh` |

`cmd/common` is a shared helper package for the benches (not under `internal/`).

## Decision needed (Isaac)

| # | Item | Recommendation | If yes, also |
|---|------|----------------|--------------|
| D-1 | Class B packages `k8s`, `container`, `global`, `ha`, `slo`, `integrations` (21.7k src LOC) | Delete all six, rely on git history | `go mod tidy` drops `gopkg.in/yaml.v3` |
| D-2 | Class B engine/driver files `sla.go`, `disaster_recovery.go`, `wasm.go` | Delete | drops `tetratelabs/wazero` |
| D-3 | `crypto/postquantum.go` (+ `postquantum_test.go`) | Delete; fix plan line 1134 and `crypto/CLAUDE.md:12,35` | drops `cloudflare/circl` |
| D-4 | `api/metrics.go` + `metrics_test.go` | Delete; fix plan line 955 → `prom_metrics.go` | |
| D-5 | `api/middleware.go` + `middleware_test.go` | Delete; fix plan lines 720/1204 → `server.go` | |
| D-6 | `internal/webhooks` | Delete; fix plan line 775 → `api/events.go` | |
| D-7 | `cmd/loadtest` + `internal/loadtest` | Delete both; `tests/load` is the harness of record | |
| D-8 | `dashboard/handlers/{dashboard,files,help}.go` (+ tests) | Delete. `handlers/CLAUDE.md:321` already says they are pre-Phase-0 stubs not wired to the router. Plan 1.7 (line 295, unstarted) names `help.go`, but B3 (#355) shipped served docs; mark 1.7 superseded | |
| D-9 | Keep `analytics.go`, `cost_advisor.go`, `capacity.go`, `replicator.go`, `soc2.go` | Keep (plan names them); fix 14.2 path (`engine/` → `drivers/cost_advisor.go`) | |
| D-10 | `api/quota_management.go` handlers (R0-02) | Route them via chi in R10 or delete | drops `gorilla/mux` |

## go.mod

Verified by `go mod tidy` on this branch after the class A/C/D deletions (`go.mod` −6/+1,
`go.sum` −10 lines):

| Module | Before | After this PR | Blocked by |
|--------|--------|---------------|------------|
| `github.com/xeipuuv/gojsonschema` (+ `gojsonpointer`, `gojsonreference` indirect) | direct | **dropped** (only `gateway/validation`) | — |
| `github.com/golang/snappy` | direct | **dropped** (only `cache/ssd_cache.go`) | — |
| `cloud.google.com/go/compute/metadata` | indirect | **dropped** (transitive of a deleted package) | — |
| `github.com/klauspost/reedsolomon` | indirect | now direct (tidy correction — `cmd/erasure-bench` imports it) | — |
| `auth/{ldap,activedirectory,saml}.go` | — | used **no external library** (hand-rolled stubs), so nothing to drop | — |
| `gopkg.in/yaml.v3` | direct | kept | D-1 (`k8s`, `devops` — devops is deleted here, k8s is B) |
| `github.com/tetratelabs/wazero` | direct | kept | D-2 (`drivers/wasm.go`) |
| `github.com/cloudflare/circl` | direct | kept | D-3 (`crypto/postquantum.go`) |
| `github.com/gorilla/mux` | direct | kept | D-10 (`api/quota_management.go`, live package) |
| `github.com/Azure/azure-sdk-for-go/*` | direct | kept — **live**: `drivers/onedrive.go` + permafrost benches | not droppable |
| `github.com/restic/chunker` | direct | kept — **live**: `crypto/chunker.go` `FastCDCChunker.ChunkContext` | not droppable |
| `golang.org/x/time` | direct | kept — live: `dashboard/middleware/ratelimit.go` | not droppable |
| `github.com/prometheus/client_golang` | direct | kept — live: `api/prom_metrics.go`, `api/cert_expiry.go` | not droppable |

## What this PR changed (for the record)

| Metric | Before | After | Delta |
|--------|-------:|------:|------:|
| `internal/` packages | 72 | 32 (31 + new `testutil`) | −41 |
| `internal/` non-test LOC | 131,819 | 81,962 | **−49,857** |
| `internal/` test LOC | 103,392 | 69,747 | **−33,645** |
| Files | — | 341 deleted, 4 added, 13 modified, 2 renamed | |
| `go.mod` direct requires | 33 | 32 | −2 dropped, +1 promoted from indirect |

Product (non-test) code edits beyond file deletion — all removals of dead members that referenced
deleted types:
- `internal/auth/auth.go`: removed the never-set `activityTracker *ActivityTracker` field and
  the unreachable `TrackActivity` method (deadcode confirmed both dead; no caller anywhere).
- `internal/database`: `GetTestConfig`/`getEnv`/`GetTestDSN`/`TestConfig` removed (moved to
  `internal/testutil`).
- CLAUDE.md files updated: `engine/`, `drivers/`, `crypto/`.

Test edits (tests of live code that borrowed from deleted files):
- `drivers/idrive_test.go`: dropped `TestBandwidthQuota`, `TestEgressPredictor`, `TestSmartCache`,
  `TestRegionalFailover`; `TestIDriveIntegration` no longer exercises `SmartCache`.
- `drivers/conformance_test.go`: dropped the `ThrottledDriver`/`CompressionDriver` interface asserts.
- `crypto/benchmark_test.go`, `crypto/security_test.go`: dropped the `ProcessingBackend`/pipeline
  cases; primitive benchmarks and `KeyRotation`/`MasterKeyStrength`/`PostQuantumKeyExchange`/
  `ConstantTimeComparison` kept.
- `tenant/tenant_test.go`: dropped `TestMemoryStore`.
- `auth/helpers_test.go` (new): `setupTestDB` moved out of the deleted `db_test.go`.
- `database/postgres_test.go`, `usage/manager_test.go`, `api/db_test_helpers_test.go`: use
  `testutil` / local config.

Verification: `go build ./... && go vet ./...` clean (including `tests/` and all `cmd/*`),
`gofmt -l` clean, `golangci-lint run ./...` clean, `go test -short ./...` — see PR body.

## Invariants confirmed

- No build artifact is tracked: `git ls-files` against the artifact patterns matches only
  `bench-results/*.md` and `tests/benchmarks/baseline_results.txt`, both intentional.
- `internal/docs` (parent, OpenAPI + served pages) is live; only the nine `docs/*` subpackages
  were scaffolding. `/docs` routes are unaffected.
- `internal/cache.TieredCache` is live and is the engine's read cache (R0-01). Nothing else in
  `internal/cache` was referenced by `tiered_cache.go`, so removing the other 16 files does not
  change engine behaviour (`go build`, `go vet`, `go test -short ./internal/engine/...` pass).
- `RateLimiter` (`api/ratelimit.go`) is live via `server.go:212`; only the `RateLimitMiddleware`
  wrapper was dead.
- Azure SDK, restic/chunker, x/time, x/oauth2, prometheus, goldmark, klauspost/compress,
  pquerna/otp, fsnotify (`drivers/local.go`) all have live non-test users — none should be
  removed by any later session on "dead code" grounds.
- `tests/` (integration/acceptance/compat) imports only `internal/{api,drivers,engine,tenant}` —
  none of the deleted packages.
- CI runs `go build ./...`, `go test -race ./...` (no `-short`) and golangci-lint. The deletions
  remove ~60k LOC from every CI run.
- SSE-S3 (`crypto/sse_s3.go`) uses stdlib `crypto/mlkem` — the circl implementation is not on
  any product path.
- `geyser_admin.go` is a probe-tool library, not product-dead (32/49 funcs reachable from
  `cmd/geyser-*`); keep for R7.

## Dead code noted

**100%-dead files still present after this PR** (all in the decision list above):
`api/{metrics,middleware}.go`, `crypto/postquantum.go`, `dashboard/handlers/{dashboard,files,help}.go`,
`engine/{analytics,capacity,replicator,sla,disaster_recovery}.go`, `drivers/{cost_advisor,wasm}.go`,
`compliance/soc2.go`, `drivers/geyser_admin.go` (tooling), and packages `k8s`, `container`,
`global`, `ha`, `slo`, `integrations`, `webhooks`, `loadtest`.

**Partially dead live files** (dead/total funcs) for the owning session:

| Session | File | Dead/total |
|---------|------|-----------:|
| R1 | `api/server.go` (`SetAuthService`, `SetAuditLogger`, `WrapWithRBACPermission`, `GetRouter`) | 4/26 |
| R2 | `api/s3_engine_adapter.go` (`TranslateRequest`, `HandleList`) | 2/21 |
| R3 | `api/s3_inventory.go` (`GenerateReportNow`), `api/s3_notifications.go` (`FireSync`, `SetHTTPClient`) | 1/8, 2/10 |
| R5 | `auth/auth.go` 5/23, `auth/handlers.go` 7/14, `auth/apikey.go` 4/14, `auth/slug.go` 1/6, `auth/mfa.go` 1/5, `auth/profile.go` 1/2; `rbac/*` 62 dead: `inheritance.go` 13/18, `dynamic.go` 11/19, `templates.go` 11/19, `assignment.go` 8/13, `audit.go` 6/10, `middleware.go` 5/9, `permissions.go` 4/8, `roles.go` 3/4, `api_handlers.go` 1/8; `api/rbac_integration.go` (`AddRBACToServer`) 1/6; `dashboard/auth/auth.go` 2/24 | |
| R6 | `engine/errors.go` 3/5, `engine/interface.go` 2/5 | |
| R7 | `drivers/idrive.go` 2/20, `drivers/lyve_console.go` 3/7, `drivers/egress_tracker.go` 1/7 | |
| R8 | `crypto/encryption.go` **26/27** (entire `Encryptor` family), `crypto/keymanager.go` 14/16, `crypto/compression.go` 16/31, `crypto/chunker.go` 10/14 (`FixedChunker`, `ChunkBytes`, `NewChunkerFromConfig`), `crypto/gci.go` 1/24, `crypto/config.go` 1/2 | |
| R9 | `database/postgres.go` **15/17** (`Postgres` wrapper: `CreateTables`, `CreateTenant`, `GetArtifact`, `ListArtifacts`, `Exec*`, `Query*`) | |
| R10 | `api/quota_management.go` 4/6 (R0-02), `api/bandwidth.go` 1/11 (`BandwidthTracker.Record`), `api/account_deletion.go` 2/5 (`GetDeletionStatus`, `ExecuteDeletion`), `billing/stripe.go` 4/13, `billing/webhook.go` 3/10, `tenant/tenant.go` 1/5 | |
| R11 | `compliance/gdpr.go` 5/10, `compliance/consent.go` 2/10, `compliance/breach.go` 1/12 | |
| R12 | `dashboard/middleware/flash.go` 1/4, `dashboard/middleware/ratelimit.go` 1/5 (`LoginRateLimiter.Cleanup` — a cleanup that never runs; check for unbounded map growth) | |
| R14 | `email/templates.go` 1/5 | |

Regenerate with:
```
go run golang.org/x/tools/cmd/deadcode@latest ./cmd/vaultaire
```

## Follow-up work packages

| WP | Title | Files | Size | Depends on |
|----|-------|-------|------|------------|
| WP-R0-1 | Execute decisions D-1..D-8 (class B + stale-reference deletions, plan line fixes, `go mod tidy` for yaml/wazero/circl) | packages + files listed in Decision table; `docs/IMPLEMENTATION_PLAN.md:720,775,955,1134,1204,1932` | M (mechanical, one PR) | Isaac's answers |
| WP-R0-2 | Quota admin routes: port `api/quota_management.go` to chi or delete; drop gorilla/mux | `internal/api/quota_management.go`, `quota_management_test.go`, `go.mod` | S | R10 verdict |
| WP-R0-3 | Engine read cache: `cache.TieredCache` is an unbounded `map[string][]byte` with `Config` fields ignored; decide cap/eviction or removal (memory says WP-2 #337 capped a cache — verify which) | `internal/cache/tiered_cache.go`, `internal/engine/engine.go:34-98,193,452,494,693-710` | S–M | R6 |
| WP-R0-4 | Prune partially-dead live files (table above), starting with `crypto/encryption.go` and `database/postgres.go` | per session | S each | R5/R8/R9/R10 |
| WP-R0-5 | `make clean` + `make deadcode` targets | `Makefile` | XS | R15 |
| WP-R0-6 | Per-directory CLAUDE.md sweep for files deleted here (engine, drivers, crypto, handlers done in this PR; verify api/database/auth) | `internal/*/CLAUDE.md` | XS | — |

Proposed `Makefile` target (WP-R0-5, not applied by R0):
```make
clean:
	rm -rf bin/ coverage.out coverage.html
	rm -f vaultaire vaultaire-bin vaultaire-linux
	rm -f bench-compare bench-compare-linux bench-linux lighthouse-bench loadtest loadtest-linux
	rm -f permafrost-* pixeldrain-bench pixeldrain-bench-linux quotaless-bench-v2 quotaless-bench-v2-linux
	rm -f geyser-smoke-bin geyser-test *.bin test*.txt test_output.log downloaded.txt
	go clean

deadcode:
	go run golang.org/x/tools/cmd/deadcode@latest ./cmd/vaultaire
```
