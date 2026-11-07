# OneDrive / Permafrost Fleet

## Overview

OneDrive fleet storage via Microsoft Graph API. NOT a performance or cost tier —
it's a **durability layer** for async background replication from iDrive/Quotaless/Geyser.
Customers never read from OneDrive directly. Value proposition: "15+ independent
copies across Microsoft's infrastructure" at $0 cost.

## Pricing and capacity (corrected 2026-11-05)

- Plan: Microsoft 365 Business Basic seats, **300 users maximum per Business tenant** (Microsoft's
  plan-options page). Owner 2026-11-05: 300 seats per tenant; Graph shows ONE licensed, provisioned
  user per tenant today, each drive **5.00 TiB** (the 5 TB per-user quota is an admin setting that needs
  5+ licensed users; 25 TB per user is a support request for "eligible enterprise and government"
  customers — not Business plans). Capacity = licensed AND provisioned users × 5 TB; the three live
  drives hold 15 TiB together, ~34 GiB used.
- Cost: list ≈ $6 per seat-month (annual), i.e. ≈ $1.20 per TB-month at 5 TB a seat. The current seats
  are promotional (owner: free for 3 years) — put the renewal date on the calendar: an **unlicensed**
  OneDrive goes read-only after 60 days, is archived after 93 and billed $0.05/GB-month in Microsoft 365
  Archive ($0.60/GB to reactivate); from 2026-07-01 a 365-day non-payment clock ends in deletion
  (Microsoft Learn "Manage unlicensed OneDrive user accounts", MC836942).
- No egress fees, no API call charges — the API is metered in resource units instead (below).
- **Terms (what the fleet may hold):** the OneDrive service description says per-user storage "is
  designed to serve the needs of individuals"; "storage of data other than an individual's personal work
  files, including system back-ups and departmental and organizational level data, is not supported,
  nor is the assignment of a per user license to a bot, department, or other non-human entity"; the
  Product Terms forbid "commercial hosting services to third parties". So the fleet holds the licence
  holders' OWN data (and its parity) as of right; customer data needs Microsoft's written consent (the
  Sync precedent) — never 299 synthetic users per tenant. Research: `research_notes/ONEDRIVE-FLEET-LIMITS-2026-11-05.md`.

## Tier Architecture

| Tier | Backend | Cost | Role |
|------|---------|------|------|
| Performance | iDrive E2 | $4.125/TB | Default hot, <50TB |
| Bulk/Egress | Quotaless | ~€0.60/TB | >50TB, free egress |
| Archive | Geyser | $1.55/TB | Cold tape, compliance |
| Deep Archive | Vault18 | $1/TB | 7+ year retention |
| **Durability** | **OneDrive fleet** | **$0** | **Background replication, 15 copies** |
| CDN | R2 / Pixeldrain | $15 / €4 | Public content (on-demand) |

## Azure App Registration

- Client ID: see `.env.bench` (`TENANT_N_CLIENT_ID` — one app registration per fleet account; the old single-tenant `AZURE_*` vars are dead, read only by the retired gitignored tools)
- Auth: Client credentials (service principal), not user-delegated
- Permissions: `Files.ReadWrite.All`, `User.Read.All`, `Sites.Read.All`
- Scopes: `https://graph.microsoft.com/.default`

## Tenant Fleet

| Tenant | Domain | UPN | Status |
|--------|--------|-----|--------|
| tenant-1 | (see .env.bench) | (see .env.bench) | Active |
| tenant-2 | (see .env.bench) | (see .env.bench) | **Check: client secret may be expired** |
| tenant-3 | (see .env.bench) | (see .env.bench) | Active |
| tenant-4..15 | (planned) | — | Not yet provisioned |

Credentials: `TENANT_N_ID`, `TENANT_N_CLIENT_ID`, `TENANT_N_SECRET`, `TENANT_N_USER`
env vars in `.env.bench` (gitignored).

## Microsoft Graph API

### Endpoints

| Operation | Method | Path | RU Cost |
|-----------|--------|------|---------|
| List drives | GET | `/users/{upn}/drives` | 1 |
| Create folder | POST | `/drives/{id}/items/root/children` | 2 |
| Upload (<250MB) | PUT | `/drives/{id}/items/{parentId}:/{name}:/content` | 2 |
| Upload (>4MB) | POST+PUT | `.../createUploadSession` + chunked PUT | 2 |
| Download | GET | `/drives/{id}/items/{id}/content` | 1 |
| Download URL | GET | item metadata → `@microsoft.graph.downloadUrl` | 1 |
| Delete | DELETE | `/drives/{id}/items/{id}` | 2 |
| List children | GET | `/drives/{id}/items/{id}/children` | 2 |
| Delta query (with token) | GET | `/drives/{id}/root/delta` | **1** |
| Delta query (no token) | GET | `/drives/{id}/root/delta` | 2 |
| Batch (up to 20) | POST | `/$batch` | per-request |

### Upload Strategy

**What the driver does (WP-FLEET-4, 2026-11-06; `onedrive_upload.go`):**

