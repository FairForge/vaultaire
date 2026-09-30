# R15 Tests, CI, tooling, dependencies, repo hygiene — 2026-09-30

**Repo state reviewed:** `main` @ `afb5b63` (#523). Branches: `review/R15-deletions` (#524), `review/R15-ci-tooling` (#525), `review/R15-deps` (#526), `review/R15-tests` (#527), `review/R15-tests-ci-synthesis` (this file + `SYNTHESIS.md`, #528). **Reviewer:** Claude (session R15, the last of `docs/CODE_REVIEW_PLAN.md`). **Depends on:** every findings file R0–R14 including their post-merge sections (all read in full before any change).

**Method.** Every behavioural claim below was run, not read: the coverage and flake runs are pasted; gosec/govulncheck/golangci-lint were run locally with the CI flags; the goroutine leak was proven with a test that is red on `main`; the dependabot claim was checked against the modules' own `go.mod` files; the nightly workflow's real outcome was read from its job log. Nothing on the prod box was touched — every prod fact in `SYNTHESIS.md` is carried from the read-only checks of earlier sessions, plus one anonymous `GET /metrics` (public, R1-04).

## Scope reviewed

| Area | Files | How |
|------|-------|-----|
| Workflows | `.github/workflows/{ci,deploy,nightly,release,security}.yml` | read; run history via `gh run list`; nightly job log read line by line |
| Build/tooling | `Makefile`, `.pre-commit-config.yaml`, `.gitignore`, (no `.golangci.yml` existed), `go.mod`/`go.sum`, `deploy/*`, `.claude/skills/verify` | read; `make build && make clean && git status --ignored` run |
| Tests | `tests/**` (10 suites + 2 scripts + plan), `internal/testutil`, every `_test.go` that reads `DATABASE_URL`, `internal/api/compat_test_helpers.go`, `internal/auth/db_test_fix.go`, `internal/crypto/gci_test.go`, the api fixtures' GCI cleanups | read; `go test -short -race -coverprofile` (baseline), `go test -race -count=3 ./internal/...` ×2 (before and after the fixes), `go vet -tags integration ./...` |
| Commands | all 23 `cmd/*` dirs, `scripts/*` (12), `tools/geyser-grabber` | read the header of each; `go list -deps ./cmd/vaultaire` for linkage; grep for `.env`/`.private`/hard-coded keys |
| Dependencies | the 11 open dependabot PRs, `go mod why` for every module R0/R11 orphaned, `govulncheck ./...` | `gh pr diff` per PR; downloaded the candidate x/ modules and read their `go.mod` |
| Deletion list | the items handed over by R3-26, R7-14, R12-27/D-8, R11 (audit logger), R5 (BasicAuth, in-memory backup codes), WP-R6-4/5/6, R10 (`BandwidthBytes`), R0 (`soc2.go`, `internal/webhooks`, `internal/events`) | `deadcode ./cmd/vaultaire` (x/tools v0.50.0) + grep for callers before each deletion; build + vet (both tag sets) + full suite after |
| The aws-chunked decoder | `internal/api/s3_engine_adapter.go:150-240` (`newAWSChunkedReader`), `internal/auth/sigv4*.go` | read against the SigV4 streaming spec (chunk signature = HMAC over the previous signature + the chunk hash); decision recorded below |
| Skipped | `deploy.yml`'s prod steps beyond reading (read-only by instruction); `tests/load` internals (harness of record, env-gated, untouched); the class-B packages (D-1..D-3, Isaac's) | |

## Findings

Severity: P0 data loss / security / cross-tenant / money · P1 wrong behaviour a customer hits · P2 correctness risk / unsafe pattern · P3 quality, dead code, docs.

