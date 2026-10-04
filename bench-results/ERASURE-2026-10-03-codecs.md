# Erasure and fountain codecs, measured — RaptorQ vs Reed-Solomon, 2026-10-03

Codec-only throughput (no backend, no network): what the CPU cost of each code is, so Phase 11
chooses on facts. Companion to the driver-level rehearsals (`ERASURE-2026-09-24.md`,
`ERASURE-2026-09-25-all-backends.md`, `ERASURE-2026-09-27-standard-shapes.md`), which measured the
*network* side: 107–211 MB/s reads across the real backends. Every decode below was SHA-256
verified against the payload.

**Tool:** `~/fairforge/ec-codec-bench/` (outside this repository on purpose: two of the RaptorQ
modules need Go 1.26 / 1.27rc1, which CI's Go 1.25 cannot build; the Go toolchain auto-downloaded
1.27rc1 for the run). Raw logs beside it (`ecbench-slc*.txt`, `ecbench-mac*.txt`, `rust-32k.txt`).
Random payload; Reed-Solomon decodes lose *m* data shards (the expensive case); fountain decodes
lose a random third of all symbols (a backend holding a third is gone) and reconstruct from the
survivors; `gor=N` is klauspost's goroutine count.

## Reference machine: SLC (Ryzen 9 9950X, 32 threads, 123 GB), `nice -n 10`

Reed-Solomon, `klauspost/reedsolomon` v1.14 GF(2^8) (the Phase 11 codec of record):

| Shape | 16 MiB enc / dec | 64 MiB | 256 MiB | 1 GiB | 1 GiB single core enc / dec |
|---|---|---|---|---|---|
| RS(8,4) | 21.2 GB/s / 2.7 | 21.5 / 2.8 | 20.4 / 2.9 | 19.5 / 1.6 | 19.3 / 1.8 |
| RS(10,6) | 28.2 / 2.2 | 22.7 / 2.2 | 19.4 / 2.6 | 19.6 / 1.3 | 18.3 / 1.5 |
| RS(12,6) | 59.7 / 3.2 | 25.6 / 2.5 | 19.8 / 2.7 | 21.2 / 1.5 | 12.6 / 1.5 |
| RS(12,12) | 41.8 / 2.8 | 15.7 / 2.7 | 14.0 / 2.4 | 14.1 / 1.4 | 5.1 / 1.2 |
| RS(20,10) | 66.9 / 2.9 | 21.9 / 3.0 | 20.1 / 2.6 | 16.2 / 1.5 | 7.8 / 1.5 |

Forced Leopard GF(2^16) (`WithLeopardGF16`) at these shard counts: encode 8–25 GB/s, decode 1.1–2.3 GB/s
— slower than GF(2^8), as expected; Leopard's O(N log N) wins only past a few hundred shards, where
klauspost switches to it on its own.

RaptorQ (RFC 6330), three pure-Go implementations, 32 KiB symbols, 60 % repair symbols generated,
decode from a random two thirds of all symbols (≈ 1.07 K):

| Implementation | 16 MiB (K=512) enc / dec | 64 MiB (K=2048) | 256 MiB (K=8192) | 1 GiB (K=32768) |
|---|---|---|---|---|
| `mstephenholl/graptor-q` v0.1.1 (SIMD kernels, Go ≥ 1.26, MIT) | 2.56 GB/s / 0.81 | 1.98 / 0.91 | 1.42 / 0.84 | 1.20 / 0.51 |
| `xssnick/raptorq` v1.6 (Go 1.25, MIT) | 1.42 / 0.97 | 0.74 / 0.50 | 0.59 / 0.47 | 0.59 / 0.46 |
| `takeyourhatoff/raptorq` (Go 1.27rc1, MIT) | 0.30 / 0.17 | 0.28 / 0.14 | 0.27 / 0.15 | 1.01 / 0.14 |

