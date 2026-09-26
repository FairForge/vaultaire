# R1 Entry point, server wiring, config, shutdown — 2026-09-26

**Repo state reviewed:** `main` @ `9c3330f` (#474). Branch: `review/R1-server-wiring` → PR #475.
**Depends on:** [R0-dead-code.md](R0-dead-code.md) — handoff row confirmed below; D-4/D-5 stubs
(`api/metrics.go`, `api/middleware.go`) ignored as instructed, the live code is `prom_metrics.go`
and the middlewares in `server.go`.

**Verification runs (all green before any change):**

| Command | Result |
|---|---|
| `make test-db` | `vaultaire_test: migrations applied` |
| `go vet ./cmd/... ./internal/api/... ./internal/flags/... ./internal/config/...` | clean |
| `go test -race ./internal/api/ -run 'Server\|Health\|Flag\|Config\|Middleware\|RateLimit\|Probe\|Cert' -v` | 45 pass / 14 skip with no `DATABASE_URL`; **59 pass / 0 skip** with `DATABASE_URL=…/vaultaire_test` |
| `go test -race ./internal/flags/...` | 1 pass / 4 skip without `DATABASE_URL`; **5 pass** with it |
| Prod box (`vaultaire-slc`, read-only) | `haproxy.cfg`, `systemctl show vaultaire`, `ss -ltnp`, `ufw status`, env var **names** in `/opt/vaultaire/configs/.env` |
| Public probes | `curl https://stored.ge/{metrics,health?details=true,health/backends,version,health/ready}`; `dig` for every stored.ge host |

## Scope reviewed

| File | Lines | Read |
|---|---:|---|
| `cmd/vaultaire/main.go` | 373 | fully |
| `internal/api/server.go` | 1,237 | fully |
| `internal/api/flags_wiring.go` | 41 | fully |
| `internal/config/config.go` | 49 | fully |
| `internal/flags/service.go` | 274 | fully |
| `internal/api/health_handlers.go` | 286 | fully |
| `internal/api/backend_probes.go` | 159 | fully |
| `internal/api/cert_expiry.go` | 253 | fully |
| `internal/api/prom_metrics.go` | 141 | fully |
| `internal/api/management_ratelimit.go` | 74 | fully |
| `internal/api/ratelimit.go` | 45 | fully |
| `internal/api/cors.go` | 57 | fully |
| `internal/api/not_found.go` | 45 | fully |
| `.env.example`, `configs/*.yaml` | 5 + 136 | fully |
| `.github/workflows/deploy.yml` | 188 | swap/health-check timing only |
| `deploy/monitoring/*.yml`, `deploy/monitoring/README.md` | 147 | metric names + scrape/exposure notes only |
| Lifecycle-only reads (internals are R13's) | | `bandwidth.go`, `cdn_analytics.go`, `access_log.go`, `dedup_gc.go`, `s3_inventory.go`, `smart_demotion.go`, `multipart_reaper.go`, `bandwidth_alerts.go`, `idempotency.go`, `auth/sts.go`, `billing/metered.go`, `dashboard/auth/auth.go` (`StartCleanup`), `engine/engine.go` (`NewEngine`, `Shutdown`, `SetPrimary`, `CheckDriver`), `engine/tiering.go` (`Start/Stop`) |
| Client-IP callers | | `s3.go:843-858` (`extractClientIP`), `s3.go:331`, `waitlist.go:39`, `dashboard/middleware/ratelimit.go`, `dashboard/router.go:60,81,131`, `cdn.go:40-75`, `cdn_middleware.go` |

**Skipped and why:** handler bodies behind the router (R2–R4, R11, R12); job internals (R13); the auth
package beyond the nil-DB guards and `CheckIPAllowlist` (R5); `engine` beyond lifecycle (R6);
`docs/DEPLOY.md` (stale, describes a `serve --config` CLI that does not exist — R14).

## Findings

| ID | Sev | file:line | What | Why it matters | Proposed fix |
|---|---|---|---|---|---|
| R1-01 | **P0** | `internal/api/s3.go:845-858` (`extractClientIP`), used at `s3.go:331` (API-key IP allowlist) and `:585` (access-log source IP), `waitlist.go:39`; `internal/dashboard/middleware/ratelimit.go:83-98` (`ClientIP`), used by the login/reset/abuse limiters at `dashboard/router.go:60,81,131` and the session IP at `:450,541,658` | Both helpers trust `CF-Connecting-IP` and the **first** `X-Forwarded-For` entry from any peer. Verified on the box: HAProxy has `option forwardfor` (appends, never strips) and no header sanitising; `s3.stored.ge` resolves to the origin `38.147.105.54` (grey-cloud, deliberately — `deploy/ufw-cloudflare-lockdown.sh` header), so on the S3 API a client-supplied `CF-Connecting-IP` reaches the handler verbatim. Through Cloudflare, `X-Forwarded-For` is *appended* (client-supplied first entry preserved), so the dashboard helper is spoofable there too. | `CheckIPAllowlist` (`auth/scoped_keys.go:90`) is defeated by one header — a stolen IP-restricted key works from anywhere. Login brute-force limiter (5/min per IP) keyed on an attacker-chosen string. Access-log/waitlist IPs are attacker-chosen. | **Fixed here** (`internal/clientip`, PR below): peer = last XFF entry (the one HAProxy appended) or `RemoteAddr`; `CF-Connecting-IP` honoured only when that peer is inside Cloudflare's published ranges. Belt-and-braces on the box: `http-request del-header CF-Connecting-IP unless { src -f /etc/haproxy/cloudflare.lst }` (WP-R1-1). |
| R1-02 | **P1** | `cmd/vaultaire/main.go:370-372` with `:341-353` | `http.Server.ListenAndServe` returns `ErrServerClosed` the moment `Shutdown` closes the listener — before in-flight requests drain and before `Shutdown` returns. `main` treats that as `logger.Fatal` → `os.Exit(1)`. Proof program (stdlib only): Serve returned **0 ms** after `Shutdown()` was called; the in-flight handler finished **+1.7 s**; `Shutdown` returned **+2.1 s**. | On every `systemctl stop` (= every deploy) the process exits with status 1 as soon as the listener closes: active uploads/downloads are cut, and the H-3 flush at `server.go:1199-1207` never runs. Graceful shutdown (5.15.1, "COMPLETE") has never worked. | **Fixed here**: `ErrServerClosed` is not fatal; `main` waits for the shutdown goroutine, which no longer calls `os.Exit`. Test `cmd/vaultaire/main_test.go`. |
| R1-03 | **P1** | `cmd/vaultaire/main.go:350-351`; `internal/engine/engine.go:820-834` | `eng.Shutdown` runs **before** `server.Shutdown` and closes the shared `*sql.DB` (`engine.go:831` — the same handle `NewServer` received at `main.go:335`). | Requests still draining lose the database (head-cache, quota, key lookups, manifest installs), and the tracker flush in `server.Shutdown` executes against a closed pool (`sql: database is closed`). Compounds R1-02: even with the exit race fixed, the flush #419 shipped could not succeed. | **Fixed here**: `server.Shutdown` (drain + flush) first, then `eng.Shutdown`. Order asserted by test. |
| R1-04 | P2 | `internal/api/server.go:588-593` (`/health`, `/health/backends`, `/metrics` unauthenticated); HAProxy `default_backend s3_backend` for every host | `curl -s https://stored.ge/metrics` → **200, 11 KB**: full driver inventory (every `idrive-<region>`, `permafrost`, `r2`, …) as `vaultaire_backend_circuit_open{backend}` labels, Go runtime + process RSS/FDs/CPU. `/health?details=true` and `/health/backends` (with `last_error` text from signed probes) are public too. `/health/ready` runs `runtime.ReadMemStats` (STW) per unauthenticated call. | Backend topology and process telemetry to anyone; nothing secret, but it is the map an attacker wants and the scrape target Prometheus reads from `localhost`. | WP-R1-1 (ops): HAProxy `http-request deny if { path_beg /metrics }` on both frontends; consider the same for `/health/backends`. Code alternative: serve `/metrics` on `127.0.0.1:<MetricsPort>` (`config.ServerConfig.MetricsPort` exists and is unused). Drop `memory_mb` from `/health/ready`. |
| R1-05 | P2 | `internal/api/s3.go:648` (`handleHeadObject`); chain `server.go:425-429` has no recover middleware (dashboard/admin do: `dashboard/router.go:136,313`) | With `db == nil` a HEAD dereferences `s.db` → nil-pointer panic. Verified with a throwaway in-package probe (`NewServer(…, nil)`, `testMode`): PUT bucket/object, GET, LIST, multipart-initiate, DELETE, `/cdn`, `/api/v1/manage`, `/dashboard`, `/health/ready`, `/metrics` all answer cleanly; **HEAD → `http: panic serving … nil pointer dereference`**, client gets EOF. `net/http` recovers, so the process survives, but the panic goes to stderr (not Zap), is not a 5xx (not counted in `vaultaire_errors_total`), and the client sees a dropped connection. | Nil-DB is the documented dev mode ("degrades gracefully"). Prod-relevant half: any future handler panic on the S3/API chain is invisible to metrics and alerting. | WP-R1-2: `recoverMiddleware` first in the chain (Zap error + stack, count as 5xx, S3 `InternalError` XML); guard `handleHeadObject` like `s3.go:345`. |
| R1-06 | P2 | `internal/api/server.go:174-178,256-276,312,359` | 12 goroutines are started in `NewServer` on `context.Background()` (table in *Invariants*). Nothing cancels them; the flushers' own `ctx.Done` final-flush branches (`bandwidth.go:121-123`, `cdn_analytics.go:71-73`, `access_log.go:80-82`) are unreachable. `Shutdown` compensates with a synchronous flush (H-3). | Benign once R1-02/03 are fixed (process exits right after the flush). It is still a latent double-flush window: a ticker flush can run concurrently with the shutdown flush (safe today — buffer is swapped under the mutex — but only by construction). Dead code in every flusher. | WP-R1-2: one server-lifetime `ctx` created in `NewServer`, cancelled at the end of `Shutdown` after the sync flush; delete the dead `ctx.Done` flush branches or make them the only flush. R13 owns the job internals. |
| R1-07 | P2 | `docs/IMPLEMENTATION_PLAN.md:1229-1232`; `vaultaire.service` on SLC | Plan says `TimeoutStopSec=45` "verified". `systemctl show vaultaire` → `TimeoutStopUSec=1min 30s` (distro default, nothing set), `KillMode=control-group`; unit also `Wants=redis-server.service` (no Redis in the stack). 90 s > the 30 s app budget so it works by accident. | A future unit edit that sets a shorter default, or a Redis unit that fails to start, changes shutdown behaviour with no code change. Plan text is false. | WP-R1-1 (ops): add `TimeoutStopSec=45`, drop the Redis `Wants/After`, fix plan lines. |
| R1-08 | P2 | `internal/email/email.go:38-62`; prod `.env` has no `EMAIL_PROVIDER` | Prod runs `LogSender`: `POST /auth/password-reset` (`server.go:946-953`) renders the reset link with its token and the sender **logs the full email** to journald. | Reset tokens for any address land in `journalctl -u vaultaire` until email is wired (known last launch item, seq 5.6). Recorded so R14 sizes it, not new. | R14: redact bodies in `LogSender` (log recipient + subject only) regardless of provider. |
| R1-09 | P3 | Config surface (full diff in *Config surface* below) | **Undocumented (read, not in CLAUDE.md table):** `VAULTAIRE_ENDPOINT` (`server.go:892`, `auth/handlers.go:345`; prod sets it), `IDRIVE_BUCKET` (`drivers/idrive.go:48`), `TENANT_N_{ID,CLIENT_ID,SECRET,USER}` N=1..15 (`drivers/onedrive.go:125-133`; prod sets 1-3), `EMAIL_PROVIDER/EMAIL_FROM/RESEND_API_KEY/SMTP_{HOST,PORT,USER,PASSWORD}` (`email.go`), `SIGV4_ENFORCE` (`auth/sigv4.go:33`), `S3COMPAT_INSECURE_TLS` (`drivers/s3compat.go:29`), `VAULTAIRE_TUNED_TRANSPORT` (`drivers/transport.go:18`), the five concrete `STRIPE_PRICE_*` names (`server.go:867-871`). **Documented but unread:** `ONEDRIVE_CLIENT_ID/CLIENT_SECRET/TENANT_ID` (nothing reads them — the fleet driver uses `TENANT_N_*`), `ENV` (present in prod `.env`, no reader), all five vars in `.env.example` (`S3_ENDPOINT/S3_BUCKET/S3_PREFIX` have no reader; the S3 driver takes no endpoint). **Default mismatches:** `DATA_PATH` `/tmp/vaultaire-data` (`main.go:170`, `server.go:665`) vs `/tmp/vaultaire` (`management_routes.go:159,302`); `QUOTALESS_ENDPOINT` `https://us.quotaless.cloud:8000` (`main.go:212`) vs `https://io.quotaless.cloud:8000` (`server.go:540`); storage-mode auto-detect `idrive > quotaless > s3 > geyser > local` (`main.go:316-326`) vs the dashboard's copy without idrive (`server.go:667-678`) vs CLAUDE.md ("Quotaless > S3 > Geyser > local … iDrive not yet in auto-detect" — stale). **Nothing loads `configs/*.yaml`** (zero readers of `configs/`, no YAML decoder in `cmd`/`internal`); `config.Config` has yaml tags for Engine/Pipeline/Events/Cache/Backends/MetricsPort/LogLevel that nothing reads; `configs/production.yaml` advertises `read_timeout: 30s / write_timeout: 30s` — the exact #398 footgun — plus port 8080, Redis, Quotaless primary. | Operators reading CLAUDE.md or `configs/` get the wrong picture; `.env.example` is useless for a new dev; two different `/tmp` roots in dev. | WP-R1-4: delete `configs/` and prune `config.Config` to `Server.Port`; unify the two defaults; make `server.go` take `storageMode`/`dataPath` from `main` instead of re-deriving; rewrite `.env.example` from the real table; R14 rewrites the CLAUDE.md env table + auto-detect sentence. |
| R1-10 | P3 | `internal/api/server.go:1016-1022`, `health_handlers.go:159`, `server.go:1037` | `/version` is hard-coded `"0.1.0"` / build `"2025-08-12"`; `/health` and `/status` echo `"0.1.0"`. Live: `{"build":"2025-08-12","go":"go1.25.14","version":"0.1.0"}`. | No way to tell from the box which commit is serving after a deploy/rollback. | WP-R1-3: `-ldflags "-X …/internal/api.buildSHA=$GITHUB_SHA"` in `deploy.yml:31`; print it in `/version`, `/health`, the boot banner and the deploy gate. |
| R1-11 | P3 | `internal/api/server.go:433-437,456`, `cdn_middleware.go:10-23` | The host-based CDN router (`cdn.stored.ge`, `cdn.stored.cloud`) is a bare chi router: no request-ID, no body limits, no logging, no request/5xx counting. Path-based `/cdn/…` on other hosts gets the full chain. | CDN traffic invisible in `vaultaire_requests_total`/`_errors_total` and logs once `cdn.stored.ge` DNS lands ([YOU] item). | WP-R1-5: apply the same `Use` list to `cdnRouter` (one loop). |
| R1-12 | P3 | `internal/api/backend_probes.go:83-96`; `main.go:269-280` | Probes exist for `idrive`, `geyser`, `r2`, `lyve` only. The 12 `idrive-<region>` drivers (bucket-level region routing) and `permafrost` are never probed — `/metrics` shows `circuit_open` for them but no `vaultaire_backend_health` series, so a dead per-region key is invisible until a customer bucket in that region fails. | Same class of outage the alerting WP was written for (dead key for two weeks). | WP-R1-6 (R7/R13): probe every registered `idrive-*` driver with a staggered signed HeadBucket; decide whether permafrost deserves one. |
| R1-13 | P3 | `internal/api/{admin_flags,flags_wiring,s3_chunking_flag,s3_lock,s3_notifications,smart_demotion,shutdown_flush}_test.go`, `internal/flags/service_test.go:22-24` | These skip on a bare `os.Getenv("DATABASE_URL")` instead of `testutil.DSN()`, so `make test` on a dev box never runs the flag/lock/notification/demotion/shutdown-flush integration tests (14 api + 4 flags + 1 skipped in this session's first run). R0-13 moved the helpers but not these call sites. | The shutdown-flush test — the one guarding H-3 — is skipped locally by default. | WP-R1-7 (R15): replace the guards with `testutil.DSN()`; keep `DATABASE_URL` override. |
| R1-14 | P3 | `internal/api/ratelimit.go:28-32`, `management_ratelimit.go:31-33,58`; `internal/dashboard/middleware/ratelimit.go:69-79` | CDN and management limiters cap the map at 10k keys by **dropping the whole map** (every tenant's bucket refills at once — acceptable, keys are authenticated tenant IDs / bucket slugs). `LoginRateLimiter.Cleanup` is never called (R0 flagged it): three instances grow one entry per distinct client IP forever. `X-RateLimit-Reset` (`:58`) is always `now+0.6s` — not a reset time. | Unbounded map on a public, unauthenticated endpoint (login) — slow memory growth under a scan. Wrong header. | WP-R1-8: run `Cleanup` on a ticker from `dashboard.RegisterRoutes` (or cap like the others); compute Reset from `reservation.DelayFrom`. |
| R1-15 | P3 | `internal/api/server.go:1087,1100-1117` | `/webhook/stripe` is excluded from the S3-upload exemption and is not `/api/`, so it gets the 64 KB `defaultLimit`. Stripe events are usually < 20 KB, but `invoice.*` with many lines can exceed it → `413`, and Stripe retries a failing endpoint for days. | Money path (R10) with a size cliff nobody documented. | R10: give `/webhook/` the 10 MB `managementLimit`. |
| R1-16 | P3 | `internal/api/server.go:174,187,197,204,207` | Boot-time DB calls (`flags.Refresh`, `LoadFromDB`, `LoadMFAFromDB`, backfills) use `context.Background()` with no deadline. `sql.Open` does not dial, so a hung/blackholed Postgres makes the first of these block boot indefinitely — the deploy gate then rolls back a binary that is fine. | Deploy false-negative under a DB stall. | WP-R1-10: one `bootCtx, cancel := context.WithTimeout(ctx, 30s)` for the NewServer prologue. |
| R1-17 | P3 | `internal/flags/CLAUDE.md:34-40` | Registered-flags section lists `signups` and `chunking`; `smart_demotion` (registered `server.go:173`, gated `smart_demotion.go:213`) is missing. | Docs drift on the live-iteration kit. | One line — R14 or with WP-R1-4. |
| R1-18 | P3 | `internal/api/server.go:1122-1128,1143`; S3 handlers call `generateRequestID()` separately | `requestIDMiddleware` mints a UUID into the **response** header only; it ignores an incoming `X-Request-Id`/`CF-Ray`, never puts the ID in the context, and the S3 error bodies carry a *different* `RequestId`. | Two IDs per failed request; cannot correlate a customer's `RequestId` with the access log line. | WP-R1-8: honour/generate once, store in ctx, use everywhere. |
| R1-19 | P3 | `.gitignore:6` | The bare pattern `vaultaire` (meant for the root build output `./vaultaire`) also matches the **directory** `cmd/vaultaire/`, so any new file there is silently ignored — `git status` did not show this session's `cmd/vaultaire/main_test.go`. Existing tracked files are unaffected, which is why nobody noticed. | A test added next to `main.go` never reaches CI. | **Fixed here**: pattern anchored to `/vaultaire`. R15/WP-R0-5 (`make clean`) should re-check the other artifact patterns for the same mistake. |

## Fixes made in this session (PR below)

| Finding | Change | Verification |
|---|---|---|
| R1-01 | New `internal/clientip` (`FromRequest`, `IsCloudflare`, embedded Cloudflare ranges); `api.extractClientIP` and `dashboard/middleware.ClientIP` delegate to it | `internal/clientip/clientip_test.go` (7 tests: forged `CF-Connecting-IP` from a non-Cloudflare peer ignored, forged first XFF entry ignored, Cloudflare peer honours the CF header incl. IPv6, garbage never returned); existing `waitlist_test.go`, `dashboard/middleware/ratelimit_test.go`, `auth/sigv4_test.go` unchanged and passing |
| R1-02, R1-03 | `cmd/vaultaire/main.go`: `gracefulShutdown` (server → engine), `serveUntilShutdown` (`ErrServerClosed` waits for the sequence; real errors still fatal), no `os.Exit` in the signal goroutine, exit 0 + "shutdown complete" | `cmd/vaultaire/main_test.go` (order; wait; real-error path). **E2E with the built binary** (local, DB = `vaultaire_test`): register → 6 MB SigV4 PUT at 1 MB/s → `SIGTERM` 2 s in → log shows `shutting down…` at 07:54:34, `artifact stored` +3.8 s, `shutting down engine`, `shutdown complete`; curl reports `http=200 time=5.87s`; process exits after ~5 s. Before the fix the same sequence exited 1 at +0 s (proof program in R1-02). |

## Invariants confirmed

**Q1 Startup order / nil-DB.** `main.go` builds the engine (`:131`), drivers (`:166-310`), `SetPrimary`
(`:328`), then `NewServer` (`:333-338`). `NewServer` never touches a driver: probes are built only in
`Start()` from `eng.GetDriverNames()`/`CheckDriver` (`backend_probes.go:59-96`). With `db == nil` every
constructor path is guarded: `auth.LoadFromDB`/`LoadMFAFromDB` no-op (`auth.go:160`), sessions fall back
to `MemoryStore` (`server.go:250`), SSE/GCI skipped (`:218,237`), the three trackers are nil-DB safe
(`Flush` returns before `ExecContext`), `InventoryRunner`/`DedupGCRunner`/`MultipartReaper`/
`SmartDemotionRunner` constructors return `nil` and their `Start*` methods are nil-receiver safe,
`flags.New(nil)` serves defaults, `BandwidthAlerter.Start` returns on nil db, `Start()` gates the
idempotency/STS cleanups and DB session cleanup on `s.db != nil` (`:1161-1174`). Handler-level guards
exist in 25 files; the one unguarded hot path found is R1-05 (HEAD). `EnableCaching: false` at
`main.go:131-136` recorded (R0-01/R0-15 → R6, WP-R0-3); the comment still points at the deleted
`cache/lru.go`. `nilQuotaManager` reports a 1 GiB limit (`main.go:25`) — dev only.

**Q2 Goroutine lifecycle.** Every background goroutine, its context, and what stops it:

| Started at | Goroutine | Context | Stopped by `Shutdown`? | Writes DB after `db.Close()`? |
|---|---|---|---|---|
| `server.go:178` | `flags.Start` refresh loop (15 s) | `Background` | no | reads only; error logged |
| `server.go:256` | `BandwidthTracker.StartFlusher` (5 s) | `Background` | no (sync `Flush` in `Shutdown`) | would try; flush errors are logged, not retried |
| `server.go:261-262` | `CDNAnalyticsTracker.StartFlusher` (5 s) + `StartRollup` (1 h, runs once at boot) | `Background` | no (sync `Flush`) | same |
| `server.go:267-268` | `S3AccessLogTracker.StartFlusher` (5 s) + `StartLogDelivery` (5 min) | `Background` | no (sync `Flush`) | same |
| `server.go:272` | `InventoryRunner.StartInventoryJob` (1 h, acts at 00:00 UTC) | `Background` | no | yes if a tick lands mid-exit |
| `server.go:276` | `DedupGCRunner.StartDedupGC` (24 h) | `Background` | no | yes (rare) |
| `server.go:312` | `SmartDemotionRunner.Start` (24 h, flag-gated per tenant) | `Background` | no | yes (rare) |
| `server.go:359` | `MultipartReaper.Start` (immediate + 1 h) | `Background` | no | yes (rare) |
| `server.go:1158` | `startHealthChecks` → one `runBackendHealthLoop` per probe (30 s / 60 s Lyve) + `certMonitor.run` (1 h) | `Start` ctx, cancelled via `RegisterOnShutdown` | **yes** | no DB |
| `server.go:1162` | `DBStore.StartCleanup` (1 h) | `Start` ctx | **yes** | no |
| `server.go:1168` | idempotency `StartCleanup` (1 h) | `Start` ctx | **yes** | no |
| `server.go:1173` | `auth.StartSTSCleanup` (1 h) | `Start` ctx | **yes** | no |
| `server.go:1178` | `MeteredReporter.StartMeteredReporting` (immediate + 1 h) | `Start` ctx | **yes** | no |
| `server.go:1182` | `BandwidthAlerter.StartBandwidthAlerts` (immediate + 1 h) | `Start` ctx | **yes** | no |
| `engine.go:813` | `TieringEngine.Start` | — | never started (`StartTiering` has no caller); `Shutdown` closes its stop channel anyway (`engine.go:823`, harmless once) | — |

After this PR the process exits only after `server.Shutdown` (drain + sync flush) and `eng.Shutdown`
(closes the pool) return; the `Background` loops die with the process. The "writes after Close" column
is therefore a sub-second window on a periodic tick, and the trackers already log-and-drop on error.
R1-06 proposes the proper fix.

**Q3 Shutdown.** `http.Server.Shutdown` (not `Close`) is used (`server.go:1197`); `RegisterOnShutdown`
cancels the `Start` context so the probe loops exit (`:1156`). Budget is 30 s (`main.go:347`); systemd
gives 90 s (R1-07) so the app's own deadline always wins. `deploy.yml:161-171` runs `systemctl stop`
(blocks until exit), swaps, `start`, sleeps 15 s, then `curl --retry 5 --retry-delay 2
--retry-connrefused /health/live` — a 25-30 s boot window that `NewServer`'s synchronous boot work
(`LoadFromDB`, backfills, `flags.Refresh`, immediate multipart reap) fits today (uptime showed the
last deploy healthy). R1-02/R1-03 were the two reasons the sequence was not graceful; both fixed
here. `defer db.Close()` in `main` (`:124`) now actually runs (previously skipped by `os.Exit`);
double-close of `*sql.DB` is a no-op.

**Q4 Config surface.** Full diff is R1-09. `ReadTimeout`/`WriteTimeout` are unset, `ReadHeaderTimeout`
15 s, `IdleTimeout` 120 s, `MaxHeaderBytes` 1 MiB (`server.go:454-460`) — #398 holds. The 30 s values
in `configs/production.yaml` are not loaded by anything. HAProxy's `timeout client/server 50000` are
inactivity timeouts, not total-duration limits, so they do not reintroduce #398; they do mean a
backend stall > 50 s before first byte (Geyser cold read) surfaces as a HAProxy 504 — for R7/ops.
Nothing R0 deleted orphaned an env var (`ENV` in prod `.env` predates R0 and never had a reader).

**Q5 Middleware chain** (`server.go:425-429`, in order): `requestIDMiddleware` → `requestLimitsMiddleware`
→ `versionMiddleware` → `rbacService.InjectUserContext` → `loggingMiddleware` → routes. No recoverer
(R1-05) and no CORS on this chain — CORS is CDN-only (`cors.go`, applied inside `handleCDNRequest`),
which is correct: the S3 API must not answer arbitrary origins. `loggingMiddleware` (`:1139-1145`)
logs method, `r.URL.Path`, status, request-id, latency only — never the query string (presigned
`X-Amz-Signature`/`X-Amz-Credential` live there), never headers or cookies. `versionMiddleware` logs
only the client's `X-Vaultaire-Version` at Debug. `requestLimitsMiddleware`: S3 PUT/POST unlimited,
`/api/*` mutations 10 MB, everything else 64 KB (R1-15 for the webhook). Panics cannot kill the process
(`net/http` recovers; verified by the R1-05 probe) but see R1-05 for observability. Client-IP trust:
R1-01 (fixed).

**Q6 Health endpoints.**

| Route | Checks | Cost | Public exposure |
|---|---|---|---|
| `/health` (`health_handlers.go:154`) | in-memory probe states + engine breaker map; **always 200** (HAProxy `httpchk GET /health inter 5s fall 3` on all three backends — matches the 2026-07-30 rationale) | µs | yes; `?details=true` lists backend names |
| `/health/live` (`:203`) | process alive | µs | yes; **deploy gate uses this** (`deploy.yml:171`) — correct choice |
| `/health/ready` + `/ready` (`:215`) | 503 unless ≥1 probed backend healthy (true when none registered); `ReadMemStats` | STW | yes |
| `/health/backends` (`:236`) | per-backend state incl. `last_error` | µs | yes (R1-04) |
| `/status` (`server.go:1025`) | HTML from the same in-memory state | µs | yes, fine |

Probe rules (`backend_probes.go:121-141`): signed `HeadBucket` via `eng.CheckDriver` for idrive/
geyser/r2, Lyve console `RSCustomerDetails` with the root/probe key, TCP dial otherwise — **no bare
HTTP GET anywhere**. Quotaless is probed only when `QUOTALESS_ACCESS_KEY` is set (`server.go:537`,
test `TestConfiguredBackends_QuotalessOnlyWithCredentials`) and only from `Start()`, so the
"unconditional Quotaless boot health check" in the plan's known-items list is already resolved — no
change made. Lyve probe default region is `us-west-1` (`server.go:553`), equal to the driver default
(`main.go:197`); prod sets `LYVE_REGION` and `LYVE_PROBE_*` explicitly, and with the console probe the
address is diagnostic only — the "us-east-1 nit" is resolved. Live: 4 backends probed, all healthy.

**Q7 Feature flags.** `Enabled` reads the in-memory snapshot under `RLock` (`service.go:99-114`) — no
DB on the hot path; refresh every 15 s (`:85,155-173`); a failed refresh keeps the previous snapshot
(`:118-151`), nil-DB serves registered defaults; `Set`/`Unset` write through and reload. Boot order:
`Register` × 3 → `Refresh` → `Start` → `SetSignupsEnabledFunc`, all before `setupRoutes` (`server.go:170-179`,
`:431`) — loaded before the first request. Precedence tenant row → `'*'` row → in-code default, and the
`signups` default is `SIGNUPS_ENABLED` (`flags_wiring.go:34-41`) — matches CLAUDE.md. All call sites go
through the cache: `landing.go:44`, `s3_engine_adapter.go:1038`, `smart_demotion.go:213`, `admin_flags.go`
(admin API), dashboard via `Deps.Flags`. Nothing bypasses it.

**Q8 Prometheus.** Registry built once per `Server` with `prometheus.NewRegistry()` (`prom_metrics.go:29-54`),
so tests constructing many servers never double-register (default registry untouched). Labels: `backend`
(driver names, ≤ 15) and `sni` (env list) — **no tenant or bucket labels**. Every series referenced by
`deploy/monitoring/vaultaire-backends.yml` and `vaultaire-tls.yml` exists with the same name and labels:
`vaultaire_backend_health{backend}`, `vaultaire_backend_probe_failures_total{backend}`,
`vaultaire_backend_circuit_open{backend}`, `vaultaire_backend_write_failures_total`,
`vaultaire_errors_total`, `vaultaire_requests_total`, `vaultaire_tls_cert_expiry_timestamp_seconds{sni}`,
`vaultaire_tls_cert_probe_ok{sni}` (`prom_metrics.go:31-38,80-94`, `cert_expiry.go:221-226`).
`vaultaire_backend_probe_latency_seconds{backend}` is exported but unused by rules. Exposure: `/metrics`
**is public through Cloudflare/HAProxy** (R1-04); the app's `:8000` and HAProxy stats `:8404` are not
(UFW allows 22/80/443 only — verified); Prometheus binds `127.0.0.1:9090`.

**Q9 Rate limiters.** CDN limiter keyed `cdn:<slug>:<bucket>` (`cdn.go:57`) — per public bucket, not
per IP, so proxy topology is irrelevant. Management/webhook limiters keyed by tenant ID from the JWT
context (`management_ratelimit.go:45`), same property. Only the dashboard login/reset/abuse limiters are
per-IP, and their key was spoofable (R1-01, fixed). Growth/cleanup: R1-14.

**Other confirmed:** `not_found.go` mirrors the engine taxonomy as documented (R2/R6 own the
`isBackendFailure` casing bug); `cors.go` matches origins case-insensitively and emits `Vary: Origin`
for non-wildcard matches; `cert_expiry.go` keeps the last good `NotAfter` across probe failures (tested);
`countingResponseWriter` implements `Flush`/`Unwrap` so streaming and `http.ResponseController` work
through the logging wrapper.

## Config surface (Q4 detail)

Read in `cmd/vaultaire` + `internal` (non-test), grouped:

- **In CLAUDE.md table and read:** `PORT`, `DB_*`, `DATA_PATH`, `STORAGE_MODE`, `S3_ACCESS_KEY/SECRET_KEY`,
  `LYVE_*`, `LYVE_PROBE_*`, `QUOTALESS_*`, `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_METER_*`,
  `GOOGLE_*`, `GITHUB_*`, `VAULTAIRE_BASE_URL`, `JWT_SECRET`, `SIGNUPS_ENABLED`, `VERIFY_SECRET`, `GEYSER_*`,
  `IDRIVE_*` (+ per-region), `R2_*`, `ENCRYPTION_MASTER_KEY`, `MULTIPART_*`, `CHUNK_*`,
  `TLS_CERT_PROBE_TARGETS`, `SECURITY_POLICY_URL`, `SMART_DEMOTION_*`.
- **Read but undocumented / documented but unread / default mismatches:** see R1-09.
- **Prod `.env` (names only, 2026-09-26):** `DATA_PATH DB_* ENV GEYSER_{ACCESS_KEY,BUCKET,SECRET_KEY}
  IDRIVE_{ACCESS_KEY,ENDPOINT,REGION,SECRET_KEY} JWT_SECRET LYVE_{ACCESS_KEY,SECRET_KEY,REGION}
  LYVE_PROBE_{ACCESS_KEY,SECRET_KEY} PORT R2_* SIGNUPS_ENABLED STORAGE_MODE TENANT_{1,2,3}_* TLS_CERT_PROBE_TARGETS
  VAULTAIRE_BASE_URL VAULTAIRE_ENDPOINT` — no Stripe, no email provider, no master key (all known).

## Dead code noted

R0 handoff row **confirmed**: `SetAuthService` (`server.go:988`), `SetAuditLogger` (`:992`),
`WrapWithRBACPermission` (`:996`), `GetRouter` (`:1212`) have no callers. Not deleted here (R0's rule:
function-level pruning is R-session work; they are trivially removable in WP-R1-4).

New: `engine.CoreEngine.HealthCheck` (`engine.go:579`, no callers); `engine.StartTiering` (`:813`, no
callers — so `TieringEngine.Start` never runs; R6 decides); `config.Config` fields other than
`Server.Port` (`config.go:8-49`); `configs/*.yaml` (never loaded); `.env.example` (five unread vars);
the three flusher `ctx.Done` branches (R1-06); `LoginRateLimiter.Cleanup` (R1-14); `BackendHealthChecker.checkInterval`
(`health_handlers.go:21,42`, never read); prod env `ENV`.

## Follow-up work packages

| WP | Title | Files | Size | Depends on |
|---|---|---|---|---|
| WP-R1-1 | **Ops hardening on SLC ([YOU])**: HAProxy `http-request deny if { path_beg /metrics }` (+ consider `/health/backends`); `http-request del-header CF-Connecting-IP unless { src -f /etc/haproxy/cloudflare.lst }` (list = the ranges `deploy/ufw-cloudflare-lockdown.sh` fetches); `TimeoutStopSec=45` + drop Redis `Wants/After` in `vaultaire.service`; fix `IMPLEMENTATION_PLAN.md:1229-1232` | box config, `docs/IMPLEMENTATION_PLAN.md` | XS | R1-01/04/07 |
| WP-R1-2 | Server-lifetime context + recover middleware on the S3/API chain + HEAD nil guard (R1-05, R1-06) | `server.go`, new `recover.go`, `bandwidth.go`, `cdn_analytics.go`, `access_log.go`, `s3.go:648` | S | R13 for job internals |
| WP-R1-3 | Build identity via `-ldflags -X` in `/version`, `/health`, banner, deploy gate (R1-10) | `deploy.yml`, `server.go`, `health_handlers.go`, `main.go` | XS | — |
| WP-R1-4 | Config surface cleanup: delete `configs/`, prune `config.Config`, unify `DATA_PATH`/`QUOTALESS_ENDPOINT` defaults, pass `storageMode`/`dataPath` from `main` into `NewServer`, rewrite `.env.example`, delete the four dead `Server` methods; R14 rewrites the CLAUDE.md env table (R1-09, R1-17, dead code) | `config/config.go`, `configs/`, `.env.example`, `server.go`, `main.go`, `management_routes.go`, `CLAUDE.md`, `flags/CLAUDE.md` | S | R14 for prose |
| WP-R1-5 | CDN host router through the middleware chain (R1-11) | `server.go:433-437` | XS | — |
| WP-R1-6 | Probe every `idrive-<region>` driver + decide permafrost (R1-12) | `backend_probes.go`, rules | S | R7 |
| WP-R1-7 | Test-DB hygiene: `DATABASE_URL` skips → `testutil.DSN()` (R1-13) | 8 `_test.go` files | XS | R15 |
| WP-R1-8 | Limiter cleanup + honest `X-RateLimit-Reset`; single request-id through ctx (R1-14, R1-18) | `dashboard/middleware/ratelimit.go`, `dashboard/router.go`, `management_ratelimit.go`, `server.go` | XS | R12 |
| WP-R1-9 | `/webhook/` body cap → 10 MB (R1-15) | `server.go:1113` | XS | R10 |
| WP-R1-10 | Boot deadline for `NewServer`'s DB prologue (R1-16) | `server.go:170-210` | XS | — |
| WP-R1-11 | `LogSender` redaction (R1-08) | `internal/email/email.go` | XS | R14 |

## Areas with no test (of the nine)

1. Startup order / nil-DB — none exercised S3 traffic on a nil-DB server (only `openapi_drift_test.go`
   walks routes). 2. Goroutine lifecycle — none. 3. Shutdown — `shutdown_flush_test.go` (DB-backed,
   skipped locally by default, R1-13); **this PR adds `cmd/vaultaire/main_test.go`** for the signal path
   (order + `ErrServerClosed` handling). 4. Config — only `TestSignupsDefaultFromEnv`; no test for
   `main.go` env parsing or defaults. 5. Middleware — limits (`security_hardening_test.go`) and 5xx
   counting (`prom_metrics_test.go`) only; no chain-order test, no "logs never contain the query
   string/Authorization" test; **this PR adds `internal/clientip` tests** for proxy trust. 6. Health —
   covered. 7. Flags — covered (DB-backed part skipped locally, R1-13). 8. Prometheus — collectors
   covered; no test that every series a rule file references exists (cheap to add: parse
   `deploy/monitoring/*.yml`). 9. Rate limiters — unit-covered; no proxy-behaviour test before this PR.

## Post-merge review (2026-09-26, same day) — correction to the R1-01 fix

| ID | Sev | file:line | What | Why it matters | Proposed fix |
|---|---|---|---|---|---|
| R1-20 | **P0** | `internal/clientip/clientip.go:82` (as merged in #475) | `trustedPeer` read `r.Header.Get("X-Forwarded-For")` and took the last comma-separated entry. HAProxy `option forwardfor` does **not** merge into an existing header: it adds a *new* `X-Forwarded-For` occurrence at the end of the header list (HAProxy 2.8 manual, `option forwardfor`: "this header is always appended at the end of the existing header list, the server must be configured to always use the last occurrence of this header only"; prod is 2.8.16 with plain `option forwardfor`, no `if-none`). Go's `Header.Get` returns the **first** occurrence — i.e. whatever the client sent. Proof: a request with two XFF lines (`9.9.9.9` client, `198.51.100.4` HAProxy) made `FromRequest` return `9.9.9.9`. | The R1-01 bypass (API-key IP allowlist, login/reset/abuse limiters, access log) was still open for any client that sends its own `X-Forwarded-For` header line. The #475 tests only exercised a single comma-joined value. | **Fixed:** `Header.Values` → last occurrence → last entry; `CF-Connecting-IP` honoured only when it occurs exactly once (two occurrences = ambiguous → peer). Tests: `TestFromRequest_ClientSuppliedForwardedForLineIsIgnored`, `TestFromRequest_AmbiguousCloudflareHeaderFallsBackToPeer`. |

Also noted (no change): the Security workflow is red on `main` for the same pre-existing gosec
findings in `cmd/backend-matrix` and `cmd/geyser-*` probe tools (G703/G124/G117) — R15 / WP-R0-11.
WP-R1-1 for [YOU] stands; the HAProxy `del-header CF-Connecting-IP unless { src -f cloudflare.lst }`
line is now the belt to this brace.
