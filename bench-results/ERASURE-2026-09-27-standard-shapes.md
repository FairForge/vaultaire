# Erasure rehearsal, Standard/Performance shapes + Backblaze B2 + Worker egress — 2026-09-27

Third pass of `cmd/erasure-bench`, this time on the **online-only** layouts a
Standard or Performance tier could run (no tape in the read path), with
Backblaze B2 added as a leg (`B2_*` env, application key — B2 rejects the
master key on its S3 API). All from `slc-vaultaire-01`, log
`~/vaultaire-bench/ec-std-0927-0107.log`, script `ec-std.sh`. Every successful
reconstruction verified its SHA-256. Companion to
`ERASURE-2026-09-25-all-backends.md` (Vault shapes).

Two bugs found on the way, both fixed in this PR:

- `drivers.NewS3Driver` ignored its `region` argument and signed everything as
  `us-east-1`. Wasabi and R2 tolerate that; B2 does not (its SigV4 scope must
  name the account's region, here `us-east-005`). Regression test
  `TestNewS3Driver_SignsWithGivenRegion`.
- The bench truncated backend errors to 80 chars, which hid the real B2
  failure (below). Now 400.

## The B2 caveat that shaped every B2 number here

Every B2 shard **GET** in the run failed with

    403 AccessDenied: Cannot download file, download bandwidth or transaction
    (Class B) cap exceeded. See the Caps & Alerts page to increase your cap.

The account still has Backblaze's default daily download cap (no card on
file, caps never raised). PUTs are not capped, so the write-side B2 numbers
are real; every read that needed a B2 shard fell back to the other legs after
`-retries` and the `lose:idrive` / `lose:lyve` scenarios that required B2
shards failed outright. **Raise the caps in the B2 console and rerun S2, S3,
P1 and the B2-only control before quoting any B2 read number.** Yesterday's
direct aws-cli reads (299/273 MB/s for 1 GiB from SLC) ran before the cap
tripped.

## Layouts (256 MiB, two runs, `-retries 1`)

`commit` = client-visible PUT once the `-sync` legs have landed; `protected` =
all 12 shards landed. `first-8` = all shards requested, first 8 win.

### S1 — RS(8,4) iDrive 4 data / Lyve 4 data / OneDrive 4 parity, commit = iDrive+Lyve

| | PUT commit | protected | first-8 | lose:idrive | lose:lyve | lose:onedrive |
|---|---|---|---|---|---|---|
| 256 MiB ×2 | 108–110 MB/s (2.3–2.4 s) | 4.9–6.2 s (OneDrive) | 135–204 MB/s | 127–133 | 178 / **33** (OneDrive straggler 7.6 s) | 79–81 |
| 256 MiB, all-sync ×1 | 54 MB/s (4.8 s, OneDrive gates) | same | 153 | 143 | 31 | 84 |
| 1 GiB ×1 | 2.2 s commit | 12.7 s | 188 | 95 | 60 | 182 |
| 8 MiB ×2 (small object) | 215–283 ms | 1.4–2.0 s | 196–903 ms | 1.1–1.2 s | 0.7–0.8 s | **164–168 ms** |

Reading: with OneDrive parity-only and async, the write path is iDrive+Lyve
speed and the read race is iDrive+Lyve speed. Losing iDrive entirely (the
September account-suspension scenario) still reads at 95–133 MB/s from
Lyve+OneDrive. The weak spot is `lose:lyve`: iDrive+OneDrive reads are gated by
OneDrive's slowest shard (7–16 s on a bad run).

Small objects: the 8 MiB race is 5× slower when a OneDrive shard wins a slot
(903 ms vs 164 ms with OneDrive excluded). For objects under ~16 MiB the read
planner must not race OneDrive, or Standard must store them whole.

### S2 — RS(8,4) iDrive 4 / Lyve 4 / B2 2 + OneDrive 2 parity, commit = iDrive+Lyve

| PUT commit | protected | B2 shard PUT | first-8 | lose:b2 | lose:idrive / lose:lyve |
|---|---|---|---|---|---|
| 218–227 MB/s (1.1–1.2 s) | 4.5–4.9 s | 1.5–2.0 s per 32 MiB shard | 151–156 (2 B2 retries each) | 150–226 | FAILED — B2 cap |

### S3 — RS(8,4) iDrive 4 / B2 4 / OneDrive 4 parity, commit = iDrive+B2 (post-Lyve shape)

| PUT commit | protected | first-8 | lose:b2 | lose:idrive / lose:onedrive |
|---|---|---|---|---|
| 93–103 MB/s (B2 gates at 2.5–2.8 s) | 4.2–4.6 s | 147–163 (from iDrive+OneDrive after 4 B2 retries) | 164–168 | FAILED — B2 cap |

### P1 — RS(8,4) iDrive 4 / B2 4 / Lyve 4 parity, all hot, commit = iDrive+B2

| PUT commit = protected | wire | first-8 | lose:b2 | lose:idrive / lose:lyve |
|---|---|---|---|---|
| 99–130 MB/s (2.0–2.6 s) | 148–195 MB/s | **33–39** (polluted: 4 B2 retries then fallback) | 61–82 | FAILED — B2 cap |

With three fast legs the fully-protected write is 2.0–2.6 s for 256 MiB, the
best write shape of the day. Reads are meaningless until the B2 cap is lifted.