`google/gofountain` (Raptor R10, RFC 5053, 2016, Apache): **failed its own round trip** — hash
mismatch on every decode and an index-out-of-range panic at K = 8192. Unmaintained; not a candidate.

## Second machine: Apple M1 Pro (10 cores, 16 GB)

Same ordering. RS(12,6) 16–256 MiB: encode 16–19 GB/s, decode 1.5–3.0 GB/s; graptor-q: encode
1.0–1.5 GB/s, decode 0.2–0.9 GB/s; xssnick 0.6–1.0 / 0.3–0.8. **The 1 GiB column is memory-bound
on 16 GB** (RS decode fell to 95–1,000 MB/s, RaptorQ decode to 41–61 MB/s) — the SLC table is the
reference for large objects.

Rust reference `cberner/raptorq` 2.0.1 (`cargo bench`, release, M1 Pro, single core): at its
default 1,280-byte network symbols, encode 2.0–8.2 Gbit/s (250–1,020 MB/s) and decode 1.3–4.7 Gbit/s
(160–585 MB/s) for K = 10 … 50,000. At 32 KiB symbols: encode 3.9–5.2 Gbit/s (490–645 MB/s), decode
2.6–5.3 Gbit/s (320–660 MB/s) for K = 100 … 2,000. The best Go port (graptor-q) is on par with or
ahead of the Rust reference at storage-sized symbols.

## What the numbers say

1. **CPU is not the constraint for either code.** The slowest useful decode here (RaptorQ in Go, 0.5 GB/s
   at 1 GiB on SLC) is 3–5× the fastest network read the rehearsals ever saw (211 MB/s); RS decodes at
   1.2–3 GB/s and encodes at 14–60 GB/s. A 1 GiB object costs ≈ 50 ms to RS-encode and ≈ 0.7 s to
   RS-reconstruct from a six-shard loss on one core.
2. **Plan 11.4's premise is false:** "RaptorQ: better performance than RS for large objects". Measured,
   Reed-Solomon is 10–20× faster to encode and 2.5–3× faster to decode at every size up to 1 GiB, on
   both machines. The order reverses only past a few hundred shards, where Leopard/FFT Reed-Solomon
   (O(N log N)) and fountain codes both beat quadratic GF(2^8).
3. **What RaptorQ buys is a property, not speed:** it is rateless. Repair symbols are unlimited and
   interchangeable, so a backend can hold "40 % of the symbols" without a fixed shard position, a new
   backend can be given fresh symbols without re-deriving a layout, and ≈ K+2 of *any* symbols rebuild
   the object (RFC 6330: failure 10⁻⁶ at K+2 — not strictly MDS like RS, which needs exactly k of n).
   Neither code reduces **repair traffic** below k shards' worth; that needs regenerating or locally
   repairable codes (below).
4. **Edge reconstruction is feasible, not free.** At 0.5–0.9 GB/s single-core, a 64 MiB object
   reconstructs in ≈ 0.1 s of Worker CPU in Go-equivalent code (WASM will be 2–3× slower); the binding
   limit is the 128 MB isolate, so edge reconstruction is for objects under ≈ 40 MiB and for a
   container above that. The design of record stays: the hub reconstructs once, R2 serves the result
   (Phase 40.1). The one new use is a **DR read plane**: a Worker or container holding the shard map
   and read-only vendor keys can serve a hot miss when the SLC box is down (40.1(d)).

## Patents and licences (checked 2026-10-03)

- **RaptorQ / RFC 6330.** Qualcomm's IETF IPR declaration #2554 (2015-03-19) lists US 7,139,960,
  US 7,451,377, US 2009/0158114 (→ US 8,887,020), EP 1665539 and US 2011/0299629 (→ US 9,419,749).
  The two core systematic-Raptor patents **expired on 2024-11-18 and 2024-12-08** (priority
  2003-10-06). Two continuations are **still active: US 8,887,020 to 2028-11-09 and US 9,419,749
  (permanent inactivation in the decoder, which RFC 6330 §5.4 uses) to 2030-12-05.** Qualcomm's grant:
  for a device implementing RFC 6330 without a wireless wide-area standard, Qualcomm "will not assert"
  the listed claims — conditional on the implementation **fully** implementing the RFC, with a
  defensive-termination clause. For us: usable now through the non-assert as long as the library is
  RFC-compliant (all three Go ports claim RFC 6330 compliance); unconditionally free from 2031.
