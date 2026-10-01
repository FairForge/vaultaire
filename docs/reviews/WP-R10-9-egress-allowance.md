# WP-R10-9 — egress allowance enforcement, built

**Built 2026-10-01 on `wp/R10-9-egress-allowance` (PR #539), from `main` @ #536 (rebased on #537).**
**Decision this was built under:** D-25, the throttle shape — applied by default, every number a
knob, **needs Isaac's confirmation** (see "Decisions taken"). **Findings closed:** R10-22
(the allowance is copy, not code), R13-16 (two definitions of "used"), the R14 legal-claims rows
22, 23 and 32, SYNTHESIS table A row WP-R10-9. **No migration** (next is still 074).

## Before-state, reproduced on `main` (#536) against `vaultaire_test`

Local build, `STORAGE_MODE=local`, `DB_NAME=vaultaire_test`, port 8099, a freshly registered
free-tier tenant (5 GiB quota, so a sold allowance of 2.5 GiB), one 8 MiB object, aws-cli 2.28 and
curl. Transcript (script: register → `s3 mb` → `put-object` → the steps below):

```
tier|storage_limit|bandwidth_limit: free|5368709120|NULL
## 1. sold allowance (0.5 x 5 GiB = 2.5 GiB) enforced nowhere: 10 GiB egress recorded this month
GET 8 MiB (presigned): 200 8388608 bytes in 0.027216s
## 2. the only limit: admin-set bandwidth_limit_bytes = 1 GiB -> refusal on GET and PUT; List untouched
An error occurred (SlowDown) when calling the GetObject operation: Monthly bandwidth limit exceeded. Upgrade your plan or wait for the next billing cycle.
HTTP/1.1 429 Too Many Requests
<Code>SlowDown</Code>
An error occurred (SlowDown) when calling the PutObject operation: Monthly bandwidth limit exceeded. Upgrade your plan or wait for the next billing cycle.
obj.bin                                   <- list-objects-v2 still answers
## 3. ingress counts toward the limit: egress reset to 0, limit 20 MiB, three 8 MiB uploads
month ingress|egress: 25165824|487
An error occurred (SlowDown) when calling the GetObject operation: Monthly bandwidth limit exceeded. ...
## 4. /cdn is not covered: bucket public-read, limit 20 MiB, month ingress+egress 35229496
GET S3 (presigned): 429
GET /cdn: 200 8388608 bytes in 0.007413s
## 5. bytes are recorded when the request ends: a 4 KiB/s reader holds a GET open
egress recorded while the response is open: 0
## 6. month boundary: session time zone vs UTC
SHOW timezone: America/Denver  date_trunc('month', CURRENT_DATE): 2026-10-01 00:00:00-06
```

So, on `main`: the sold allowance does nothing (1); an admin limit refuses downloads **and
uploads** (2) and uploads alone can trip it (3); the public CDN path ignores it (4); a response
in flight counts for nothing until it ends (5); and the month boundary follows the session time
zone (6). One correction to the review rows: the refusal's wire status was **429**, not 503
(`s3_errors.go` mapped `SlowDown` to `StatusTooManyRequests`).

## What was built

### A. One definition of the allowance — `internal/usage/egress.go`

| Piece | Where |
|---|---|
| `QuotaManager.EgressAllowance` — one query, **derived at read time**. Precedence: admin override (`bandwidth_limit_bytes > 0`) → the house (ratio × each floor limit) → no house (Standard ratio × `storage_limit_bytes`; free tier = 2.5 GiB). Returns `Bytes`, `PlanBytes`, `OverrideBytes`, `Source` | `internal/usage/egress.go:66` |
| Ratios from `prices.json` through `landing.Get()` | `:51`, `:59` |
| UTC calendar month: `EgressMonthStart`, `EgressResetAt`; `MonthEgressBytes` sums `egress_bytes` only | `:100`, `:106`, `:113` |
| The D-25 knobs and the rate: `max(Min, Factor × allowance / 2,592,000 s)` | `:126-150` |
| `EgressStatus`, `EgressStatusReader`, `EgressStatusFromDB`, `EgressOver` — what every page shows | `:154-199` |

`SetHouse` and `ClearHouse` are untouched and never write `bandwidth_limit_bytes`: nothing is
seeded, nothing can drift, and the override survives a webhook replay and a cancelled
subscription (the hard requirement; `TestEgressAllowance_OverrideSurvivesSetHouseReplayAndClearHouse`).

### B. One counter — `internal/api/egress_meter.go`

| Piece | Where |
|---|---|
| `egressMeter`: bounded map (20,000 entries) of per-tenant month egress | `internal/api/egress_meter.go:77` |
| `tenant()` — get-or-create; the first request loads the month from `bandwidth_usage_daily` once, a racing second request waits on that load | `:207`, `:256` |
| Live count: `egressSpan.count` on the `countingResponseWriter` of every authenticated S3 response and every `/cdn` GET — bytes count as they are written | `:504`, `internal/api/bandwidth.go:24-51`, `internal/api/s3.go:501` |
| UTC month rollover without a reload (the process saw every byte of the new month) | `:280`, `:287` |
| Allowance cached 30 s per tenant; one goroutine refreshes, the rest keep the old value | `:328`, `:372` |
| Eviction: only entries idle ≥ 2 min with no open response (nothing unflushed can be lost); a full map of busy tenants serves a newcomer uncached | `:236` |
| A failed load = not throttled (fail open), retried by the next request | `:256-277` |
| `EgressStatus` for the alerter, the dashboard and the API — never grows the map | `:435` |
| Shutdown: `flushTrackers` → `drainUnrecorded` hands the bytes of responses still open to the tracker before its last flush | `internal/api/server.go:1334`, `egress_meter.go:316` |

The buffered DB flush is as it was (5 s ticker, 100-event auto-flush, the H-3 shutdown flush);
two things changed in it: every event carries its **UTC day** (`bandwidth.go:118`; the SQL used
`CURRENT_DATE`, the session time zone), and `recordResponse` (`bandwidth.go:163`) splits a
response that was open across the UTC month boundary so the bytes written before midnight stay
in the old month.

### C. The throttle — `internal/api/egress_writer.go`

| Piece | Where |
|---|---|
| Installed at dispatch, before the handler: `Server.serveGetObject` — the only S3 path that sends object bytes (plain, ranged, chunked, SSE; header-signed and presigned) | `internal/api/s3.go:645` |
| … and in `handleCDNRequest`, after the 304/412 evaluation and before any header is set | `internal/api/cdn.go:104-128` |
| Only a 200/206 body is paced; errors, 304, 416 pass through | `egress_writer.go:40`, `:53` |
| A `Write` is cut into slices ≤ the bucket's burst (16 KiB; fewer bytes when the knobs would leave a stream silent > 20 s) and waits on `rate.Limiter.WaitN` with the request context | `egress_writer.go:60-93`, `egress_meter.go:359` |
| Re-evaluated every 1 MiB, and every 5 s while paced: crossing mid-stream slows down, a raised allowance speeds up | `egress_writer.go:66`, `:102` |
| `Flush` and `Unwrap` kept; paced slices are flushed | `:89`, `:155-162` |
| Stream guard: `admit` refuses when the tenant is paced and already has `MaxStreams` paced responses **on that surface** — 503 `SlowDown` + `Retry-After: 60` (S3), 429 + `Retry-After: 60` (`/cdn`) | `egress_meter.go:414`, `s3.go:650-656`, `cdn.go:110-117` |
| `ErrSlowDown` is 503 with a message about the allowance (was 429 "Monthly bandwidth limit exceeded") | `internal/api/s3_errors.go:110`, `:157` |
| `IsOverLimit` and its GetObject/PutObject/UploadPart branch deleted | `bandwidth.go`, `s3.go` |
| Flag `egress_throttle`, default OFF, registered like the others; knobs read with the R13-19 convention | `flags_wiring.go:48`, `server.go:188`, `:273`, `egress_meter.go:166` |

### D. Warning ladder — `internal/api/bandwidth_alerts.go`

Tenants with egress recorded this UTC month (one set-based query, `:91`), each read through
`EgressStatus` — the same allowance and the same counter as the throttle (`:136`). 80 % and 95 %
for every tenant with an allowance; at 100 % the notice that states the rate and the reset date
(`:264`). Rows in `bandwidth_alerts` are created when a step is first crossed (no per-tenant
seeding pass); `enabled = false` silences a step. The R13-09 behaviour is kept: per-tenant 10 s
budget, no `last_fired_at` on a failed send; the stamp is the UTC wall clock (`:256`).

### E. Metrics — server registry, no tenant label

`vaultaire_egress_throttled_tenants` (gauge, `prom_metrics.go:48`),
`vaultaire_egress_throttle_engaged_total{surface}`, `vaultaire_egress_would_throttle_total{surface}`,
`vaultaire_egress_throttle_rejected_total{surface}`, `vaultaire_egress_throttled_bytes_total`
(`egress_writer.go:168-190`). The tenant is in the `egress allowance spent` log line (once per
tenant per month) and on the admin tenant page. Rule file `deploy/monitoring/vaultaire-egress.yml`:
one info-level rule, `EgressThrottleActive`.

### F. Dashboard and API

Overview egress bar: `populateEgress` (`internal/dashboard/handlers/egress.go:25`, called from
`overview.go:177`) — "N % of X free this month, then rate-limited to R" and, when the tenant is
being paced, "All X of this month's egress allowance is used: downloads are rate-limited to R
until <date> (UTC)" (`templates/customer/dashboard.html:269`). Admin tenant page: used /
allowance / plan / override / state (`tenants.go:271`, `templates/admin/tenant_detail.html:108`);
the form says "0 = the plan's allowance" (`:141`, handler `tenants.go:463`) and points at the
flag row for exemptions. The live counter reaches the dashboard through `Deps.Egress`
(`dashboard/router.go:51`, `api/server.go:771`). `/api/v1/user/usage`: `egress_allowance`,
`egress_used`, `egress_throttled`, `egress_rate_limit_bytes_per_sec`, `egress_resets_at`
(`api/usage.go:27`, OpenAPI `internal/docs/openapi.go:1602`; the drift guard is green).
`/admin/flags` describes `egress_throttle` (`admin_flags.go:37`).

### G. Copy, from one source

`prices.json` `egress` now carries the three sentences (`internal/api/landing/prices.json:12-14`);
`landing.EgressTokens` (`prices.go:162`) feeds the two Markdown guides (`docs_pages.go:81`),
`build.py:91` the landing page, the `egress` template function (`dashboard/handlers/assets.go:39`)
the terms, and `llms_txt.go:22` reads them directly.

| Page | Before | Now |
|---|---|---|
| `docs_faq.md:39` | "beyond that throughput may be throttled … Vault restores … beyond that are queued" | a "What about egress?" answer: the two ratios, the long sentence, the one-allowance sentence, the 16-parallel-downloads note |
| `docs_faq.md:87` | "is throttled, not charged" | "is rate-limited, not charged" |
| `docs_rclone.md:73` | "beyond that throughput may be throttled" | ratios + the long sentence + a `--transfers` ≤ 16 note |
| `landing.src.html:605`, `:652` | "then may be throttled — never billed" | "then rate-limited — never billed" (`__EGRESS_PAST__`) |
| `landing.src.html:636` (Vault fine print) | "beyond that, restores are queued, never billed" | "then rate-limited — never billed (one allowance and one rate limit for the whole account, no restore queue)" |
| `templates/legal/terms.html:48` | "may be throttled; archive restores beyond their allowance are queued" | ratios, both sentences, "a limited number of concurrent downloads; requests beyond that are answered with a retryable error" |
| `llms_txt.go:22` | "then may be throttled" | ratios, both sentences, the 503 note, the usage fields |
| `prices.json` comment, `handlers/billing.go:279` comment | "may be throttled (Standard) or restores queue (Vault)" | what the code does |

`landing.html` and `templates/generated/house.html` are regenerated (`make landing`). "Never
refused" appears nowhere (`TestEgressCopy_OneSourceOnEveryServedPage` bans it). No changelog
entry. `handlers/help.go`, named in the prompt, does not exist (the R10 row quoting `help.go:52`
predates its removal).

## Tests

Red first: the `internal/usage` tests, the end-to-end tests, the alerter tests, the dashboard,
API, metrics and copy tests were written and run failing before the code they cover (14 of the
end-to-end and alerter tests failed on the unwired tree; the rest did not compile). **Not red
first:** `egress_meter_test.go` and `egress_writer_test.go` were written right after the first
draft of those two files.

- `internal/usage/egress_test.go` — ratios equal `prices.json`; house 6+1 = 4 TB; attic-only;
  no house; free tier 2.5 GiB; override wins; override of 0 = the plan; unknown tenant;
  **override survives SetHouse replay, a resize and ClearHouse**; UTC month from a Denver clock;
  egress only, this month only; the rate and its floor.
- `internal/api/egress_meter_test.go` — one load then no query in 500 requests; bytes counted
  while the response is open; HEAD counts nothing; UTC rollover (mid-response and idle entry);
  a response open across the boundary is split in two events; events land on the UTC day;
  bounded map (fresh entries never evicted, an open response pins its entry, 500 extra tenants
  stay inside the bound); fifty requests racing the first load = one load, no byte lost or
  doubled; allowance change arrives after the TTL (≤ 60 s); load failure fails open and
  retries; `EgressStatus` never grows the map; no quota row = no allowance; the env knobs
  (defaults, valid, three rejected values → three Warn lines).
- `internal/api/egress_writer_test.go` — **one 64 MiB Write** paced at 32 MiB/s (≥ 1.5 s, every
  underlying write ≤ 16 KiB, flushed); under the allowance a Write is not sliced; **context
  cancel** returns in < 200 ms with no goroutine left; Flush and Unwrap pass through; 404 / 403 /
  500 / 416 / 412 bodies are not paced; an allowance raised mid-response speeds it up; the slice
  shrinks so no paced stream is silent > 20 s under any knob setting.
- `internal/api/egress_e2e_test.go` (DB-backed, a real HTTP server, one tenant per test) —
  under the allowance = full speed; **pushed over by real downloads → the next GET takes ≥ 1.4 s
  for 8 MiB at 4 MiB/s** while PUT, List and HEAD stay fast; flag off → nothing slowed and
  `would_throttle_total` moves (S3 and CDN); per-tenant flag row = exemption, removing it paces;
  `/cdn` full and ranged share the bucket and the counter; **crossing mid-download** slows the
  rest; **eight parallel 16 MiB downloads started 64 KiB under the line** get out at most
  64 KiB + 1 MiB per stream + the paced rate; the stream guard (503 `SlowDown` + `Retry-After` on
  S3, 429 + `Retry-After` on `/cdn`, per surface, PUT/List/HEAD unaffected, a finished response
  gives its slot back); **1,000 connections** → 16 served, 984 refused, every slot returned;
  304 / HEAD (hit and miss) / 416 count no body bytes and the database equals the counter to the
  byte; restart mid-month reloads the same number, on the UTC day; an aborted CDN download is
  counted; **a backend reset mid-body does not charge the breaker** (20 resets → `closed`; the
  control, 20 failed Gets, opens it); every object-body path (plain, native range, SSE whole,
  SSE range, chunked whole, chunked range) passes its bytes through the throttle; shutdown
  records the bytes of a response still in flight, once; public-bucket readers spend the
  owner's allowance and the owner is then paced, not refused.
- `internal/api/bandwidth_alerts_test.go` (rewritten, DB-backed) — **alerter and throttle
  agree on "used"** (104 MiB of uploads = no notice and no throttle; 32 MiB of unflushed
  downloads = three notices and `Throttled`); the ladder (79 % nothing and no row, 85 %, 120 %
  with the flag off sends 95 % and holds the 100 % notice, flag on sends it with rate and
  reset date, once per month, re-armed next month); a disabled row stays silent and the
  override is the denominator; a failed send is retried next pass (R13-09); the candidate
  query.
- `internal/api/usage_test.go`, `prom_metrics_test.go`, `egress_rules_test.go`,
  `egress_copy_test.go`, `internal/dashboard/handlers/egress_test.go` — the API fields, the five
  series with no tenant label, the rule file, the copy on every served page, the overview bar in
  three states, the admin tenant page, the terms page.
- `TestShutdown_FlushesBufferedTrackers` (H-3) stays green.

`make test-db` then `go test -race ./...` against `vaultaire_test`: every package ok.
`make lint`: 0 issues. `make gosec`: 0 issues (its non-zero exit on this box comes from the
gitignored `.private` bench files, as on `main`).

## Live proof (the branch binary, `STORAGE_MODE=local`, `DB_NAME=vaultaire_test`)

Booted with `EGRESS_THROTTLE_MIN_BYTES_PER_SEC=2097152 EGRESS_THROTTLE_MAX_STREAMS=2
EGRESS_THROTTLE_FACTOR=banana`. A registered free-tier tenant with 10 GiB of egress already
recorded this month; aws-cli and curl:

```
"msg":"invalid EGRESS_THROTTLE_FACTOR (need 0 < f <= 1000), keeping default","value":"banana"
## 1. flag egress_throttle OFF (default)
header-signed GET 8 MiB: real 0.41
vaultaire_egress_would_throttle_total{surface="s3"} 1
usage API: {'egress_allowance': 2684354560, 'egress_used': 10745806848, 'egress_throttled': False, 'egress_rate_limit_bytes_per_sec': 2097152, 'egress_resets_at': '2026-11-01T00:00:00Z'}
## 2. the flag turned on for THIS tenant (a feature_flags tenant row)
header-signed GET, 8 MiB at 2 MiB/s (0.5 MiB burst): real 4.27   bytes identical
presigned GET:        200 8388608 bytes in 3.753041s
/cdn GET (anonymous): 200 8388608 bytes in 3.982220s
ranged GET of 2 MiB:  206 2097152 bytes in 0.754418s
put-object 8 MiB: real 0.45   list-objects-v2: real 0.40   head-object: real 0.39   missing key: 404 in 0.003877s
usage API: {... 'egress_throttled': True, 'egress_rate_limit_bytes_per_sec': 2097152, ...}
## 3. the stream guard (2 per surface): two paced S3 downloads open, a third arrives
An error occurred (SlowDown) when calling the GetObject operation: Please reduce your request rate: this account has used its monthly egress allowance and already has the maximum number of rate-limited downloads in progress. Retry shortly.
HTTP/1.1 503 Service Unavailable
Retry-After: 60
-- two CDN downloads open, a third arrives
HTTP/1.1 429 Too Many Requests
Retry-After: 60
-- a PUT while both guards are full: "b7db7493d5c124b1a9798013b981a9c1"
vaultaire_egress_throttle_engaged_total{surface="cdn"} 3
vaultaire_egress_throttle_engaged_total{surface="s3"} 5
vaultaire_egress_throttle_rejected_total{surface="cdn"} 1
vaultaire_egress_throttle_rejected_total{surface="s3"} 2
vaultaire_egress_throttled_bytes_total 3.3800192e+07
vaultaire_egress_throttled_tenants 1
## 4. an admin raises the override to 100 GiB; 32 s later
GET: 200 8388608 bytes in 0.009739s
usage API: {'egress_allowance': 107374182400, ..., 'egress_throttled': False, ...}
vaultaire_egress_throttled_tenants 0
## 5. the log line, and the rows after SIGTERM
"msg":"egress allowance spent","tenant_id":"<tenant>","used_bytes":10737418240,"allowance_bytes":2684354560,"throttled":false,"rate_bytes_per_sec":2097152
rows (date | egress): 2026-10-01 | 10787997324   UTC today: 2026-10-01   session zone: America/Denver
```

The last line: after a graceful stop the database holds 10,787,997,324 bytes — the number the
usage API last reported from the live counter — on the UTC date, in a Denver-zoned session.

## Adversarial pass — what it found in my own code

Each item was proved with a test or the live run above; the ones marked **fixed** were defects
in the first draft of this branch.

1. **Bytes of a response still in flight at shutdown never reached the database — fixed.**
   A handler records when it ends; a deploy flushed the buffer and exited, so the restarted
   process reloaded a counter short by every open download. `flushTrackers` now drains the
   meter's unrecorded bytes first, and `recordResponse` records only what is still unrecorded,
   so a handler that finishes late does not count twice
   (`TestEgress_ShutdownRecordsBytesOfResponsesStillInFlight`). A crash (no shutdown) still
   loses the bytes in flight and up to 5 s of buffered events — an undercount, in the
   customer's favour.
2. **Anonymous readers could lock the owner out — fixed.** With one stream guard per tenant,
   sixteen strangers downloading from a public bucket of a throttled tenant took every slot and
   the owner's own S3 GETs got 503. The guard is now per surface (16 for S3, 16 for `/cdn`); the
   token bucket is still one (`TestEgress_StreamGuard_503OnS3_429OnCDN_PerSurface`). This is a
   deviation from the letter of D-25 ("for one tenant") — see decisions.
3. **A panic in the GET handler would leak a slot for good — fixed.** `gw.close()` ran after
   the handler; it is deferred now (`s3.go:658`).
4. **A raised allowance did not reach a paced download for up to 16 s on top of the TTL —
   fixed.** At the floor rate a megabyte (the re-evaluation interval) takes 16 s; a paced
   response now also re-reads every 5 s (`TestEgressWriter_AllowanceRaisedMidResponseSpeedsUp`).
5. **Knob settings could starve streams past the proxies' timeouts — fixed.** 16 KiB slices
   at a lower floor or with more streams leave a connection silent for longer than HAProxy's
   50 s. The slice now shrinks (to 1 KiB at least) so that with both surfaces full every stream
   sends within 20 s, and boot warns when even that cannot hold
   (`TestEgressMeter_SliceKeepsEveryPacedStreamSending`).
6. **An aborted CDN download cost the tenant nothing — fixed (it was so on `main` too).** The
   CDN recorded only when the copy finished; it now records what was written, however the
   handler returns (`TestEgress_AnAbortedCDNDownloadIsStillCounted`).
7. **HEAD "bytes".** net/http accepts and discards a body written to a HEAD response; the
   counting writer counted an error document that was never sent. HEAD counts nothing now.
8. **1,000 connections** — 16 paced, 984 refused at the cost of one map lookup each, one map
   entry, every slot returned after the clients leave (`TestEgress_AThousandConnections`).
9. **The public bucket** — readers spend the owner's allowance; that is the product (a public
   bucket's egress is the owner's), the cost to the owner is speed, never money or access
   (`TestEgress_PublicBucketReadersSpendTheOwnersAllowance`). The owner's levers are the
   bucket's CDN budget and its visibility. **Not solved:** sixteen strangers can hold every
   `/cdn` slot of a tenant that is already past its allowance, and other visitors get 429.
