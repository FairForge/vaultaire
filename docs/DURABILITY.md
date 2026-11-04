# Durability — the copies a byte has, and who reads them back

*The page a base layer sells: what Stored keeps of every byte, per tier, and the jobs that prove the copies are still there and still right. Every number here is held to the code by `internal/api/durability_doc_test.go`; change a constant or this page and the test fails until they agree. Operator detail: `CLAUDE.md` (the tables and jobs), `docs/reviews/WP-VAULT-2.md` (the parity scrub and the tape audit), `docs/reviews/WP-STRIPE-10.md` (the blob scrub).*

## The copies, per tier

| Tier (floor) | Where the bytes are | Second copy | Read back by |
|---|---|---|---|
| **Standard** | the primary (one S3-class vendor bucket per region, the vendor's own redundancy inside it) | a **mirror copy** on another vendor for a tenant with `edge_mirror` (`blob_copies`, kind `mirror`; the edge tees the body, the `blob_copies` job copies the rest) | the **blob scrub** (every blob, whole, once a cycle) and routing truth (a daily sample, the first 4 KiB) |
| **Vault** | tape (Geyser) — the four data shards of an RS 4+4 stripe are the object itself | four **parity shards** on another backend (`vault_parity`): any 4 of the 8 shards rebuild the object, so the parity alone is a complete copy | the **parity scrub** (every shard end to end once a cycle, one rebuild from the parity alone per pass) and the **tape audit** (one object restored from tape and hashed every 7 days) |
| **Chunked** (large Standard objects, deduplicated) | chunk blobs at one address each on the primary (`global_content_index`) | the mirror leg's copy of a codec chunk (`blob_copies`, bucket `''`) | the blob scrub (every ready chunk, verified by its own hash) |

What a vendor does inside its own bucket (replication, erasure coding) is its claim, not ours; what we count is what we can read back.

## The blob scrub (WP-VAULT-2 Part 2)

The `blob_scrub` job (`internal/api/scrub.go`) reads every hot blob **whole** on its recorded backend once per `SCRUB_CYCLE` and checks it against what its row recorded:

- **A whole object or a retained version:** the bytes' MD5 against `COALESCE(body_md5, a single-part ETag)`; a sealed object against the backend's own ETag of its stored bytes (`backend_etag`). A multipart ETag with no `body_md5`, or a sealed object whose stored bytes' MD5 was never recorded, is *unverifiable*: still read (so a blob that cannot be read or is gone is still found), counted, never a finding.
- **A chunk:** a plaintext chunk by its SHA-256 after decompression; a convergent one by its stored bytes' SHA-256; an edge-codec (`AES256-EW`) chunk opened under its recorded key version and bound by its keyed hash.

**Cadence:** each run reads the backend's footprint × the run's share of the cycle (the parity scrub's A1 rule), at least one blob; a cycle that finished early waits for the next. Unset `SCRUB_CYCLE` = the job is dark; the per-tenant flag `blob_scrub` (default OFF) decides who is in it.

**Never past the vendor's free egress:** before a backend's slice, the account's free egress this month (`egressFreeBytes`) less what was already spent (the vendor's month-to-date where it reports one, at least what this job and the blob copies read) bounds the slice. Nothing left = the backend is skipped (`vendor_allowance`), never read past it. A metered backend with no capacity run yet is not read.

**Findings and repair:** only what is not ok is written (`scrub_findings`, migration 125: mismatch, unreadable, missing, repaired). A bad blob with a ready mirror copy is rewritten from it at the same address after the copy's bytes match the copy row's MD5 and size as they stream — a copy that does not match is never written over the blob. The repair is its own finding, with the leg it came from. A blob with no second copy stays a finding for the operator.

**What the customer sees:** the bucket page says *"Every file here was read back and checked on &lt;date&gt;"* once a full cycle over every backend holding the tenant's bytes finished and nothing it found in the bucket is left unrepaired (*"…; N files were repaired from a second copy"* when some were); nothing while a cycle runs.

**Alerts:** `ScrubFailing` (any blob found wrong in the last hour) and `ScrubCycleStale` (a cycle running past 1.5 cycles) — `deploy/monitoring/vaultaire-scrub.yml`.

## The Vault's parity and the tape

- **Parity:** RS 4+4 over the object, 1 MiB per shard per stripe; the four data shards are the tape's, the four parity shards go to the first registered leg of `VAULT_PARITY_LEGS`. A GET whose tape read fails is rebuilt from the parity.
- **The parity scrub** (the `vault_parity` job's scrub pass): every parity shard of a `complete` row read end to end once per `VAULT_PARITY_SCRUB_CYCLE` (default 30 days), each checked against its SHA-256; one object a pass rebuilt from its four parity shards alone and hashed against the stored bytes' SHA-256. A failed row is re-protected.
- **The tape audit:** every 7 days, one object restored from tape and hashed against its row.

## What is not claimed

- A blob the scrub found wrong with no second copy is not repaired by Stored; the finding names it.
- The vendor's internal redundancy is not read by us and not counted here.
- An object's first cycle has not run until a cycle over its backend finished: until then the bucket page says nothing.