| ID | Sev | file:line | What | Why it matters | Proposed fix |
|----|-----|-----------|------|----------------|--------------|
| R15-01 | **P1** | `internal/api/s3.go:447-461` (at `afb5b63`), `internal/events/logger.go:27-33` | `handleS3Request` constructed a **new `events.EventLogger` per authenticated request**; its constructor started `go el.process()` ranging over a 1000-slot channel that nothing ever closed. One goroutine plus its buffer leaked per S3 request for the life of the process. Proven: `TestHandleS3Request_DoesNotLeakGoroutinesPerRequest` is **red on `main`** — 400 requests grew the goroutine count from 3 to 403 — and green after. The log line it produced duplicated `loggingMiddleware`. R1's goroutine table covered only the background loops; prod's daily redeploys (R13-07) hid the growth (`go_goroutines 46` after 94 min and 4,070 mostly-unauthenticated requests when checked). | Unbounded memory growth on the hot path, invisible until a quiet week without deploys. | **Fixed in #524:** `internal/events` deleted with the call site; the regression test stays. |
| R15-02 | **P1** (ops) | `.github/workflows/nightly.yml`, `tests/benchmarks/*`, `tests/k6/*.js`, `tests/chaos/*` | The nightly workflow had reported **success every night** (8/8 recent runs) while every step failed: the benchmarks step ends `FAIL … s3_bench_test.go:37: upload failed: 403` (every PUT unauthenticated against a SigV4 server), the k6 step ends `thresholds on metrics 'errors' have been crossed … exit code 99`, the chaos script prints `AccessDenied` XML and declares "Chaos testing complete!". All three steps were `continue-on-error: true` and the artifact upload made the job green. `docs/SCALE_TESTING.md` and `docs/DEPLOY.md` described it as an informational signal; it was no signal. | A green nightly that measures nothing is worse than none — the plan cited it as the load/chaos evidence. | **Fixed in #525:** `nightly.yml` and the eight dead `tests/*` suites deleted (table below); `tests/load` (SigV4, env-gated) is the load gate and the synthetic customer check (R13) is the canary; docs corrected. |
| R15-03 | P2 (CI flake, root-caused) | `internal/api/s3_quota_accounting_test.go:108-113`, `s3_chunked_copy_test.go:39`, `s3_chunking_string_tenant_test.go:36`, `s3_chunking_test.go:480` (fixture cleanups); `internal/api/dedup_gc_test.go:204,254,305` (table-wide counts + backdates) | The api fixtures' cleanup ran `DELETE FROM global_content_index WHERE dedup_scope = $1 OR NOT EXISTS (SELECT 1 FROM tenant_chunk_refs …)` — every GCI row with no reference **table-wide**. `internal/crypto`'s suite (running in parallel on the same DB since R8 made it run) inserts rows with no manifest by design; when the two packages overlapped, `TestGCI_RefCounting` found its row gone mid-test: `DecrementRef failed: … converting NULL to int` (run 1 of 3 in `go test -race -count=3 ./internal/...` below). The three `TestDedupGC_*` tests also asserted `COUNT(*) FROM global_content_index = 0` and backdated `marked_for_deletion` rows table-wide (R8's "live chunked rows break TestDedupGC_*" gotcha). | The last member of the R9-05/R10-11 class: cross-package writes on one CI database. | **Fixed in #527:** one `cleanupTenantChunkRows` helper — captures the (scope, hash) pairs the tenant's manifests referenced, deletes the refs, then deletes only those GCI rows that no other tenant references; the GC tests count/backdate their own pairs (`tenantChunkPairs`, `gciRowsFor`, `backdateGCIRows`). Second 3× run: green. |
| R15-04 | P2 | `.github/workflows/security.yml:33-40` (gosec), `internal/audit/audit.go:245-249`, `internal/dashboard/handlers/buckets.go:170-186`, `cmd/geyser-cloudsync-probe/main.go:45,46,91,224`, `cmd/backend-matrix/main.go:57`, `cmd/geyser-console-probe/main.go:47` | gosec was red on every push since #404 (8 issues with the CI flags, reproduced locally: G202 in `audit.List` — the `#nosec` sat on the `QueryContext` line while the flagged concatenation was the `strings.Join(where…)` above it; G703 on the dashboard's `os.MkdirAll(DATA_PATH/<bucket>)` from a form value; G703/G704/G124/G117 in the three probe tools). `-exclude-dir=internal/testing` pointed at a directory R0 deleted. | A red required-in-spirit gate nobody reads; the two product findings were real smells (assembled SQL; a filesystem sink on the dashboard create path that the other two entry points did not have). | **Fixed in #525** by construction, not `#nosec`: `audit.List` is one static statement (`($1 = '' OR tenant_id = $1) AND …`, prepared by `TestSQLLiteralsMatchSchema`); the dashboard create goes through `createBucketRegistry` via `Deps.CreateBucket` (WP-R12-10) — no MkdirAll, and the registry now also derives `data_residency` for all three entry points; probes/benches live under `cmd/tools/` and are `-exclude-dir`'d (operator CLIs; inputs are the operator's own flags — see R15-11 for why that is honest); `make gosec` runs the identical command. **0 issues** locally and in CI (#525's gosec job). Making it a required check: see *Recommendation* below. |
| R15-05 | P2 | `go.mod` (`go 1.25.0`), dependabot PRs #452/#454/#455/#456 | "The batch is blocked on Go 1.26" was **true for four of eleven**: `golang.org/x/crypto` 0.56.0 and 0.57.0, `x/net` 0.59.0, `x/sys` 0.48.0, `x/time` 0.16.0 and `x/text` 0.42.0 each declare `go 1.26.0` in their own `go.mod` (verified from the module cache), so those PRs rewrite ours to `go 1.26.0` and `build-and-test` fails on the 1.25 toolchain the four workflows pin. The other seven (aws group #363, smithy-go #451, azcore #453, azidentity #449, goldmark #450, testify #448, codeql-action #406) build on 1.25 and had green `build-and-test` (only gosec red). `govulncheck ./...`: the two x/crypto advisories Trivy flags (GO-2026-6354, GO-2026-6355, fixed in 0.56.0) are **in packages this code does not call**; the 37 stdlib findings are against the local go1.25.0 and are fixed in the 1.25.x patch releases CI and prod use (`/version` reported go1.25.14 in R1). | Two MEDIUM CVE flags on every PR with no exploitable path; the real cost is that every future x/ bump needs 1.26. | **#526** merges the seven that build; the four x/ PRs are closed with the reason; decision **D-24** (move to Go 1.26 — one-line change in `go.mod` + four `setup-go` lines, one prod deploy) is Isaac's; `stripe-go` untouched (WP-R10-6). |
| R15-06 | P2 | `internal/api/s3_engine_adapter.go:164-200` (`awsChunkedReader`), `internal/auth/sigv4_payload.go:46-48` | **`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` chunk signatures are stripped, not verified** (R3 and R5 both flagged it; nobody owned it). What the seed signature binds: every signed header including `x-amz-decoded-content-length` and the credential scope; what it does not bind: the body bytes. Per the SigV4 streaming spec each chunk carries `HMAC(signingKey, "AWS4-HMAC-SHA256-PAYLOAD\n"+date+"\n"+scope+"\n"+prevSig+"\n"+sha256("")+"\n"+sha256(chunk))`; we accept any value. **What an attacker gains:** with TLS end to end (HAProxy terminates, plain HTTP is redirected — R14 invariant 1) nothing in transit; the residual is a *captured* signed streaming request (only someone on the box or the origin path can capture one) replayed within the 15-minute skew with a different body of the same decoded length — i.e. the same class as any SigV4 replay, minus the body binding. Header-auth requests with a real `x-amz-content-sha256` are fully bound (WP-4). | Accepted deviation; the honest statement is "SigV4 header + payload-hash requests are verified; aws-chunked streaming bodies are framed, not signed". | **Not fixed here — recorded in `SYNTHESIS.md` as an accepted deviation with the reasoning above; WP-R15-1 (S) implements verification with a raw aws-cli capture as the fixture.** P2, not launch-gating. |
| R15-07 | P2 | `.golangci.yml` (new), 110 findings under it | With the linters the team wants there were 110 real findings: **80 loops that never checked `rows.Err()`** (a partial page or export rendered as complete — the R9-02 shape), 9 `err == sentinel` compares that fail on wrapped errors, 3 `%v`-wrapped sentinels, the S3 credential lookups (`auth/handlers.go:160,177,222`) and presign lookups (`s3_presign.go:92,100,130`) on `QueryRow` **without the request context** (R9-20 — on the hottest path), OAuth userinfo requests and the SMTP implicit-TLS dial without ctx, 14 `rows.Close()` not deferred, 3 stale `nolint` directives, one gofmt -s. | The rule "ctx first and propagated" was unenforced; silent short listings are a real bug class. | **Fixed in #525** (all of them; `sqlclosecheck` deliberately off — the explicit-close-before-next-query pattern in the job runners is right, and the linter only accepts `defer`). The seven void `populate*` dashboard helpers record the iteration error under `data["RowsError"]` (WP-R15-2 renders it). |
| R15-08 | P2 | `internal/api/version.go` (new), `server.go:1076-1078,1101`, `health_handlers.go:159`, `deploy.yml:31`, `ci.yml:73`, `release.yml:25-27`, `Makefile` | `/version`, `/health` and `/status` reported `"0.1.0"` / `"2025-08-12"` since August 2025 (R1-10, WP-R14-4). | No way to tell from the box which commit serves after a deploy or rollback. | **Fixed in #525:** `api.BuildSHA`/`BuildDate` via `-ldflags -X` in `make build`, `deploy.yml`, `ci.yml`, `release.yml`; `make version`. |
| R15-09 | P2 (dev) | `internal/drivers/s3upload.go:63-73` (R7-12) | On the manager (> 16 MiB) path the SDK reads until `io.EOF` and commits whatever arrived; a body that ends *cleanly* short of `ContentLength` was stored truncated under the declared size. Safe today only because Go's server body returns `ErrUnexpectedEOF` — a transport property, not a check. | Correctness by accident. | **Fixed in #527:** a counting reader; a clean-EOF short body returns `ErrUnexpectedEOF` with the counts (`TestS3Upload_CleanEOFShortBodyIsAnError`); the blob is not deleted (R2-05 class until WP-R2-1), the head row is not written, the reservation is released. |
| R15-17 | **P1** (money) | `internal/api/quota_accounting.go:190-196` (`atomicHeadUpsertReleasing`, the head-row upsert every writer uses: plain/chunked PUT, copy, multipart complete, background reports) | **Two concurrent first writers of the same new key both saw "no previous row"**: the displaced size is captured with `SELECT … FOR UPDATE`, but a row that does not exist yet cannot be locked, so writer B's probe ran before writer A's `INSERT … ON CONFLICT` committed; B's upsert then replaced A's row while reporting `displaced = 0`, and `settlePutQuota` never released A's reserved bytes. Found by the new `TestConcurrentSameKeyPut_QuotaStaysExact` failing once under whole-suite load in the pre-commit hook (`storage_used_bytes` 5120 vs `SUM(size_bytes)` 2048; after the delete 3072 stayed charged), then reproduced deterministically: `TestAtomicHeadUpsert_ConcurrentFirstWritersAccountOnce` drives two writers through the helper with a barrier between the probe and the insert — **red on `main`** (both reported 0; final row 2000). R2-15's "concurrent same-key PUTs bill exactly (arithmetic walked)" assumed the row existed. | The tenant's ledger over-counts by one object per such race **until an admin reconcile** (there is no scheduled reconcile): a parallel uploader that retries the first PUT of a key, or two clients writing the same new key, leaves the customer charged for bytes that are not there — and the delete then under-releases. | **Fixed in #527:** a transaction-scoped `pg_advisory_xact_lock(hashtext(tenant/bucket/key))` taken before the probe serialises writers of one key (lock order: key → head row → manifest rows; the GCI locks use the two-int lock space, no interaction; deleters/reclaim take only the row lock and never wait for the key lock, so no cycle). Both tests green ×3 under `-race`; `TestSQLLiteralsMatchSchema` prepares the new statement. |
| R15-10 | P3 | `internal/auth/db_test_fix.go` (`//go:build integration`), `scripts/verify_{full_system,implementation,step,steps,working}.sh`, `scripts/test_no_auth.go`, `scripts/{deploy,run-dev,run-prod}.sh` | R0-19 / WP-R0-11: `go vet -tags integration ./...` failed on a second `setupTestDB`; the five verify scripts checked for ~25 files R0 deleted and nothing invoked them; `test_no_auth.go` was a loose `package main` in `scripts/`; `deploy.sh` described a ghcr/docker deploy that never existed; `run-dev.sh` exported `LOCAL_STORAGE_PATH`/`DATABASE_URL`, which `main.go` does not read; `run-prod.sh` sourced a `.env.production` prod does not use (systemd `EnvironmentFile`). | Noise every reviewer rediscovers. | **Fixed in #524/#525:** all deleted; `go vet -tags integration ./...` passes; `push-to-slc.sh`, `bench-vaultaire.sh`, `validate-backends.sh` kept (current; paths follow the tools move). |
| R15-11 | P3 | `cmd/*` (23 dirs), `.gitignore:101-102`, `tools/geyser-grabber` | 22 of 23 commands were probes/benches beside the product (table below); four were benches of the dead Quotaless account; none reads a credential file from the repo (all `os.Getenv`, typically `source .env.bench`, gitignored); `bench-compare`/`validate` binaries piled up in the repo root (~35 ignored files). | Classification and hygiene. | **Fixed in #525:** 16 tools moved under `cmd/tools/` with a README classifying each (current / historical / frozen-by-decision), the four quotaless benches deleted, `make clean` removes every artifact pattern, `.gitignore` follows. `tools/geyser-grabber` (a browser extension, not Go) stays. `go list -deps ./cmd/vaultaire` names no `cmd/tools` package. |
| R15-12 | P3 | `internal/api/compat_test_helpers.go`, `tests/compatibility/s3_compat_test.go:43` | R0-08: five exported `Handle*` wrappers on `Server` existed only for the external compat suite, which also skipped on a bare `DATABASE_URL` (the last of R1-13's eight). | Product API surface for a test; the suite never ran locally. | **Fixed in #525:** suite moved in-package (`internal/api/compat_s3_test.go`, `testutil.DSN()`), wrappers deleted. |
| R15-13 | P3 | `.pre-commit-config.yaml` | `go test ./... -short` ran on every commit; with the DB-backed packages it takes minutes (auth alone ~70 s under `-race`). | Contributors skip hooks that take minutes. | **Fixed in #525:** tests on the `pre-push` stage; fmt + lint stay on commit. |
| R15-14 | P3 | `.github/workflows/release.yml`, `git tag` | Triggered by `v*` tags; the last tag is `v0.28.0` (2025-09-05) and every run failed (2025-09-09). It also cross-compiles `GOOS=windows`, which does not build (`drivers/local.go:397 syscall.Stat_t`, R0-19 — still true). Prod deploys through `deploy.yml`; nothing consumes GitHub releases. | Dead workflow that would fail if a tag were pushed. | **Deleted in #528** (this PR). If binaries are ever wanted, build linux only. |
| R15-15 | P3 | `ci.yml:18` (postgres:15) vs prod PostgreSQL 16.13 (R9) | CI tests against 15; prod runs 16.13; local dev 15.13. The migrations are proven idempotent on both (R9 double-apply on 15, prod schema == migrations on 16). | Minor version skew; nothing observed. | Bump the service image to `postgres:16` in WP-R9-3 (with the runner timeouts) — one line, not done here to keep #525 to hygiene. |
| R15-16 | P3 | `deploy.yml` (read-only) | Confirmed, unchanged: migrations run with `ON_ERROR_STOP`, `-w`, `< /dev/null` before the swap; stop → mv → chmod → start; 15 s + `/health/live` gate with `--retry 5`; rollback to `.prev` through the same three NOPASSWD'd commands (`systemctl`, `mv /tmp/vaultaire-linux`, `chmod`); prod secrets scoped to the `production` environment (#440); `concurrency: cancel-in-progress: false`. The migration runner still has no `lock_timeout` (WP-R9-3). | — | none here |

**Recommendation on the gosec gate:** #525's gosec job is green; the deletion PR (#524) and the deps PR (#526) show it red only because they predate #525 on their branches. Once #525 is on `main`, add `gosec` to the `main-protection` ruleset's required checks — but only after one more PR from `main` shows it green (the linux-only lint finding R15-04 caught in CI is the reminder that this Mac does not compile every file). Not done in this session: the ruleset is a repo setting, and the prompt's condition ("stays green for the whole session") is met only from #525 onward.

## Coverage baseline

`go test -short -race -coverprofile=cov.out ./internal/...` on `main` @ `afb5b63` (before any R15 change), 2026-09-30:

| Package | Cov | Package | Cov | Package | Cov |
|---------|----:|---------|----:|---------|----:|
| api | 73.0% | api/landing | 67.0% | audit | **1.7%** |
| auth | 65.4% | billing | 61.0% | clientip | 88.6% |
| compliance | 67.8% | crypto | 75.8% | dashboard | 82.8% |
| dashboard/auth | 46.4% | dashboard/handlers | 66.7% | dashboard/middleware | 94.0% |
| database | **27.2%** | docs | 98.3% | drivers | 59.8% |
| email | 67.5% | engine | 65.4% | flags | 82.7% |
| tenant | 46.2% | testutil | 41.2% | usage | 55.1% |
| *(unlinked, D-1)* container 84.4% · global 83.7% · ha 88.1% · integrations 71.5% · k8s 86.9% · loadtest 90.6% · slo 89.8% · webhooks 72.0% *(deleted)* | | | | | |

Against the 2026-09-25 plan baseline: api 39.6 → 73.0 %, billing 38.2 → 61.0 %, crypto 61.5 → 75.8 %, flags 14.0 → 82.7 %, database 22.7 → 27.2 % — the review PRs' red-first tests. Under 40 % and launch-critical: **`audit` (1.7 %)** — `Record`/`List` are exercised end to end through `internal/api` (`TestR11_AdminFlagSet_WritesAuditRow`, the R12 login tests) so the number understates it; `audit_test.go` has two DB tests. **`database` (27.2 %)** — the package is `NewPostgres`/`Close`/`DB` plus the migration tests; `TestSQLLiteralsMatchSchema` and `TestMigrations_*` are what matters and they run. The "untested" lists R2–R13 handed over, with what R15 did:

| Hand-off | Test written? |
|----------|---------------|
| R7 clean-EOF short body on the manager path | **yes** (`TestS3Upload_CleanEOFShortBodyIsAnError`, with the guard, R15-09) |
| R2-15 concurrent same-key PUT | **yes** — and it found R15-17: `TestConcurrentSameKeyPut_QuotaStaysExact` (16 writers × 6 rounds on a new key, one head row, ledger exact, delete releases exactly) plus the deterministic `TestAtomicHeadUpsert_ConcurrentFirstWritersAccountOnce`; the backend-vs-row divergence itself is WP-R2-1 and is documented, not asserted |
| R3 chunk-signature stripped-not-verified | no — accepted deviation (R15-06), WP-R15-1 |
| R13 reclaim `object_gone` vs re-PUT | no — needs the backend write to land inside the reclaim's DELETE window; no deterministic hook without WP-R2-1 (R13 said the same) |
| R1 "logs never contain the query string/Authorization", "every rule-file series exists" | no (XS each; listed in WP-R15-2's neighbourhood for the next session) |
| R5 JWT `GenerateJWT`/`ValidateJWT`/`requireJWT` | no — covered indirectly by every JWT route test since R11; a direct unit test is XS |
| R15 goroutine leak | **yes** (`TestHandleS3Request_DoesNotLeakGoroutinesPerRequest`) |
| R15 dashboard create through the registry | **yes** (`TestDashboardBucketCreator_RegistryEndToEnd` on the real DB + the handler verdict tests) |

## Flake runs

Run 1 — `go test -race -count=3 ./internal/...` on `main` @ `afb5b63` (DATABASE_URL = `vaultaire_test`), wall clock ~9 min:

```
--- FAIL: TestGCI_RefCounting (0.08s)
    gci_test.go:261: DecrementRef failed: failed to decrement ref: sql: Scan error on column index 0, name "decrement_chunk_ref": converting NULL to int is unsupported
FAIL	github.com/FairForge/vaultaire/internal/crypto	8.078s
ok   every other package ×3 (api 167 s, auth 219 s, drivers 124 s, dashboard/handlers 84 s, dashboard 80 s, loadtest 36 s, database 23 s, …)
```

Root cause R15-03 (the api fixtures' table-wide GCI cleanup). `TestRequestQueue`/`TestPipeline_Run` (#310) no longer exist (their packages went in R0); the Quotaless boot health check is conditional (`TestConfiguredBackends_QuotalessOnlyWithCredentials`, confirmed by R1). `TestFloorAccounting_*` and the demotion tests were clean ×3 (R10's fixes hold).

Run 2 — same command on `review/R15-tests` after scoping the four cleanups (+ `./cmd/...`): `internal/crypto` green ×3, but a second member of the same class surfaced — `TestDedupGC_ReconcilesOrphan` inflated `ref_count` **table-wide** and then asserted on every row, and the GC fixture ran with `GracePeriod = 0`, so its reconcile touched `internal/crypto`'s fresh `refcount_test_hash…` row (`expected: 0, actual: 1`). Scoped the inflate + the assertion to the fixture's pairs; the GC fixture gets a one-hour grace and tests that want their rows collectable backdate exactly their own.

Run 3 — `go test -race -count=3 ./internal/... ./cmd/...` after both fixes: **green, 31 packages × 3** (api 168 s, auth 206 s, drivers 127 s, dashboard/handlers 86 s, dashboard 81 s, loadtest 36 s, database 25 s, crypto 7 s, …), no `DATA RACE`, exit 0. Plus `go test -race -count=5 -run 'DedupGC|GCI|Chunked|CrossTenant' ./internal/api/ ./internal/crypto/` (the two packages that shared the table) green ×5.

## tests/ triage

| Suite | Ran in CI? | Needs | Verdict |
|-------|-----------|-------|---------|
| `tests/load` (harness.go + s3_load_test.go) | yes — skips unless `VAULTAIRE_LOAD_*` | live server + keys | **keep** — the load gate of record (5.15.2) |
| `tests/compatibility` | yes, only with `DATABASE_URL` | test DB | **moved** in-package (R15-12) |
| `tests/benchmarks` (5 files) | nightly only | a server on :8000 **without auth** | **deleted** — every PUT 403 (R15-02) |
| `tests/k6` (4 scripts) | nightly only | k6 + unauthenticated server | **deleted** — thresholds tripped on every run |
| `tests/chaos` (4 Go + 2 sh) | nightly only | unauthenticated server; most tests `t.Skip` | **deleted** |
| `tests/security`, `tests/regression`, `tests/acceptance`, `tests/compliance`, `tests/integration` | in `go test ./...` (compile only) | every test `t.Skip`s ("not yet implemented", "Server not running") | **deleted** with `INTEGRATION_PLAN.md` and `run_all_integration_tests.sh` (Steps 192–200 of the old master plan) |
| `internal/drivers/r7_live_test.go` | no (`//go:build live`) | a non-prod iDrive region key | keep (the only build-tagged live suite; correct shape) |

## cmd/ classification

| Command | Kind | Reads creds from | Linked into product? | Verdict |
|---------|------|------------------|----------------------|---------|
| `vaultaire` | **product** | env | — | `make build` builds only this |
| `backend-matrix` | probe | env (`IDRIVE_*`, `LYVE_*`, …) | no | → `cmd/tools/` (R7 used it) |
| `bench`, `bench-compare` | bench | env (`.env.bench`) | no | → `cmd/tools/` (bench-compare current; bench kept for its JSON shape) |
| `dedup-migrate` | one-off migration | env | no | → `cmd/tools/`, **frozen** (D-14, R8-06 races the GC) |
| `erasure-bench` | bench | env (+ `PIXELDRAIN_API_KEY`) | no | → `cmd/tools/` (2026-09-25/27 results) |
| `geyser-cloudsync-probe`, `geyser-console-probe`, `geyser-test` | probe | env (`GEYSER_*`) | no | → `cmd/tools/` |
| `geyser-admin-test`, `geyser-smoke` | probe | env | no (untracked, gitignored) | → `cmd/tools/` (gitignore updated) |
| `lighthouse-bench`, `pixeldrain-bench`, `uloz-bench`, `permafrost-v2`, `permafrost-v3` | bench (evaluations) | env | no | → `cmd/tools/`, historical |
| `loadtest` | bench | server URL + keys | no | → `cmd/tools/`, **frozen** (D-7) |
| `onedrive-bench` | bench | env (`TENANT_N_*`) | no | → `cmd/tools/` (the driver-path bench, R7) |
| `validate` | probe | server URL + keys | no | → `cmd/tools/` (`scripts/validate-backends.sh` drives it) |
| `quotaless-bench`, `quotaless-bench-v2`, `quotaless-debug`, `quotaless-full-bench` | bench | env | no | **deleted** (dead account, WP-R7-3) |
| `tools/geyser-grabber` | browser extension (JS) | — | no | stays under `tools/` |

No command reads a credential file checked into the repo: every reference is `os.Getenv` or a comment saying `source .env.bench` (gitignored); the R7 `.private/*.env` references are the same pattern in comments.

## Dependencies

| PR | Module | Outcome |
|----|--------|---------|
| #363 | aws-sdk-go-v2 group (5) | merged via #526 (v1.47.1 / config 1.33.6 / credentials 1.20.6 / manager 1.23.10 / s3 1.113.4) |
| #451 | smithy-go 1.27.8 → 1.28.2 | merged via #526 |
| #453, #449 | azcore 1.23.1, azidentity 1.14.1 | merged via #526 |
| #450 | goldmark 1.8.6 | merged via #526 |
| #448 | testify 1.12.1 | merged via #526 |
| #406 | codeql-action upload-sarif v3 → v4 | merged via #526 |
| #455 | x/crypto 0.55 → 0.57 | **closed**: requires `go 1.26` (D-24); CVEs unreachable (govulncheck) |
| #452, #454, #456 | x/net 0.59, x/time 0.16, x/sys 0.48 | **closed**: require `go 1.26` (D-24) |
| — | stripe-go v75.11 | untouched (WP-R10-6: moves the 2023-08-16 pin) |

`go mod tidy` is a no-op on `main` (before and after). Orphan check (`go mod why`): `cloudflare/circl` ← `crypto/postquantum.go` (D-3), `tetratelabs/wazero` ← `drivers/wasm.go` (D-2), `gopkg.in/yaml.v3` ← `internal/k8s` (D-1), `klauspost/reedsolomon` ← `cmd/tools/erasure-bench` (tool), Azure SDKs ← `drivers/onedrive.go` (live) + permafrost benches, `fsnotify` ← `drivers/local.go` (live). `gorilla/mux` went in #518; no `msgraph`/`k8s.io` module exists. Weight waiting on D-1/D-2/D-3: three direct requires.

## Deletion list (what went, LOC, and what waits)

| Item | Handed by | Verified dead by | Removed in | LOC |
|------|-----------|------------------|------------|----:|
| `internal/webhooks` (pkg) | R0 D-6, R11-24 | not in `go list -deps`; no importer | #524 | 884 |
| `internal/events` (pkg) + its per-request use | R11-24 (rename?) | the only caller was the leak (R15-01) | #524 | 52 + 15 |
| `internal/cache` (`tiered_cache.go`) + `cachingReader` | WP-R6-6 / WP-R0-3 | `EnableCaching: false` since #337 | #524 | 76 + ~60 |
| `internal/intelligence` (4 files) + engine `LogAccess`/`GetHotData`/… + retention policy row | WP-R6-5 | override removed in R6-03; analytics hard-coded; `access_patterns` → D-12 | #524 | 493 + ~80 |
| `engine/tiering.go`, `selector.go`, `cost_optimizer.go` (+ tests), `SetBackup`/`replicateToBackup`, `SetCostConfiguration`/`SetEgressCosts` + `main.go` price table, `Config{CacheSize,EnableML,EnableCaching}` | WP-R6-4 | `StartTiering` no caller; selector/optimizer never consulted; `SetBackup` no caller | #524 | ~1,300 |
| `compliance/soc2.go` (+ test) | R0 (813 lines unreachable) | deadcode 14/14 | #524 | 1,087 |
| `dashboard/handlers/{files,help,dashboard}.go` (+ tests) | D-8, R12-27 | no route | #524 | 311 |
| `auth.AuditLogger`, `GenerateAPIKeyWithAudit`, `ValidateAPIKeyWithAudit`, `APIKeyAuditEvent`, `KeyRotationPolicy`/`CheckRotationNeeded`, `SetAuditLogger`; the dead `AuthHandler` (`Register`/`Login`/`RequestPasswordReset` that returned the token in the body/`CompletePasswordReset`), `validateTimestamp`, `getEndpointURL`, `ValidateSlug` | R11, R5-20, WP-R5-4 | deadcode + grep (three tests existed only for `AuthHandler`) | #524 | ~330 |
| `MFAService.ValidateBackupCode` (in-memory) + `backupCodes` map | R5 | the persisted `AuthService.ValidateBackupCode` is the live one | #524 | 32 |
| `dashauth.NewBasicAuth`/`BasicAuth.Validate` | R5 | 0 callers | #524 | 22 |
| `IDriveDriver.PutWithSize`/`putMultipart`/`abortMultipartUpload` (+ fields), `LoadIDriveConfig`, `NewIDriveDriverFromConfig` | R7-14 | 0 callers | #524 | 168 |
| `NotificationDispatcher.SetHTTPClient` | R3-26 | 0 callers | #524 | 8 |
| `usage.FreeTierLimits.BandwidthBytes` | R10-26 | read by nothing | #524 | 3 |
| `database.Postgres` CRUD wrapper (`CreateTables`, `*Artifact*`, `Exec`/`Query*`) | R9-21, WP-R0-4 | deadcode 15/17 | #525 | ~170 |
| `scripts/verify_*.sh` ×5, `test_no_auth.go`, `auth/db_test_fix.go` | R0-19, WP-R0-11 | invoked by nothing | #524 | 710 |
| `scripts/{deploy,run-dev,run-prod}.sh` | R15-10 | stale | #525 | ~120 |
| `nightly.yml` + 8 `tests/*` suites + plan/script | R15-02 | see triage | #525 | ~2,400 |
| `compat_test_helpers.go` | R0-08 | wrappers only for the moved suite | #525 | 41 |
| `cmd/quotaless-*` ×4 | R15-11 | dead account | #525 | ~890 |
| `internal/api/compat_test_helpers.go`, `release.yml` | R0-08, R15-14 | — | #525, #528 | 41 + 30 |

**Total removed by R15:** #524 −5,988/+172, #525 −3,704/+970, #528 (docs) — about **9.7k lines** net.

**Waits on a decision (not deleted):** D-1 (`k8s`, `container`, `global`, `ha`, `slo`, `integrations` — 21.7k src LOC), D-2 (`engine/{sla,disaster_recovery}.go`, `drivers/wasm.go`), D-3 (`crypto/postquantum.go`), D-4 (`api/metrics.go`), D-7 (`cmd/tools/loadtest` + `internal/loadtest`), D-9 (`engine/{analytics,capacity,replicator,load_balancer}.go`, `drivers/cost_advisor.go`), D-12 (39 orphan tables incl. `bandwidth_rollover`, `billing_charges`, `subscriptions`, `access_patterns`, `tiering_policies`, `tenant_cost_daily`), D-14 (`cmd/tools/dedup-migrate`), WP-R11-5 (`compliance/{gdpr,consent,ropa,privacy,portability}` + `/api/compliance` mount). `HealthScorer` (`engine/health.go`) is **kept**: it is what `/health` and `/status` display.

## Invariants confirmed

- `go list -deps ./cmd/vaultaire | grep FairForge`: 23 internal packages, none under `cmd/tools` or `tests` (was 26 before #524).
- `go build ./... && go vet ./... && go vet -tags integration ./...` clean; `GOOS=linux` build and lint clean (the linux-only `sparse_unix.go` finding was caught by CI and fixed); `GOOS=windows` still does not build (`local.go:397`, R0-19 — recorded, release.yml deleted).
- `golangci-lint run ./...` 0 issues with `.golangci.yml`; `gosec` 0 issues with the `security.yml` flags; `govulncheck` reports no reachable module vulnerability.
- `go mod tidy` no-op; `make build && make clean && git status --ignored` leaves only `.DS_Store`/`.vscode`/`__pycache__`(removed by clean)/`bench-results/*.json`/`.private` — no build output.
- Tenant-isolation invariants 1–10 (R5) re-verified on `main` @ `afb5b63` — see `SYNTHESIS.md`; all hold.
- Every rule-file series still exists in code (R13 invariant 7 re-checked by grep after the deletions: no metric was registered by a deleted package).

## Dead code noted (confirm removed)

Everything in the deletion table above. Still present by decision: the D-1/D-2/D-3/D-4/D-7/D-9/D-12/D-14 items. New from this session's `deadcode` run (270 unreachable functions on `main` before #524; the remaining ones after are the class-B files, `crypto/{encryption,compression,chunker,keymanager}.go` partials (WP-R0-4), `api/metrics.go` (D-4), `drivers/geyser_admin.go` (tool library), the `Execute/Query/Train/Predict/GetContainerMetadata/GetArtifactMetadata` interface stubs (XS, with D-9), `NotificationDispatcher.FireSync` and `InventoryRunner.GenerateReportNow` (test seams, kept), `ResetAvailableIDriveRegions` (test seam), `engine/errors.go` `ErrPermissionDenied`/`WrapError`, `engine/interface.go` `WithUserMetadata`, `LoginRateLimiter.Cleanup` (kept exported), `dashboard/middleware/flash.go` `GetFlash`, `landing/hash.go` two helpers, `tenant.MustFromContext`, `crypto/config.go` `GetPreset`, `crypto/gci.go` `LookupChunks`.

## Follow-up work packages

| WP | Title | Files | Size | Depends on |
|----|-------|-------|------|------------|
| WP-R15-1 | Chunk-signature verification for `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` (R15-06): per-chunk HMAC checked in `awsChunkedReader` with the header signature as seed; fixture = a raw aws-cli capture against a known test secret | `auth/sigv4*.go`, `api/s3_engine_adapter.go`, `s3_multipart.go` | S | — |
| WP-R15-2 | Render `data["RowsError"]` in the four `populate*` dashboard helpers (or return the error) instead of a silently short page; add the two XS tests R1 asked for (log lines never carry the query string/Authorization; every rule-file series exists) | `dashboard/handlers/{overview,usage,buckets,bucket_analytics}.go`, templates; `api/prom_metrics_test.go` | XS | — |
| WP-R15-3 | Go 1.26 toolchain (D-24): `go.mod`, four `setup-go` lines, re-run the suite, then re-take the four x/ bumps and WP-R10-6 | `go.mod`, `.github/workflows/*.yml` | XS | Isaac |
| WP-R15-4 | Make `gosec` a required check in `main-protection` once one more `main` run is green; bump CI's `postgres:15` → `16` with WP-R9-3 | repo settings, `ci.yml` | XS | one green run |
| WP-R15-5 | `GOOS=windows` build (`drivers/local.go:397 syscall.Stat_t` behind a `_unix` file) or a build constraint on the whole driver — only if a Windows binary is ever wanted | `drivers/local*.go` | XS | nobody asked |
