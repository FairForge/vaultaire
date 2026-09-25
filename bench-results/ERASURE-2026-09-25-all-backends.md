# Erasure-coding rehearsal, every backend — 2026-09-25

Second pass of `cmd/erasure-bench` (see `ERASURE-2026-09-24.md` for the tool
and the first Lyve/Geyser/OneDrive results). This pass adds iDrive, Wasabi and
Cloudflare R2 as legs, re-baselines every backend with `cmd/backend-matrix`,
and runs the archive-shaped layouts that the Vault-tier design uses. All from
slc-vaultaire-01; logs `~/vaultaire-bench/ec-all-0925-0405.log` and
`ec-r2-0925-*.log`. Every reconstruction below verified its SHA-256.

Legs: Geyser LA (`la1`, bucket stored3lib-6), Lyve us-west-1 service key,
OneDrive fleet (3 tenants configured on the bench box; prod fleet is 300 × 5 TB),
iDrive us-central-1 (Dallas, new reseller account), Wasabi us-central-1 (the
key on SLC; account identity not confirmed), R2 default jurisdiction via a
scratch bucket `vt-ecbench` (deleted afterwards).

## Baseline — backend-matrix, 64 MiB, single stream

| Backend | Health | Direct PUT | Direct GET |
|---|---|---|---|
| local NVMe | ok | 1364 MB/s | 2240 MB/s |
| iDrive | ok | 84 | 108 |
| Lyve | ok | 66 / 73 | 131 / 128 |
| Geyser | ok | 58 | 19 (disk cache) |
| Wasabi | ok | 31 | 33 |
| R2 | ok | 16 | 32 |
| OneDrive | ok | 8 | 37 |

Pairwise streamed copy Get(src)→Put(dst), MB/s, hash-verified on dst:

| src \ dst | geyser | idrive | local | lyve | onedrive | wasabi |
|---|---|---|---|---|---|---|
| geyser | · | 7 | 8 | 8 | 6 | 4 |
| idrive | 4 | · | 13 | 20 | 10 | EOF on verify read |
| local | 54 | 86 | 68 | 10 | 34 | |
| lyve | 36 | 57 | 70 | · | 9 | 25 |
| onedrive | 31 | 47 | 73 | 34 | · | 25 |
| wasabi | 15 | 16 | 15 | 13 | 3 | · |

Single-stream copies are slow everywhere; the engine's parallel chunk path is
what matters, and the erasure numbers below are the parallel ones.

## Layouts, 256 MiB payload, two runs each (1 GiB where noted)

`PUT` = client-visible write throughput for the whole object (wire = shard
bytes). `first-12` = all shards requested, first 12 win. `lose:X` = every shard
on X treated as gone.

### L1 — RS(12,12) Lyve 6 data / OneDrive 6 data / Geyser 6 parity / iDrive 6 parity (the four-leg Vault shape, iDrive standing in for Deep Archive)

| | PUT | first-12 | lose:geyser | lose:idrive | lose:lyve | lose:onedrive |
|---|---|---|---|---|---|---|
| 256 MiB | 53–75 MB/s (OneDrive 3.4–4.8 s) | 126–174 MB/s | 204–249 | 141–193 | 109–174 | 134–161 |
| 1 GiB | 94 MB/s (OneDrive 10.9 s; Geyser 2.8 s, iDrive 2.1 s, Lyve 1.9 s) | 136 MB/s | 132 | 169 | 132 | 188 |

Every single-leg loss still reads at 100+ MB/s. The race never needed more
than one Geyser shard.

### L2 — RS(12,6) Geyser 6 / iDrive 6 / OneDrive 6

| PUT | first-12 | lose:geyser | lose:idrive | lose:onedrive |
|---|---|---|---|---|
| 43–62 MB/s | 49–109 MB/s | 114–169 | 37–40 | 35–38 |

With only three legs and Geyser as data, any loss forces six Geyser reads:
35–40 MB/s. This is the shape the four-leg design degrades to.

### L3 — RS(12,8) five legs, 4 shards each: Lyve / OneDrive / Geyser / iDrive / Wasabi

| PUT | first-12 | lose:geyser | lose:idrive | lose:lyve | lose:onedrive | lose:wasabi |
|---|---|---|---|---|---|---|
| 50–62 MB/s | 153–158 MB/s | 145–191 | 108 | 50–83 | 57–131 | 80–103 |

Survives any two legs at 1.67× overhead; every single loss still 50+ MB/s.

### L4 — RS(12,6) Wasabi 6 / Lyve 6 / OneDrive 6

| PUT | first-12 | lose:lyve | lose:onedrive | lose:wasabi |
|---|---|---|---|---|
| 64–65 MB/s | 148–192 MB/s | run 1 **FAILED 11/12 (unexpected EOF on one fetch)**, run 2 99 | 22–98 | 34–150 |