10. **A slow reader holding a backend connection** — the breaker charges a failed `Get`, not a
    failed `Read`: twenty mid-body resets leave it `closed`, twenty failed Gets open it
    (`TestEgress_BackendResetMidBodyDoesNotChargeTheBreaker`). A chunked GET holds no backend
    connection while it waits (chunks are fetched whole, ≤ 4 × 16 MiB per stream in memory).
11. **Every entry point** — grep for every `engine.Get` / `GetRange` / `io.Copy` to a response:
    `HandleGet` (plain, native range, fallback range, SSE, chunked) and `handleCDNRequest`
    (full, range) are the only two that send object bytes to a client; `s3_copy.go` reads a
    source server-side. Both are wrapped at dispatch; the six S3 body paths are each proved to
    pass their bytes through the paced writer.
12. **The invariant against the path that returns first** — the wrapper is installed in
    `serveGetObject` / at the top of the CDN body section, before any handler code; nothing
    inside a handler can write around its own `ResponseWriter`. An unloaded tenant (the
    database was down at its first request) is not throttled — fail open, as `IsOverLimit` was.
13. **Restart, racing first load, rollover during a long download, 304/HEAD/416** — tests
    named above.
14. **Seen on the way, not changed:** the CDN sets `Content-Length` before `engine.Get` and
    then answers a backend failure with `http.Error` under that length; the native ranged GET
    opens two backend readers and does not attribute its backend
    (`GetRange` never calls `SetBackendUsed`); `/usage/alerts` divides by a zero limit. And the
    CDN bucket budget (`cdn_stats_daily`, `CheckBudget`) still uses `date_trunc('month',
    CURRENT_DATE)` — a different ledger, UTC on prod.

