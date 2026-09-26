# Vaultaire Full Code Review Plan

**Created:** 2026-09-25 · **Repo state:** `main` @ `cd61047` (#467) · **Owner:** Isaac

This plan splits the whole codebase into 16 review sessions (R0–R15). Each session is
sized to fit one fresh Claude Code session and has a copy-paste prompt below. Run them in
order where a dependency is listed; otherwise any order works. Findings from each session
go in `docs/reviews/RNN-<slug>.md` so later sessions (especially R15) can read them.

## Status tracker

Update this table at the end of every session.

| # | Section | Depends on | Status | Findings file | PRs |
|---|---------|-----------|--------|---------------|-----|
| R0 | Dead code & scaffolding triage | — | **done 2026-09-26** — 41 pkgs + 77 files deleted (−83.5k LOC); post-merge review added R0-13..19 + test-DB isolation; 6 pkgs + 15 files await decisions D-1..D-10 | [R0-dead-code.md](reviews/R0-dead-code.md) | #473, #474 |
| R1 | Entry point, server wiring, config, shutdown | R0 | **done 2026-09-26** — 19 findings (1 P0 client-IP trust, 2 P1 shutdown order/exit race — all three fixed; 5 P2, 11 P3 incl. a `.gitignore` rule hiding `cmd/vaultaire/*_test.go`, fixed → 11 WPs); nine areas answered with evidence; ops items for [YOU] in WP-R1-1 | [R1-server-wiring.md](reviews/R1-server-wiring.md) | #475, #476 (R1-20 XFF last-occurrence) |
| R2 | S3 object data path (GET/PUT/HEAD/DELETE, chunked) | R0 | not started | | |
| R3 | S3 multipart, copy, batch, reaper | R2 | not started | | |
| R4 | S3 bucket-level features | R2 | not started | | |
| R5 | Auth, SigV4, keys, STS, sessions, RBAC, tenant isolation | R0 | **done 2026-09-26** — 28 findings (1 P0 key revocation never persisted, 6 P1 — copy-source scope bypass, reset-token replay, MFA disable without password, scoped-key create failure, OAuth MFA bypass, Google unverified-email link; 5 of 7 P0/P1 fixed here, OAuth-MFA → WP-R5-2); SigV4 verified with real aws-cli + rclone captures; 10 tenant-isolation invariants for R15; RBAC stub decision D-11 for Isaac | [R5-auth.md](reviews/R5-auth.md) | #477 |
| R6 | Engine core (routing, failover, tiering, replication) | R0 | **done 2026-09-26** — 27 findings (6 P1 fixed here: SDK-v2 404 casing + client-cancel breaker pollution, outage reported as 404, access-tracker relocating `auto`-bucket overwrites to Lyve, write failover onto r2/geyser/permafrost/EU regions, Delete ignoring the recorded location after restart; 1 P1 handed to R13 — access-log/inventory writers use the wrong container namespace; 9 P2, 11 P3 → 9 WPs); placement narrative for ARCHITECTURE.md + driver contract table for R7; cache decision = delete (WP-R0-3) | [R6-engine.md](reviews/R6-engine.md) | PR_PLACEHOLDER |
| R7 | Storage drivers | R6 | not started | | |
| R8 | Crypto, chunking, GCI dedup, GC | R2 | not started | | |
| R9 | Database layer & migrations | R0 | not started | | |
| R10 | Billing, quota, usage, account lifecycle | R9 | not started | | |
| R11 | Management/user/webhook/compliance/admin APIs | R5 | not started | | |
| R12 | Dashboard (customer + admin), templates, OAuth | R5 | not started | | |
| R13 | Background jobs & maintenance runners | R6, R8 | not started | | |
| R14 | Public surface, docs, OpenAPI drift, email | R11, R12 | not started | | |
| R15 | Tests, CI, tooling, repo hygiene + final synthesis | all | not started | | |

## Codebase facts the reviews should know

Measured 2026-09-25 (`wc -l`, non-test lines / test lines):

| Package | Source | Tests | Notes |
|---------|-------:|------:|-------|
| internal/api | 19,056 | 19,557 | 160 files; `s3_engine_adapter.go` alone is 2,111 lines |
| internal/drivers | 9,593 | 6,518 | 93 files; `geyser_admin.go` 1,282, `local.go` 1,192, `onedrive.go` 1,100 |
| internal/dashboard/handlers | 7,375 | 6,863 | 39 handler files, 41 templates/static files |
| internal/auth | 5,677 | 4,126 | saml/ldap/AD/oauth/serviceaccount files are 100% unreachable |
| internal/crypto | 4,125 | 5,673 | `gci.go` 870 |
| internal/engine | 3,839 | 2,259 | `engine.go` 870 |
| internal/cache | 3,838 | 2,152 | ~~every function unreachable~~ **R0 correction:** `tiered_cache.go` (`TieredCache`) is live — it is the engine's read cache (`engine.go:34`); the other 16 files were dead and are removed |
| internal/usage | 2,142 | 1,131 | |
| internal/billing | 1,071 | 540 | |
| internal/database | 435 | 326 | 61 migration files, 003→063 (001/002/053 absent, 004 duplicated) |
| cmd/* | ~13,000 | ~500 | 24 commands; only `cmd/vaultaire` is the product |

**Reachability (from `go list -deps ./cmd/vaultaire` and `deadcode ./cmd/vaultaire`):**

- 23 internal packages are linked into the binary. **48 are not linked at all**:
  alerting, apikeys, apm, audit, container, devops, docs/* (9 subpackages), gateway (+3),
  global, ha, integrations, k8s, loadtest, logger, logging, metrics, monitoring, perf,
  pipeline, queue, ratelimit, reporting, retention, slo, storage, streaming, testing/* (10),
  tracing, webhooks. Together roughly 60k lines of source + tests that CI builds and runs.
- Inside the 23 linked packages, `deadcode` reports **928 unreachable functions**:
  drivers 219, cache 132, auth 132, crypto 104, engine 75, usage 63, rbac 62,
  compliance 54, api 32, database 22, handlers 12, billing 7.
- 80+ source files are 100% unreachable (every function dead). Full list is regenerated by
  the R0 prompt (R0 measured 96; 62 removed, the rest listed in `reviews/R0-dead-code.md`);
  headline ones: all of `internal/cache/*` **except `tiered_cache.go`**, `auth/{saml,ldap,activedirectory,
  oauth,serviceaccount,sso}.go`, `crypto/{postquantum,pipeline,tls,backend_integration}.go`,
  `drivers/{geyser_admin,regional_failover,retry,circuit_breaker,smart_cache,cost_advisor,
  egress_predictor,resumable,throttle,bandwidth_quota,s3_auth,s3_iam,fallback,health,wasm}.go`,
  `engine/{load_balancer,disaster_recovery,capacity,analytics}.go`, `compliance/{soc2,iso27001,
  dashboard}.go`, `api/{metrics,middleware}.go`.

**Unit-test coverage baseline** (`go test -short -cover ./internal/...`, 2026-09-25, all
packages passing; linked packages only):

| Package | Cov | Package | Cov | Package | Cov |
|---------|----:|---------|----:|---------|----:|
| api | 39.6% | auth | 62.9% | billing | 38.2% |
| crypto | 61.5% | drivers | 55.6% | engine | 59.1% |
| database | 22.7% | flags | 14.0% | usage | 62.1% |
| dashboard | 76.3% | dashboard/auth | 46.0% | dashboard/handlers | 57.8% |
| dashboard/middleware | 93.5% | rbac | 74.5% | compliance | 72.0% |
| email | 67.7% | tenant | 73.3% | docs | 100% |

The launch-critical packages with the lowest coverage are api, billing, database and
flags. Note that api's 39.6% is over 19k lines, so the untested surface there is the
largest in absolute terms.

**Runtime shape (from `cmd/vaultaire/main.go` + `internal/api/server.go:129-483`):**

- Drivers registered by env: local (always), s3, lyve, quotaless, geyser, idrive (+ one per
  region), r2, permafrost (OneDrive fleet). Primary auto-detect: idrive > quotaless > s3 >
  geyser > local.
- `NewServer` starts at least 12 background goroutines on `context.Background()`:
  flags cache, bandwidth flusher, CDN analytics flusher + rollup, access-log flusher +
  delivery, inventory job, dedup GC, smart demotion (flag-gated), multipart reaper,
  bandwidth alerter, backend health loops, cert monitor. `Shutdown` should be checked
  against each.
- Route families: S3 (catch-all), `/api/v1/manage`, `/api/v1/user`, `/api/v1/webhooks`,
  `/api/v1/events`, `/api/v1/sts`, `/api/v1/admin`, `/api/rbac`, `/api/compliance` (~40
  routes), `/dashboard/*`, `/admin/*`, `/cdn/*`, `/auth/*`, `/webhook/stripe`, public pages.

**Known open items carried in from memory (verify, do not assume):**

- `isBackendFailure` SDK-v2 404 casing bug pollutes circuit breakers (noted 2026-09-24).
- Multipart uploads bypass the chunking/dedup path (deferred since #400).
- Chunk decrypt ignores key rotation; WP-4 body-hash verification not done; `SIGV4_ENFORCE`
  kill-switch removal pending.
- Unconditional boot health check in `server.go` (Quotaless) is a CI flake source.
- Prod Lyve health check dials us-east-1 while the driver default is us-west-1.
- gosec is RED again (G703/G704/G124 via #404 probe tool).
- Dependabot batch blocked on Go 1.26 (2 MEDIUM x/crypto CVEs). `go.mod` says `go 1.25.0`;
  `CLAUDE.md` says Go 1.24.6.
- No `.golangci.yml` exists; `make lint` runs golangci-lint defaults only.
- CSAM detection (plan item 5.5.1) is missing from the launch gate list.

---

## Shared preamble (paste at the top of every review prompt)

```
You are reviewing the Vaultaire codebase at /Users/viera/fairforge/vaultaire (Go 1.25,
chi/v5, PostgreSQL, Zap). This is review session <RNN> of the plan in
docs/CODE_REVIEW_PLAN.md — read that file first, then the root CLAUDE.md and every
CLAUDE.md in the directories you touch. If the plan lists a dependency session, read its
findings file in docs/reviews/ before starting.

Ground rules
- Review first, fix second. Produce the findings file before changing product code.
- Every finding must cite file:line and be verified by reading the code (and running the
  relevant package tests if it is a behavioural claim). No speculation, no "might".
- Severity scale: P0 = data loss / security / cross-tenant / money; P1 = wrong behaviour a
  customer would hit; P2 = correctness risk or unsafe pattern not yet triggered;
  P3 = quality, dead code, docs drift.
- Non-negotiables from CLAUDE.md apply to your review lens: stream never buffer, ctx
  first parameter, errors wrapped with context, HEAD served from object_head_cache, ETag
  = MD5 of stream, authenticated backend probes only.
- Do NOT touch main. If you fix things, branch `review/<RNN>-<slug>` from main, TDD
  (failing test first), commit as `fix(scope): description [Review RNN]`, open a PR with
  `gh pr create --base main`, and merge with `gh pr merge --auto --squash` only after
  build-and-test is green. Never `--admin`. No Co-Authored-By lines.
- Fix in this session only P0/P1 items that are small and isolated. Everything else goes
  in the findings file as a proposed work package.
- Write findings to docs/reviews/<RNN>-<slug>.md using this layout:
    # <RNN> <title> — <date>
    ## Scope reviewed (files, line counts, what was skipped and why)
    ## Findings (table: ID | Sev | file:line | What | Why it matters | Proposed fix)
    ## Invariants confirmed (things you checked that are correct — so R15 can trust them)
    ## Dead code noted (hand to R0 if R0 has not run, otherwise confirm removed)
    ## Follow-up work packages (each: title, files, est. size, depends-on)
- Finish by updating the Status tracker row in docs/CODE_REVIEW_PLAN.md (status, findings
  file, PR numbers) and committing that together with the findings file on your branch.
```

---

## R0 — Dead code & scaffolding triage

**Why first:** roughly a third of the repository never links into the product binary.
Removing or quarantining it shrinks every later session and makes `deadcode`/`gosec`/CI
signal meaningful. Deleting is Isaac's call for anything tied to a future plan phase, so
this session classifies, proposes, and only deletes the unambiguous set.

```
<shared preamble>

R0 — Dead code & scaffolding triage

Goal: produce an authoritative inventory of code not reachable from cmd/vaultaire,
classify it, delete the unambiguous set in one PR, and leave a decision list for the
rest.

Step 1 — measure (do not trust the numbers in the plan, regenerate):
  go list ./internal/... | sort > /tmp/all.txt
  go list -deps ./cmd/vaultaire | grep vaultaire/internal | sort > /tmp/used.txt
  comm -23 /tmp/all.txt /tmp/used.txt            # packages never linked
  go run golang.org/x/tools/cmd/deadcode@latest ./cmd/vaultaire > /tmp/deadcode.txt
  For each file in /tmp/deadcode.txt compare dead-func count to `grep -c '^func '` to
  find files that are 100% dead. Also list every cmd/* directory and note which are
  benches/probes vs product.

Step 2 — classify every unreachable package and every 100% dead file into exactly one of:
  A. Scaffolding with no reference in docs/IMPLEMENTATION_PLAN.md and no importer outside
     its own tests → delete.
  B. Referenced by a plan phase ≥ 6 (k8s, global, ha, federation, gateway, streaming,
     queue, etc.) → propose quarantine (move under internal/future/ or delete-and-rely-on-
     git). Do NOT delete in this session; list with the plan line that references it.
  C. Dead inside a live package (e.g. auth/saml.go, cache/*, drivers/retry.go,
     crypto/postquantum.go) → delete if no plan phase references it, else B.
  D. Test-only helpers that are wrongly in non-_test files (api/compat_test_helpers.go,
     api/test_db_fix.go, database/test_helpers.go, drivers/test_helpers.go) → move to
     _test.go or an internal/testutil package.
  Grep the plan for each package/file name before classifying; cite the plan line.

Step 3 — delete class A and D in ONE branch `review/R0-dead-code`. After each package
  removal run `go build ./... && go vet ./... && go test -short ./...`. Also remove
  the corresponding go.mod requirements if `go mod tidy` drops them (gorilla/mux, wazero,
  azure sdk, gojsonschema, restic/chunker, circl are candidates — verify, do not assume).
  Report the LOC delta and go.mod delta in the PR body.

Step 4 — findings file docs/reviews/R0-dead-code.md must contain: the class A/B/C/D
  tables with LOC, the plan-line citations for every B, the list of 100%-dead files
  inside live packages, and the go.mod dependencies that could be dropped after B is
  decided. Add a "Decision needed" section for Isaac listing B items with a
  recommendation each.

Also record (do not fix): the untracked build artifacts in the repo root (bench-*,
permafrost-*, vaultaire-bin, *.bin, test*.txt) — they are gitignored, just confirm none
are tracked and propose a Makefile `clean` target that removes them.
```

---

## R1 — Entry point, server wiring, config, shutdown

```
<shared preamble>

R1 — Entry point, server wiring, config, graceful shutdown

Files: cmd/vaultaire/main.go (373 lines), internal/api/server.go (1,237),
internal/api/flags_wiring.go, internal/config/*, internal/flags/service.go,
internal/api/health_handlers.go, internal/api/backend_probes.go, internal/api/cert_expiry.go,
internal/api/prom_metrics.go, internal/api/middleware.go, internal/api/management_ratelimit.go,
internal/api/ratelimit.go, internal/api/cors.go, internal/api/not_found.go,
.env.example, configs/.

Questions to answer with evidence:
1. Startup order. main.go builds engine → drivers → server. Does anything in NewServer
   assume a driver or the DB exists before it is set? What happens with db == nil (the
   nilQuotaManager path): walk every s.db use in server.go and the handlers it wires and
   list nil-deref risks.
2. Goroutine lifecycle. Enumerate every goroutine started in NewServer/Start (grep
   `go s.`, `go func`, `Start(`); for each, what context is it on, does Shutdown stop it,
   and can it write to the DB after db.Close()? Produce a table.
3. Shutdown. main.go's signal handler calls eng.Shutdown then server.Shutdown then
   os.Exit(0) with a 30s budget. Verify in-flight uploads drain, the HTTP server is
   actually asked to stop (http.Server.Shutdown vs Close), and background flushers get a
   final flush. Check the deploy workflow's swap/health-check timing assumes the same.
4. Config surface. Diff every os.Getenv in the tree against the CLAUDE.md env table and
   .env.example. List undocumented, documented-but-unused, and default mismatches.
   Check ReadTimeout/WriteTimeout/IdleTimeout and MaxHeaderBytes (see #398 history).
5. Middleware chain. Print the chain in order (router.Use). Check request-ID, logging,
   recover, request-limit, version, CORS ordering; confirm panics in handlers cannot
   kill the process and that the logging middleware never logs Authorization or
   presigned signature values.
6. Health endpoints. /health, /health/live, /health/ready, /health/backends, /ready:
   what does each actually check, is any of them unauthenticated-but-expensive, and
   does the deploy workflow's health check hit the right one? Confirm the backend
   probe rules in CLAUDE.md (signed HeadBucket / console / TCP only). Check the
   known nit: Lyve probe region vs driver default region.
7. Feature flags. internal/flags/service.go cache semantics, default fallbacks when the
   DB is down, and every call site (`flags.` grep) — is any flag read on the hot path
   without the cache?
8. Prometheus. Metric names/labels cardinality (tenant IDs as labels?), registration
   duplicates, and whether /metrics is reachable from the public internet through HAProxy.

Run: go test -race ./internal/api/ -run 'Server|Health|Flag|Config' -v, and
`go vet ./cmd/... ./internal/api/...`.
```

---

## R2 — S3 object data path

```
<shared preamble>

R2 — S3 object data path: GET / PUT / HEAD / DELETE, plain and chunked

Files: internal/api/s3.go (860), internal/api/s3_engine_adapter.go (2,111 — the core),
internal/api/s3_chunked_put_pool.go, internal/api/range.go, internal/api/conditional.go,
internal/api/metadata.go, internal/api/s3_content_disposition.go,
internal/api/s3_content_encoding.go, internal/api/s3_errors.go, internal/api/patterns.go,
internal/api/quota_accounting.go (the PUT-side reserve/release), internal/api/cdn.go.
Read internal/api/CLAUDE.md first.

Walk each operation end to end and write the call graph into the findings file:
1. Request parsing (s3.go ParseRequest/determineOperation): path-style vs virtual-host,
   URL-encoding of keys (spaces, +, %2F, unicode), query-string sub-resource routing
   order (?acl ?tagging ?uploads ?versioning etc. — see the #442 ?acl bug history),
   and what happens with a bucket name that fails validation.
2. PUT (HandlePut → handleChunkedPut / engine.Put). Check in this order:
   a. Streaming: is the body ever read fully into memory? Trace every io.ReadAll,
      bytes.Buffer, []byte allocation sized from Content-Length. The aws-chunked reader
      and the chunked-PUT pool are the suspects.
   b. ETag: MD5 computed over the stream, never of an empty string, and what ETag a
      chunked/dedup'd object returns (must be stable across GET/HEAD/List).
   c. Content-Length vs actual bytes: mismatch handling, 0-byte objects, missing
      Content-Length, Transfer-Encoding chunked, x-amz-decoded-content-length.
   d. Quota: reserve before write, release on failure, and the failure ordering when the
      backend write succeeds but the head-cache/DB write fails (orphan on backend?).
   e. Storage-class resolution (resolvePutStorageClass, publicBucketStorageClass,
      bucketTierStorageClass, bucketRegionDriver): confirm PUBLIC → r2 only, that a
      tier/residency preference cannot route a private object to r2, and that
      storageClassDisablesChunking is consistent with the Vault archive path.
   f. Head-cache write: every field HEAD later needs (size, etag, content-type,
      metadata JSONB, encoding, disposition, storage class, version id) is written.
   g. Versioning + object lock interactions on overwrite.
3. GET (HandleGet → handleChunkedGet). Range requests (single range, suffix range,
   unsatisfiable → 416, multi-range rejected cleanly), conditional headers
   (If-Match/If-None-Match/If-Modified-Since with weak ETags), Content-Encoding
   passthrough, the fire-and-forget access-time touch goroutine at ~line 305 (does it
   leak under load, is it bounded?), prefetch worker count and error propagation in
   handleChunkedGet (a failing chunk mid-stream must not produce a 200 with truncated
   body), and the missing-object → 404-not-500 fix from #415.
4. HEAD: served from object_head_cache only. Verify no backend call on any HEAD path,
   including versioned HEAD and HEAD on a delete marker.
5. DELETE: versioning (delete marker vs hard delete), object lock refusal, quota release,
   head-cache invalidation, chunk refcount decrement path (hand details to R8), and the
   "delete of a non-existent key returns 204" S3 rule.
6. Error mapping (s3_errors.go): every engine error → S3 code; look for any path that
   returns 500 for a client error or leaks internal error text into the XML body.
7. Tenant scoping: for every DB query and every engine call in these files, confirm
   the tenant ID comes from the auth context, never from the request. Write the list
   of query sites into "Invariants confirmed".

Tests: go test -race ./internal/api/ -run 'S3|Put|Get|Head|Delete|Range|Chunk' and
note which of these paths have no test at all. If the local DB is available, use the
`verify` skill to run one aws-cli put/get/head/range/delete cycle against a local build.
```

---

## R3 — S3 multipart, copy, batch, reaper

```
<shared preamble>

R3 — S3 multipart upload, CopyObject, batch delete, multipart reaper

Files: internal/api/s3_multipart.go (829), internal/api/s3_copy.go (510),
internal/api/s3_batch.go (212), internal/api/multipart_reaper.go (203),
internal/api/s3_chunked_put_pool.go (shared with R2), migrations 024_multipart_uploads.sql,
plus the multipart env knobs (MULTIPART_MAX_UPLOAD_BYTES, MULTIPART_ABANDON_HOURS,
MULTIPART_TERMINAL_RETENTION_DAYS). Read docs/reviews/R2-*.md first.

1. State machine. Draw it: initiate → upload part (with retries/overwrites of the same
   part number) → complete / abort → reaper. For every transition list the DB writes,
   the backend writes, and what a crash between them leaves behind. The two goroutines
   at s3_multipart.go ~455 and ~485 — what are they, are they bounded, what context?
2. Complete. Part ordering validation, ETag-per-part validation, the final ETag format
   (`<md5-of-md5s>-<n>`), minimum part size rule (5 MiB except last), total size vs the
   per-upload byte cap, and quota reservation (is it reserved per part or at complete?).
   The known gap "multipart bypasses chunking": document exactly what a completed
   multipart object looks like on the backend and in object_head_cache versus a
   chunked PUT of the same bytes, and whether GET/HEAD/Copy/Versioning behave
   identically. Propose the WP to close it.
3. Assembly. How are parts stitched — server-side concatenation via streaming, or a
   buffered assembly? Trace memory and temp-disk use for a 50 GiB upload with 10,000
   parts. Confirm the framing-corruption fix from item 1.11 (2026-07) is covered by a
   test.
4. Abort + reaper. Idle-upload abort, terminal-row purge, orphan part cleanup on the
   backend, and the boot-reap path. Is the reaper safe if two instances run (advisory
   lock)? Does it release quota reservations?
5. UploadPartCopy and CopyObject (s3_copy.go). Metadata directive REPLACE vs COPY,
   cross-bucket same-tenant copy, the deferred cross-tenant copy (must be refused with
   403 not 404), copy of a chunked source into a non-chunked destination, copy onto
   itself with metadata change, and Object Lock / versioning on the destination. Memory
   profile of the copy (streamed?). Note the Lyve CopyObject CRC32 quirk from memory.
6. Batch delete (s3_batch.go): 1,000-key limit, quiet mode, per-key error reporting,
   versioned deletes, and that a failure mid-batch does not report success for
   undeleted keys.
7. ListMultipartUploads / ListParts: pagination correctness and tenant scoping.

Tests: go test -race ./internal/api/ -run 'Multipart|Copy|Batch|Reaper' -v. List
untested transitions.
```

---

## R4 — S3 bucket-level features

```
<shared preamble>

R4 — S3 bucket-level features and sub-resources

Files: internal/api/s3_buckets.go (369), s3_list.go (393), s3_list_versions.go (270),
s3_versioning.go (220), s3_lock.go (495), s3_tagging.go (212), s3_acl.go (211),
s3_notifications.go (382), s3_inventory.go (409), s3_logging.go (173),
s3_restore.go (128), s3_mfa_delete.go (89), s3_presign.go (306), presigned.go (77),
access_log.go (291), cors.go (57). Migrations 025, 026, 027, 028, 035, 036, 040, 041.
Read docs/reviews/R2-*.md first.

For each sub-resource, answer: (a) is it routed before the object catch-all and does
`?acl`-style routing tolerate empty values and extra query keys; (b) is the XML schema
what aws-cli / rclone / s3cmd / Cyberduck send (test with the real request bodies, not
hand-written ones); (c) tenant scoping of every query; (d) what the S3 error is when the
bucket does not exist vs is not yours (must both be the same code to avoid enumeration).

Specific checks:
1. Buckets: name validation (DNS rules, 3–63, no uppercase, no IP form), create-on-
   existing (same owner = 200, other owner = 409 BucketAlreadyExists), delete non-empty
   → 409 BucketNotEmpty including versions and in-progress multiparts, and the slug /
   visibility / CORS / cache-TTL fields in the buckets table being validated on write.
2. List (v1 + v2): delimiter/common-prefixes correctness with keys containing the
   delimiter more than once, marker/continuation-token round trip, max-keys=0, encoding-
   type=url, key ordering (byte order), and cost — does a list of a 1M-key bucket page
   through the DB or the backend, and is the query indexed (check migration 057)?
3. Versioning: enable/suspend semantics, is_latest maintenance on PUT/DELETE, version
   listing pagination with key-marker + version-id-marker, delete-marker handling in
   List vs ListVersions, and GET ?versionId of a non-latest version served from the
   right backend location.
4. Object Lock / WORM: retention mode COMPLIANCE vs GOVERNANCE, bypass header +
   permission, legal hold, retention extension only (never shorten in COMPLIANCE),
   what DELETE and overwrite do while locked (#442 mapped ObjectLocked→AccessDenied —
   confirm), and whether lock state survives copy/multipart complete.
5. MFA delete: header parsing, that it cannot be enabled without MFA on the account,
   and the interaction with STS/scoped keys.
6. Presigned URLs: expiry bounds (max 7 days), signed-header subset validation, the
   Cloudflare HEAD→GET flake (memory item 5.1), query-string signature vs header
   signature precedence, and that a presigned PUT cannot change storage class or ACL
   beyond what the signer was allowed.
7. Notifications / webhooks (bucket-level config): config validation, event filtering,
   and what dispatches them (hand to R13 if a runner).
8. Inventory + logging + access log: output format, destination bucket must be same
   tenant, delivery job scheduling (hand runner internals to R13), and that access-log
   lines never include the Authorization header or query signature.
9. Restore (V18.2 minimum recall): 403 InvalidObjectState on cold GET, restore request
   idempotency, x-amz-restore header format, staged-restore 500 fix from #428 tested.
10. CORS: preflight handling on both the S3 path and /cdn, and that a bucket's CORS
    config cannot be set to reflect arbitrary Origin with credentials.

Tests: go test -race ./internal/api/ -run 'Bucket|List|Version|Lock|Tag|ACL|Notif|
Inventory|Logging|Restore|MFA|Presign|CORS'. Use the verify skill for one aws-cli pass
over versioning + lock + presign if the local DB is up.
```

---

## R5 — Auth, SigV4, keys, STS, sessions, RBAC, tenant isolation

```
<shared preamble>

R5 — Authentication, authorization, and tenant isolation

Files: internal/auth/{auth.go, handlers.go, sigv4.go, sigv4_payload.go, apikey.go,
scoped_keys.go, sts.go, auth_mfa.go, mfa.go, password_reset.go, email_verify.go,
slug.go, profile.go, preferences.go, backfill.go}, internal/api/sts_routes.go,
internal/api/rbac_integration.go, internal/rbac/*, internal/dashboard/auth/*,
internal/dashboard/middleware/*, internal/tenant/*, internal/apikeys/* (unreachable —
confirm), migrations 005, 006, 007, 021, 022, 023, 031, 032. Read internal/auth/CLAUDE.md.
If R0 has run, ignore the deleted saml/ldap/AD/oauth/serviceaccount files; if not, note
them as dead and do not review them.

This is the highest-stakes section. Write the "tenant isolation invariants" list
carefully — R15 will re-verify it.

1. SigV4. Canonical request construction (URI encoding rules, query sorting, duplicate
   query keys, header trimming/lowercasing, signed-headers subset), UNSIGNED-PAYLOAD and
   STREAMING-AWS4-HMAC-SHA256-PAYLOAD handling (sigv4_payload.go), clock skew window,
   date header sources (X-Amz-Date vs Date), region/service pinning (does any region
   validate?), constant-time signature comparison, and replay: is there any nonce or
   is the 15-min window the only protection? Check the SIGV4_ENFORCE kill switch: what
   does the non-enforcing path allow, and is it safe to remove now.
2. Credential lookup order: tenants (primary) → api_keys (VLT_ scoped) → sts_tokens
   (ASIA). For each: hashing at rest, timing of the DB lookups (fixed-cost on miss?),
   suspended-tenant handling before vs after signature check, key expiry, revoked-key
   caching (can a revoked key keep working for N seconds?), and per-key rate limiting.
3. Scoped keys: the scope grammar, prefix/bucket/permission intersection, and every
   S3 handler that must consult the scope (grep for the scope-check helper and list the
   handlers that do NOT call it — those are findings).
4. STS: token minting, scope intersection with the parent key (can a token escalate?),
   expiry, hourly cleanup, and that ASIA tokens cannot mint further tokens or manage
   keys.
5. JWT (management + dashboard APIs): alg pinning (no `none`, no alg switching), secret
   source (JWT_SECRET required), expiry, audience/issuer, revocation on password change
   and on MFA disable, and where the token is carried (cookie flags: HttpOnly, Secure,
   SameSite; header).
6. Passwords: hashing algorithm + params, reset-token entropy/expiry/single-use, email
   verification token HMAC (VERIFY_SECRET), enumeration resistance on login/reset/
   register responses and timing, and the signups feature-flag gate at
   CreateUserWithTenant covering all three entry points (form, API, OAuth).
7. MFA/TOTP: secret storage, backup codes hashing + single-use, the pending-MFA store
   (memory? expiry?), the admin reset path, and MFA enforcement for admin role.
8. Sessions (dashboard/auth): DB-backed store, fixation on login, rotation on privilege
   change, IP/UA binding behaviour, revoke-all, and the memory-store fallback when
   db == nil.
9. RBAC: role → permission table, the RequirePermission/RequireRole middleware, every
   admin route's guard (grep `requireAdmin`, `RequireRole`, `/admin`), and the RBAC
   audit endpoints' own auth.
10. Tenant isolation sweep: grep every `WHERE` in internal/api, internal/auth,
    internal/dashboard/handlers, internal/usage, internal/billing for a tenant_id /
    user_id predicate; list every query that lacks one and justify it or flag it.
    Also confirm no handler takes tenant/user ID from a query param, path, or header.
11. Registration invariant: users → tenants → api_keys → tenant_quotas in one
    transaction; what state remains if step 3 or 4 fails.

Tests: go test -race ./internal/auth/... ./internal/rbac/... ./internal/dashboard/auth/...
./internal/dashboard/middleware/... Add negative tests for any gap you fix.
```

---

## R6 — Engine core

```
<shared preamble>

R6 — Engine: routing, failover, tiering, replication, caching

Files: internal/engine/{engine.go (870), interface.go, failover.go, tiering.go,
routing.go, selector.go, health.go, monitor.go, storage_class.go, replicator.go,
migrator.go, migration_progress.go, cost_optimizer.go, sla.go, performance_monitor.go,
errors.go, types.go, context.go}, internal/intelligence/*, internal/common/*.
Read internal/engine/CLAUDE.md. Skip files R0 removed.

1. The Driver interface contract (interface.go). For each method write the contract as
   the engine assumes it (error for missing object? List semantics? Exists cost?) and
   check that contract against docs/DRIVERS.md. R7 will verify each driver against it.
2. Get/GetRange/Put/Delete/List in engine.go. Trace buildCandidateList /
   buildWriteCandidateList / applyBackendRecommendation: how is the backend chosen,
   what does HintBackend do, and can a read for an object stored on backend X ever be
   served from backend Y (stale/dup — see the Permafrost delete-resurrection history)?
   How are object → backend locations persisted (migration 048_object_locations)?
3. Failover (failover.go + the #402 body-safety fix): retry on a consumed body must be
   impossible; breaker open/half-open/close thresholds; the isBackendFailure SDK-v2
   404-casing bug (a NotFound must never count as a backend failure) — fix it here with
   a test if confirmed; what error the client gets when all candidates fail.
4. Put write path: primary write, replicateToBackup goroutine (bounded? context?
   failure surfaced anywhere?), writeFailures counter, size-tracking reader, and the
   cachingReader — is anything buffered here?
5. Tiering (tiering.go, storage_class.go): the storage-class → driver map for every
   class (STANDARD, STANDARD_IA, GLACIER/DEEP_ARCHIVE → geyser, PUBLIC → r2, resilient
   → lyve, permafrost), StartTiering job, and confirmation that nothing in the engine
   can place a private object on r2.
6. Health (health.go, monitor.go): how driver health feeds candidate selection, probe
   cadence, and what "unhealthy primary" does to writes (fail loudly per #347?).
7. Shutdown: what it waits for.
8. Dead/unused: GetHotData/GetAccessPatterns/GetRecommendations/Execute/Query/Train/
   Predict — confirm callers or hand to R0.
9. Concurrency: run go test -race ./internal/engine/... and read every mutex — is
   drivers map access guarded on AddDriver/GetDriver/GetDriverNames?

Findings should include a one-page "how an object gets placed and found" narrative for
docs/ARCHITECTURE.md (R14 will merge it).
```

---

## R7 — Storage drivers

```
<shared preamble>

R7 — Storage drivers

Files: internal/drivers/{local.go (1,192), s3.go, s3compat.go, s3upload.go, lyve.go,
lyve_console.go, idrive.go, idrive_regions.go, geyser.go, quotaless.go, r2.go,
onedrive.go (1,100), transport.go, sparse_unix.go, xattr_unix.go, plugins/*},
internal/drivers/*_README.md, internal/drivers/CLAUDE.md. Read docs/reviews/R6-*.md
first; skip files R0 removed (geyser_admin.go is a probe-tool dependency — check whether
cmd/geyser-* still need it before treating it as dead).

For EVERY driver fill in one row per interface method with: error on missing object
(exact error type), List semantics (prefix, pagination, max keys, ordering), Exists
cost, HealthCheck = signed HeadBucket / console action / TCP (must match CLAUDE.md
rule 1), streaming (no io.ReadAll / bytes.Buffer of the body), context propagation to
the SDK, retry/timeouts, and multipart threshold used by the SDK uploader.

Then per driver:
1. local.go: path traversal on container/artifact (".." and absolute paths), atomic
   write (temp + rename), fsync policy, sparse/xattr use, directory listing cost, and
   permissions of created files/dirs.
2. s3.go / s3compat.go / s3upload.go: shared SigV4 client config, path-style toggle,
   region handling, checksum settings (aws-sdk-go-v2 default CRC32 vs endpoints that
   reject it — the Lyve CopyObject quirk), and the manager uploader part size /
   concurrency vs the engine's own parallelism (double parallelism?).
3. lyve.go: per-region bucket homing (README), console probe with the root key (note
   the memory item: prod uses the ROOT key — findings must recommend a service user),
   and the us-east/us-west probe mismatch.
4. idrive.go / idrive_regions.go: per-region key fallback rules, endpoint table
   correctness vs the reseller API doc, and what happens when a regional key is
   missing.
5. geyser.go: whole-object archive path (>64 MB rule), restore initiation and status
   polling, and never treating a cold-read 403 as a backend failure.
6. r2.go: single fixed bucket with tenant-prefixed keys — prefix cannot be escaped by a
   crafted key; jurisdiction endpoint handling; and that the driver refuses non-PUBLIC
   use if asked (or document that the guard lives in the API layer only).
7. quotaless.go: raw HTTP + UNSIGNED-PAYLOAD requirement; the unconditional boot health
   check flake — make it conditional on the driver being configured.
8. onedrive.go: FNV deterministic placement + probe fallback, Delete fleet-sweep,
   dual-transport (HTTP/2 API, HTTP/1.1 CDN), token refresh concurrency, and the
   "never reorder TENANT_N" warning being enforced or at least logged at boot.
9. transport.go: shared http.Transport limits (MaxIdleConnsPerHost, timeouts,
   ForceAttemptHTTP2) and that no driver builds its own unbounded transport.

Tests: go test -race ./internal/drivers/... -short. For live checks use the
backend-matrix tool (cmd/backend-matrix) only with creds already in the environment —
do not paste keys.
```

---

## R8 — Crypto, chunking, GCI dedup, GC

```
<shared preamble>

R8 — Encryption, FastCDC chunking, global content index, dedup GC

Files: internal/crypto/{gci.go (870), chunker.go, chunk_encryption.go, keymanager.go,
encryption.go, sse_s3.go, ssec.go, config.go, compression.go}, the chunked paths in
internal/api/s3_engine_adapter.go (handleChunkedPut ~1045, storeChunkLocked ~1363,
fetchAndVerifyChunk ~1416, handleChunkedGet ~1474), internal/api/dedup_gc.go,
internal/api/s3_chunked_put_pool.go, migrations 016, 037, 051, 052, 054, 057, 058.
Read internal/crypto/CLAUDE.md, .private/PRIVACY_NORTH_STAR.md (decisions), and
docs/reviews/R2-*.md. Skip postquantum/pipeline/tls/backend_integration if R0 removed
them.

1. Threat model statement first: who holds which key (ENCRYPTION_MASTER_KEY, per-tenant
   keys, SSE-C customer keys), what an attacker with DB read gets, what one with backend
   read gets, and what one with both gets. Compare with the north-star doc.
2. Chunker: FastCDC parameters (min/avg/max), determinism across versions, and the
   bounded memory of one chunk (max chunk size × CHUNK_PUT_CONCURRENCY = peak per PUT —
   compute it and compare with the 50 GiB multipart cap and server RAM).
3. GCI (gci.go): row schema, ciphertext-hash-on-row fix (WP-7), refcount increment /
   decrement atomicity (IncrementRef rows-affected fix from #344), dedup scope
   (per-tenant vs global — migration 054 — and the convergent-encryption privacy
   implication), the advisory locks in storeChunkLocked, and the manifest format
   (versioned? what happens to old manifests on a format change — the "versioned-chunk
   manifests" deferred WP).
4. Chunk encryption: AEAD choice, nonce derivation (never reused across chunks with the
   same key), key ID stored with the chunk, and the KNOWN ISSUE "decrypt ignores key
   rotation" — confirm, and write the fix WP (decrypt with the key ID recorded on the
   chunk).
5. SSE-S3 / SSE-C: header handling, SSE-C key never persisted or logged, the SSE-C chunk
   leak fix (#340), and HEAD/GET of an SSE-C object without the key → 400 not 500.
6. Body integrity: WP-4 "body hash verification" is open — document exactly where a
   corrupted chunk would be detected (fetchAndVerifyChunk) and where it would not
   (non-chunked path?), then propose the WP.
7. Dedup GC (dedup_gc.go): sweep candidates, the sweep cache invalidation + advisory
   lock fixes (#344), displaced-manifest leak fix (#343), stale-blob fallthrough (#341),
   and a crash mid-sweep. Is the GC safe against a concurrent PUT that re-references a
   chunk being deleted? Show the interleaving.
8. Key management: master-key format check (64 hex), absent = disabled (is that loud?),
   rotation procedure documented, and keymanager cache eviction.
9. Compression: where it sits relative to encryption (compress-then-encrypt only),
   and whether it is actually enabled anywhere.

Tests: go test -race ./internal/crypto/... and the chunk tests in internal/api. Add a
test for any nonce/refcount claim you make.
```

---

## R9 — Database layer & migrations

```
<shared preamble>

R9 — Database layer, schema, migrations, query audit

Files: internal/database/{postgres.go, history.go, config.go, migrations.go},
internal/database/migrations/*.sql (61 files, 003→063), internal/database/CLAUDE.md,
.github/workflows/deploy.yml (migration step), scripts/deploy.sh.

1. Migration runner: how migrations are applied in deploy (the workflow runs them
   before swap), ordering by filename, idempotency claims (CREATE IF NOT EXISTS) — grep
   every migration for statements that are NOT idempotent (ALTER TABLE ADD COLUMN
   without IF NOT EXISTS, CREATE INDEX without IF NOT EXISTS, INSERT seeds, DROP,
   ALTER TYPE). Explain the numbering gaps (001, 002, 053 missing; two 004 files) and
   whether the runner tolerates them. Is there a schema_migrations table, and if not,
   how is "already applied" decided?
2. Build the schema: apply all migrations to a scratch DB (createdb vaultaire_review;
   apply in order) and dump it. Then grep every SQL string in internal/ (`"SELECT`,
   `"INSERT`, `"UPDATE`, `"DELETE`, `"WITH`, backtick SQL) and check each referenced
   table/column exists in the dump. List mismatches (this catches queries against
   columns that no migration created).
3. Indexes: for each hot query (head cache lookup, list objects by prefix, versions
   is_latest, chunk refcount, sts token lookup, sessions, idempotency) confirm a
   covering index exists and the query uses it (EXPLAIN on the scratch DB with a few
   thousand rows). Review 057_index_cleanup.sql for anything dropped that is still
   needed.
4. Referential integrity + deletion: tenant/user deletion (038, 056) cascade order —
   what rows survive an account deletion (should be: nothing tenant-scoped except the
   audit trail). Compare with account_deletion.go's ExecuteDeletion (note: deadcode says
   it is unreachable — is deletion ever executed?).
5. Types: tenant_id text vs uuid inconsistencies (058_chunk_tenant_text is a hint),
   timestamp vs timestamptz, bigint for sizes everywhere (no int4 sizes), JSONB metadata
   validation.
6. Connection handling (postgres.go): pool limits, statement timeouts, context use on
   every query, transaction helpers, and behaviour when the DB goes away mid-run
   (degrade gracefully claim).
7. Backups: daily pg_dump at 03:00 UTC with 7-day retention — is restore documented and
   tested (Gate A / DR test from #335)? Point to the runbook.
8. Test infra: test_helpers.go / test_config.go live in a non-test file (hand to R0 if
   not done); DATABASE_URL handling in CI vs local.

Output: a docs/DATABASE.md draft (tables grouped by domain, owner package, retention)
for R14 to merge.
```

---

## R10 — Billing, quota, usage, account lifecycle

```
<shared preamble>

R10 — Stripe billing, quotas, usage accounting, bandwidth, account export/deletion

Files: internal/billing/{stripe.go, webhook.go, metered.go}, internal/usage/*,
internal/api/{quota_accounting.go, quota_management.go, user_quota.go, usage.go,
bandwidth.go, bandwidth_alerts.go, cdn_analytics.go, account_export.go,
account_deletion.go, idempotency.go}, internal/dashboard/handlers/{billing.go,
admin_revenue.go, admin_costs.go, usage.go}, migrations 019, 020, 034, 043, 060.
Read internal/billing/CLAUDE.md, .private/SMART_TIER_DESIGN.md (the 2026-09-21
QUOTA-SOLD decision), and docs/reviews/R9-*.md.

1. Pricing model vs code. The decision is hard quota, no launch meters, allowances keyed
   to quota. Write the table: plan → Stripe price env → storage quota → egress
   allowance → what happens at 100% (reject PUT with which S3 error) → what happens on
   egress overage (throttle? bill? nothing?). Every cell must cite code. Flag any
   metered path (metered.go, STRIPE_METER_*) that is still live at launch.
2. Quota accounting: reserve/commit/release around PUT, multipart, copy, delete, and
   version overwrite; the reconciliation job (quota-reconcile admin endpoint); logical
   vs physical bytes (dedup means physical < logical; we charge logical); and drift
   detection. Race: two concurrent PUTs that each fit but together exceed.
3. Stripe: checkout session creation, webhook signature verification, event dedup
   (stripe_events table), idempotent handling of subscription.created/updated/deleted,
   invoice.paid/failed → tenant state transitions (suspend on failure? grace period?),
   plan change proration, and what happens if the webhook arrives before the local
   subscription row exists. Test mode vs live key detection.
4. Free tier defaults (034) and signups-closed interaction.
5. Bandwidth: tracker flush cadence, daily rollups (060), alert thresholds, the
   Cloudflare-fronted egress attribution, and per-tenant bandwidth limit enforcement
   (admin sets it — where is it enforced on GET?).
6. Account export (GDPR portability): what is included, async job status, download
   auth, and expiry of the export artifact. Account deletion: request → grace → execute
   — deadcode reports ExecuteDeletion unreachable; determine whether deletion ever runs
   and file P0/P1 accordingly.
7. Idempotency cache for management API: key scope (per tenant), body-hash check,
   24h TTL cleanup goroutine.
8. Admin revenue/costs pages: are the COGS numbers hard-coded, and do they match
   .private/BUSINESS_OUTLOOK_2026-09-22.md?

Tests: go test -race ./internal/billing/... ./internal/usage/... and the quota tests
in internal/api. Stripe webhook tests must use fixture payloads with valid signatures.
```

---

## R11 — Management, user, webhook, compliance, admin APIs

```
<shared preamble>

R11 — JSON APIs: /api/v1/manage, /api/v1/user, /api/v1/webhooks, /api/v1/events,
/api/v1/sts, /api/v1/admin, /api/rbac, /api/compliance

Files: internal/api/{management_routes.go (852), user_api.go (456), webhooks_routes.go
(452), events.go (334), sts_routes.go, admin_flags.go, management_errors.go,
management_ratelimit.go, idempotency.go, rbac_integration.go, server.go:587-810
(setupRoutes + registerComplianceRoutes)}, internal/compliance/* (linked parts only),
internal/webhooks/* (unreachable — confirm), internal/events/*, internal/audit/*
(unreachable — confirm), migrations 008–015, 029, 033, 045, 046, 047.
Read docs/reviews/R5-*.md first.

1. Route inventory with auth. Produce a table of every route under these prefixes with:
   middleware chain, auth type (JWT / API key / admin / none), rate limit, idempotency
   support. PRIORITY: registerComplianceRoutes mounts ~40 routes at /api/compliance —
   determine whether they are behind any auth at all and whether they act on the
   caller's tenant or on a tenant ID in the body. Same for /api/rbac.
2. Management API: bucket create/delete/tier/residency parity with the S3 path (same
   validation, same quota, same audit), key create returns the secret exactly once,
   pagination (limit clamp, starting_after), and error envelope consistency
   (management_errors.go) — no stack traces or SQL in responses.
3. User API: profile/preferences update mass-assignment (can a user set is_admin,
   tenant_id, plan?), API key rotate/expire semantics, MFA enable/disable requiring a
   fresh password or TOTP, and DELETE /api/v1/user vs the account-deletion flow in R10.
4. Webhooks: URL validation (block private ranges / localhost / metadata IPs — SSRF),
   secret generation and HMAC signing of deliveries, retry/backoff, delivery log
   retention, and the test-delivery endpoint being rate limited. Events listing:
   cursor pagination and tenant scoping.
5. Admin API: requireAdmin implementation, the trigger endpoints (dedup-gc,
   smart-demotion, quota-reconcile) being idempotent / single-flight, flags PUT
   validation (unknown key, per-tenant override of a global kill switch).
6. Compliance package: SAR, deletion requests, consent, breach, ROPA, privacy controls —
   which of these are real features vs scaffolding with in-memory state that vanishes
   on restart? For anything that stores customer PII, check retention and export.
7. Rate limiting: management_ratelimit.go keying (per tenant? per IP? behind
   Cloudflare — is CF-Connecting-IP trusted only when the peer is Cloudflare/HAProxy?),
   and the memory growth of the limiter map.
8. Audit trail: which of these actions write an audit row, and which should but do not.

Tests: go test -race ./internal/api/ -run 'Mgmt|Management|UserAPI|Webhook|Event|STS|
Admin|Compliance|Idempot|RateLimit'. Add negative auth tests for every route you find
unprotected.
```

---

## R12 — Dashboard (customer + admin), templates, OAuth

```
<shared preamble>

R12 — Web dashboard: router, handlers, middleware, templates, OAuth, public forms

Files: internal/dashboard/{router.go, *.go}, internal/dashboard/handlers/* (39 files,
7,375 lines), internal/dashboard/middleware/*, internal/dashboard/templates/** (41),
internal/dashboard/static/**, internal/dashboard/handlers/oauth.go (Google/GitHub),
internal/api/{landing.go, waitlist.go, docs_pages.go}. Read internal/dashboard/CLAUDE.md
and internal/dashboard/handlers/CLAUDE.md, and docs/reviews/R5-*.md.

1. Route/guard table for every /dashboard/* and /admin/* route: session required,
   email-verified required, admin required, CSRF token checked on POST, rate limit.
   Any POST without CSRF is a finding. Any /admin route not under the admin guard is P0.
2. Templates: html/template everywhere (no text/template for HTML), no `template.HTML`
   / `template.JS` wrapping of user data, every user-controlled value (bucket names,
   object keys, notes, abuse reports, support notes, tenant names, emails) rendered
   escaped; inline JS reading data attributes rather than interpolation; and the
   Content-Security-Policy header if any. Check htmx partial responses for the same.
3. Handlers: mass assignment on settings/profile/bucket-settings forms, file upload
   handler (size limit, streaming to engine, content-type), bucket_objects listing
   pagination for big buckets, restore/restore-status handlers, API-key generation
   showing the secret once, sessions revoke, and export/delete-account confirmations.
4. Admin handlers: tenant suspend/enable/quota/tier/bandwidth-limit/reset-mfa write an
   audit row with the admin's ID; abuse action handler state machine; support notes
   PII; backends set-primary/force-check safety (can an admin click set r2 primary?);
   flags set/clear; notifications count polling cost.
5. OAuth (dashboard/handlers/oauth.go): state parameter (random, bound to session,
   single-use), PKCE for GitHub/Google where supported, email-verified claim required
   before linking, account linking to an existing email (takeover risk), and the
   B2 OAuth residual (#417).
6. Public forms: /register, /login, /forgot-password, /reset-password, /abuse,
   /api/waitlist — rate limits, enumeration, the signups flag, honeypot/captcha
   presence, and abuse-form input size limits.
7. Legal pages: templates exist for aup, baa, cookies, data-act, dpa, gdpr, privacy,
   terms — check the effective dates and that pricing/plan text matches the QUOTA-SOLD
   decision and the Oct 31 launch copy (memory: launch copy has stale competitor and
   pack prices).
8. Static assets: served with cache headers, no directory listing, no source maps
   with secrets, and htmx/alpine versions pinned with SRI if loaded from a CDN.
9. Cookie flags and session lifetime, logout clears server-side session, and
   "remember me" if present.

Tests: go test -race ./internal/dashboard/... Use the verify skill to load the login,
dashboard, and one admin page against a local build and check response headers.
```

---

## R13 — Background jobs & maintenance runners

```
<shared preamble>

R13 — Background jobs: smart demotion/promotion, dedup GC, inventory, access-log
delivery, multipart reaper, bandwidth alerter, backend health loops, cert monitor,
flag cache, STS cleanup, idempotency cleanup

Files: internal/api/{smart_demotion.go (503), smart_promotion.go (362), dedup_gc.go,
s3_inventory.go (runner part), access_log.go (delivery part), multipart_reaper.go,
bandwidth_alerts.go, cdn_analytics.go (rollup), backend_probes.go, cert_expiry.go,
idempotency.go (cleanup), server.go:510-585}, internal/flags/service.go,
internal/auth/sts.go (cleanup), internal/engine/tiering.go (StartTiering).
Migrations 055, 062, 063. Read docs/reviews/R6-*.md and R8-*.md, and
docs/IMPLEMENTATION_PLAN.md §5.15.8.

Build ONE table with a row per job: trigger (ticker interval / boot / admin endpoint),
feature flag gate, context used, single-flight protection (advisory lock / mutex),
multi-instance safety (two prod binaries during deploy swap — is that possible?), DB
transaction boundaries, per-run cap (bytes/rows/time), failure handling (log-and-
continue vs abort), metrics emitted, and shutdown behaviour. Then:

1. Smart demotion (5.15.8): candidate selection query (hot fraction, idle days, min
   age, max GB per run, tiers), the etag guard on reclaim after grace, what happens if
   the cold write succeeds and the ledger insert fails (double copy) or vice-versa
   (data loss — P0 if possible), and read-time promotion (063) racing a demotion of the
   same object. Confirm the flag defaults OFF and the env knobs are parsed with sane
   bounds.
2. Dedup GC: covered in R8 for correctness — here check scheduling, run overlap with
   smart demotion (both move chunks?), and lock ordering between them.
3. Inventory + access-log delivery: destination writes go through the normal PUT path
   with quota accounting? Output bucket ownership re-checked at delivery time?
4. Multipart reaper: covered in R3 — here confirm cadence and boot-reap logging.
5. Bandwidth alerter + email: dedup of alerts (once per threshold per period), email
   sender failure handling, and alert state reset at period rollover.
6. Backend health loops + cert monitor: probe cadence, breaker interaction, metric
   names match deploy/monitoring/*.yml alert rules (grep each rule's metric in code).
7. Flag cache: ~15s refresh; behaviour when the DB is unavailable (last-known vs
   default) and boot ordering (are flags loaded before the first request?).
8. STS + idempotency cleanup: cadence and that they delete by expiry only.
9. Deploy interaction: .github/workflows/deploy.yml swaps binaries with a health check;
   what does an in-flight demotion or GC do when SIGTERM arrives — is the ledger left
   consistent?

Tests: go test -race ./internal/api/ -run 'Demot|Promot|GC|Inventory|Reaper|Alert|
Probe|Cert|Flag'. Write a table of untested job transitions.
```

---

## R14 — Public surface, docs, OpenAPI drift, email

```
<shared preamble>

R14 — Public HTTP surface, generated docs, OpenAPI vs routes, email, repo docs

Files: internal/api/{landing.go, docs_pages.go, llms_txt.go, security_txt.go,
changelog.go, health_handlers.go (status page), waitlist.go, cdn.go, not_found.go,
cdn_analytics.go}, internal/docs/* (linked package: OpenAPI + Swagger UI; the 9
subpackages are unreachable — confirm removed by R0), internal/email/* + templates,
docs/{README-level docs: API.md, ARCHITECTURE.md, CONFIG.md, DEPLOY.md, DRIVERS.md,
PRODUCT_FEATURES.md, SCALE_TESTING.md, index.md, guides/, references/}, README.md,
CLAUDE.md, every per-directory CLAUDE.md, deploy/monitoring/README.md.
Read docs/reviews/R11-*.md and R12-*.md.

1. OpenAPI drift: extract every route from the chi router (write a tiny test that
   walks s.router with chi.Walk and prints method+pattern) and diff against
   internal/docs' OpenAPI spec. List routes missing from the spec, spec paths that do
   not exist, and parameter/response mismatches for /api/v1/manage and /api/v1/user.
2. /docs/* pages, /llms.txt, /changelog, /status, /.well-known/security.txt: content
   correctness (pricing, plan names, limits, endpoints, region names), HEAD support,
   cache headers, and that /status does not leak backend names/creds/IPs.
3. CDN path (/cdn/{slug}/{bucket}/*): public-bucket gate, tenant context fix (#410),
   Range + conditional support, cache TTL from the bucket row, rate limiter keying, and
   the analytics tracker not being a write amplifier on hot objects.
4. Email: sender abstraction, provider config (memory: email-in-prod was the last
   skipped launch item), templates escaping, link building from VAULTAIRE_BASE_URL only,
   and which flows send mail (verify, reset, bandwidth alert, deletion, billing).
5. Docs accuracy sweep: for each file in docs/ and each CLAUDE.md, list statements that
   contradict the code as reviewed in R1–R13 (Go version, env vars, storage-mode order,
   table names, migration count, backend list, health probe rules). Fix the docs in this
   session (docs-only PR). Merge the ARCHITECTURE narrative from R6 and DATABASE.md from
   R9.
6. README.md / CONTRIBUTING.md / CODE_OF_CONDUCT.md: are they for the public repo or
   internal? Remove anything that exposes internal hostnames, and make the build/test
   instructions match the Makefile.
```

---

## R15 — Tests, CI, tooling, repo hygiene, final synthesis

```
<shared preamble>

R15 — Test suite, CI/CD, tooling, dependencies, and synthesis of R0–R14

Files: Makefile, .pre-commit-config.yaml, .github/workflows/{ci,deploy,nightly,release,
security}.yml, go.mod/go.sum, tests/** (chaos, k6, benchmarks, compatibility, security,
regression, acceptance, integration, load), cmd/* (24 commands), scripts/*.sh,
tools/geyser-grabber, deploy/*, .claude/skills/verify, and ALL docs/reviews/R*.md.

Part A — test suite and CI
1. Coverage baseline: go test -short -race -coverprofile=cov.out ./internal/... then
   per-package %; record in the findings file. Identify packages under 40% that are
   launch-critical (api S3 paths, auth, engine, drivers, billing) and list the missing
   tests by name (pull from the "untested" lists in R2–R13).
2. Flakes: run go test -race -count=3 ./internal/... and list anything that fails
   intermittently; cross-check the CI flake memory (TestRequestQueue, TestPipeline_Run,
   Quotaless boot check).
3. tests/: which suites run in CI, which need live creds, which are dead. Decide keep/
   move/delete for each (k6 scripts, chaos shell scripts, benchmarks). Move live-cred
   suites behind a build tag if they are not already.
4. CI workflows: ci.yml (Postgres service, env vars, cache), nightly.yml and
   security.yml (what runs, is gosec red and why — fix the #nosec misuse from #404
   or the findings, and get gosec green), release.yml (is it used?), deploy.yml
   (migration step, swap, rollback on /health/live failure, secrets scoped to the
   environment — #440). Add golangci config (.golangci.yml) with the linters the team
   actually wants and make it pass.
5. Dependencies: go.mod says go 1.25.0, CLAUDE.md says 1.24.6, dependabot is blocked on
   1.26 — pick the version, update CLAUDE.md/CI/Dockerfile if any, and merge the
   dependabot batch (x/crypto CVEs first). Remove dependencies R0 orphaned.
6. cmd/*: classify the 24 commands (product / probe tool / bench / dead). Move probes
   and benches under cmd/tools/ or tools/ with a README per tool, or delete. Ensure none
   are built by `make build` and none read creds from files checked into the repo.
7. scripts/: verify_*.sh, run-*.sh, deploy.sh, push-to-slc.sh — which are current?
   Delete or fix. scripts/test_no_auth.go should not be a loose Go file.
8. Makefile: add clean of root artifacts (from R0), a `deadcode` target, `lint` using
   the new config, and `test-integration` that actually exists.

Part B — synthesis
9. Read every docs/reviews/R*.md. Build docs/reviews/SYNTHESIS.md:
   - All P0 and P1 findings in one table, deduplicated, with status (fixed in PR #,
     open), owner, and launch-gating yes/no (launch is 2026-10-31).
   - The tenant-isolation invariants list from R5, re-verified against the current
     main (spot-check 10 query sites yourself).
   - Work packages proposed across sessions, merged and ordered: launch-gating first,
     then post-launch, each with files, size estimate, and dependencies.
   - Docs/plan updates needed in docs/IMPLEMENTATION_PLAN.md (add a "Review 2026-09"
     section listing WPs by ID).
10. Update the Status tracker in docs/CODE_REVIEW_PLAN.md to complete, and write a
    memory-style handoff paragraph at the top of SYNTHESIS.md for the next session.
```

---

## How to run a session

1. Open a fresh Claude Code session in the repo.
2. Paste the **Shared preamble** with `<RNN>` filled in, then the section prompt.
3. When the session ends, check that `docs/reviews/RNN-*.md` exists and the Status
   tracker row was updated. Merge the review branch (findings + any fixes) via PR.
4. Sessions with no listed dependency on each other can run in parallel in worktrees.
   Suggested batches: {R0} → {R1, R5, R6, R9} → {R2, R7, R8, R10} → {R3, R4, R11, R12}
   → {R13, R14} → {R15}.