| Body | Method | Why |
|------|--------|-----|
| ≤ 4 MiB, any | Simple PUT (read into memory) | one request |
| ≤ 200 MiB and **re-readable** (`io.ReaderAt`: the `materialize` spool, a `bytes.Reader`) | Simple PUT, the burst attempts, each attempt reading its own section | one request instead of a session; Microsoft allows 250 MB (the 2026-11-05 checks: the simple PUT works on Business) |
| anything **streamed** > 4 MiB (the parity shards' pipes, a client body) | Upload session | a streamed simple PUT is ONE request that cannot be re-sent: a 429 burst would lose the shard (WP-FLEET-3). The session rides it fragment by fragment |
| > 200 MiB | Upload session | |

**Sessions** (D-F4-3): fragments of **180 × 320 KiB = 58,982,400 B** (`odChunkSize`; Microsoft asks for < 60 MiB in multiples of 320 KiB — the old 60 MiB sat on the bound), the largest being the fastest (~25 MB/s per session against ~13 at 20 MiB); **double-buffered** — the next fragment is read from the source while one is in flight (two pooled buffers per upload; a pipelined session from a client body wrote 24.6 MB/s against 7.6 read-then-send). No `Authorization` header on fragment PUTs (the URL is pre-authenticated). A fragment that exhausts its attempts resumes from `nextExpectedRanges`; a session gone (404) restarts a seekable body once (Throttle Handling below).

**Every write is verified** (D-F4-4): the body is hashed with `quickXorHash` as it streams (`quickxor.go`) and compared with the item Graph answers — the simple PUT's, or the final fragment's (a final 202 = the item is read back by path). A different hash (or a different size with a hash) deletes the item for good (`permanentDelete`) and fails the Put with `engine.ErrObjectChanged`, counted in `vaultaire_permafrost_hash_mismatch_total{account}`. An answer naming no hash is accepted (Business always sends one).

**Reads** (D-F4-1): a Get of ≥ 10 MiB reads ranges of 8 MiB below 64 MiB, **32 MiB** to 1 GiB, **64 MiB** from 1 GiB — a 256 MB object is ≤ 8 requests of the per-user budget instead of 32. A Get holds at most 256 MiB of ranges (8 × 32, 4 × 64), and every fleet Get past its first range shares one 1 GiB budget, so under load a Get reads fewer ranges at once, never more memory. **A Get under 10 MiB is the same reader with one stream** (RB3-2, 2026-11-07): a part that is the whole object is fetched with a plain GET (no Range header), 200 with exactly the object's bytes — a body that ends early is asked again, one byte more is another object and fails — with the burst attempts, the hedge and the budget a range gets; a 0-byte object is an empty body with no CDN call. Until then a small Get was ONE plain GET: a 429 failed it and a cut body streamed through.

### Throttling Limits (Microsoft Learn, "Avoid getting throttled or blocked in SharePoint Online", read 2026-11-05)

Resource-unit (RU) costs per Graph request: 1 = a single-item query, delta with a token, a download-URL
fetch; 2 = list children, create, update, delete, **each upload fragment**; 5 = permission operations.

| Scope | Window | Limit (0–1,000 licences; the RU rows grow with licence count) | What it means |
|-------|--------|-------|-------|
| Per app per tenant | 1 min | **1,250 RU** | ≈ 625 writes/min ≈ **10 writes/s per tenant** |
| Per app per tenant | 24 h | 1,200,000 RU | ≈ 600k writes a day per tenant |
| Per app per tenant | 1 h | **400 GB ingress AND 400 GB egress** (separate rows, no licence bound) | ≈ **111 MB/s each way per tenant**, whatever the user count |
| Tenant, all apps | 5 min | 18,750 RU | three apps' worth; apps share the tenant bucket |
| Per user | 5 min | 3,000 requests | |
| Per user | 1 h | 50 GB ingress, 100 GB egress | **14 MB/s in, 28 MB/s out sustained per drive** — the fleet's ceiling while each tenant has one user |

Microsoft: don't register several AppIDs for the same job (they share the tenant bucket); limits are
per geo under Multi-Geo; "Microsoft reserves the right to lower limits on Unpaid/Unlicensed usage".

**Retry-After is the contract.** The same page now says SharePoint Online "does not return or support
IETF RateLimit headers" and that apps "should instead honor the Retry-After header" — while another
section still describes `RateLimit-Limit` / `-Remaining` / `-Reset` as early signals. Treat the three
headers as optional hints when present; never depend on them.

### Throttle Handling

On **429** (Too Many Requests) or **503** (Server Too Busy):
1. Read `Retry-After` header (seconds to wait)
2. If both `Retry-After` and `RateLimit-Reset` present, honor the **greater** value
3. Pause ALL requests to that tenant (not just the failed one) — throttled requests still consume RU
4. Use decorrelated jitter for backoff (not fixed exponential)
5. Cap backoff at 60 seconds

**What the driver does (WP-R7-4, R7-08):** the S3-class drivers' bound —
**2 attempts on a request path, 3 under `common.WithJob`** (a job run) — for
every Graph call and every upload fragment; every wait between attempts
selects on the caller's context (it was a `time.Sleep` no caller could end:
5 attempts, up to 30 s each, so one call to a dead account held a request
~30 s after its client left); `Retry-After` is honoured up to 30 s
(`odMaxRetryWait`) and a wait that would outlast the caller's deadline is not
waited for at all — the call fails at once; a caller already gone is never
retried. Responses are bounded by request class (RP9-1): a Graph call's
headers 15 s, a CDN range's first byte 25 s, an upload fragment's answer
2 min (the last fragment commits the file — the exception, like an S3
Complete). Errors are typed: `odError{Status, Code}` (Graph's
`{"error":{"code":…}}`); a miss is a 404 `itemNotFound` (or a 404 with no
Graph body), wrapped as `engine.NotFoundError` by Get/Delete; another 404
code (`ResourceNotFound`: the drive or the user gone) is the account's
failure. A conflict creating the root folder is decided by status/code, never
the text.

**Ranges and fragments ride the bursts (WP-FLEET-3 PR A, 2026-11-06).** Above
the published per-user rates Microsoft does not cap: it answers **429 bursts of
about a minute, about every five minutes** (the two-hour soak: 574 range 429s,
the next round always clean). A CDN range had ONE attempt (the window's retry
runs only with hedging, which the fleet never set) and a fragment two, so a
burst lost the object. Now a range (`fetchRange`) and a fragment (`putChunk`)
get **`odBurstAttempts` = 6 attempts on a request path, 8 under a job**: a
transport error, a body cut short, a 429/503 or another 5xx is asked again —
the same range of the same URL, the same bytes of the same session, nothing
doubled. The wait is the larger of a backoff (2, 4, 8, 16, 32 s … jittered up
by half) and the answer's `Retry-After` (seconds or a date), capped at
**`odBurstMaxWait` = 90 s**; every wait is on ctx and none is begun past the
caller's deadline. A 200 to a range (Range ignored), a wrong Content-Range,
another 4xx and a caller gone are not retried. A read under a burst may take
minutes — the trade (an error is worse; the caller's context bounds it). The
per-attempt bounds are unchanged (25 s first byte, 2 min fragment answer).
Graph metadata calls (`graphDo`) keep 2/3 attempts and the 30 s cap. A whole
Get under 10 MiB rides the same attempts since RB3-2 (it is one part of the
ranged reader, fetched without a Range header).

**A range that does not come fails the body (D-F3-1).** The soak also saw
**135 `size mismatch: got 0`**: a GET whose ranges were refused returned a
reader that gave 0 bytes and then a CLEAN end. The cause was in `parfetch`: a
part's error that wrapped `io.EOF` (`io.ReadFull` of an empty body, or Go's
transport answering `EOF` for a dropped connection) passed every
`errors.Is(err, io.EOF)` check, so `Reader.WriteTo` — what `io.Copy` uses —
returned `(0, nil)`. Now a part's failure is `parfetch.ErrPartCut` (the chain
to `io.EOF` cut, the words kept), the only `io.EOF` a stream hands out is its
own end (compared by identity), and the driver reports an empty range body as
`io.ErrUnexpectedEOF`. Every `parfetch` user (the chunked GET, the parallel
ranged GET, the parity rebuild) gets the same guarantee.

**Pacing — the good-tenant posture (WP-FLEET-3 PR B, `onedrive_pace.go`).**
The driver paces at Microsoft's PUBLISHED rates, even though the soaks drew
several times more, and treats a 429 as the budget's signal:

| Knob | Default | Bucket |
|---|---|---|
| `PERMAFROST_ACCOUNT_RPS` | 8 (burst 100) | requests per account (3,000 / 5 min) |
| `PERMAFROST_ACCOUNT_IN_GBH` | 50 | bytes written per account per hour |
| `PERMAFROST_ACCOUNT_OUT_GBH` | 100 | bytes read per account per hour |
| `PERMAFROST_TENANT_GBH` | 400 | bytes per TENANT (app registration) per hour, each way, shared by every slot in it |
| `PERMAFROST_RANGE_HEDGE_AFTER` | 2s | a CDN range not done after max(this, 3 × the Get's median range time) is asked again |

GB = 10⁹ (Microsoft's). `0`/`off` turns a bucket (or the hedge) off; a
rejected value is a Warn and the default. A bytes bucket holds a minute of
its rate (≥ 64 MiB): a lone read after a quiet spell is not slowed, a
sustained one runs at the rate (100 GB/h ≈ 28 MB/s per drive; 400 GB/h ≈
111 MB/s per tenant). **The tenant bucket is the one that bites first in a
dense tenant** (two drives of one tenant flat out lost reads in 12 of 21
rounds, 2026-11-06). Callers WAIT — never a refusal; the caller's context
bounds it. A request token before every Graph call, range attempt, fragment
attempt and session query; a range's bytes once, before its first attempt; a
fragment's bytes as it is READ from the source (back-pressure, the
`sia_write.go` pattern: the fragment's answer bound times Microsoft only, a
retry re-sends the buffer without taking its bytes again). The pacer is the
one `drivers/pacer.go` the sia budgets use.

**A 429 holds the whole account (D-F3-6):** any 429/503 marks the account
held for its `Retry-After` (5 s if none; at most 90 s) and drains its
buckets; every request of every caller on that account waits for the hold's
end (`throttledUntil`, which nothing read before). A retry's own wait covers
the hold; a hold past the caller's deadline fails the request as throttled at
once (`errODThrottled`), nothing sent. `RateLimit-Remaining` below 20 % of
`RateLimit-Limit`, when Microsoft sends them, holds likewise until
`RateLimit-Reset` (never seen in any run).

**A slow range is hedged (D-F3-4)** on another connection to the same
download URL; the first verified copy wins, the loser is cancelled; never a
fragment. The hedge waits out the hold and takes a request token like any
request. **A fragment that exhausted its attempts resumes its session
(D-F3-3):** the session is asked for `nextExpectedRanges` and the rest of the
fragment is sent once from the buffer; a fragment the session already holds
(its answer was lost) is skipped — the session's view wins; a session that is
gone (404) restarts the upload in a new session only for a rewindable body
(the materialize spool, a `bytes.Reader`); a streamed one fails as before.
**Token renewal (D-F3-10):** one renewer at a time, 5 min before the token
ends; the others keep the valid token; a failed renewal pauses renewals 15 s
(a valid token is still served, an expired one fails at once — no renewal per
request).

Metrics: `vaultaire_permafrost_throttles_total{account,op=graph|range|fragment|session}`,
`vaultaire_permafrost_paced_seconds_total{account,dir=requests|in|out|hold}`,
`vaultaire_permafrost_range_hedges_total{outcome=won|lost|failed}`,
`vaultaire_permafrost_fragment_resumes_total{outcome=resumed|skipped|restarted|failed}`;
rule `PermafrostThrottling` (> 10 a minute on one account for 10 min, warn).
One Info line at boot names the pacing in force.

**What the operator sees (WP-FLEET-10).** The System page's **Permafrost Fleet** card lists every account: the drive it writes to (the id's tail, and *pinned*), the quota its last probe read (used, the recycle bin, total), the 429/503s of the last hour (a per-minute ring the account's hold feeds), the probe's outcome, and the licence (WP-FLEET-6; "—" until then). Rules (`deploy/monitoring/vaultaire-drivers.yml`, promtool tests beside them):

- `PermafrostThrottling`: more than 10 throttles a minute on one account for 10 min;
- `PermafrostHashMismatch`: any quickXorHash mismatch in the last hour;
- `PermafrostFallbackWalking`: reads walking the fleet for 15 min, so the fingerprint is not settled.

Beside them, `PermafrostAccountNearFull` and `PermafrostFleetMismatch`. On the parity side, `vaultaire_vault_parity_protects_in_flight` shows the protects running now.

**User-Agent decoration** (Microsoft's documented format for prioritised ISV
traffic, on every Graph, CDN and fragment request since WP-FLEET-3 D-F3-7;
`main` stamps the build with `drivers.SetPermafrostBuild(api.BuildSHA)`):
```
ISV|Stored|permafrost/<BuildSHA>
```

### Delta Queries (Efficient Listing)

Delta queries cost only **1 RU with a token** (vs 2 RU for regular list). Use for:
- Sync/scan operations
- Detecting changes since last check
- Store `@odata.deltaLink` for incremental sync

### Batch Operations

Up to 20 requests per batch (`POST /$batch`). Each sub-request evaluated
individually for RU cost. Useful for bulk deletes or metadata fetches.

## Transport Optimization — Three Generations

### v1 (SDK + tuned transport) — 2026-04-18

Single transport, Graph SDK for everything. Applied 256KB buffers, 200 connection pool, HTTP/2, compression off.

```go
http.DefaultTransport = &http.Transport{
    MaxIdleConns:        200,           // connection pool size
    MaxIdleConnsPerHost: 200,           // per-host pool (default: 2)
    ReadBufferSize:      256 * 1024,    // 256KB (64x fewer syscalls vs default 4KB)
    WriteBufferSize:     256 * 1024,
    DisableCompression:  true,
    ForceAttemptHTTP2:   true,
    ...
}
```

**Result**: +69% per-tenant upload (11→18.62 MB/s), +158% large file throughput. Downloads unchanged at ~12 MB/s per tenant.

### v2 (raw HTTP, no Graph SDK) — 2026-04-19

Bypass the Graph SDK entirely. Use `azidentity` for auth only. Raw HTTP for all data operations. Binary drops from 44MB → 10MB (-77%).

- 1MB read/write buffers
- http2.ConfigureTransport() for explicit HTTP/2 config
- Chunked uploads for >4MB files (10MB aligned to 320KiB)
- Pre-warming (eliminates TLS handshake from timing)
- User-Agent: `ISV|FairForge|Vaultaire/1.0`
- Decorrelated jitter backoff: `sleep = min(cap, random(base, prev*3))`

**Result**: 100MB uploads +62% to 98 MB/s fleet. Worker scaling shifted — SDK topped out at 25, raw HTTP scales to 100+ workers. Downloads **still stuck at 12 MB/s fleet ceiling**.

### v3 (HTTP/1.1 for CDN + Range requests) — 2026-04-19

**Breakthrough: Go's HTTP/2 has a known flow-control bug** (github issues #54330, #47840, #63520) where slow streams block fast streams on the same multiplexed connection. Rclone's OneDrive backend documented a **66x speed improvement** by forcing HTTP/1.1 for bulk downloads.

**The fix: dual transport architecture.**

```go
// HTTP/2 for Graph API JSON calls (small, latency-sensitive)
func graphTransport() *http.Transport {
    t := &http.Transport{
        MaxIdleConns:        200,
        MaxIdleConnsPerHost: 200,
        ReadBufferSize:      1 << 20,    // 1MB
        WriteBufferSize:     1 << 20,
        DisableCompression:  true,
        ForceAttemptHTTP2:   true,
        ...
    }
    // HTTP/2 comes from ForceAttemptHTTP2 (the standard library); x/net http2.ConfigureTransport is deprecated
    return t
}

// HTTP/1.1 for CDN bulk downloads (bypasses HTTP/2 flow-control bug)
func cdnTransport() *http.Transport {
    return &http.Transport{
        // Empty TLSNextProto disables HTTP/2 ALPN negotiation
        TLSNextProto:     make(map[string]func(string, *tls.Conn) http.RoundTripper),
        MaxConnsPerHost:  64,
        ReadBufferSize:   4 * 1024 * 1024,  // 4MB — bigger for bulk data
        WriteBufferSize:  4 * 1024 * 1024,
        DisableCompression: true,
        ...
    }
}

// HTTP/2 for uploads (multiplexing helps write path)
func uploadTransport() *http.Transport {
    // Same as graphTransport but with 4MB buffers
}
```

**Range requests for single-file parallelism:**

```go
// Split one file into N parallel byte-range downloads
func (a *authClient) downloadRanges(ctx context.Context, dlURL string, fileSize int64, streams int) (int64, error) {
    rangeSize := fileSize / int64(streams)
    var wg sync.WaitGroup
    var totalBytes atomic.Int64
    for i := 0; i < streams; i++ {
        start := int64(i) * rangeSize
        end := start + rangeSize - 1
        if i == streams-1 { end = fileSize - 1 }
        wg.Add(1)
        go func(s, e int64) {
            defer wg.Done()
            req, _ := http.NewRequest("GET", dlURL, nil)
            req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s, e))
            resp, _ := a.cdnHTTP.Do(req)   // HTTP/1.1 transport
            defer resp.Body.Close()
            n, _ := io.Copy(io.Discard, resp.Body)
            totalBytes.Add(n)
        }(start, end)
    }
    wg.Wait()
    return totalBytes.Load(), nil
}
```

**60MB upload chunks** (up from 10MB — Microsoft's max, 35% faster).

**Batch metadata prefetch** via `POST /$batch` (up to 20 requests per call, 2.1x metadata speedup).

**v3 Results (SLC, 3 tenants, 2026-04-19):**

| Download strategy | Fleet MB/s | Per-tenant | vs v2 |
|---|---|---|---|
| HTTP/2 (v2 baseline) | 12.78 | 4.26 | — |
| HTTP/1.1 5w | 68.56 | 22.85 | **+437%** |
| HTTP/1.1 + Range(4) 5w | **214.46** | **71.49** | **+1,578%** |
| Batch + HTTP/1.1 10w | 125.56 | 41.85 | +883% |
| Batch + Range(4) + HTTP/1.1 5w | 182.55 | 60.85 | +1,329% |

**Single 100MB file with HTTP/1.1 + 2-stream Range**: 66 MB/s (single HTTP/2 was 9.25 MB/s — **7.1x speedup**).

**v3.1 Results (SLC, 3 tenants, 2026-04-20, ALL kernel tuning applied):**

| Download strategy | Fleet MB/s | Per-tenant | vs v2 |
|---|---|---|---|
| A: HTTP/2 5w (v2 baseline) | 97.53 | 32.51 | — |
| B: HTTP/1.1 5w | 117.12 | 39.04 | +20% |
| C: HTTP/1.1 range(4) 5w | 190.54 | 63.51 | +95% |
| **D: batch + HTTP/1.1 10w** | **200.83** | **66.94** | **+106%** |
| E: batch + range(4) + H1 5w | 88.18 | 29.39 | -10% |
| F: pipeline + adaptive + H1 5w | 161.69 | 53.90 | +66% |

**Key findings:**
- HTTP/2 single-stream: 49.70 MB/s — **72% faster than HTTP/1.1** (28.90 MB/s) for single files
- Range 8 streams: 108.47 MB/s peak (was 4-stream default, now 8-stream for >= 50MB files)
- Batch prefetch: 2.9x metadata speedup (758ms vs 2.2s for 10 URLs)
- Upload 60MB chunks: +28% vs 10MB (9.07 vs 6.53 MB/s)
- Worker counts aligned to tenant multiples (1,3,6,12,24) — avoids uneven distribution dip

## SLC TCP Tuning (Prerequisite)

Persistent in `/etc/sysctl.d/99-vaultaire.conf`:
```
net.ipv4.tcp_rmem = 4096 87380 67108864   # 64MB max (was 16MB)
net.ipv4.tcp_wmem = 4096 65536 67108864
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.ipv4.tcp_congestion_control = bbr
```

Verify with `sysctl net.ipv4.tcp_rmem` — should return `67108864` as max.

Without TCP tuning, v3 results drop ~30%. The 64MB buffers are required for the Range/parallel-download optimizations to reach full bandwidth.

### Additional TCP tuning (ALL APPLIED 2026-04-20)

From [Fornax's Guide To Ridiculously Fast Ethernet](https://docs.pixeldrain.com/posts/2024-03-07_network_optimizations/) (pixeldrain's 100Gbps optimization guide). All settings persistent in `/etc/sysctl.d/99-vaultaire.conf`.

```
net.ipv4.tcp_shrink_window = 1              # Cloudflare: prevent buffer bloat
net.ipv4.tcp_slow_start_after_idle = 0      # keep cwnd warm on idle connections
net.ipv4.tcp_fastopen = 3                   # client+server TFO
net.ipv4.tcp_mem = 12897485 16121856 19346227  # 40/50/60% of 123GB RAM
net.ipv4.tcp_tw_reuse = 1                   # reuse TIME_WAIT sockets for burst workloads
```

**Impact:** OneDrive 1-worker 22→101 MB/s (+348%), fleet 12→68 MB/s HTTP/2 (+473%), Lyve ingest 522→650 MB/s (+25%)

**Verify:** `ssh vaultaire-slc 'sysctl net.ipv4.tcp_shrink_window net.ipv4.tcp_slow_start_after_idle net.ipv4.tcp_fastopen net.ipv4.tcp_tw_reuse'`

## Benchmark Tools

| Tool | Purpose |
|------|---------|
| `onedrive-bench` | **Driver-path bench** — the only tool that measures production behavior. Constructs `NewOneDriveFleetDriver` and drives Put/Get/List/Delete through the `engine.Driver` contract: FNV placement, home-first probes, List union, Delete sweep all on the measured path. Prints the real placement histogram. |
| `permafrost-v2` | Raw HTTP transport explorer (no SDK), chunked uploads, RU tracking, 7 tests |
| `permafrost-v3` | Transport explorer — HTTP/1.1 CDN + Range downloads, dual transport, 60MB chunks, batch prefetch |

**The permafrost tools do not measure the driver.** They hand-roll auth and
spread files evenly across accounts themselves — their "fleet" numbers are a
transport ceiling, not the production number. Use `onedrive-bench` for
driver numbers; keep v2/v3 for isolating transport-level questions.

Retired 2026-07-31: `permafrost-fleet` + `permafrost-stress` (Graph-SDK-based,
superseded; removing them dropped msgraph-sdk-go and 8 kiota deps from
go.mod). The pre-fleet `permafrost-benchmark` + `permafrost-parallel`
(single-tenant, `AZURE_*` env vars) were local-only; source archived in
`.private/retired-bench/`.

### Build & Deploy

```bash
# Build for SLC
GOOS=linux GOARCH=amd64 go build -o permafrost-v3-linux ./cmd/tools/permafrost-v3/
scp permafrost-v3-linux vaultaire-slc:/tmp/

# Run on SLC (source env vars first)
ssh vaultaire-slc 'cd /tmp && source .env.bench && ./permafrost-v3-linux'

# Quick local test (all tenants)
source .env.bench
go run ./cmd/tools/permafrost-v3/

# Build+run any tool (substitute tool name)
GOOS=linux GOARCH=amd64 go build -o <tool>-linux ./cmd/<tool>/
scp <tool>-linux vaultaire-slc:/tmp/
ssh vaultaire-slc 'cd /tmp && source .env.bench && ./<tool>-linux'
```

### SLC `.env.bench` — Two Credential Sets

The SLC file at `/tmp/.env.bench` needs BOTH sets of credentials:

1. **S3 provider creds** (Lyve, R2, Quotaless, iDrive, Geyser) — for `bench-compare`
2. **OneDrive tenant creds** (`TENANT_1_ID`, `TENANT_1_CLIENT_ID`, `TENANT_1_SECRET`, `TENANT_1_USER`, etc.) — for `permafrost-*` tools

The Mac-local `.env.bench` has both. The SLC copy was originally created for S3 benchmarks only (April 2026) and the OneDrive tenant vars were appended on 2026-04-19. If SLC's `.env.bench` is recreated, re-sync the tenant vars:

```bash
# From Mac — append OneDrive tenant vars to SLC
grep "^export TENANT" .env.bench | ssh vaultaire-slc 'cat >> /tmp/.env.bench'

# Verify
ssh vaultaire-slc 'grep -c TENANT /tmp/.env.bench'  # should be 12 (4 vars × 3 tenants)
```

### Benchmark Tests (historical — retired stress tool)

The results below were produced by `permafrost-stress` (retired 2026-07-31);
kept for context on the prior-results tables that follow. Note the March/April
"Aggregate"/"Fleet" numbers summed per-tenant rates over different time
windows, which over-reports — treat them as directional, not comparable to
wall-clock numbers from current tools.

1. **File Size Scaling** — 1/4/10/50/100 MB files, 5 each, measures throughput vs file size
2. **Worker Count Scaling** — 10/25/50/75/100 workers, 50 × 1MB files, finds optimal concurrency
3. **Download Speed** — 1/10/50 MB files, measures restore performance
4. **Mixed Workload** — Simultaneous uploads + downloads across all tenants
5. **Throttle Stress** — 100 workers, 200 × 512KB files, watches for 429 errors

## Prior Benchmark Results

### March 6, 2026 — Pre-TCP-tuning

**Fleet benchmark (3 tenants, 25 workers/tenant):**

| Location | tenant-1 | tenant-2 | tenant-3 | Aggregate |
|----------|----------|----------|----------|-----------|
| MacBook (SLC ISP) | 14.68 | 8.82 | 16.07 | 39.56 MB/s |
| SLC server | 11.00 | 11.10 | 10.90 | 32.99 MB/s |

**Stress test (SLC server, 3 tenants):**

| Test | Result |
|------|--------|
| File size scaling | 8.19 -> 35.02 MB/s (1MB -> 100MB) |
| Best worker count | 25 workers, 30.30 MB/s fleet |
| Download (50MB) | **40.92 MB/s peak** (4x MacBook) |
| Mixed workload | 37.54 MB/s aggregate |
| Throttle stress | **0/200 failures**, ~400/1250 RU |

**15-tenant projections (linear scale):**
- Upload: ~175 MB/s / ~12.6 TB/day
- Download: ~450 MB/s / ~38.9 TB/day
- Storage: 75 TB free

### April 18, 2026 — Post-TCP-tuning + transport optimization (SLC, 2 tenants)

**File size scaling (per-tenant avg):**

| Size | MB/s | vs March |
|------|------|----------|
| 1 MB | 2.73 | ~same (latency-bound) |
| 4 MB | 11.13 | new test |
| 10 MB | 18.31 | new test |
| 50 MB | 28.79 | +146% |
| 100 MB | **30.21** | **+158%** |

**Fleet benchmark (25 workers/tenant):**

| Tenant | April MB/s | March MB/s | Change |
|--------|-----------|-----------|--------|
| tenant-1 | **18.62** | 11.00 | **+69%** |
| tenant-3 | **14.63** | 10.90 | **+34%** |
| Fleet (2 tenants) | **33.25** | 32.99 (3 tenants!) | matches with fewer tenants |

**Other results:**
- Downloads: 34-36 MB/s (50MB files) — comparable to March 40.92 peak
- Mixed workload: **47.99 MB/s** aggregate (+28% vs March 37.54)
- Throttle: 0/200 failures, ~2,297 RU/min (exceeded published limit, no throttle)
- Optimal workers: 25/tenant (confirmed again)

**15-tenant projections (updated):**
- Upload: **~249 MB/s / ~21.5 TB/day** (was ~175 MB/s, +42%)
- Download: ~530 MB/s / ~45.8 TB/day
- Storage: 75 TB free, cost: $0

## Production Driver Status

`internal/drivers/onedrive.go` — fully implemented, wired in
`cmd/vaultaire/main.go`. Complete `engine.Driver` contract (conformance
WP-R7-4, 2026-10-19): Put (simple <4MB, chunked upload sessions above —
streamed from the body when ContentLength is known, from the spool
`materialize` made when it is not; one pooled 60 MiB fragment buffer per
upload), Get (CDN download URL; ≥ 10 MiB = 8 MiB HTTP/1.1 ranges, adaptive
2/4/8 in flight, through `internal/parfetch`'s ordered window: at most
streams × 8 MiB held — 64 MiB — never the object; a range that is not
exactly the 206 asked for fails the stream), List (**recursive**: every key
relative to the container, `prefix` honoured — a folder is walked only when a
key under the prefix can be in it — all pages, the fleet's union; a folder
that cannot be listed fails the listing; ≤ 100,000 folders per account),
`RemoveContainerTree` (the erasure sweep's last step on this backend: the
container's emptied folders, on every account, only when List is empty),
Delete (whole-fleet sweep), Exists (home-first probe), HealthCheck (**every
account**, 8 at once, within the probe's 15 s: the worst is reported, each
failing account named; an account whose outcome changed is logged
`permafrost account probe: up|failing` — the first probe after a boot names
them all), RateLimit-header throttle tracking with bounded decorrelated-jitter
backoff (above), token refresh mutex, the fleet fingerprint (below).

### Fleet placement (WP-FLEET-5, 2026-11-07; `onedrive_placement.go`)

An object's home — a (tenant, drive) — is a pure function of its storage path
(`t-<tenant>/<container>/<artifact>`), under the placement **version** the
fleet's markers record:

- **Version 1** (2026-07-30 → WP-FLEET-5): `home = FNV-1a(path) % fleetSize`
  over the slot order. Prod's three accounts are version 1 until the
  rebalance tool moves them; a marker without `placement_version` is one.
- **Version 2** (the current one; what a first boot records): the key's
  **group** is its parent folder and its **shard index** *j* is the integer
  of a leaf `p<j>` (the parity layout `<tenant>__parity/<digest>/<etag>/p<j>`),
  else 0. The tenants stand in **columns** — tenant *k* (its position in the
  fleet's order of first appearance, append-only) is in column *k* mod
  `PERMAFROST_PLACEMENT_SPREAD` (default 4) — and shard *j* takes a tenant of
  column *j* mod spread by rendezvous (HRW, FNV-1a 64) on (tenant key, group);
  the drive inside the tenant is rendezvous on (drive identity, group). So the
  four parity shards of one object sit on four tenants whenever four exist
  (⌈4/n⌉ per tenant at fewer: an empty column falls back to every tenant under
  that cap); every file of one group that lands on a tenant lands on ONE drive
  of it; a tenant appended to the fleet joins one column and moves about one
  shard in (its size + 1) of that column's — an eighth of all shards on a 4 → 5
  step, measured — and a drive appended to a tenant moves about one file in
  *d* of THAT tenant's and nothing elsewhere. The price is balance: a column
  with fewer tenants gives each of its tenants a larger share (5 tenants at
  spread 4: three hold a quarter, two an eighth), so keep the tenant count a
  multiple of the spread (`docs/FLEET.md` § Sizing a tenant). A ranking shared
  across shards, or an exclusion of the tenants earlier shards took, moved half
  of every shard on a 4 → 5 step (the red test): with as many tenants as
  shards the assignment is forced and every change cascades.

- **Put** writes to the home under the current placement — a rebalance's
  target while one runs — even a throttled one (`graphDo` backs off). Writing
  elsewhere would strand the object where reads don't look first.
- **Get / Exists / Delete** ask the home; while the fleet is **not placed**
  (an accepted order change not yet rebalanced, a rebalance running, no
  verdict yet) they ask one more account: the home under the placement the
  markers say the data is laid out for — **computed, never a walk** (D-F5-3;
  it was a walk of up to eight accounts, which could not reach a remapped
  object past them). Two accounts at most, at any fleet size; the second look
  is counted in `vaultaire_permafrost_fallback_walks_total{op}` and a hit
  there logged (`onedrive fallback hit`). Delete takes both copies.
- **ListDir** of a **group folder** (two folders under the container — the
  parity layout's `<digest>/<etag>/`, whose members are files) asks the group's
  spread set: shards 0..spread−1's drives, one listing each; a shallower
  folder, whose members are folders that may sit anywhere, and **List** are
  the union of every account. **RemoveEmptyDir** of a group folder deletes it
  on the spread set.

**⚠️ Never reorder or remove `TENANT_N_*` entries.** The fleet's order is
what the fingerprint below checks and what version 1 places over; version 2
places over the tenants' order of first appearance in it. Appending is the
growth path: a new drive (a new, higher index) in an existing tenant moves
about 1/d of that tenant's files; a new tenant about 1/(spread · (column size
+ 1)) of the shards. `PERMAFROST_FLEET_ACCEPT` blesses the new order, and
`cmd/tools/permafrost-rebalance` (WP-FLEET-5 PR B) moves what changed home.

### The fleet file and the driver at scale (WP-FLEET-11, 2026-11-06; `onedrive_scale.go`)

Past 15 drives the env slots cannot hold the fleet. **`PERMAFROST_FLEET_FILE`** names a JSON file:

```json
{"tenants": [
  {"tenant_id": "<directory id>", "client_id": "<app id>",
   "secret": {"credential": "fleet_vierafun"},
   "drives": [{"index": 1, "user": "vierafun@viera.fun", "drive_id": "b!…"},
              {"index": 4, "user": "stored-fleet-001@vierafun.onmicrosoft.com", "drive_id": "b!…"}]}
]}
```

- `secret` or `certificate` (exactly one per tenant): `{"credential": "<systemd credential name>"}` or `{"file": "<path>"}`, never the value. A certificate is a PEM holding the certificate and its key (WP-FLEET-6).
- Unknown fields are refused, so a `client_secret` typed into the file is an error.
- **Index N is `tenant-N`, in the same place as env slot N.** Moving from the slots to a file changes neither placement nor the metric labels.
- Placement is by ascending index. Indexes are unique and append-only: a new drive takes a new, higher index, and `PERMAFROST_FLEET_ACCEPT` blesses a changed order as before.
- The file and `TENANT_N_*` together are refused at boot. Without a file, the slots are read as before.

**One credential, one token and one transport set per tenant** (D-F11-2). This holds for slots of one directory and app too: the tenant's first account renews the token, and the others ask it.

**Placement-authoritative reads** (D-F11-3; RB3-1, the review of the chain, 2026-11-07; WP-FLEET-5 D-F5-3). Once the fingerprint is settled (`match`, `recorded`, `repaired`, `accepted`) **and every marker records its data laid out for that order and placement** (`placed_under` = the current digest, one `placement_version`, no `placement_target`), `Get`, `Exists` and `Delete` ask the home account only, so a miss is a miss and costs one request (at 300 drives it was 300). While the fingerprint is `mismatch` or `unknown`, any marker is placed under another order (an accepted change not yet rebalanced — the order is settled, the data is not), or a rebalance target is set, the second look is the home under the placement the markers record — computed from the marker's `fleet`, `tenants` and `placement_version` — never a walk (it was one, capped at 8 accounts, which could not reach a remapped object past them); each second look is counted in `vaultaire_permafrost_fallback_walks_total{op}` (rule `PermafrostFallbackWalking`), and `vaultaire_permafrost_placement_version` is 0 meanwhile (1 or 2 once placed). The fingerprint proves the ORDER is the recorded one; it says nothing about where the objects sit — `PERMAFROST_FLEET_ACCEPT` rewrites every marker, so without `placed_under` the very next check (the hourly re-check, the next deploy) was `match` and every remapped object a miss.

**The probe samples** (D-F11-5). A HealthCheck probes:

- `max(min(n, 8), ceil(n / 240))` accounts in rotation, so a fleet of up to 8 accounts is probed in full each time, and every account within an hour of 15 s intervals;
- plus up to 8 accounts whose last probe failed;
- and each tenant's token, once.

It reports every account whose **last** probe failed, or whose tenant's token failed.

**Boot:** the drives are initialised, and the fingerprint markers read and written, 16 at a time. 300 drives at 50 ms a call boot in about 3 s (one after the other: 45 s).

**Not done, and why:**

- `ListDir` from "the drives placement names" (D-F11-4) is WP-FLEET-5's D-F5-4 (§ Fleet placement): a group folder on its spread set, shallower folders and `List` the union.
- The fingerprint by `$batch`. A `$batch` answers a `/content` read with a redirect, so it saves only the metadata calls. The concurrent read bounds the boot instead; `$batch` is WP-FLEET-9's.

### Fleet fingerprint (WP-R7-4, R7-09)

The rule above is now checked. Every account carries a marker,
`vaultaire/_fleet/fingerprint.json` (outside every `t-<tenant>/` tree): its
index in the fleet and the ordered fleet — one 16-hex hash per account of its
`TENANT_N_ID` + `TENANT_N_USER`, never a secret — the fleet's digest, and
since RB3-1 `placed_under`: the digest of the order the account's DATA is laid
out for (a first boot writes the current digest; an accept carries each old
marker's value forward, a brand-new account gets the current digest; a marker
written before the field reads as placed under its own digest — prod's
markers); since WP-FLEET-5 `placement_version` (absent = 1), `tenants` (the
fleet's grouping by directory as written — key = a hash of the directory id,
`drives` = fleet positions — so the old placement can be computed exactly) and
`placement_target` (set by the rebalance tool while it runs: writes go to the
target placement, reads look there first and at the recorded one second).
`cmd/vaultaire` reads every marker at boot (`CheckFleetFingerprint`, 20 s):

| Found | Outcome |
|---|---|
| no account has a marker | first boot: written on every account (`state=recorded`, Info) |
| every marker records this fleet | `match`, nothing written |
| the markers present match, some accounts have none | `repaired`: the missing ones written |
| a marker records another order or size | `mismatch`: **one Error line** `permafrost fleet fingerprint mismatch` with `diffs` (`size 3 → 2`, `index 1: was <hash>, now <hash>`, `account tenant-3 (<hash>) is index 1, recorded index 2`) and `fingerprint` (the current digest); `vaultaire_permafrost_fleet_mismatch` 1, rule `PermafrostFleetMismatch` (warning); nothing rewritten |
| a marker cannot be read | `unknown`: a Warn, no verdict, nothing written, the gauge stays 0 — **re-checked at the next probe** (below) |

Boot is never blocked: reads find a remapped object by the second look at
its previous placement — on a `mismatch`, and after an accept until a
rebalance rewrites `placed_under` (the `placed` field of the verdict's log
line; `authoritative()`).

The check does not end with the boot (RP18-1, Prompt 17 Part 0): the
driver's probe (every 30 s) re-runs it at its next run after an `unknown`
verdict — Graph down at deploy time used to leave the gauge at 0 for the
slot's lifetime — and then every hour, 10 s inside the probe, with the
`PERMAFROST_FLEET_ACCEPT` value the boot read. A verdict is logged once per
change (the same `match` every hour is no line); the gauge follows every
check. The journal line to look for after a deploy during a Graph outage is
the same `permafrost fleet fingerprint` / `… mismatch` line, a probe later.

**Procedure when the order must change** (an account lost for good, a new
account added):

1. Expect the mismatch. Note the Error line's `fingerprint`.
2. Accept the new order: `PERMAFROST_FLEET_ACCEPT=<fingerprint>` in the
   `.env`, `sudo vaultaire-switch restart`. The boot rewrites every marker
   with the new order (`state=accepted`, a Warn line) and the gauge goes to
   0 — but each marker keeps `placed_under` = the OLD order (an account
   appended holds nothing and gets the new one), so reads keep looking at
   the previous placement's home second (`PermafrostFallbackWalking` warns
   while they do; `vaultaire_permafrost_placement_version` is 0) and the
   `onedrive fallback hit` Info lines name what is off its home. A value
   left set does nothing once the markers match, and never accepts a
   different order (it must equal that order's digest). Remove it at the
   next edit.
3. Rebalance: `cmd/tools/permafrost-rebalance -dry-run`, then `-run` (every
   file whose home changed copied to its new home, verified by quickXorHash,
   the source `permanentDelete`d; resumable; paced through the driver's own
   buckets), then `-commit`, which rewrites every marker's `placed_under`
   to the new digest and `placement_version` to the current one — the
   moment reads are authoritative again (**PR B of WP-FLEET-5; until it
   lands an accepted fleet keeps its second look for ever** — the honest
   state, and the one an erasure needs: an off-home object the home-only
   `Delete` cannot see leaves its container non-empty, `RemoveContainerTree`
   refuses, and the tenant's erasure is deferred hourly until the object is
   found).

Restoring the old `TENANT_N` order instead is a plain restart: the markers
match again.

### Drive selection (WP-FLEET-1, 2026-11-05)

A fleet user can have more than one drive. `GET /users/{upn}/drives` lists the
user's own library, named **`OneDrive`** (also `/users/{upn}/drive`, the
default), and on two of prod's three users a hidden system library,
**`PersonalCacheLibrary`**, *before* it — in an order Graph does not pin and
that differs per account. The driver took `drives[0]` until WP-FLEET-1, so on
those two accounts (`vierafun@viera.fun`, `fromhis@fromhis.com`) the whole
`vaultaire/` tree and its fingerprint marker were written into the cache
library; on `vierapics@viera.pics` the `OneDrive` library is listed first and
holds it.

The account's drive is now, in order (`getDriveID`, `chooseDrive`):

1. **`TENANT_N_DRIVE_ID`** when set — used as is, no listing (a pin);
2. else the one drive listed, when the user has one (new accounts);
3. else the drive named `OneDrive`;
4. else the one `driveType: business` drive when exactly one is;
5. else nothing: one **Error** line `permafrost account skipped` naming the
   drives found, and the slot is **skipped** — as an incomplete slot is, so
   the fleet's order changes and the fingerprint reports the mismatch.

A listing Graph cannot answer at boot keeps the slot (retried on first use,
as before). Every kept slot logs one Info line `permafrost account drive`:
`account`, `drive_id`, `pinned`, `other_drives` (the names the listing did
not choose). A drive id (`b!…`, 66 characters) is not a secret.

**The marker records its drive.** A marker written since carries `drive_id`
(additive: a marker without it reads as before). At every fingerprint check
(boot, then the probe — hourly) each account gets a drive verdict:

| Found | Outcome |
|---|---|
| the marker's `drive_id` is the chosen drive, or absent | 0 |
| the marker's `drive_id` names another drive | **one Error line per account** `permafrost account drive mismatch` (`drive_id`, `marker_drive_id`, `moved_to`), `vaultaire_permafrost_drive_mismatch{account}` 1 |
| the chosen drive has no marker, but another drive of the same user does (no pin: the listing named it) | the same Error line with `marker_on_other_drive: true` and the gauge 1; the fleet's ORDER is judged from that marker, and **no fresh marker is written** on the chosen drive — a deploy that changed an account's drive used to look like a first boot there |

Never an automatic switch, never a block of boot. The marker's `drive_id` is
rewritten only through `PERMAFROST_FLEET_ACCEPT` (the existing door rewrites
every marker, now with the drive) or `cmd/tools/permafrost-migrate-drive
-switch`. The line is logged on a change of verdict, the gauge follows every
check.

**Prod (D-F1-3):** before the build that chooses by name was deployed, every
slot's `TENANT_N_DRIVE_ID` was pinned to the drive that held its `vaultaire/`
that day — the cache library on slots 1 and 3, `OneDrive` on slot 2 — so the
deploy moved nothing; the migration tool then moves slots 1 and 3 one at a
time (below).

State on 2026-11-06: slot 3 moved (`-copy` → `-switch` → its pin set to the
`OneDrive` library → restart; read back whole through the driver, 0 fallback
hits); its old tree is retired after 24 h; slot 1 next; slot 2 stays (its
`-switch` only records `drive_id`). The procedure, per slot, with
`cmd/tools/permafrost-migrate-drive -slot N` on SLC (`.env.bench`):

1. `-dry-run` — which drive holds `vaultaire/`, how much;
2. `-copy` — Graph's `copy` to the `OneDrive` library, then the verify (count,
   bytes, `quickXorHash`); exit 2 = a difference, nothing switched;
3. `-switch` — verify again (it refuses while the still-pinned old drive holds
   writes the copy lacks: re-run `-copy`), `drive_id` on both markers, the old
   one `moved_to`; it prints the `TENANT_N_DRIVE_ID` line;
4. prod `.env` (backup first) gets that line, `sudo vaultaire-switch restart`;
   the boot's `permafrost account drive` line names the new drive and
   `vaultaire_permafrost_drive_mismatch` stays 0 (a 1 = the marker names
   another drive: set the old pin back and restart);
5. read back through the driver (`blob-verify -driver permafrost -list …`
   with prod's pins) and, where prod has rows on `permafrost`, `routing_truth`;
6. after 24 h green, `-retire -confirm-retire` — the old tree
   `permanentDelete`d once its marker names the move and the new tree holds
   every old item.

### Bench caveat

No `permafrost-*` tool goes through this driver — they hand-roll their own
auth and spread files evenly across accounts themselves. Their "fleet"
numbers are an upper bound on the driver: real placement is hash-based
(balls-in-bins imbalance; a single-key stream is capped at one account's
rate), and the tools skip the driver's per-Put folder-ensure overhead.
Driver-path numbers need a bench that constructs `NewOneDriveFleetDriver`
and calls `Put`/`Get`/`List` directly.

### Patterns from SnapShelter (Node.js reference implementation)

Key patterns in `snapshelter/src/services/storage-providers/onedrive.js`:

- **`_graphFetch` wrapper**: Captures RateLimit-* headers on every response, handles 429/503
- **Token refresh mutex**: `_refreshPromise` prevents concurrent refresh attempts
- **Chunked uploads**: 10MB chunks aligned to 320KiB boundary, 3-attempt retry per chunk
- **Quota check throttle**: Only check once per 60 seconds during bulk operations
- **`@microsoft.graph.downloadUrl`**: Pre-authenticated URL from item metadata (no 302 redirect)
- **Conflict behavior**: `replace` for generated filenames (UUIDs)

## OneDrive Gotchas

1. **Simple upload is 250 MB on both drive types** (Graph `PUT …/content`, read 2026-11-05) — the driver's `simpleUploadMax` of 4 MB predates that and is a to-do: one PUT for most parity shards instead of create-session + fragments. Upload-session fragments must be **less than 60 MiB**; the driver's `odChunkSize` is exactly 60 MiB (it has worked; 180 × 320 KiB = 58,982,400 B is the value inside the bound)
2. **No intermediate folder creation**: Must create folders explicitly before uploading
3. **File name restrictions**: `" * : < > ? / \ |` invalid. Business also restricts `# %`
4. **Path encoding**: Use `encodeURIComponent` per segment. Better: use item IDs after initial upload
5. **Upload session chunks**: MUST be multiples of 320 KiB. Non-aligned chunks silently fail
6. **Upload session auth**: Do NOT send `Authorization` header on chunk PUT requests
7. **Download URLs expire**: `@microsoft.graph.downloadUrl` is pre-authenticated (no `Authorization` header, Range honoured) and Microsoft says it "might expire within minutes" — fetch per read, never cache
8. **409 on folder create**: Expected when folder exists — not an error
9. **Tenant-2**: Client secret was expired as of March 2026. Rotate at Azure portal — secrets live ≤ 24 months; certificate credentials (`azidentity.NewClientCertificateCredential`) are the fix, and `Application.Read.All` would let the probe export the expiry (the app registrations cannot read it today, 2026-11-05)
10. **Delete goes to the recycle bin and keeps counting — the driver now deletes for good (WP-FLEET-2).** `DELETE /drives/{id}/items/{id}` parks the item in the user's recycle bin for 93 days, and the bin AND file versions count against the drive's quota; after one bench day the three fleet drives held 68.4 / 23.2 / 20.9 GiB of bin against ~21 / 9 / 6 GiB of live files. Since WP-FLEET-2 every driver delete — `Delete`, `RemoveContainerTree`, `RemoveEmptyDir` (the erasure sweep and the parity reconcile go through these) — is **`POST /drives/{id}/items/{id}/permanentDelete`** (`deleteItem`; Graph v1.0, `Files.ReadWrite.All`, app-only, 204; a folder goes with its subtree; a 404 = already gone; a 403/5xx is the error it was): no bin entry. `TestPermanentDelete_NoGraphDeleteRequestInTheDriver` fails on any `DELETE` request left in `onedrive*.go`. **What the bin still holds** is from before, or from another app (the bench tools, a person in the web UI). Graph cannot empty it: that is the SharePoint REST `POST https://<tenant>-my.sharepoint.com/personal/<alias>/_api/site/RecycleBin/DeleteAll` with a SharePoint app-only token (`Sites.FullControl.All`, certificate) — the data app has no SharePoint role; the admin certificate apps on SLC do: `python3 -I ~/vaultaire-bench/fleet-admin/fleet-recyclebin.py` lists every fleet drive's bin (read-only), `--empty` purges it. **The one-time purge of prod's bins is the owner's decision** (not run by WP-FLEET-2). **Quota as Graph reports it** (`GET /drives/{id}?$select=quota`, read by every probe into `vaultaire_permafrost_quota_bytes{account,kind}` + `vaultaire_permafrost_quota_state{account,state}`): `used` EXCLUDES the bin and the user's other library; `deleted` IS the bin; `remaining` = total − used − deleted (prod 2026-11-05: 5120 − 20.61 − 68.37 = 5031.02 GiB) — so Graph alone under-reports the fill by 4× if you read `used` (SharePoint `site/usage` said 89.6 GiB where `used` said 23.1); the rule `PermafrostAccountNearFull` reads (total − remaining) / total > 0.8 for 1 h. Every in-place rewrite still makes a version (the driver's new addresses avoid that for objects; the fingerprint marker is rewritten in place)
11. **Hard limits**: 250 GB per file (a parity shard is object/4 → objects over 1 TB cannot be protected here), 400 decoded characters per path (Stored keys up to 857 bytes do not fit — generated names do), listing a folder fails near 100,000 files (rclone; fine at 50,000) — fan folders out before that

## References

- [Graph API throttling](https://learn.microsoft.com/en-us/sharepoint/dev/general-development/how-to-avoid-getting-throttled-or-blocked-in-sharepoint-online)
- [Upload large files](https://learn.microsoft.com/en-us/graph/sdks/large-file-upload)
- [Delta queries](https://learn.microsoft.com/en-us/graph/delta-query-overview)
- [JSON batching](https://learn.microsoft.com/en-us/graph/json-batching)
- [driveItem: createUploadSession](https://learn.microsoft.com/en-us/graph/api/driveitem-createuploadsession)
- [Graph API error responses](https://learn.microsoft.com/en-us/graph/errors)
