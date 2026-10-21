# WP-VAULT-2 — the parity scrub (design, 2026-10-21; not built)

**Why now.** On 2026-10-21 Lyve us-east-1 answered HEAD 200 — right size, right ETag,
one version — and GET `500 InternalError` for every read of week-old chunk blobs, for
about 45 s in prod's view and ~20 min in a hand probe's, then read back clean
(`bench-results/LYVE-READ-FAULT-2026-10-21.md`). Nothing we run reads a parity shard
until the day Geyser fails: the `vault_parity` job writes four shards (a PUT 200, a
size check on the WebDAV leg) and the reconcile pass LISTS folders. `routing_truth`
samples head rows and chunks, never `vault_parity` rows — and until #837 it
asked HEAD only. A parity copy that cannot be read is no copy. The pricing page's
second-copy sentence rests on shards nobody has read since they were written.

**What.** A **scrub pass** inside the `vault_parity` job (same advisory lock, same leg
rules, same `job_runs.result`), scheduled like the reconcile: its own time slice,
**first** in the run when it is due, so a protect can never starve it (the 0.1 lesson).

## 1. The pass

- Cadence `VAULT_PARITY_SCRUB_EVERY` (default `6h`, 1h–7d; the result says
  `scrub_deferred` in between), slice `ScrubSlice` (5 min), `VAULT_PARITY_SCRUB_SAMPLE`
  rows per pass (default 20) and `VAULT_PARITY_SCRUB_MAX_BYTES` (default 2 GiB) — on the
  Sync leg at ~27 MB/s that is ≤ 80 s of reading; Warn-and-default for every knob.
- **Which rows:** `complete` rows in `(tenant_id, digest)` order from a keyset cursor
  `scrub_cursor` carried in `job_runs.result` (the reconcile's pattern), wrapping at the
  end — every row is visited in turn, no row is picked twice before the others; oldest
  `scrubbed_at` first would skew to the same stragglers on a leg that is down.
- **Per row, per shard** (`p0`–`p3` on the leg its `legs` record): a read end to end —
  `engine.RangeGetter` where the leg has one, else `Get` — hashed as it streams, bounded
  by the slice and `MaxBytes`. Verdicts: `ok` (size = `shard_bytes` × stripes, digest =
  the recorded one when there is one), `unreadable` (the leg answered the listing/HEAD and
  not the read: a 5xx, a body cut after the status line, a short body), `missing`,
  `corrupt` (a digest that does not match), `error` (no answer within the deadline, an
  open breaker — the whole pass is skipped on that leg, never a verdict).
- **Digests (migration, next free number): `vault_parity.shard_digests TEXT[4]`,
  `scrubbed_at TIMESTAMPTZ`, `scrub_failures INT NOT NULL DEFAULT 0`.** `protect` records
  each shard's SHA-256 as it writes it (a hashing tee on `tolerantWriter`); a row written
  before the migration has none, so its first scrub verifies size and readability only
  and RECORDS the digests it computed (the baseline, as `geyser-restore-probe` does) — a
  corruption that predates the first scrub is not caught, which is said in the note.
- **A transient is not a fault:** a shard `unreadable` on its first read is read once more
  after 30 s inside the pass; only the second failure counts. `missing` and `corrupt`
  count at once.

## 2. Re-protect

A row with a failed shard is made `partial` (attempts reset, the failed shard removed
from `legs`, `scrub_failures` + 1) under the job lock — exactly `markIncomplete`'s shape
— and the existing pending pass re-protects it: `protect` reads the object from Geyser
(a tape read ≈ size / 22 MB/s with 8 streams; an object on tape is `ErrArchived` and the
row stays `partial` until the restore lands, as today) and rewrites all four shards
(`clearPriorShards` erases the three good ones first; a one-shard rewrite would need the
object anyway — the parity of a stripe is a function of its data). A row that fails
re-protect `VaultParityScrubMaxFailures` (3) times in a row stays `partial` with
`last_error` (the attic's `partial` is already paged by the backlog gauge). Nothing is
deleted by the scrub itself except through that path.

## 3. Metrics and rules

`vaultaire_vault_parity_scrub_shards_total{outcome=ok|unreadable|missing|corrupt|error}`,
`vaultaire_vault_parity_scrub_rows_total{outcome=ok|reprotect|skipped}`,
`vaultaire_vault_parity_scrub_last_success_timestamp_seconds` (from `job_runs.result`),
`vaultaire_vault_parity_scrub_backlog` = `complete` rows with `scrubbed_at` NULL or older
than 30 d (read from the table on scrape, 1 min cache). Rules (`vaultaire-jobs.yml`):
`VaultParityShardUnreadable` (an `unreadable`/`corrupt`/`missing` shard in the last run —
warning; the Warn line names tenant, bucket, key, leg, shard, error), `VaultParityScrubStale`
(no scrub in 24 h while `complete` rows exist — warning), `VaultParityScrubBacklog`
(backlog > 0 for 48 h — info).

## 4. Tests (red first)

A stub leg that serves a shard, then answers 500 on it (unreadable → re-read → counted,
row `partial`, the next protect rewrites four shards); a shard whose bytes were changed
on the leg (corrupt by digest); a deleted shard (missing); a leg with an open breaker
(pass skipped, no verdict); the cursor and the byte budget (a 25-row table scrubbed over
two passes, nothing twice); a pre-migration row baselined on its first scrub; the
reconcile and the protect untouched by a scrub that overran its slice (the run's deadline
math as in `vault_parity_2b_test.go`); the series at 0 from boot and the rule file.

## 5. By hand until then

`cmd/tools/blob-verify -driver sync -tenant <id> -list <id>__parity -mode full` reads
every shard of a tenant through the real driver (sha256, size unknown → readability);
`-driver permafrost -list <id>__parity` the same on the fleet. 2026-10-21: `vault_parity`
holds 0 rows on prod (the bench tenant's vault objects were deleted), so there is nothing
to scrub today; the Sync and permafrost `_parity` folders are being walked for orphans.

## 6. Out of scope

Verifying a parity row against the DATA (a Geyser read per row): the attic's restore
semantics make that a restore per object; the scrub trusts the digest it recorded at the
write. A scrub of the hot copies (`idrive`, `lyve` whole objects) is `routing_truth`'s
read probe.