## Prod (read-only, 2026-10-01 23:26 UTC)

`SHOW timezone` = **UTC**, so `date_trunc('month', CURRENT_DATE)` and the UTC month already
agreed on the box; the code no longer depends on it. Five tenants, no `feature_flags` rows, no
`bandwidth_alerts` rows, no override, no floor rows (nobody has a house yet).

| Tenant (by role) | Plan row | Would-be allowance | Egress Oct (1 day) | Egress Sep | Egress Aug | Throttled the day the flag flips? | Recommended |
|---|---|---|---|---|---|---|---|
| Demo (durability / show stack; one public bucket) | starter, 50 GiB | 25 GiB | 50 KB | 134 MiB | 1.8 GiB | no | **exemption row** — a public demo is the one tenant strangers can push over |
| Bench (1 TiB) | bench | 512 GiB | 0 | 91 MiB | 239 MiB | no | **exemption row** — a benchmark that is paced measures the throttle |
| Bench-test (100 GiB) | standard | 50 GiB | 0 | 0 | 0 | no | exemption row (same reason) |
| Bench 2 (1 TiB) | vault100 | 512 GiB | 0 | 0 | 0 | no | exemption row (same reason) |
| Tenant zero (owner) | free, 5 GiB | 2.5 GiB | 0 | 0 | 0 | no | **none** — customer zero should meet the product as sold; the allowance follows the house when one is bought |
| Synthetic check | not created yet (checklist row 4) | 2.5 GiB (free) | — | — | — | no: ≈ 86 MiB a month of 4 KiB GETs | **exemption row when it is created** — a paced canary pages |

