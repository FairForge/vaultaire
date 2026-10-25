# Geyser LA2: tape reclaim, restore proof, prod move (2026-10-22 → 10-25)

What the LA2 (`la2.geyserdata.com`) tape tests answered, what prod runs on
since 2026-10-25, and the viability verdict. Companion to
`GEYSER-MULTIPART-2026-10-22.md` (the multipart PUT bench on LA1). Console
facts come from the Geyser partner console API; S3 facts from aws-cli on
`slc-vaultaire-01` with the prod Geyser key.

## Setup

| Thing | Value |
|---|---|
| Test collection | `StoredLA2r1`, LA2, 1 cartridge (console `size` 1 → shows 18 once a tape is assigned), encryption off |
| Its tape | `160165L9` (LTO-9, 17,549,999,734,784 B usable) |
| Test buckets | `la2rot1-66bc4ddf…` (the 2 GiB / 16-part multipart canary, sha256 `cceac3bb…`) and `la2rot2-5fe33a4d…` (`rot-test/rot-256m.bin`) |
| Snapshots | hourly console reads of every collection's `tapeUsage` (`~/geyser-tape-tests/partner-snap-*.json` on the Mac) |

## Results

**Multipart restore works on LA2.** The 2 GiB / 16-part canary was `GLACIER`
by 2026-10-24 06:00 UTC, `RestoreObject` took 459 s, the read 68 s
(~32 MB/s), sha256 matched. (The same canary on LA1 never left `STANDARD` in
69 h: LA1's eviction is volume-driven and the bucket's lifecycle has no
transition rule.)

**T1 — a delete before the nightly flush never reaches tape.** ~60 GiB was
written and deleted on 10-22 before the ~11:00 UTC tape write; the tape
shows 2,747,269,120 B used: the 2 GiB canary, the 256 MiB object and
metadata. The landing zone absorbs same-day churn for free.

**T3 — deleting a bucket reclaims nothing on a shared tape.** `la2rot1` was
emptied (`delete-object --version-id null`) and deleted at 03:53–03:54 UTC
on 10-25. The tape's used bytes:

| Snapshot (UTC) | Used | Note |
|---|---|---|
| 10-25 03:52 | 2,747,269,120 B (2.559 GiB) | bucket just deleted |
| 10-25 05:59 | 2,771,386,368 B (2.581 GiB) | +23 MiB, metadata |
| 10-25 06:59 → 13:01 (seven reads) | 2,771,386,368 B | unchanged across the 11:00 flush |

The 2 GiB the deleted bucket held stayed on the cartridge. Within a
collection, deleted bytes are never freed (Geyser: compaction needs > 95 %
deleted across several tapes). T2 (empty the second bucket, watch again) was
skipped as redundant.

**Deleting the collection is the reclaim.** With both buckets gone the console
accepted `DELETE /tapeCollections/{id}` at 13:29 UTC (it refuses while a
bucket exists, and a bucket while an object exists); the collection and its
18 TB billing line disappeared at once. **The unit of reclaim is the
collection, so Vault capacity must be rotated between collections, never
managed in place** (the rotation-GC work package).

**Billing shape.** $1.55/TB-mo; a collection bills 18 TB from the moment a tape
is assigned; the account-wide minimum is 100 TB = $155/mo flat.
`encryptionRate` and `compressionRate` are $0.33/TB-mo each **outside the
minimum**. Prod's collection was created with encryption on and turned off
the same day: the console has no PATCH (405); a `PUT /tapeCollections/{id}`
with the full record (`name, size, datacenterId, customerId, dualCopy,
compression, encryption, color, icon`) flips the flag on the same id, nothing
else changes. Geyser-side encryption stays OFF: Vail holds the key, it covers
the cartridge only, and our SSE-S3 sealing is the control we attest.

## Prod since 2026-10-25 04:20 UTC

Collection `StoredVaultLA2` (`95ce0f4b-43fd-470e-bbec-45e84b9f7f8c`, LA2,
encryption off, no tape assigned yet), bucket
`storedvault-feb0cfb1-4ff6-436f-bcb5-7da9e1aca6c8`, proven with the prod key
(HeadBucket, PUT/GET/DELETE, lifecycle attached). `.env`:
`GEYSER_ENDPOINT=https://la2.geyserdata.com`, `GEYSER_GET_CONCURRENCY=32`,
`GEYSER_MULTIPART_MIN=128MiB`, `VAULT_PARITY_LEGS=lyve,permafrost,sync` (the
parity second copy on a leg we control). LA1's `Stored3` (20 TB) is kept and
never deleted. The restart surfaced the ephemeral-port collision recorded in
`docs/DEPLOY.md` § Host tuning.

**Vault stays dark** until the master-key ceremony and `GLACIER` made
sealed-only: tape is plaintext otherwise, and the first real write assigns the
tape and starts the 18 TB billing step.

## Verdict

Viable as the cold leg behind a second copy; never as the only copy.

| Condition | Status 2026-10-25 |
|---|---|
| Parity second copy on `lyve` | done |
| Prod on LA2, not LA1 (two S3 outages, closed to growth) | done |
| Master-key ceremony + `GLACIER` sealed-only | owner ceremony, then one PR |
| Collection-rotation GC | work package not written |
| Bulk restore throughput (many objects) | not run |
| Second site (LON1 / SP1) bench for dual-site | not run |
| Geyser answers: landing-zone protection, LA1 outage cause/SLA, dualCopy semantics | e-mail not sent |
