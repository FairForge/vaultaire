# The parity copy as a serving copy: the fast rebuild (WP-VAULT-3, 2026-11)

The Vault E2E proof of 2026-11-02 (`VAULT-E2E-LA2-2026-11.md`) served a 96 MiB SSE-C Vault
object from its four Lyve parity shards in 27.04 s: ~1 s for Geyser's 404, **6.5 s to the first
stripe**, **19.5 s for the remaining 92 MiB (≈ 4.7 MB/s)**. The scrub's rebuild of the same object
took the same 19 s. This record holds what the WP measured and changed.

## 1. Where the 5 MB/s came from (PR 0)

`cmd/tools/parity-read-bench` (operator tool) writes m = 4 random objects of `-shard-bytes` under
`t-_bench-parity/<run>/p<j>` on `stored-us-east-1` (prod's Lyve region), reads them back in five
shapes and deletes them. It ran on SLC as user1, 2026-11-02 14:29–14:39 UTC, with the key of
`~/vaultaire-bench/lyve-service.env` — the same data-plane key as prod's `.env`. (Corrected in PR C:
this paragraph first said the box had no separate bench Lyve key. It has one — the `export
LYVE_*` lines of `~/vaultaire-bench/.env.bench`, which a grep for `^LYVE_` missed — and that is
the key the next run should use.) The tool touched only its own `t-_bench-parity/` prefix and
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

## 2. The prod proof (PR C, 2026-11-02 16:58–19:20 UTC)

Bench tenant (`~/vaultaire-bench/.env.bench` `VAULTAIRE_BENCH_*`), bucket `vault-serve-20261102`,
three random objects stored as GLACIER SSE-C (the only way to seal a Vault write on prod today, which
has no master key; #1061 refuses an unsealed GLACIER write). The helper is
`~/vaultaire-bench/vault-serve/p.sh`. Prod ran `12d1c2ca` (PR A) for steps b–c and `c6ad85f5` (PR B)
for step d.

**Deviation from the prompt:** the 1 GiB object was PUT to the active slot on the box
(`http://127.0.0.1:$ACTIVE_PORT`, the same signature and handlers), not through `s3.stored.cloud`.
Cloudflare refuses a body over 100 MB, and a multipart SSE-C upload is the 0x05 container, which the
parity job does not protect (RP19-1). Every GET went through `s3.stored.cloud` unless the row says
otherwise.

| Step | Object | What | Result |
|---|---|---|---|
| a | 96 MiB, 1 GiB | PUT GLACIER SSE-C, parity row `complete` on `lyve,lyve,lyve,lyve` within ~2 min | rows sized at the sealed length (100,664,861 / 1,073,758,237) |
| b | 96 MiB | GET, Geyser landing zone (normal path) | 200, server latency 6.57 s, SHA-256 equal |
| b | 1 GiB | same | 200, server latency 29.05 s (37 MB/s), SHA-256 equal |
| c | both | Geyser blobs deleted by hand (prod Geyser key; still in the landing zone, no archive class); then GET | `x-vaultaire-served-from: parity`, SHA-256 equal |
| c | 96 MiB | rebuilt from parity, flag OFF (the fallback, PR A's reader) | **server latency 1.85 s, 1.79 s** (27.04 s on 2026-11-02 03:07); first stripe ≤ 1.5 s on all three rebuilds (mean 1.21 s) |
| c | 1 GiB | same, through `s3.stored.cloud` | **16.41 s ≈ 65 MB/s** |
| c | 1 GiB | same, read from the origin slot directly (no Cloudflare) | 15.22 s (70 MB/s), 10.49 s (102 MB/s); `vaultaire_vault_parity_rebuild_bytes_per_second` 103 MB/s; 25 slow-range hedges (`vaultaire_large_get_hedges_total{path="parity"}`) |
| c | — | `vaultaire_vault_parity_shard_reads_total{kind="range",leg="lyve"}` | 12 = 3 rebuilds × 4 shards, all by range; `unavailable` 0 |
| d | 96 MiB #3, its Geyser blob PRESENT | flag OFF: GET | from Geyser (`served_from` absent), 2.96 s wall |
| d | same | `vault_parity_serve` ON for the bench tenant only (`PUT /api/v1/admin/flags/vault_parity_serve`, the admin JWT the owner's TOTP minted), two GETs | **`served_from: parity` both**, server latency 1.71 s / 2.43 s, `served_first` +2, `unavailable` 0, the Geyser blob untouched afterwards (HEAD 100,664,861 bytes) |
| d | same | flag row removed (DELETE, answer `enabled: false`, no override), GET | from Geyser again |
| e | all | DELETE ×3 and the bucket through `s3.stored.cloud` | `object_head_cache`, `object_versions`, `vault_parity`, `vault_parity_orphans`, `buckets` rows 0; the tenant's Lyve parity container `t-<tenant>/<tenant>__parity/` 0 objects; the bucket's Geyser prefix 0 objects; key and object files shredded |

**Acceptance.** The 96 MiB object served from parity in ≤ 3 s server latency (1.79–1.85 s, and
1.71 / 2.43 s on the flag path), first stripe ≤ 1.5 s: **met**. The 1 GiB object ≥ 100 MB/s:
**not met** — 65–103 MB/s, 100 MB/s on one read of four (median ~70). It is above the 50 MB/s line
where the prompt calls the proof failed, and 14–22× the 4.7 MB/s of 2026-11-02. The remaining
spread is Lyve's per-range latency at 2 ranges in flight per shard (25 hedges in two reads); PR 0's
lab run with 4 per shard (`-parts 16`) read 118–162 MB/s. That knob is shared with the parallel
GET (`LARGE_GET_PARALLEL_PARTS`), so changing it is an open question (WP-VAULT-3.md), not a step
here.

**One owner step was spent twice:** the first TOTP sign-in's flag PUT sent two malformed bodies
(the JSON was brace-expanded by bash inside `"$( … )"` in the step-d script) — refused 400, nothing
written; the second code set it.

## 3. What changed for the customer

- A Vault object whose tape copy cannot be read (Geyser down, the blob gone, its breaker open) is
  served from the parity copy at tens of MB/s instead of ~5 MB/s: a 96 MiB object in under 2 s
  where it took 27 s. Same bytes, same ETag, `x-vaultaire-served-from: parity`.
- With `vault_parity_serve` on for an account, a Vault object is read from the parity copy first.
  It answers 200 while its tape copy is archived, with no restore, no recall started by the GET,
  and no 403 `InvalidObjectState`. The flag is off for every tenant; nothing sold says so yet.
- HEAD of an archived object answers within 3 s even when tape does not.

## 4. Residuals

- **RB-V3-1** the 1 GiB rebuild misses 100 MB/s (§ 2). A per-path ranges-in-flight knob for the
  parity read, or 4 per shard by default, is the measured lever.
- **RB-V3-2** a leg without ranged reads (sync, permafrost) gets the 4-stripe read-ahead ring:
  45–55 MB/s on 1 GiB in PR 0's lab against 142–146 for free-running streams. Not prod's leg.
- **RB-V3-3** the proof's 1 GiB object went in by the origin slot (Cloudflare's 100 MB body limit);
  a customer cannot store a sealed Vault object over ~100 MB with parity through `s3.stored.cloud`
  today: SSE-C multipart (0x05) is not protected (RP19-1), single PUTs over 100 MB are refused at the
  edge, and SSE-S3 needs the master key (F-14).
