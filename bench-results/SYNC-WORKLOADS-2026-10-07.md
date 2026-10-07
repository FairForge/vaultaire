# Sync tier — workloads, parity mixes, Cloudflare, assumptions (2026-10-07)

Prod box (SLC), prod build with the `sync` backend on five Sync.com bridges (one account,
five device profiles, `internal/drivers/webdav_multi.go`). Bench tenant only: bucket
`sync-lab` (tier `sync`), `sync-pub` (tier `sync`, public-read). Every read was hash- or
md5-verified. Lab resources (a gateway Worker, a Sippy R2 bucket, a lab database, lab
folders on Sync) were deleted afterwards.

## What broke, and what was fixed

| Finding | Effect | Status |
|---|---|---|
| Sync's DNS: Route 53 weights ~1 in 20 answers for `us-api-lb.sync.com` to `us-lb403.sync.com`, which has no A record | a resolver that caches that branch answers NXDOMAIN for up to 300 s → every bridge returns 500, the `sync` backend is down (seen four times in one night, 2–8 min each) | box: `us-api.sync.com` pinned in `/etc/hosts` (comment carries the re-check); reported to Sync |
| Bridge refuses names (`: ? * " < > \| \`, control chars, leading space, trailing `.`/space, any leading `~`, Windows device names, `desktop.ini`, `Thumbs.db`), discards `.DS_Store` after a 201, caps a name at 248 characters, cannot hold file `x` and folder `x` | a refused name tripped the `sync` breaker (5 in 60 s) and every tenant's sync-tier writes failed over to the primary; the client got 503 | #620: reversible %-escaping, 248 cap up front, refusal = `ErrInvalidInput` → 400, no failover — live: every key shape stores on `sync` |
| Keys `x` and `x/y` land on different bridges (HRW); the bridge writing `x/y` has not yet seen file `x`, creates folder `x`, and Sync drops the file | silent loss of `x` | #623: every object leaf carries the marker `%o`, so files and folders never share a name; an unstorable key reads as a miss — live: both readable after 120 s |
| Pack upload body not seekable | a bridge's 500 on a 256 MiB pack failed the seal | #619 |
| `TestEgress_AThousandConnections` | flaked once in the merge queue (cancel before the 16th 200) | noted, not fixed |
| CompleteMultipartUpload writes the whole object into ONE bridge PUT (~30 MB/s) before answering | through Cloudflare (100 s) a 2 GiB, 5 GiB or streamed 2 GiB upload answers 504; the client retries the whole upload; nothing is stored | #621: 200 + whitespace after 10 s, outcome in the body, write detached from the client — live: 2 GiB through Cloudflare stored on `sync` in 75 s |
| DeleteObjects deletes one key at a time; Sync deletes ~14/s per account | a 1,000-key batch answers 504 and stops mid-batch | #621: 16 deletes in parallel + keep-alive — live: 1,000 keys through Cloudflare in 72.9 s, 0 errors |

## Workloads through Vaultaire (sync tier)

| Workload | Result |
|---|---|
| Photo library: 900 thumbnails (30–80 KB) + 100 originals (3–6 MB), c16 | 12.6 objects/s, PUT p50 0.77 s, p95 3.3 s |
| Thumbnail reads, c8 | 5.4/s, p50 1.09 s, p95 3.3 s; a 30-thumbnail album at c30: 3.4 s |
| 2,000 × 4 KiB, c32 | PUT p50 0.72 s; LIST of 2,000: 0.45 s; GET 18.4/s p50 1.1 s; parallel delete 13.7/s (c16) |
| Overwrite race (2 writers × 20, concurrent reads) | 0 torn reads, final whole, HEAD ETag = GET md5 |
| 2 GiB / 5 GiB multipart, origin direct | up 30 / 32 MB/s (one bridge), down 177 / 193 MB/s (parallel ranges), single stream 46 MB/s TTFB 1.4 s |
| Range reads (1 MiB, c8) | p50 0.72 s; 4 KiB at head/middle/tail 0.67–0.81 s (true ranges) |
| restic 0.18.1, `/usr/share` (36,026 files, 1.15 GiB) | init 11 s, backup 30 s, unchanged re-run 11.7 s, restore 12.4 s byte-identical, `check --read-data-subset=10%` clean, prune clean; 24 objects on `sync` |
| Key shapes before #620 | `x` + `x/y`, `case`/`CASE`, Unicode, 400-char keys all fine on the primary; on Sync see the table above |

## Pack store on Sync (`cmd/tools/pack-bench`)

6,000 members (16 KiB–16 MiB mix, 5.5 GB) in 24 packs of 256 MiB: **write 60 members/s,
55 MB/s** (4 writers); cold member reads c16: 16/s, p50 0.83 s, p95 2.4 s; the last member
of a pack (offset 254 MiB) reads as fast as the first (0.80 vs 0.77 s) — the bridge serves
true ranges; index recovery from the pack file alone 6.7 s; delete 60 % + GC: 17 packs
rewritten, 1,911 members moved, 281 s, survivors intact; GC also found the 6 orphan packs a
failed run had left; delete-all + GC leaves no rows and no files.

## Parity mixes (`erasure-bench`, 256 MiB, 2 runs each, SLC)

Vault parity (RS 4+4, data shards on Geyser, the four parity shards on the leg, commit gated
on Geyser only):

| Parity leg | protected after | rebuild from parity alone (`lose:geyser`) | `lose:<leg>` (Geyser) |
|---|---|---|---|
| sync | 6.1–10.4 s | 105–112 MB/s | 39–49 MB/s |
| permafrost (OneDrive) | 5.2–6.2 s | 59–68 MB/s | 38–41 MB/s |
| lyve | 2.9–3.5 s | 239–242 MB/s | 42–46 MB/s |

Hot multi-vendor layouts (all reads hash-verified):

| Layout | PUT (all legs) | first-k read | worst single-vendor loss |
|---|---|---|---|
| 4+2 idrive:2 lyve:2 sync:2 | 32–48 MB/s (sync gates) | 113 MB/s | 23 MB/s once (lose lyve), else ≥ 86 |
| same, sync async | commit 191–268 MB/s, protected 4.9–6.3 s | 108–294 MB/s | 23 MB/s once, else ≥ 86 |
| 4+2 idrive lyve b2 | 29–58 MB/s | 133–160 MB/s | 110 MB/s |
| 4+2 idrive lyve wasabi | 100 MB/s | 77–118 MB/s | 5 MB/s once (Wasabi stall), else ≥ 85 |
| 6+3 idrive:3 lyve:3 sync:3 | 46–54 MB/s | 135 MB/s | 125 MB/s |
| 4+4 idrive lyve r2 sync | 28–56 MB/s | 114 MB/s | 98 MB/s (R2 shard reads needed 4 retries) |

## Cloudflare in front of the sync tier (SLC → DEN edge; home → DFW edge)

| Path | thumbnail cold → warm | 200 MiB warm | notes |
|---|---|---|---|
| Signed S3 GET on stored.ge | 0.92 s → 0.96 s | 40 MB/s | no caching; every read is a Sync read |
| `/cdn/<slug>/<bucket>/<key>` (public bucket) | 1.5 s → 0.14 s | 225–241 MB/s (SLC) | tiered cache: the DFW edge's first request was already a HIT |
| Gateway Worker + Cache API (private) | 2.4 s → 0.14 s | 45–60 MB/s | per data centre: DFW started cold again (2.6 s) |
| Sippy → R2 (private) | 1.3 s → 0.16 s | 33–51 MB/s | one global copy: warm from DFW after the DEN fill |

A Worker request with urllib's default User-Agent is refused (403) by the zone before the
Worker runs — send a User-Agent.

## Verdict

- Sync is a **cold / parity / backup** tier: restic, packs, Vault parity and large reads work
  well; per-object latency (~0.7–1 s) and ~14 metadata ops/s per account rule out browsing
  straight from it. Put a cache in front of anything interactive (public: `/cdn`; private:
  Sippy/R2 or a tiered Workers Cache, not the per-colo Cache API).
- **Vault parity leg:** Lyve is fastest (240 MB/s rebuild); Sync rebuilds at ~110 MB/s,
  nearly twice OneDrive, and is the leg Sync agreed to — a sound second choice and the right
  home once Lyve's promo ends.
- **Hot erasure:** Sync is a fine *async* parity leg (commit at hot-leg speed, protected
  ~5–6 s later); synchronous it caps the PUT at ~40 MB/s. All five bridges are one account —
  one vendor for the "≤ m shards per vendor" rule.
- **Large single objects** are capped at one bridge (~30 MB/s up); striping a large object
  across bridges (or packing it in 256 MiB pieces) is what would reach the 150 MB/s the five
  bridges deliver in parallel.
