# Edge relay baseline — a Worker moving bodies to Lyve and iDrive (2026-10-26)

*The "one honest unknown" of plan 40.10 slice 1, measured before WP-EDGE-1 Part A was built (Stage 1a Prompt 44). A throwaway Worker on the bench account (`stored-edge-bench.isaacv17.workers.dev`, deleted after), presigned URLs on Lyve us-west-1 (a temporary bucket, deleted) and iDrive us-central-1 (`t-_lab/edge-bench/`, deleted). Clients: the owner's Mac (a home line) and the SLC box. MB/s = bytes / 1e6 per second over the whole request, best of 2–3 runs. Scripts were the session's scratchpad (`presign.py`, `bench.py`, `par.sh`, the Worker); nothing is in the repo but this page.*

## The answer

**A Worker is fast only when no JavaScript touches the bytes.** The request body handed straight to the subrequest (`fetch(to, {body: req.body})`) moves at 42–54 MB/s per stream from SLC and scales with parallel streams (150 MB/s at 8). Any JS in the byte path — a `for await` loop, a `tee()` into a `DigestStream`, a `FixedLengthStream` — halves it to 24–33 MB/s. That is also why the 2026-10-04 bench's relay ran at 7–12 MB/s. Reads through a Worker are fine: a presigned GET streamed back ran at 111 MB/s from SLC and 45–57 MB/s to the Mac, with a better first byte than direct.

Direct from SLC to the backends for the same bodies: 138 MB/s (Lyve) and 101 (iDrive) single-stream, 600 MB/s at 8 parallel. So the Worker leg costs per-stream speed; it buys the bandwidth ceiling (40.10).

## Single stream, both clients

| client | backend | size | PUT direct | PUT relay (tee+MD5) | PUT echo (body dropped in JS) | GET direct | GET through the Worker | Worker colo |
|---|---|---|---|---|---|---|---|---|
| Mac | lyve | 4 MiB | 6 | 7 | 9 | 6 | 15 | DFW |
| Mac | lyve | 64 MiB | 33 | 16 | 19 | 12 | 45 | DFW |
| Mac | idrive | 4 MiB | 7 | 7 | 9 | 8 | 14 | DFW |
| Mac | idrive | 64 MiB | 38 | 16 | 19 | 12 | 57 | DFW |
| SLC | lyve | 4 MiB | 15 | 19 | 28 | 17 | 89 | SLC |
| SLC | lyve | 64 MiB | 138 | 26 | 33 | 119 | 111 | SLC |
| SLC | idrive | 4 MiB | 12 | 17 | 30 | 14 | 117 | SLC |
| SLC | idrive | 64 MiB | 101 | 26 | 33 | 122 | 103 | SLC |

The Mac's uplink (~19 MB/s into the PoP) is its own limit; the Mac rows say nothing about the Worker beyond "the PoP is nearer than SLC" (GET first byte 0.11–0.13 s through the Worker against 0.16–0.24 s direct). MD5 from the Worker equalled the local MD5 and the backend ETag in every relay run.

## How the body is handled, 64 MiB from SLC to Lyve (two runs each)

| Worker code path | MB/s (curl, whole request) | Worker's own ms |
|---|---|---|
| `for await (chunk of req.body)` (count only) | 30 / 30 | 1384 / 1457 |
| `req.body.pipeTo(WritableStream)` | 58 / 27 | 267 / 1638 |
| `await req.arrayBuffer()` (buffers 64 MiB) | 47 / 47 | 885 / 913 |
| `req.body.pipeTo(DigestStream MD5)` | 35 / 23 | 1131 / 1652 |
| relay: `tee` → DigestStream + FixedLengthStream → backend | 25 / 24 | 1749 / 1922 |
| **relay: `body: req.body` pass-through, no JS on the bytes** | **54 / 42** | 870 / 1139 |
| relay: `pipeThrough(FixedLengthStream)` only | 51 / 27 | 945 / 1468 |
| relay: `tee` → DigestStream, other branch pass-through | 24 / 25 | 1867 / 1802 |

The backend accepted the pass-through PUT with a `Content-Length` (no 411), so `FixedLengthStream` is not needed when the subrequest body is the request body itself.

## Parallel streams from SLC, 64 MiB each, pass-through relay vs direct

| streams | relay (pass-through) | direct to Lyve |
|---|---|---|
| 4 | 99 MB/s aggregate (2.70 s) | 323 MB/s (0.83 s) |
| 8 | 150 MB/s aggregate (3.57 s) | 600 MB/s (0.89 s) |

## Limits seen

- **100 MB request body on `workers.dev` too** (Workers Paid): a 128 MiB PUT is Cloudflare's HTML 413 before the Worker runs (once a 502 on the Lyve relay). The zone plan's limit of 40.10 holds on the bench route as well — parts stay at 64 MiB.
- When the Worker answers before reading the body (the broken-URL runs), curl still pushed 64 MiB into the PoP in 0.34–0.59 s (113–197 MB/s): the PoP's TCP accept is not the limit; the Worker's consumption of the body is.
- HTTP/1.1 against HTTP/2 from the client made no difference (echo 30–33 MB/s both).

## What this changes in WP-EDGE-1 Part A

1. **The Worker never touches bytes on the write path**: `body: req.body`, nothing else. No MD5 at the edge, no SHA-256, no `FixedLengthStream`. The "backend ETag against the Worker's streaming MD5" check of 40.10 is dropped — a Worker that is not in the byte path cannot corrupt it; integrity is the backend's own (TLS + its checksum of what it received) plus the client's `Content-MD5` when it sends one (signed into the presigned URL, enforced by both backends — the 2026-10-25 proof).
2. **`aws-chunked` bodies (aws-cli v2's CRC32 trailer) cannot be decoded in the Worker without paying the JS cost** (≈ 25 MB/s per stream). Part A either (a) passes them through to the origin (`{edge:false}` when `x-amz-content-sha256` starts with `STREAMING-`) and the docs tell aws-cli users to set `request_checksum_calculation = when_required`, or (b) decodes at 25 MB/s. Choose (a) in Part A; measure a decoder later if aws-cli traffic matters.
3. **`EDGE_MIN_BYTES`** (the owner-session addendum, default 4 MiB) stands: below it the origin is faster and the bytes do not matter.
4. **Per-customer expectation to document**: a single stream through the edge is 40–55 MB/s; eight parallel parts reach ~150 MB/s per client. A customer on a 1 Gbps line with `rclone --transfers 8` is near their line; a 10 Gbps box uploading to us is faster through SLC — which stays a host. The data node (Prompt 47) has no per-request ceiling; it is the answer for the few customers who need more than ~150 MB/s per client.
5. Reads (Part B): the presigned GET streamed by a Worker runs at the backend's single-stream rate (111 MB/s here), first byte better than direct. Nothing to change.
