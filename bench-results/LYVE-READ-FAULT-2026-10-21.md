# Lyve Cloud read fault, 2026-10-21 — scope, verification, verdict

**What was seen (prod, 13:48:55–13:49:39 UTC):** a bench run (`tierbench`, tenant
`tenant-14a623b16b3f7012`, bucket `tuning-bench`) read three chunked objects whose chunks
are deduplicated onto blobs first stored on Lyve on 2026-10-12 and 2026-10-14. Six chunk
GETs of five distinct chunks answered `500 InternalError` from Lyve (the SDK's two
attempts, then the engine's failover walk — three Lyve request ids per chunk), and the
chunked GET cut the connection (`vaultaire_chunked_get_truncated_total{reason="fetch"}`
= 4). A session probing by hand right after found **23 chunks** (of 778 referenced) that
answered HEAD 200 (size, ETag, one version, `ServerSideEncryption: AES256`) and GET 500 or
502, whole or 100-byte range, retried twice; three were written on 10-12, twenty on 10-14
(17:22 UTC). Example hashes `2cb93b8b…`, `f35642e5…`, `e7254720…`, `863f648d…`.

**Correction to the first report:** the blobs are in **`stored-us-east-1`**, not
`stored-us-west-1` — prod's `LYVE_REGION` is `us-east-1` (the region whose PUTs stalled
on 2026-10-17). Every HEAD of the chunk keys in `stored-us-west-1` is 404.

**The chunk address:** `t-_global/_global/_chunks/<plaintext hash>`; index
`global_content_index` (`backend_id = 'lyve'`: 34,041 rows, 33,257 marked for deletion,
784 live; 778 referenced by `tenant_chunk_refs`, all by the bench tenant). Prod holds no
customer objects (`tenants`: Bench, Bench2, Stored).

## 1. Read-verify of everything on Lyve (read-only, root key from the box)

Tool: `cmd/tools/blob-verify` (new, this investigation): per key a HEAD, then a GET of
the whole body hashed as it streams and compared with the ETag (every chunk blob is a
single-part object, so every ETag is an MD5); SDK attempts 1, the tool's own 3 attempts
with 1/4/16 s between them, every failed attempt recorded with status, code and request id.
From `slc-vaultaire-01`, 14:21–14:35 UTC.

| Bucket (region) | Keys | Bytes read | Outcome | Transient failures |
|---|---|---|---|---|
| `stored-us-east-1` chunk prefix (every index row, live + marked) | 34,041 | 70.36 GB at 265 MB/s | **34,041 ok**, MD5 = ETag on every one | 0 |
| `stored-us-east-1` every other key (2025 bench litter, probe canary) | 2,072 | 8.89 GB | 2,072 ok | 0 |
| `stored-us-west-1` (us-west-1) | 403 | 1.56 GB | 403 ok | 0 |
| `vaultaire-bench-2026` (homed US-EAST-1) | 1,610 | 0.24 GB | 1,610 ok | 0 |
| `vbench-lyve-<region>` ×7, `lyve-lifecycle-canary-20260927` | 0 | — | empty | — |
| `vaultaire-worm-us-central-2-1755439655294` | 1 | 10 B | ok | — |

Nothing on the account is unreadable at 14:21–14:35 UTC. The four hashes the journal
names read back with the prod's **scoped** key (`vaultaire-prod`) too, through both the
us-east-1 and the us-west-1 endpoint, MD5 = ETag (14:29 UTC). So the fault was not the
key, not SSE, not the endpoint, and not the bytes: **a transient read-side fault on
Lyve us-east-1 of roughly 45 s in prod's view (13:48:55–13:49:39 UTC) and ~20 min in the
hand probe's**, on objects written a week earlier, which cleared by itself.

By write hour of the 34,041 blobs (none unreadable): 10-12 14h 123, 10-14 03–05h 665,
10-14 17–18h 907, 10-15 96, 10-17 320, 10-18 30,819, 10-19 1,111. Sizes 0–11 MiB.

**Not found:** any pattern by date, hour, size, multipart or SSE — because nothing is
unreadable any more. The 23-chunk set of the hand probe is not recoverable from the
journal (the probe ran outside prod); prod's own failures name 5 chunks: `2cb93b8b`
(three objects), `863f648d`, `f35642e5`, `e7254720`.

**Vendor status:** status.lyvecloud.seagate.com and lyvecloud.statuspage.io say
"No incidents reported" for every day 10-07 → 10-21; status.wasabi.com (Wasabi bought
Lyve Cloud in April 2026) lists nothing for US-East-1 / Virginia in the window.

