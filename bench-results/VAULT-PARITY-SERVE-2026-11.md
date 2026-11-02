# The parity copy as a serving copy: the fast rebuild (WP-VAULT-3, 2026-11)

The Vault E2E proof of 2026-11-02 (`VAULT-E2E-LA2-2026-11.md`) served a 96 MiB SSE-C Vault
object from its four Lyve parity shards in 27.04 s: ~1 s for Geyser's 404, **6.5 s to the first
stripe**, **19.5 s for the remaining 92 MiB (≈ 4.7 MB/s)**. The scrub's rebuild of the same object
took the same 19 s. This record holds what the WP measured and changed.

## 1. Where the 5 MB/s came from (PR 0)

`cmd/tools/parity-read-bench` (operator tool) writes m = 4 random objects of `-shard-bytes` under
`t-_bench-parity/<run>/p<j>` on `stored-us-east-1` (prod's Lyve region), reads them back in five
shapes and deletes them. It ran on SLC as user1, 2026-11-02 14:29–14:39 UTC. The box has no
separate bench Lyve key: `~/vaultaire-bench/lyve-service.env` (the file used) holds the same
data-plane key as prod's `.env`. The tool touched only its own `t-_bench-parity/` prefix and
deleted every object it wrote.

| Case | What it does |
|---|---|
| `lockstep` | the production `LyveDriver`; four whole GETs opened one after another, then 1 MiB from shard 0, 1, 2, 3 per stripe — what `parityRebuilder.decodeNext` did |
| `lockstep-h1` | the same through a bare SDK client on `TunedHTTPClient(WithHTTP1Only())` — no op-split transport, no driver code in the body path (H3) |
| `concurrent` | four whole GETs opened at once, each read to EOF by its own goroutine (the wire's ceiling for four streams) |
| `readahead` | four whole GETs opened at once, each read ahead into a ring of 4 stripes, consumed by stripe (D-V3-2) |
| `ranged` | `parfetch` over `GetRangeInfo`: 16 MiB ranges, 2 in flight per shard (8 in all), consumed by stripe (D-V3-1) |

**The first runs were misleading.** With one process reading the same objects case after case,
only the first case was slow (6.1 MB/s); every later case reused the warm connection pool and
read at 30–50 MB/s whatever its shape. A fresh object set per case (`-fresh`) did not change
that. A new connection pool per case (`-cold-conns`) did — and a rare parity read on prod opens
cold connections (the pool's idle timeout is 90 s). So the table that matters is the cold one.

100 MiB object (4 × 25 MiB shards), `-fresh -cold-conns`, 3 runs:

| Case | Open (ms, median) | First stripe (ms, median) | MB/s (median) | MB/s (min–max) |
|---|---|---|---|---|
| lockstep | 853 | 6,954 | **3.8** | 3.5–4.4 |
| lockstep-h1 | 844 | 2,816 | 5.5 | 4.6–9.6 |
| concurrent | 215 | 492 | **101.7** | 98.6–106.6 |
| readahead | 214 | 492 | 70.9 | 9.6–73.7 |
| ranged | — | 963 | **105.1** | 83.6–108.9 |

1 GiB object (4 × 256 MiB shards), `-fresh -cold-conns`, 2 runs:

| Case | First stripe (ms) | MB/s |
|---|---|---|
| concurrent | 490, 492 | 145.7, 142.0 |
| readahead | 486, 491 | 45.3, 55.2 |
| ranged (2 per shard) | 1,174, 931 | 95.6, 212.6 |
| ranged (4 per shard, `-parts 16`) | 1,403, 1,298 | 161.6, 117.9 |

The codec alone (`BenchmarkParityRebuild`, 4 × 25 MiB shards in memory, M1 Pro):
**5,117 MB/s**, 315 MB allocated per 100 MiB rebuilt (a fresh 1 MiB buffer per shard per stripe
plus Reconstruct's own data pieces).

**Which hypothesis held:**

- **H1 — half right.** The lockstep pull is the cause: `lockstep` reproduces prod (3.8 MB/s, a
  ~7 s first stripe against prod's 4.7 MB/s and 6.5 s), while the same four streams drained
  independently read at ~100 MB/s. The HTTP/2 half is wrong: the Lyve driver's client is
  `s3ObjectHTTPClient()` = `TunedHTTPClient(WithHTTP1Only(), …)` (pinned since the 2026-08-05
  ALPN audit, `internal/drivers/idrive.go`); the four shards travel on four HTTP/1.1
  connections. The likeliest mechanism, not proven here: each cold connection's receive window
  grows only as fast as the application drains it, and a reader that takes 1 MiB from one
  stream while the other three wait keeps every window small.
- **H2 — a minor share.** Sequential opens cost ~850 ms against ~215 ms concurrent; the rest of
  the 7 s first stripe is the lockstep read of the first stripe and the sealed header.
- **H3 — rejected.** The bare SDK client (`lockstep-h1`) is as slow as the driver.

So D-V3-1 (ranged, parallel, read ahead independently of the consumer) is right for a Lyve leg:
~100 MB/s on 100 MiB and 96–213 MB/s on 1 GiB, cold. D-V3-2's 4-stripe ring is much better than
lockstep but well below a free-running stream on a large object (45–55 MB/s at 1 GiB): one lagging
shard fills the other rings and stalls their connections. It serves only legs without ranged
reads (sync, permafrost), not prod's leg — residual RB-V3-1.

Logs on the box: `~/vaultaire-bench/parity-read-bench-{20261102,fresh-20261102,cold-20261102,1g-20261102}.log`.

Commands:

```
ssh vaultaire-slc 'cd ~/vaultaire-bench && set -a && . ./lyve-service.env && set +a &&
  ./parity-read-bench -runs 3 -fresh -cold-conns -access-key-env LYVE_SERVICE_ACCESS_KEY -secret-key-env LYVE_SERVICE_SECRET_KEY'
go test ./internal/api -run xxx -bench ParityRebuild -benchtime 3x
```
