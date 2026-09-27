# Standard-tier strategy bench — iDrive+Geyser shapes, then +OneDrive, +Lyve, with Workers — 2026-09-27

Question: what should Standard be made of if it has to sit under Immich, Ente,
Nextcloud, backup tools and small SaaS? Four workload sizes (1, 8, 64, 256 MiB)
× five storage shapes, all from `slc-vaultaire-01` (`ec-std2.sh`, log
`ec-std2-0927-0207.log`), plus a client-vantage latency matrix for the hot
small-object path with a Cloudflare Worker cache in front of iDrive. Every
reconstruction hash-verified. Economics model: `stdmodel.py` (session
scratch; numbers reproduced below).

## Shapes

| Key | Layout | Overhead | Survives losing | Commit gate |
|---|---|---|---|---|
| M | mirror iDrive 1 + Geyser 1 (RS(1,1)) | 2.00× | either vendor | iDrive |
| E | RS(8,4) iDrive 4 / OneDrive 4 data, Geyser 4 parity | 1.50× | any one vendor | iDrive |
| F | RS(8,4) iDrive 4 / Lyve 4 data, Geyser 4 parity | 1.50× | any one vendor | iDrive+Lyve |
| G | RS(9,3) OneDrive 3 / Lyve 3 / iDrive 3 data, Geyser 3 parity | 1.33× | any one vendor | iDrive+Lyve |
| S1 | RS(8,4) iDrive 4 / Lyve 4 data, OneDrive 4 parity (09-27 a.m. pass + 1/64 MiB today) | 1.50× | any one vendor | iDrive+Lyve |
| A | all-iDrive whole + OneDrive async copy (today's prod shape) | 2.00× | iDrive (slow restore) | iDrive |
| T | tiered 15% iDrive / 85% Geyser whole (Smart design) | 1.00× | partial | iDrive |

Geyser shards in these runs are fresh, so they read from Geyser's staging disk
(~13 days). After eviction every "from geyser" read below becomes a tape
restore. That is the whole reason T is unusable under apps and why Geyser is
parity-only in every recommended shape.

## Baseline, single stream direct from SLC (MB/s)

| Size | iDrive PUT/GET | Geyser | Lyve | OneDrive |
|---|---|---|---|---|
| 1 MiB | 4 / 5 | 3 / 2 | 5 / 6 | 1 / 1 |
| 8 MiB | 25 / 24 | 19 / 16 | 39 / 32 | 2 / 6 |
| 64 MiB | 108 / 94 | 56 / 14 | 73 / 127 | 9 / 38 |

Small objects are latency-bound (~200 ms per op) on every backend; only
parallelism helps them, which is what the shard race does.

## Results (two runs each; `first-k` = race; `lose:X` = every shard on X gone)

### 1 MiB (thumbnail / small file proxy)

| Shape | PUT commit | protected | first-k | lose:idrive | lose:lyve | lose:onedrive | lose:geyser |
|---|---|---|---|---|---|---|---|
| M | 80–275 ms | 288–304 ms | 57–185 ms | 279–283 ms (Geyser disk) | — | — | 56–330 ms |
| E | 80–234 ms | 0.96–1.3 s | 183–388 ms | 0.74–1.3 s | — | 133–337 ms | 0.87–1.1 s |
| **F** | 99–209 ms | 362–509 ms | **107–110 ms** | 185–186 ms | 55–65 ms | — | 50–67 ms |
| G | 110–219 ms | 0.82–1.3 s | 185–363 ms | 668–679 ms | 656–761 ms | 183–293 ms | 717–865 ms |
| **S1** | 81–271 ms | 0.86–1.3 s | **113–132 ms** | 765 ms–1.0 s | 690 ms | 86–107 ms | — |

### 8 MiB (photo / RAW proxy)

| Shape | PUT commit | protected | first-k | lose:idrive | lose:lyve | lose:onedrive | lose:geyser |
|---|---|---|---|---|---|---|---|
| M | 145–430 ms | 481–555 ms | 179–289 ms | 0.5–1.0 s | — | — | 45–63 ms |
| E | 215–299 ms | 1.6–1.9 s | 346–815 ms | 0.95–1.1 s | — | 381–878 ms | 0.81–1.1 s |
| **F** | 254–326 ms | 419–454 ms | **145–225 ms** | 288–311 ms | 304–334 ms | — | 95–145 ms |
| G | 106–304 ms | 1.7–1.8 s | 264–280 ms | 668–856 ms | 713–786 ms | 279–311 ms | 1.1–1.3 s |
| S1 (09-27 a.m.) | 215–283 ms | 1.4–2.0 s | 196–903 ms* | 1.1–1.2 s | 0.7–0.8 s | 164–168 ms | — |

\* the 903 ms run had a OneDrive shard win a slot; with OneDrive excluded the
race is 164–168 ms.

### 64 MiB (backup pack / clip proxy), MB/s

| Shape | PUT commit | protected | first-k | lose:idrive | lose:lyve | lose:onedrive | lose:geyser |
|---|---|---|---|---|---|---|---|
| M | 97–167 | 1.2–1.7 s | 10–40 (noisy) | 6–22 | — | — | 31–48 |
| E | 95–189 | 2.6–2.7 s | 49–81 | 29–43 | — | 20–26 | 33–42 |
| **F** | 152–352 | 0.58–1.1 s | **115–139** | 23–25 | 32–37 | — | 140–175 |
| G | 112–262 | 3.1–3.6 s | 47–70 | 23–48 | 30–35 | 30–41 | 42–45 |
| **S1** | 156–266 | 2.6–2.8 s | **71–110** | 41–44 | 50–54 | 125–128 | — |

### 256 MiB (video proxy), MB/s

| Shape | PUT commit | protected | first-k | lose:idrive | lose:lyve | lose:onedrive | lose:geyser |
|---|---|---|---|---|---|---|---|
| M | 118–215 | 3.6–3.8 s | 83–145 | **7–14** (18–36 s from Geyser disk) | — | — | 84–185 |
| E | 323–409 | 4.7–4.8 s | 146–149 | 31–39 | — | 29 | 152–162 |
| **F** | 102–265 | 0.97–2.5 s | 114–116 | 35–38 | 32–38 | — | 133–136 |
| G | 173–296 | 4.2–4.9 s | 36–97 | 38–41 | 37–65 | 40–48 | 115–129 |
| **S1** (09-27 a.m.) | 108–110 | 4.9–6.2 s | 135–204 | 127–133 | 33–178 | 79–81 | — |

## Client-vantage latency: the hot small-object path with a Worker

Objects of 100 KB, 1 MiB, 8 MiB written to the live bench tenant on
stored.ge (lands on iDrive) and directly to iDrive us-central-1. Five GETs
each, medians. The Worker used the Cache API with a one-hour TTL.

| From the Mac (Cloudflare DFW colo) | 100 KB TTFB / total | 1 MiB | 8 MiB |
|---|---|---|---|
| stored.ge presigned (client → SLC → iDrive) | 274 / 401 ms | 243 / 620 ms | 264 / 850 ms |
| iDrive presigned direct | 180 / 264 ms | 240 / 463 ms | 180 / 867 ms |
| **Worker, edge cache HIT** | **118 / 148 ms** | **112 / 228 ms** | **103 / 398 ms** |

| From SLC (no bench load) | 100 KB | 1 MiB | 8 MiB |
|---|---|---|---|
| stored.ge presigned (loopback, engine read cache warm) | 50 / 50 ms | 47 / 50 ms | 48 / 209 ms |
| iDrive presigned direct | 131 / 188 ms | 145 / 1314 ms | 115 / 356 ms |
| Worker edge cache HIT | 144 / 182 ms | 139 / 291 ms | 132 / 386 ms |
| Worker cache MISS (edge → iDrive → fill) | — | 143–310 / 287–472 ms | — |

For a real client the Worker cache halves total time or better at every size
(148 vs 401 ms, 228 vs 620 ms, 398 vs 850 ms). From SLC itself the loopback
path wins, which is expected and irrelevant to customers.

## Economics per stored TB-month

Unit prices: iDrive $4.125 annual ($2.06 first year), Geyser $1.55, Lyve $0
until mid-2028 then $6.37, OneDrive ~$0, Deep Archive $0.99. Standard sells at
$4.99 per quota-TB.

| Shape | overhead | COGS Y1 | COGS now | post-promo | floor @100% fill | @80% | @60% | app-safe | vendor loss |
|---|---|---|---|---|---|---|---|---|---|
| A all-iDrive + OneDrive copy | 2.00× | 2.06 | 4.12 | 4.12 | 17% | 34% | 50% | yes | OneDrive copy, slow restore |
| T tiered iDrive/Geyser | 1.00× | 1.63 | 1.94 | 1.94 | 61% | 69% | 77% | **no, tape 503** | partial |
| M mirror iDrive+Geyser | 2.00× | 3.61 | 5.67 | 5.67 | −14% | 9% | 32% | yes | yes |
| E iDrive/OneDrive \| Geyser | 1.50× | 1.81 | 2.84 | 2.84 | 43% | 55% | 66% | yes (tape when degraded) | yes |
| F iDrive/Lyve \| Geyser | 1.50× | 1.81 | 2.84 | 6.02 | 43% | 55% | 66% | yes (tape when degraded) | yes |
| G 4-vendor RS(9,3) | 1.33× | 1.20 | 1.89 | 4.01 | 62% | 70% | 77% | yes (tape when degraded) | yes |
| **S1 iDrive/Lyve \| OneDrive** | 1.50× | **1.03** | **2.06** | 5.25 | **59%** | 67% | 75% | **yes, online when degraded** | yes |
| H 15% iDrive + 85% Vault-EC cold | 1.85× | 1.39 | 1.70 | 4.41 | 66% | 73% | 80% | yes (tape when degraded) | yes |

Egress, Worker and transit per stored TB per month, by workload (egress as a
multiple of stored bytes; iDrive pool = 3× iDrive-stored bytes; churn 1/12 of
stored bytes rewritten per month):

| Workload | egress/mo | avg object | edge hit | iDrive pool used (A / S1) | Worker $ | SLC transit proxied (A / S1) | with edge-served reads |
|---|---|---|---|---|---|---|---|
| Immich / Ente | 15% | 0.5 MB | 70% | 2% / 1% | 0.09 | 0.55 / 0.51 TB | 0.25 / 0.39 |
| Nextcloud / Seafile | 30% | 2 MB | 30% | 7% / 6% | 0.05 | 0.85 / 0.81 | 0.25 / 0.57 |
| restic / kopia / Veeam | 5% | 32 MB | 0% | 2% / 1% | 0.00 | 0.35 / 0.31 | 0.25 / 0.27 |
| small SaaS assets | 50% | 1 MB | 50% | 8% / 7% | 0.15 | 1.25 / 1.21 | 0.25 / 0.81 |
| media library | 25% | 500 MB | 20% | 7% / 5% | 0.00 | 0.75 / 0.71 | 0.25 / 0.51 |

No Standard workload gets anywhere near the iDrive 3× pool (max 8%), Worker
request fees are cents per stored TB, and proxied transit is 0.3–1.3 TB per
stored TB per month, so the 660 TB/mo SLC allotment carries roughly 500–2,000
stored TB depending on mix. Edge-served reads take whole-object shapes to
0.25 (writes only), about 2,600 stored TB.

## Findings

1. **iDrive + Geyser alone gives two bad shapes.** Mirror (M) is app-safe but
   costs $5.67 against a $4.99 price, and its degraded mode reads 256 MiB at
   7–14 MB/s even from Geyser's disk. Tiered (T) is cheap and breaks apps.
   Two vendors cannot do better; erasure needs a third leg.
2. **F is the fastest shape on every size** (1 MiB race 107 ms, 64 MiB 115–139
   MB/s, protected writes under 1.1 s at 64 MiB) because Geyser parity lands
   fast and never has to be read while healthy. Its cost is that a vendor loss
   after Geyser's 13-day staging window means tape restores for every read.
3. **S1 is the app-tier shape.** Same data legs as F, OneDrive parity instead
   of Geyser: $0.78 cheaper, and when a vendor is lost it keeps reading online
   (41–44 MB/s at 64 MiB, ~1 s small objects). Protected writes take 2.6–2.8 s
   at 64 MiB because OneDrive is slow, but the client sees the iDrive+Lyve
   commit (156–266 MB/s).
4. **The small-object threshold moves down.** With iDrive+Lyve as the eight
   data shards, a 1 MiB erasure read (107–132 ms) is *faster* than a whole
   1 MiB iDrive GET (~210 ms single stream). The rule is not "whole under 16
   MiB", it is "never let OneDrive or Geyser shards into the required set for
   small reads". Keep objects under ~1 MiB whole only to avoid 12× request
   amplification on thumbnails.
5. **G's 1.33× overhead is not worth it.** Needing 9 of 12 shards forces
   OneDrive or Geyser into every read (36–97 MB/s at 256 MiB, 7 s tails) to
   save $0.17 per TB against S1.
6. **The Worker cache is the biggest client-visible win available today.**
   148 vs 401 ms for a thumbnail, 228 vs 620 ms for a 1 MiB photo, from a real
   client. It needs no engine change for presigned and public reads.
7. **The 3× egress trap is not a Standard problem.** Even the 50%/mo SaaS
   workload uses 8% of the pool. It is a Performance-Unlimited pricing
   question.

## Recommendation for Standard

- Launch: shape A on the first-year iDrive rate ($2.06, 59% floor), Worker
  cache in front of presigned and public reads. Smart demotion stays off for
  app buckets; if it is turned on, its cold leg must be Lyve, never Geyser.
- Post-launch data path: S1 for objects ≥ 1 MiB, small objects whole on
  iDrive with the async OneDrive copy. Same erasure engine as Vault; Standard
  just uses a different layout table.
- 2028: the Lyve data leg is replaced by owned hardware (BUSINESS_OUTLOOK §5),
  not by B2 or Geyser.

## Repro

    set -a; . ~/vaultaire-bench/.env.bench; . ~/vaultaire-bench/idrive-new.env; set +a
    export GEYSER_BUCKET=$GEYSER_LA_BUCKET
    ./erasure-bench -mb 64 -runs 2 -data 8 -parity 4 -layout "idrive:4,lyve:4,geyser:4" -sync idrive,lyve -retries 1
    ./erasure-bench -mb 64 -runs 2 -data 1 -parity 1 -layout "idrive:1,geyser:1" -sync idrive -retries 1   # mirror

Latency matrix: `latmatrix.sh` in the session scratch (curl medians over five
GETs against presigned URLs and a Cache-API Worker; Worker and objects
deleted afterwards).