### B2-only control — RS(8,4) b2:12

256 MiB PUT 70 MB/s (3.7 s), reads 0/8 (cap). 64 MiB rerun after the region
fix: PUT 5 MB/s (12.2 s) — looks like B2 throttling the capped account — and
the 403 above in full.

## Worker egress path, measured from SLC

Throwaway Workers deployed via the Cloudflare API and deleted afterwards.

| Origin behind the Worker | single stream | TTFB | 8 × 32 MiB parallel | Range | edge cache |
|---|---|---|---|---|---|
| iDrive presigned URL (us-central-1) | 57–63 MB/s | 0.15–0.38 s | **299 MB/s** | 206 ok | **HIT** on 2nd read (iDrive's headers are cacheable) |
| B2 native download + download-auth token (09-26) | 31–35 MB/s | 0.30–0.45 s | 160 MB/s | 206 ok | BYPASS (B2 sends no-cache; needs Cache API) |
| direct iDrive presigned, no Worker | 146 MB/s | 0.11 s | — | — | — |
| direct B2 presigned, no Worker | 54 MB/s | 0.26 s | — | — | — |

The Worker halves single-stream throughput against either origin but keeps
aggregate throughput with parallel ranges, and it puts Cloudflare's cache in
front of iDrive for free. iDrive is not a Bandwidth Alliance member, so a
Worker in front of it removes SLC transit but not iDrive's 3× egress rule.

## Cost per stored TB, storage only (unit prices: BUSINESS_OUTLOOK 2026-09-22; B2 pricing page 09-26)

Lyve at $0 during the promo (to mid-2028), $6.37 list after. OneDrive fleet ~$0.

| Combo | overhead | COGS now | post-promo | floor @ $4.99 | floor @ $6.99 |
|---|---|---|---|---|---|
| Standard today: iDrive whole + OneDrive async copy | 2.00× | 4.12 | 4.12 | 17% | 41% |
| S1 iDrive 4 / Lyve 4 / OneDrive 4 | 1.50× | **2.06** | 5.25 | **59%** | 70% |
| S2 iDrive 4 / Lyve 4 / B2 2 + OneDrive 2 | 1.50× | 3.80 | 6.99 | 24% | 46% |
| S3 iDrive 4 / B2 4 / OneDrive 4 | 1.50× | 5.54 | 5.54 | −11% | 21% |
| P1 iDrive 4 / B2 4 / Lyve 4 | 1.50× | 5.54 | 8.72 | — | 21% |
| Perf: iDrive whole | 1.00× | 4.12 | 4.12 | — | 41% |
| Perf Unlimited: B2 whole + Worker | 1.00× | 6.95 | 6.95 | — | 1% (30% at $9.99) |
| Vault RS(12,12) Lyve/OneDrive/Geyser/AWS DA | 2.00× | 1.27 | 4.46 | 50% @ $2.55 | — |
| Public: R2 whole | 1.00× | 15.36 | 15.36 | — | — |

SLC transit per stored TB (660 TB/mo budget): whole-object proxied PUT 2×,
GET 2×; RS(8,4) PUT 2.5× (1× in, 1.5× out), GET 2× (only k shards return);
presigned-redirect or Worker-served GET 0×.

## Findings

1. **S1 is the Standard shape worth building.** Cheaper than today's
   all-iDrive Standard ($2.06 vs $4.12 while the Lyve promo runs), survives the
   loss of any one vendor, iDrive's egress pool exposure halves (only 4 of 8
   data shards live there and the race often takes Lyve's 4), and the read
   race is 135–204 MB/s. Post-promo it degrades to $5.25 — the owned-hardware
   leg in BUSINESS_OUTLOOK §5 is what replaces Lyve in that slot.
2. **Small objects stay whole.** Below ~16 MiB, erasure costs 5–12 shard
   round-trips per read and OneDrive's tail latency dominates. Store whole on
   iDrive with the async OneDrive copy, exactly as today.
3. **B2 as a data leg buys vendor independence at a price** ($5.54 with no
   Lyve) — worth it only for the post-promo world or as the unlimited-egress
   origin. As a 2-shard parity leg (S2) it costs $1.74/TB for a fourth vendor;
   cheaper than a second full copy, not free.
4. **Performance stays whole objects on iDrive.** P1's 2.0–2.6 s protected
   write is attractive, but $5.54 against $6.99 is a 21% floor and the reads
   are unproven until the B2 cap is lifted.
5. **Worker-in-front-of-iDrive is a free win for the presigned path**: 0× SLC
   transit, Cloudflare cache HITs, 299 MB/s aggregate. It does not change
   iDrive's egress bill.

## Repro

    set -a; . ~/vaultaire-bench/.env.bench; . ~/vaultaire-bench/idrive-new.env; set +a
    export GEYSER_BUCKET=$GEYSER_LA_BUCKET
    ./erasure-bench -mb 256 -runs 2 -data 8 -parity 4 -layout "idrive:4,lyve:4,onedrive:4" -sync idrive,lyve -retries 1

B2 legs need `B2_ACCESS_KEY/B2_SECRET_KEY/B2_ENDPOINT/B2_REGION` (+
`B2_BENCH_BUCKET`, default `vt-ecbench-b2`, create it first; it was deleted
after this run) and a raised download cap on the account.
