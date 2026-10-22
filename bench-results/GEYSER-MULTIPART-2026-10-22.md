# Geyser multipart PUT — LA1 from SLC, 2026-10-22

Why: every Vault write reached Geyser as one `PutObject` stream (`putStreamed`, WP-R7-2),
while the 2026-07-29 aws-cli runs showed multipart parts in parallel far faster. This measures
Vail's multipart calls one by one, picks the shape, and checks the driver end to end.

Setup: `slc-vaultaire-01` → `la1.geyserdata.com`, the prod key and bucket, objects under the
throwaway tenant prefix `t-_bench-mpu/` (all deleted; 0 objects and 0 open uploads left).
Tool: `cmd/tools/geyser-put-bench` (`-raw` = the calls timed on the driver's HTTP/1.1
transport; otherwise through `drivers.GeyserDriver.Put` with a non-seekable pseudo-random body
and `engine.WithContentLength`, read back through `Get`, SHA-256 compared). Single runs unless
noted; Vail's per-connection speed varies by the minute.

## 1. What Vail charges per phase (`-raw`)

| Parts × size | Concurrency | Create | Parts (aggregate) | Complete | Overall |
|---|---|---|---|---|---|
| 4 × 16 MiB | 4 | 1.19 s | 2.19 s (31 MB/s) | 5.34 s | 8 MB/s |
| 16 × 16 MiB | 16 | 0.28–0.35 s | 1.3–1.5 s (185–203 MB/s) | 7.2–8.8 s | 26–30 MB/s |
| 64 × 16 MiB | 16 / 32 | 0.4–0.7 s | 4.7–6.6 s (164–229 MB/s) | **21.5–25.0 s** | 35–38 MB/s |
| 32 × 32 MiB | 16 | 0.98 s | 10.2 s (105 MB/s) | **31.4 s** | 25 MB/s |
| 16 × 64 MiB | 16 | 0.33 s | 1.94 s (**553 MB/s**) | 6.31 s | **125 MB/s** |
| 16 × 64 MiB | 8 | 0.58 s | 3.94 s (272 MB/s) | 7.40 s | 90 MB/s |
| 8 × 128 MiB | 8 | 0.55 s | 3.40 s (316 MB/s) | 6.10 s | 107 MB/s |
| 4 × 256 MiB | 4 | 0.51 s | 4.22 s (255 MB/s) | 5.51 s | 105 MB/s |
| 32 × 128 MiB | 16 | 1.87 s | 10.38 s (414 MB/s) | **33.8 s** | 93 MB/s |

- **CompleteMultipartUpload costs per part, not per byte**: 5–10 s for up to 16 parts whether
  the object is 64 MiB or 1 GiB; 21–34 s at 32–64 parts.
- **A part streams at 30–65 MB/s per connection**, faster for a bigger part (16 MiB ~12, 128 MiB
  ~40, 256 MiB ~64), and parts scale with concurrency.
- So: one wave of big parts. About `concurrency` parts, as large as the RAM budget allows.

## 2. Through the driver, end to end

| Size | Streamed (`off`) | 8 × ≤512 MiB, 2 GiB budget | 16 × ≤256 MiB, 4 GiB budget (the defaults) |
|---|---|---|---|
| 256 MiB | 4.3 s, **63 MB/s** | 6.0 s, 45 MB/s | 6.1 s, 44 MB/s |
| 1 GiB | 12.3–12.8 s, 84–88 MB/s | 12.4 s, 87 MB/s | 10.3–15.5 s, 69–104 MB/s (3 runs: 69, 94, 104) |
| 2 GiB | 22.6–23.4 s, 92–95 MB/s | — | 11.4–11.9 s, **180–189 MB/s** |
| 4 GiB | 46.5 s, 92 MB/s | 27.4 s, 157 MB/s | 14.0–20.9 s, **205–307 MB/s** |
| 8 GiB | 89.9 s, 96 MB/s | — | 32.3 s, **266 MB/s** |

Every read-back hash matched (256 MiB, 1 GiB, 4 GiB through `Get`; 2 and 8 GiB were write-only).

- **The streamed PUT is faster than on record** (24–35 MB/s in July–October): it ramps with
  the object, 30 MB/s at 64 MiB → 92–96 at 4–8 GiB. Likely the 2026-10-21 SLC TCP tuning
  (`project_slc_host_tuning`), not verified.
- Multipart loses at 256 MiB (Complete's 5 s floor), ties or edges ahead at 1 GiB, and is
  2–3× from 2 GiB. **Threshold 1 GiB.**
- **Vail takes a single PUT over 5 GiB**: the streamed 8 GiB PUT succeeded (the AWS limit is
  not enforced). Not a reason to keep streaming large objects, just no longer an open risk.
- Reads are the bottleneck now: `Get` at the default 8 range streams read 22–28 MB/s on every
  size (unchanged from 10-12 / 10-14). `GEYSER_GET_CONCURRENCY=16` is the next lever (41 MB/s
  measured 10-04).

## 3. What shipped (`internal/drivers/geyser_multipart.go`)

A Put of a known length ≥ `GEYSER_MULTIPART_MIN` (1 GiB) is cut into about
`GEYSER_PUT_CONCURRENCY` (16) parts of 64 MiB…`GEYSER_PUT_PART_MAX` (256 MiB), a power of two
(buffers pool per size), more parts only past 16 × 256 MiB. Each part is read whole under the
process-wide `GEYSER_PUT_BUDGET` (4 GiB = one upload at full width; a second waits for
buffers, never refused) and sent from memory, so the SDK re-sends a failed part alone. A body
short of its declared length is aborted, never completed; a failed upload is aborted on a
context detached from the caller's (30 s). Not the SDK's `manager.Uploader`: its first part is
a growing `io.ReadAll` (≈2× a 256 MiB part in garbage) and its abort runs on the caller's
context, which is usually why the upload failed.

Open:

- Eviction and restore of a **multipart** object on tape are not yet observed (all bench
  objects were deleted). Plant one with `-keep` and check it with `geyser-restore-probe` after
  ~35 h.
- A 50 GiB Complete (200 parts) is extrapolated, not measured: at ~0.5 s a part past 16, about
  1.5–2 min, inside the Complete class's 10 min.
