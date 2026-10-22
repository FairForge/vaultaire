# Host tuning check — 2026-10-22

**When:** 2026-10-22 01:38–03:10 UTC. **From:** `slc-vaultaire-01` (the prod box). **What:** the
follow-up of the 2026-10-21 host tuning (`docs/DEPLOY.md` § Host tuning, PR #835):

1. raw `bench-compare` (commit `d1aa987`) against iDrive Dallas (`us-central-1`), Lyve
   `us-west-1` and Wasabi `us-west-1`, two passes, then `systemctl start
   vaultaire-host-tuning.service`, then two passes more — the boot unit had been enabled
   but had never run under systemd (the settings were applied by hand on 10-21);
2. a fifth raw iDrive pass and a per-request trace of iDrive PUTs, because the first four
   passes showed iDrive's concurrent uploads below its 10-20 baseline;
3. the customer path of 10-21 again (`tierbench` + `ingestbench`, bench tenant, bucket
   `tuning-bench` on the iDrive primary; L1 = loopback to the active slot, L2 =
   `s3.stored.ge` through HAProxy), to check the one unexplained "after" number of 10-21
   (32 × 4 MiB on L1, 1,078 → 708 MB/s).

Raw files: `raw-2026-10-22-tuning/`.

## 1. Findings

1. **The boot unit works.** `systemctl start` exited 0 in the same second, no `refused`
   line, the rings already at 4096 so no resize (`carrier_changes` 4 before and after),
   the IRQ map identical (same hash), governor and EPP `performance` on all 32 threads,
   `/health` 200 (`step1-systemctl-start.txt`). What it cannot show is boot ordering —
   only a reboot does.
2. **Before vs after the unit = no change beyond backend noise**, as expected (it
   re-applied the settings already live). Every Lyve line within ~5 %; Wasabi within
   ~10 % except one 15.1 s request (§ 2); iDrive swings of ±50 % pass to pass in both
   halves.
3. **iDrive's dip was transient.** Its 32 × 4 MB upload measured 129 / 476 (before) and
   168 / 663 MB/s (after), with 6–7 s worst requests in the slow passes, against 689 on
   10-20 and 835 on 10-14. A fifth pass at 02:56 gave **763 MB/s** (worst request 599 ms),
   downloads 768 MB/s and 382 MB/s sustained. A trace of 640 × 4 MiB PUTs (16 pooled
   streams × 40, the prod pair, canary prefix) had 0 failures, 0 over 5 s, p50 144 ms,
   p99 590 ms, max 1.1 s, one remote address. No action.
4. **The NIC fix holds under load.** `rx_missed_errors` stayed at 1,156 through ~100 GB of
   bench traffic; `rx_dropped` grew ~0.5/s whether the box was idle or loaded — frames the
   stack discards, not ring overflow.
5. **The customer path matches 10-21 "after".** 32 × 4 MiB on L1: 875 MB/s (between 10-21's
   1,078 and 708 — that drop was noise). The first single-stream 64 MB PUT on L1 read
   79 MB/s; three repeats gave 155 / 142 / 206 (10-21 after: 189), 256 MB 344 / 331 / 340
   (329). Small operations unchanged (34 / 29 / 0.3 / 32 ms PUT / GET / HEAD / DELETE —
   HEAD from the cache, the rest bound by iDrive). No regression from the day's deploys.
6. **Against earlier raw runs** (same tool, same box): Lyve us-west-1 is at its best —
   4 KiB PUT 165 (10-14) → 74 (10-21) → 50 ms, 32 × 4 MB in 754 → 803 → ~918 MB/s, 64 MB
   GET 84 → 94 → ~120 MB/s; sustained flat at ~600 (Lyve's ceiling). Wasabi against its one
   earlier raw run (10-14): multipart 128 → ~280 MB/s, 32 × 4 MB in 281 → ~760 MB/s. The
   tuning (the governor helps single streams) may account for part of the Lyve
   single-stream gain; the small-op latency drop is Lyve's own.

## 2. Raw, before / after the unit

Two passes each side; p50 for small ops, MB/s for transfers ("32×4 MB" = 32 parallel 4 MB
objects, capped at 1 GiB per direction). Wasabi skipped the 60 s sustained cell (Wasabi
bills deleted bytes for 90 days) and ran in prod's dormant `vaultaire` bucket under
`bench/` (prod's pair may not create buckets); iDrive in a bench bucket created and
deleted for the run; Lyve in the pre-provisioned `vbench-lyve-us-west-1`.

| | iDrive b1 / b2 → a1 / a2 | Lyve b1 / b2 → a1 / a2 | Wasabi b1 / b2 → a1 / a2 |
|---|---|---|---|
| 4 KiB PUT p50 | 31 / 30 → 31 / 31 ms | 50 / 50 → 52 / 50 | 37 / 40 → 42 / 38 |
| 4 KiB GET p50 | 27 / 27 → 27 / 28 | 47 / 48 → 48 / 44 | 31 / 28 → 30 / 29 |
| 16 MB PUT / GET | 78 / 70 → 76 / 71 · 79 / 112 → 121 / 107 | 109 / 102 → 110 / 115 · 101 / 83 → 101 / 101 | 47 / 39 → 43 / 36 · 55 / 38 → 52 / 59 |
| 64 MB PUT | 112 / 73 → 98 / 89 | 125 / 144 → 142 / 140 | 55 / 57 → 53 / 51 |
| 64 MB GET | 121 / 92 → 83 / 40 | 112 / 128 → 114 / 123 | 56 / 47 → 64 / 63 |
| 256 MB multipart, 16 parts | 246 / 199 → 160 / 172 | 232 / 211 → 236 / 232 | 280 / 286 → 281 / 275 |
| 32×4 MB in | 129 / 476 → 168 / 663 | 918 / 919 → 919 / 913 | 774 / 749 → 753 / **69** |
| 32×4 MB out | 395 / 623 → 535 / 727 | 447 / 486 → 438 / 575 | 514 / 497 → 546 / 503 |
| burst 500 ops/s | 309 / 304 → 307 / 291 | 205 / 208 → 212 / 221 | 227 / 199 → 214 / 210 |
| sustained 60 s | 328 / 274 → 360 / 348 | 595 / 590 → 582 / 606 | — |

The Wasabi 69: one PUT of the cell took 15.1 s (the rest p99 443 ms) — the edge stall of
2026-10-17; the tool has no deadline of its own, while the driver re-sends a PUT that has
no answer by its `FIXED_BUCKET_PUT_TIMEOUT` (10 s for a 4 MiB body). iDrive's slow passes are the first of each pair, before and after. The Lyve
writer ladder (+59 % on the means) moved only in its short 8–64 writer steps; at 128
writers it was 996 / 936 → 942 / 1,093 ops/s. Per-pass tables: `raw-before-after-table.md.txt`.

iDrive pass 5 (02:56, after the unit): cold dial 83 ms, 4 KiB PUT / GET / HEAD 30 / 27 /
26 ms, 16 MB 72 / 56, 64 MB 99 / 95, multipart 16p 288, 32×4 MB 763 / 768, burst 313
ops/s, ladder 516 ops/s, sustained 382 MB/s.

## 3. Customer path, 10-21 before / 10-21 after / now

`check-customer-path-table.txt` has every cell; the repeats are in
`check-customer-path-repeats.jsonl.txt`. MB/s unless marked.

| | L1 10-21 before / after / now | L2 10-21 before / after / now |
|---|---|---|
| 4 KiB PUT / GET p50 | 34 / 30 · 35 / 31 · 34 / 29 ms | 35 / 31 · 34 / 29 · 34 / 29 ms |
| single PUT 64 MB | 86 · 189 · 79, repeats 155 / 142 / 206 | 147 · 217 · 105, repeats 125 / 87 / 210 |
| single PUT 256 MB | 297 · 329 · 327, repeats 344 / 331 / 340 | 442 · 185 · 221, repeats 285 / 344 / 350 |
| single GET 64 MB | 157 · 172 · 95, repeats 136 / 139 / 160 | 162 · 180 · 124, repeats 117 / 120 / 173 |
| multipart PUT 1 GiB | 262 · 225 · 252 | 237 · 234 · 225 |
| GET of the 1 GiB multipart | error (Lyve fault) · 126 · 154 | error · 119 · 161 |
| 32 × 4 MiB ingest | 1,078 · 708 · **875** | 974 · 1,108 · 780 |
| 16 × 64 MiB ingest | 1,040 · 1,058 · 958 | 1,051 · 1,096 · 782 |

This run's ingest cells were capped at 10 GiB (`-max-mb 10240`, 11–14 s) instead of 10-21's
~20 GiB, to halve the bytes the iDrive block carries until `retired_blobs` deletes them
24 h later; every one was 0 failed. The 10-21 "before" GET errors were Lyve's read fault
(`LYVE-READ-FAULT-2026-10-21.md`).

## 4. Run log

- 01:38 smoke of the three endpoints; Wasabi `CreateBucket` refused for prod's scoped user
  (`vaultaire-prod`), so Wasabi ran in `vaultaire` under `bench/`. Lyve ran with the root
  key (the bench bucket is not the scoped user's).
- 01:41–01:53 before ×2 (each backend ~2 min). 01:53:35 `systemctl start` (step 1).
  01:53–02:05 after ×2. A deploy switched slots at 02:05 (build 02:04:20) during the last
  Wasabi cell; raw benches do not go through the service.
- Cleanup: bench objects gone in all three buckets (the tool deletes as it goes), 0 open
  multipart uploads, iDrive bench bucket deleted, the env file shredded.
- 02:56–03:10 check: raw iDrive pass 5 (own bench bucket, deleted), the PUT trace
  (`cmd/tools/wasabi-stall -op put -size 4194304 -mode pooled -count 40`, 16 at once, bucket
  `vaultaire`, prefix `t-_probe/_probe/`, each object deleted), the customer path L1 then
  L2, then three repeats of the single-stream cells on each. `tuning-bench` left empty;
  `/health` 21/21, 0 error lines in the journal for the window.
- Box: 6 GB of 10-21 bench data deleted from the `/tmp` tmpfs (`s3dbench`, `readfault`;
  their small logs kept in `~/vaultaire-bench/tmp-keep-20261022/`).

Still open: a reboot to prove boot ordering of `vaultaire-host-tuning.service` (one box, a
1–2 min outage — a maintenance window); no alert fires if the unit fails at boot.