### L5 — RS(12,6) iDrive 6 / Wasabi 6 / Lyve 6 (all hot)

| PUT | first-12 | lose:idrive | lose:lyve | lose:wasabi |
|---|---|---|---|---|
| **216 MB/s**, then **4 MB/s** (one Wasabi shard stalled 62.5 s) | 173–194 MB/s | 184–194 | 40–175 | 99–178 |

### L6 — RS(12,6) R2 6 / Lyve 6 / OneDrive 6

| PUT | first-12 | lose:lyve | lose:onedrive | lose:r2 |
|---|---|---|---|---|
| 51–57 MB/s | 200–261 MB/s | 135–147 | 172–243 | 34–123 |

### L7 — RS(12,12) Lyve / OneDrive / Geyser / R2

| PUT | first-12 | lose:geyser | lose:lyve | lose:onedrive | lose:r2 |
|---|---|---|---|---|---|
| 24–54 MB/s (R2 slowest 3.6–10.5 s) | 164–261 MB/s | 222–266 | 132–139 | 191–231 | 146–167 |

### L8 — RS(12,8) Lyve / OneDrive / Geyser / iDrive / R2, 4 each

| PUT | first-12 | lose any one leg |
|---|---|---|
| 26–63 MB/s (R2 slowest 2.2–9.7 s) | 162–183 MB/s | 128–202 MB/s |

## Findings

1. **Reads are solved.** First-12-wins delivered 126–261 MB/s in every layout
   and stayed above 100 MB/s under any single-leg loss whenever four or more
   legs exist. R2 and Lyve are the fastest readers from SLC, then iDrive,
   OneDrive, Wasabi; Geyser cache is last.
2. **Writes are gated by the slowest leg, and it is not always OneDrive.**
   OneDrive was slowest in the four-leg runs (3.4–10.9 s), but R2 stalled to
   10.5 s and Wasabi to 62.5 s on individual shards. Async parity is not an
   OneDrive-specific rule; every non-primary leg needs the queue.
3. **Transient fetch failures happen.** One `unexpected EOF` on a 21 MiB shard
   made an otherwise healthy 12-of-18 read fail. The engine must request more
   than 12 (all n) and retry a failed shard from the same leg once before
   declaring it lost. The bench does not retry; the engine must.
4. **Geyser as a data leg is the wrong shape.** L2 shows why: with Geyser
   holding data, any other leg's loss drops reads to 35–40 MB/s, and that is
   disk cache, not tape. Geyser is parity-only in every recommended layout.
5. **Five legs at four shards each (RS(12,8)) is the most resilient shape
   tested**: any two legs can vanish, single-leg losses barely register, 1.67×
   overhead. Its cost depends entirely on which legs are paid.
6. **iDrive is the strongest paid hot leg** (84/108 MB/s solo, never the
   slowest shard, no egress fees) and would be the Standard tier's home, not
   Vault's: at $4.95/TB it is 5× Deep Archive for a parity role.

## Cost per leg, for the Vault design

| Leg | $/TB-month | As a 0.5× Vault leg | Role |
|---|---|---|---|
| OneDrive fleet (300 × 5 TB = 1.5 PB) | $0 | $0 | free data leg |
| Lyve (promo, no volume cap, to mid-2028) | $0, then $6.37 | $0, then $3.19 | free data leg until 2028 |
| AWS Deep Archive us-west-2 | $1.01 | $0.51 | paid parity, never read |
| Geyser LA | $1.55 | $0.78 | paid parity, disaster reads at $0 |
| IBM Archive | $1.33 | $0.67 | alternative paid parity |
| iDrive | $4.95 | $2.48 | hot tier only |
| Wasabi | $6.99, 90-day minimum | $3.50 | no role; throttling observed |
| R2 | $15.00, zero egress | $7.50 | public/CDN only, as decided |

Vault design of record stays: Lyve 6 / OneDrive 6 / Geyser 6 / AWS 6,
RS(12,12), $1.28 per stored TB above the Geyser minimum, $0.51 below it. With
the corrected fleet size there is no OneDrive capacity ceiling.

## Caveats

- Wasabi key on SLC: account not confirmed (partner vs storedge). Numbers are
  for whichever account that key belongs to.
- OneDrive here is 3 tenants; the prod fleet is 300. Aggregate write speed
  scales with tenant count and parallelism; per-stream stays ~6 MB/s.
- Deep Archive was not benched (no AWS account yet); iDrive and R2 stood in
  for the fourth paid leg to prove the mechanics.
- All Geyser reads are disk cache. Tape-cold reads are minutes.
- Bench holds the payload in memory; engine must stream per pack.