**Earlier on this account:** `vaultaire_driver_put_retries_total{driver="lyve"}` rose by
7 on 2026-10-17 18:00–19:59 UTC (the stall probe day) and never since; the 2026-10-18/19
"object integrity" lines naming lyve are the Prompt 13/15 integrity proofs (buckets
`p13int-*`, `p13par-*`, `p15-a4v-*`), deliberate.

## 2. The other backends' blobs (read-only, prod's keys from `/opt/vaultaire/configs/.env`)

| Backend | What | Result |
|---|---|---|
| wasabi | every chunk row (1,146, all marked for deletion), full GET + MD5 | 1,146 ok, 0 transient |
| idrive (Dallas, the original reseller account) | the 501 head rows of tenant Bench2 (2026-05-12), full GET | 501 **missing** (404) — known: blobs of the 2026-10-04 account this account never held (CLAUDE.md § Entry Point); chunk prefix `t-_global/_global/_chunks/` lists 0 keys (every chunk row is on lyve or wasabi — dedup hit the Lyve-era blobs); the probe canary ok |
| geyser | the bench tenant's prefix on the tape bucket (2 objects) | both on tape: `403 InvalidObjectState` to a 4 KiB range — the archive state, not a read fault |
| blomp | the head rows on it, full GET through the driver | 2 ok (1 MiB each, sha256 streamed); 8 "missing" were objects the Blomp bench deleted between the index dump and the read (their head rows are gone too) |
| sync, permafrost (parity shards) | `<tenant>__parity` of all three tenants through the drivers | empty on both legs (`vault_parity` holds 0 rows; the reconcile has removed the orphan folders) — nothing to scrub today |

## 3. Is it still happening? (fresh writes, § 3b below)

Fresh objects written 14:40 UTC into `vbench-lyve-us-east-1` and `vbench-lyve-us-west-1`
(prefix `readfault-1792593231/`: 20 × 4 KiB, 20 × 1 MiB, 20 × 2 MiB, 5 × 16 MiB, 2 × 64 MiB as
single PutObject; 2 × 256 MiB and 1 × 1 GiB as multipart in 16 MiB parts; random bytes, the
local MD5s in `/tmp/readfault/md5.txt` on the box), read back **at once**: 70 keys per bucket,
every single-part object MD5 = ETag, every multipart object's download MD5 = the local
file's, 0 transient failures (1.89 GB per bucket: us-east-1 read back in 28 s at 67 MB/s,
us-west-1 in 8 s at 234 MB/s — the region, not a fault). **Not happening at 14:4x UTC.** Re-read at **14:55 UTC** (a quarter hour after the
write): 70/70 ok in both regions, 0 transient failures (us-east-1 158 MB/s, us-west-1
190 MB/s). The objects are left in place for a re-read after some hours
(`blob-verify -prefix readfault-1792593231/` on each `vbench-lyve-<region>` bucket with
the bench root key; delete them after).

## 4. Remediation of the bench tenant's "23 broken chunks"

**Nothing to delete.** Every chunk reads back with MD5 = ETag; the rows are live, the
references intact. The objects that referenced them (`tuning-bench/tb-1792590510/*`,
`tb-1792590546/*`, and `idb-std/tb-1792537129/*` of 10-20) were deleted by the bench
itself; their `tenant_chunk_refs` rows (2,466) are still there because a chunked
DELETE retires the manifest for 24 h (`retired_blobs`, backend `chunked`, reason
`deleted`: 18 rows) and the `retired_blobs` collector releases the references then —
the documented design, not a leak (CLAUDE.md § `global_content_index`). Had the fault
lasted, the path would have been the S3 API as the bench tenant (`DeleteObjects` of the
affected keys releases refs, quota and the manifest in one transaction), never SQL.

**What this changes in the code (follow-ups, each its own PR):**