Nobody would be throttled today or would have been in August or September. Nothing was changed
on the box.

## Decisions taken

- **D-25 applied as written by default, with one deviation and one thing to look at.**
  Shape: slowed, not refused; one token bucket per tenant for GetObject and `/cdn`;
  rate = max(65536 B/s, 1.0 × allowance / 2,592,000 s); refusal only past 16 concurrent paced
  responses; uploads, listings, HEAD and errors untouched; flag default OFF with a
  would-throttle counter; Vault has no restore queue.
  *Deviation:* the 16-stream guard is **per surface** (S3 and `/cdn` each), not per tenant —
  adversarial finding 2. Worst case 32 paced streams, each still sending every 8 s at the floor.
  *To look at:* **the floor is generous to small plans.** 65536 B/s for 30 days is ≈ 158 GiB:
  a free-tier tenant (2.5 GiB allowance) can download 63 times its allowance at the floor, and
  iDrive's free pool is 3× the bytes stored there. The pace rule only takes over above ≈ 317 GiB
  of quota (0.5 × 317 GiB / 30 d = 64 KiB/s). If that is not intended, lower
  `EGRESS_THROTTLE_MIN_BYTES_PER_SEC` (the slice shrinks with it) or decide a free-tier floor.
- **Derive, do not seed.** No column, no migration; `bandwidth_limit_bytes` stays the admin
  override and nothing else writes it.