- **Luby Transform / Tornado (the Digital Fountain base patents):** US 6,163,870 and 6,081,909 expired
  2017-11-06; US 6,307,487 and 6,320,520 expired 2019-02-05 and 2019-09-17. LT codes and Tornado codes
  are free. Online codes (Maymounkov 2002) were abandoned for infringing exactly those — now free.
  Wirehair (O(N) fountain, BSD-3) and Leopard-RS (FFT Reed-Solomon over GF(2^16), BSD-3, Lin–Chung–Han
  2014 academic work) carry no known patent. Intel ISA-L erasure (BSD-3) likewise.
- **Locally repairable codes:** Microsoft's Azure LRC family is patented (the 2018 overlapped-LRC filing
  US 11,748,009 runs to 2038-06-01; the 2012 originals to the early 2030s). **Clay codes** (Vajha et al.,
  FAST '18; the MSR construction in Ceph's erasure plugin) cut repair traffic up to 2.9× and repair time
  3× with RS's storage overhead and are open — the repair-efficient choice for 11.5/11.6, not LRC.
- **Random linear network coding (RFC 8681, RFC 9407 Tetrys):** Code On Technologies holds the RLNC
  patents (MIT/Caltech 2003–2010 filings; the earliest reach term 2023–2026, continuations later). The
  IETF IPR index could not be fetched (HTTP 403 to the tool); not for us without a check.
- **Content-defined chunking and dedup:** the Data Domain content-based-chunking patent US 9,305,008
  runs to 2031-09-30 and NetApp's two-stage CDC US 10,866,928 to 2039; the Rabin fingerprint (1981)
  and LBFS-style CDC (2001) are prior art, FastCDC (2016) is an academic paper, and restic's Rabin
  chunker (ours, WP-R8-4) is in wide free use. Convergent encryption's Stac patent (1995) expired in
  2015. New and worth reading for 8.1/38: **Chonkers** (Berger, arXiv 2509.11121, Sept 2025, CC BY-SA)
  — CDC with *provable* strict bounds on chunk size and edit locality.
- **A dependency fact for the repo:** adding graptor-q or takeyourhatoff/raptorq to `go.mod` would
  raise the module's Go requirement past CI's 1.25 (D-24); xssnick/raptorq builds on 1.25.

## Backends: the interim-primary question

iDrive's problem is a key that answers 403 to GET/HEAD while HeadBucket succeeds — a console fix
(WP-R7-5 [YOU] 0), not a reason to move the primary. If an interim hot leg is wanted anyway:
**Backblaze B2, not Wasabi.** B2: $6.95/TB, no minimum retention, API calls free since 2026-05-01,
3× egress pool, free egress through Cloudflare (`cmd/tools/erasure-bench` has a `b2` leg; raise the
account's download cap first — the 09-27 run hit it). Wasabi: $7.99/TB since 2026-07-01, a **90-day
minimum** (a temporary role is billed three months regardless) and a 1× egress policy. Both are S3
legs the engine already speaks (`wasabi` and `b2` in `erasure-bench`).

## Recommendation (→ plan 11.1 / 11.4 / 11.5, decision F-11)

Reed-Solomon GF(2^8) stays the codec for every shape up to 32 shards. RaptorQ is not for speed; keep
11.4 only for the rateless property — a fan-out of many small repair symbols to spokes or edge nodes
(Phase 20/21) or a backend mix that changes without re-layout — and use `xssnick/raptorq` (builds on
Go 1.25) or graptor-q once CI is on 1.26+. For repair efficiency on the own fleet, Clay/MSR before LRC.
