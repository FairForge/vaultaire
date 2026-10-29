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

## 7. Amendments (2026-10-29, before the build)

A review of this design against "Vault ready to sell" found four gaps. The build follows
the design above WITH these changes; where they disagree, this section wins.

- **A1 — budget by cycle, not by sample.** 20 rows and 2 GiB per 6 h is ≤ 8 GiB a day:
  at 10 TB of Vault one full pass takes ~3½ years, and `VaultParityScrubBacklog` (30 d)
  would fire for ever. Replace `VAULT_PARITY_SCRUB_SAMPLE` / `_MAX_BYTES` with
  `VAULT_PARITY_SCRUB_CYCLE` (default `30d`, 1d–180d): each pass reads
  `parity footprint × (pass interval / cycle)` bytes (the footprint is
  `SUM(shard_bytes × stripes × 4)` of `complete` rows, read once per pass), with a floor of
  one row and a ceiling of the pass's slice. The parity of an object is 1× its size, so
  10 TB on a 30 d cycle is ~333 GB/day, ~4 MB/s — nothing for Lyve (no egress fee).
  `VaultParityScrubBacklog` then means what it says: a row unscrubbed for longer than
  the cycle + 7 d.
- **A2 — prove the copy rebuilds, not only that the shards are unchanged.** Digests
  recorded at protect time match whatever protect wrote; a parity computed wrong passes
  every digest check. `protect` already reads the whole stored object to compute the
  parity, so it records `vault_parity.object_sha256` (the STORED bytes — ciphertext for a
  sealed object, so no key is needed and SSE-C works too). Each scrub pass takes ONE row
  (its own keyset cursor) and rebuilds the object from the four parity shards ALONE,
  through the same decoder the GET fallback uses, hashing the result against
  `object_sha256`. No tape read. Verdict `rebuild_ok` | `rebuild_mismatch` (→ re-protect,
  `VaultParityRebuildMismatch` warns) | `rebuild_error`. A row written before the
  migration has no `object_sha256`: its first protect after the migration records it
  (the scrub skips the rebuild check for it, `rebuild_unbaselined`).
- **A3 — re-protect must ask for the restore itself.** `vault_parity.go` has no
  `ErrArchived` handling: a scrub that makes a tape-resident row `partial` would wait out
  its attempts and stay `partial` for good, because nothing submits the restore. On
  `ErrArchived`, `protect` calls `engine.Restorer.RestoreObject` (Days 2, the default
  tier; Vail accepts Standard and Bulk alike), records `restore_requested_at`, and leaves the row
  `partial` WITHOUT spending an attempt; the next pass after the restore lands protects it
  (`RestoreAlreadyInProgress` = still waiting). A restore pending > 24 h is a note and
  `VaultParityRestoreStuck` (info). Geyser restores are free; LA2 measured 459 s for a
  2 GiB multipart object (2026-10-24).
- **A4 — audit the tape copy too (its own job, `vault_tape_audit`, weekly).** The scrub
  covers the second copy, never the first. Once a week: restore and read back ONE random
  vault-floor head row (keyset-random over `(tenant_id, bucket, object_key)`) plus the
  fixed canary, hash the stored bytes against `object_sha256` (A2) or, for a row without
  one, size + the backend ETag; outcome in `job_runs.result`,
  `vaultaire_vault_tape_audit_total{outcome}`, rule `VaultTapeAuditStale` (no `ok` in
  9 d) and `VaultTapeAuditFailed`. Promotes `cmd/tools/geyser-restore-probe` (#591) from
  a cron line to a job; the restored copy is never served or billed.

Migration (next free number): `vault_parity.shard_digests TEXT[4]`, `scrubbed_at`,
`scrub_failures` (§ 1) + `object_sha256 TEXT`, `restore_requested_at TIMESTAMPTZ`,
`rebuilt_at TIMESTAMPTZ`.

Out of scope still: a restore-gated parity leg (AWS Deep Archive, Azure Archive — a
shard there needs a 12–48 h restore before it can be read, so neither the GET fallback
nor this scrub can use it as written); decided when the Lyve promotion's end (~mid-2028)
forces the leg's next home.

Paired change, already built: **tape takes only sealed bytes** (`sealedOnlyClass` +
`coldBackendSealedOnly`, `GEYSER_ALLOW_UNSEALED`, gate `geyser_sealed`) — parity is
computed over the stored bytes, so sealing the object seals its second copy too.

## 8. Built (2026-10-29) — A1, A2, A3; A4 follows

Migration 118 (`shard_digests`, `object_sha256`, `digest_etag`, `scrubbed_at`,
`scrub_failures`, `rebuilt_at`, `restore_requested_at`, `restore_checked_at`; partial
index on complete rows' keys), `internal/api/vault_parity_scrub.go`, the protect's two
hashes (a tee of the source, a hash per shard writer) and its tape branch, five rules.
Where the build differs from § 1–3:

- **Cadence:** `VAULT_PARITY_SCRUB_EVERY` 30 min (not 6 h) in a 10 min slice (not
  5 min): with A1's budget a 6 h pass at 10 TB would read 83 GB, more than a slice reads
  on the Sync leg; 30 min keeps one pass to ~7 GB.
- **`legs` is kept on a re-protect** (§ 2 said the failed shard leaves it): the delete
  paths then still erase what is there, and the protect replaces the shard in place
  (`clearPriorShards`: same etag, same leg).
- **No re-protect cap:** a row that keeps failing is found once per cycle at most;
  `scrub_failures` counts it and the rules say so. A cap would leave it partial with no
  copy at all.
- **A baselined row has no `object_sha256`** until its next protect (its stored bytes
  are on tape; reading them to hash would be a restore per row), so it is not rebuild-
  checked before then. Prod held 0 `vault_parity` rows on 2026-10-21.
- **`changed`:** a row a delete or overwrite moved during the read (its `updated_at`)
  is no verdict.

**A4 built (`internal/api/vault_tape_audit.go`, its own PR):** the `vault_tape_audit`
job looks hourly and audits one object every 7 d: a random vault-floor head row whose
parity row is complete for its etag with `object_sha256` (≤ 1 GiB — read through SLC),
read from its RECORDED backend; on tape (`ErrArchived`) it asks for the restore (Days 1)
and keeps the object pending in `job_runs.result`, read on the first run after it lands,
an error after 48 h; an object overwritten or deleted meanwhile is dropped (`changed`).
The fixed canary of § 7 is left out: the random pick already reaches tape, and a
canary would need a write path of its own onto the vault floor. `nothing` (no
auditable object) counts as clean, so an empty attic never reads as a stale audit.