- **Admin "0" means the plan's allowance.** Exemption is the flag row. Prod has no override rows,
  so no tenant changes meaning.
- **`SlowDown` is 503** (AWS's status; SDKs retry it with backoff). It was 429.
- **The 100 % notice is sent only where the tenant is actually paced.** With the flag off it
  would say something false; it stays unfired and goes out when the flag is on.
- **Counted as egress:** every response byte of an authenticated S3 request (as before: listings
  and error documents included, a few hundred bytes each) and every `/cdn` GET body. **Paced:**
  only 200/206 object bodies.
- **Month = UTC calendar month** in the counter, the rows (dated on the UTC day), the alerter
  and every dashboard month reader of the bandwidth tables.
- **The allowance TTL is 30 s**, the first load is synchronous in the tenant's first request.
- **Copy says "rate-limited"**, states one allowance per account, and tells clients about the
  16-download limit instead of promising that nothing is refused.

## What is deliberately not done

- **Cloudflare edge bytes.** A `/cdn` object served from Cloudflare's cache never reaches the
  origin: it is neither counted nor paced. Only origin fetches are. Logpush / GraphQL analytics
  is the route (R13/R14 noted it).
- **Overage billing.** Nothing is billed past the allowance; `STRIPE_METER_EGRESS` stays dormant.
- **A separate Vault restore allowance or queue.** One number, one rate cap.
- **Per-client fairness inside the paced set.** The bucket is first-come; one client of a
  tenant can take most of the tenant's paced rate.
- **Multi-process.** The counter is per process; prod runs one. A second API node would need a
  shared counter (or each node enforces its own view, loaded from the same rows).
- **The Performance tier's "1× your quota" line on the landing page** (`landing.src.html:622`)
  is left: the tier is not for sale and has no floor in the code.
- **The dashboard's `/usage/alerts` and the CDN per-bucket budget** are untouched.

## What I could not prove

- **What the real backends do with a slow reader over hours.** At 64 KiB/s a 10 GiB object
  takes 43 h on one backend connection; whether iDrive, Lyve or R2 reset such a stream was not
  tested (prod is read-only for workers, and it needs hours). A reset ends the response short
  and the client retries or resumes by range; the breaker is not charged (proved).
- **HAProxy and Cloudflare with paced responses.** The slice cadence is computed against their
  documented silence timeouts (50 s, 100 s); it was not run through the real proxies.
- **A crash mid-month** was proved as a restart from flushed rows; a `kill -9` with responses in
  flight undercounts by those responses (stated above, not measured).
- `promtool check rules` is not installed here; the rule file is parsed by a Go test instead.

## [YOU]

1. Confirm D-25 (and the floor, above).
2. On `/admin/flags`, add an `egress_throttle` row with the flag **off** for the demo tenant,
   the three bench tenants and — when it exists — the synthetic-check tenant.
3. Install `deploy/monitoring/vaultaire-egress.yml` with checklist row 10
   (`scp … vaultaire-slc:/tmp/`, `sudo mv` into `/etc/prometheus/rules/`, `promtool check rules`,
   reload).
4. Watch `vaultaire_egress_would_throttle_total` and the `egress allowance spent` log line for
   a few days, then flip `egress_throttle` globally **before signups open**.
5. E-mail is `LogSender` on prod: the 80 / 95 / 100 % notices are logged, not delivered, until
   `EMAIL_PROVIDER` is set.
