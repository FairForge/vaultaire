# internal/packstore

Small immutable objects batched into large, content-addressed pack files on a
slow per-file backend, with the index in Postgres (migration 080). For batch
writers (jobs), never the synchronous S3 PUT path. First backend: `sync`
(Sync.com's encrypted WebDAV bridges, `internal/drivers/webdav.go`).

## Why (measured on prod's box against Sync's bridges, 2026-10-07)

- every file operation costs ~0.5–1 s (several encrypted Sync API calls);
  small writes/deletes are rate-limited per account (~15–21/s across 5
  bridges), large objects stream at 156 MB/s up / 450 MB/s down in 256 MiB pieces;
- a new folder costs ~1 s; 50,000 files per folder max; 248-character paths max;
- each bridge caches metadata — another bridge sees a new / overwritten /
  deleted file 1.5–30 s later (once 310 s): write only IMMUTABLE,
  content-addressed objects, never overwrite;
- a range read inside a large object costs ~0.8–1 s;
- **the bridge reports a wrong modification time for every file** (a file
  uploaded now lists as 1970-01-21): no logic here reads a backend modtime.
  Every age (intent grace, tombstone age) is a Postgres timestamp against
  `NOW()` — `TestGC_ABackendReportingAn1970ModtimeDeletesNoYoungPack`.

## Layout and format

- `<container>/<aa>/<sha256>.pack` (container `_packs`, `aa` = the first two hex
  of the pack's own sha256), driver calls under the reserved tenant
  `engine.ChunkAddressTenant` (`t-_global/_packs/…` on a fixed-bucket or the
  WebDAV driver — never a customer prefix, so the erasure sweep never walks
  it). 256 folders, created once each (the WebDAV driver caches its MKCOLs);
  `MaxPerFolder` (50,000) refuses a seal into a full folder (`ErrFolderFull`).
  On the WebDAV driver the file is `t-_global/_packs/<aa>/<sha256>.pack%o`
  (`%o` = the driver's object-leaf marker, `drivers.WebDAVLeafMarker`, since
  2026-10-07 — packs written before it are not listed, so GC and `Recover` see
  only marked ones; there were none on prod). The longest name is the
  71-character leaf, against Sync's 248 per name. `List`, `Get`, `Delete`
  take and return the unmarked `<aa>/<sha256>.pack` — the marker is the
  driver's business (`TestStore_OnTheWebDAVDriver` covers seal, read, GC
  compaction, `Recover` and orphan deletion through it).
- `format.go`: header `VLTPACK1` | members back to back | footer JSON
  `{"version":1,"members":[{tenant,key,offset,length,sha256}]}` | trailer
  (footer length u64 BE, footer sha256, `VLTPEND1`, 48 bytes). `ReadIndex`
  verifies header, trailer, footer checksum and member geometry. A pack is
  recoverable without the database (`Store.Recover`: download, check it hashes
  to its name, re-insert the rows a live member does not already have).

## Write path (`writer.go`)

`NewWriter` → `Add(ctx, tenant, key, size, r)` streams into a staging file
(`StagingDir`, default `<tmp>/vaultaire-packs`; never in memory); a member
that would pass `TargetSize` (256 MiB) or `MaxMembers` (4096) seals the pack
first (returned as `*SealedPack`). A member larger than `TargetSize` is
`ErrMemberTooLarge` (store it whole). `Flush` seals what is left. Seal:

1. footer + trailer appended, the file hashed → name;
2. **intent row** in `packs` (`sealed_at NULL`) BEFORE the upload — the
   orphan marker; an identical committed pack is not uploaded again (a pack is
   never overwritten); one in flight or being deleted is `ErrPackBusy`;
3. ONE `Put` with `ContentLength`, then the trailer read back at exactly
   `size-48` (retried — bridges see a new file late);
4. ONE transaction: every member row (replacing the live row of its
   tenant+key; a GC move only if its source row is still live), `live_bytes`,
   `sealed_at`. Retried on a unique-index race / deadlock with another writer.

Only after 4 is a member stored. A failure rolls the staging file back so
`Flush` can be retried (the writer reuses its own intent row); `Close`
abandons an intent best effort; GC expires the rest.

## Read path (`store.go`)

`Get` = one ranged request (`engine.RangeGetter`; else a whole GET with the
prefix discarded), length + sha256 verified as the stream ends (`ErrCorrupt`
from the last Read). `GetRange` reads an unverified window of a member.
`Exists`, `Delete` (tombstone, idempotent). A read whose pack vanished under
it (GC moved the member) looks it up once more.

## GC (`gc.go`, the `pack_gc` job in `internal/api/pack_gc.go`)

Idempotent: (1) retired packs: file then row; (2) intents older than
`IntentGrace` (6 h): retire, file, row; (3) files no row names (safe without
an age: the row is always written before the upload, and the listing is taken
before the rows are read); (4) rewrite a sealed pack with nothing live, live
bytes < `LiveThreshold` (50 %) of its size, fewer distinct member rows than
`member_count` (**the account erasure deleted them** — forced, whatever the
ratio), or a tombstone older than `TombstoneMaxAge` (30 d). Live members are
streamed out of the old pack in one GET into new packs, then the old pack is
retired (`retired_at`, so no writer re-uploads the name), deleted on the
backend, and its row deleted (members cascade). `MaxRewriteBytes` (4 GiB) per run.

## Tables (080)

`packs` (no tenant id — infrastructure, kept by the erasure; the bytes of an
erased tenant leave with the forced rewrite) and `pack_members` (tenant data:
`account.EraseRows` deletes them; export excluded as a placement ledger).

## Metrics

`vaultaire_packstore_{packs_sealed_total,pack_bytes_total,members_total,gc_rewrites_total,gc_packs_deleted_total,orphans_deleted_total,member_corrupt_total}{backend}`,
`vaultaire_packstore_member_read_seconds{backend}`.

## Not wired yet: the Vault parity leg `sync` (design)

The parity job (`internal/api/vault_parity.go`) encodes ONE stream and writes
its four parity shards concurrently through `leg.Put` (pipes), reads them with
`leg.Get`/`GetRange` in `Open`, erases them in `eraseShards` / `EraseTenant`,
and the sweep lists `_parity`. A pack writer is sequential per Writer, so the
leg needs:

1. `vaultParityLegs` gains `sync` behind a flag `vault_parity_sync`
   (default OFF, `flags_wiring.go` + `admin_flags.go`); the leg is chosen per
   object, so `vault_parity.legs[j]` records `sync` or `sync-pack`.
2. `protect`: for shard size < 16 MiB, encode into four staging files
   (bounded: 4 × 16 MiB on disk), then `Writer.Add(tenant, shardKey, …)` per
   shard; the job keeps ONE Writer per run and calls `Flush` before it marks
   rows `complete` (rows of members not yet sealed stay `partial`; a run that
   dies redoes them). Shards ≥ 16 MiB: a whole immutable object
   `t-<tenant>/_parity/<digest>/<etag>/p<j>` via `leg.Put` as today (the
   names are already content-addressed by etag; one folder per object — fine
   under 50,000 for a tenant's objects only if digests are fanned out:
   `<digest[:2]>/<digest>` — a change of `shardPrefix` for the sync leg).
3. `Open`: legs `sync-pack` → `packstore.Store.GetRange`/`Get` per shard.
4. `eraseShards` / `OnObjectDeleted` → `Store.Delete` (tombstone; bytes
   leave within `TombstoneMaxAge` or at the 50 % rewrite); `EraseTenant` →
   nothing extra (rows go with `EraseRows`, the forced rewrite removes bytes).
5. Tests: the parity fixture with a local driver named `sync`.