- `routing_truth` reads the first 4 KiB of every sampled row and chunk after a present
  HEAD and reports `unreadable` apart from `missing`, with
  `vaultaire_routing_truth_last_run_unreadable{backend}` / `_chunks_unreadable` and the
  rules `RoutingTruthUnreadable` / `RoutingTruthChunksUnreadable` (#837).
- The Vault parity scrub: `docs/reviews/WP-VAULT-2.md` (design; not built).
- `cmd/tools/blob-verify`: the end-to-end read of every blob of a prefix or a tenant
  (this note's numbers), `cmd/tools/README.md`.

## 5. Seagate support ticket — DRAFT, not sent

> **Subject:** Lyve Cloud US-East-1: objects answering HEAD 200 and GET 500 InternalError
> for ~45 s on 2026-10-21 (customer v01)
>
> Account: customer **v01** (reseller `global`), root user isaacv17@gmail.com; the
> requests below were signed by IAM user `vaultaire-prod`.
> Bucket: **`stored-us-east-1`** (homed US-EAST-1, SSE-S3 default), endpoint
> `https://s3.us-east-1.global.lyve.seagate.com`, path-style, SigV4.
>
> Between **2026-10-21 13:48:55 and 13:49:39 UTC** every GetObject of these keys
> answered `500 InternalError` ("We encountered an internal error, please try again")
> while HeadObject answered 200 with the correct Content-Length, ETag, VersionId and
> `x-amz-server-side-encryption: AES256`:
>
> | Key (under `t-_global/_global/_chunks/`) | Size | ETag | Written (UTC) | Request ids of the 500s |
> |---|---|---|---|---|
> | `2cb93b8b936b9be5c7f4f9519d2b2e941cc5afd6b429d56188d4fdb11147ba12` | 1,354,324 | f8c7c72362371bb38aab933552f80491 | 2026-10-14 17:22:38 | b29178525edc044b7147b4787223d17f, 17761e1abedb5095056307e1b1e02f65, 2d8a63b3a3f900ecb5964c7d6bdc69fa, b5864f505f94629d08bac40c364fe763, f6a02f030f6c4d0bf85a1695394a362f, 4e7186560c26df06c80b940b884ed58d, 9eaefbf312d39d5bf3619cff636d4cb9, 09d8149df9b9a4686119b641dbba49d4, ebffeee2ab770ab0628087a04619ea30 |
> | `863f648d6fd7c5e07a7ebb0f9416e42ce6235c37c1f9d932d6b3441cb439caef` | — | dc2691ed9c9bb3f679f7990e0275b5af | 2026-10-14 | 91dba094d95c1b6647eb45d0e1c0fa1d, db5981f6a7aa7507e988e990f16b878f, a6434b0e1310d8431bb3fa79a8f3ad80 |
> | `f35642e51cc5c3cba80dc34e97a04381121321f8e23ee5b7e597de4423fac533` | 2,259,632 | 75a34da27805750e709e3695579b4fa5 | 2026-10-14 17:22:29 | b3f74cc2599fc2fec0670a923ec1bc60, be057479d7aaa8c85ed5e727f3aaef07, a5ad53534311fe99b4e814530ecfd269 |
> | `e7254720384aadb2f09faa1f67352c4d9dd3ff12ad9d67551d5dd0a6774fa7c9` | — | 0e0c59874b1a9ab0827f1726909f0622 | 2026-10-14 | 7d898143ccbc027ba2f3011a6cd22715, a54dcf4ea897b85990be7b8255b436be, 5e6e2f7fcd13803bbedcb7ab12618336 |
>
> A manual probe between ~13:50 and 14:10 UTC found 23 keys under the same prefix in that
> state (GET 500, sometimes 502 Bad Gateway, whole or `Range: bytes=0-99`, retried
> twice), all written 2026-10-12 or 2026-10-14, plus ~4 % of random GETs on the prefix
> answering a transient 502 that succeeded on retry. By 14:21 UTC every key read back
> correctly (34,041 objects, 70 GB, MD5 = ETag on each), with the root key and the scoped
> user, through the us-east-1 and the us-west-1 endpoint. No deletes, overwrites or
> lifecycle actions of ours touched these keys; your status page shows no incident.
>
> Questions:
> 1. Was this a known incident in US-East-1 at that time (a storage node, a
>    metadata/data split, a KMS/SSE service)? What was the root cause and the window?
> 2. Was any data at risk of loss, or were the objects only temporarily unservable?
> 3. Is the 500 on GET with a 200 on HEAD the expected failure shape when a data node
>    is unavailable? We would prefer a 503 with Retry-After so clients retry.
> 4. Were other customers in the region affected? Will a post-incident report be
>    published on status.lyvecloud.seagate.com (or status.wasabi.com)?
> 5. Is there anything in our request pattern (a few hundred 1–4 MiB GETs/s from one
>    client on one prefix) that contributes?

## 6. Is Lyve still safe?

**(i) As the free Vault parity leg — yes, with a scrub.** The parity holds four of eight
shards; a leg that holds bytes it cannot serve for a minute is no worse than a leg
that is down for a minute, and the rebuild reads any four shards that answer. What this
fault proves is that nothing READS a shard between the write and the day Geyser fails,
so a leg could hold unservable shards for months unseen: build WP-VAULT-2 before
pointing the leg at Lyve (today it is `sync`; `vault_parity` names lyve on 0 rows).

**(ii) As a Standard primary — not alone.** A 45 s HEAD-200 / GET-500 window on
week-old objects with no vendor incident posted, on the region that stalled ~3 % of
PUTs on 2026-10-17, is a service-durability gap on top of the contract's "no durability
warranty" (lyve_README § 2). It is acceptable as a fail-over leg, a region for EU
residency reads, or behind a second copy; it is not where the only copy of a customer's
object should live. iDrive stays the primary; `lyve` stays registered for its rows.
