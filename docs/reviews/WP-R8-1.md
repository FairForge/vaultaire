# WP-R8-1: stored-random, versioned tenant keys (and the chunked GET that tells the truth)

Stage 1a, Prompt 1 (2026-10-12). Part A = PR #681 (merged `748a905`). Part B =
this PR (migration **084**). Spec: `SYNTHESIS.md` (WP-R8-1 row),
`R8-crypto-dedup.md` R8-05. Runbook: `docs/ENCRYPTION.md`.

Both parts ship dark: prod has no `ENCRYPTION_MASTER_KEY` (re-checked
read-only below), so no key code runs there. Part A's GET behaviour applies to
every GET.

## The bug, seen first

**Key versioning was a label.** The tenant chunk key was
`HKDF(master, "vaultaire-salt-v1", "vaultaire-tenant-key:v1:<tenant>")`
(`keymanager.go`). `RotateKey` bumped an in-memory counter that nothing read.
Every chunk ref recorded `encryption_key_version = 1`, and decryption never
read a version. Probe on `origin/main` before this PR:

```
BEFORE: RotateKey -> v2; chunk ciphertext identical before/after rotation: true;
        tenant key = HKDF(master, tenant, v1), recomputable by any process with the master: true
```

`TestSecurity_KeyRotation` passed throughout, because it checked the
in-memory counter. Consequences:

- **No rotation.** No version ever existed apart from v1.
- **No crypto-shredding.** A derived key cannot be deleted.
- **A one-way door.** Once the master key is set and real data is encrypted
  this way, adding versions means re-encrypting everything.

The SSE-S3 seed was random but unversioned, and its wrap had no AAD (a seed
copied onto another tenant's row would have opened).

**The chunked GET lied in three ways (2b.5 review P2s, Part A).**

- Only the rows were resolved before the status line. A dead backend under
  chunk ≥ 2 committed a 200 and stopped the body short (20,307,449 of
  20,971,520 bytes in the red test).
- An unregistered backend name was a 500.
- A failure after the status line ended the body *cleanly* whenever the client
  had no Content-Length to count against (HTTP/2, a re-chunking hop): the
  client read `err == nil` at 19,664,550 of 20,971,520 bytes.

The GCI location cache had no TTL either: a row repointed by another process
was served stale until a restart.

## What was built

### Part A (#681)

- `CoreEngine.CanAsk(backend)` observes and never charges.
  - Unregistered → `ErrAllBackendsUnavailable` wrapping `ErrBackendNotRegistered`.
  - Breaker **open** → `ErrAllBackendsUnavailable`.
  - **Half-open passes** (deviation from the prompt): only a real request
    closes a breaker. On a backend whose only traffic is chunk reads (prod's
    `idrive` rows while the primary is `lyve`), refusing half-open would keep
    it refused after the keys are fixed. The half-open read is the probe, and
    if it fails, the body is cut.
- `handleChunkedGet` checks every distinct backend of the range-trimmed plan
  before any header → 503 + `Retry-After`.
- After the status line, a fetch/integrity/decrypt failure calls
  `abortResponse`. `loggingMiddleware` then panics with `http.ErrAbortHandler`
  *after* the access log, the bandwidth record and the log line have run (a
  bare panic in the handler would have skipped all three). Without middleware,
  it panics in place. Counted in
  `vaultaire_chunked_get_truncated_total{reason}`. A client that went away is
  not counted.
- `gciCacheTTL` = 5 min.

### Part B (this PR)

- **Migration 084**, additive only:
  - `tenant_key_versions (tenant_id, key_version, chunk_key_wrapped, kem_seed_wrapped, kem_public_key, created_at, retired_at)`.
  - `global_content_index.encryption_key_version` (the blob's version; NULL = unencrypted).
  - `object_head_cache.encryption_key_version`. Not in the prompt: without it
    `vaultaire_key_rotation_pending{kind="whole"}` cannot be read from the
    tables, because a whole object's version is inside its blob. WP-R2-1 will
    need it too.
  - `retired_chunk_blobs (backend_id, storage_key, dedup_scope, plaintext_hash, retire_after)`.
    `dedup_scope` and `plaintext_hash` are also beyond the prompt's schema;
    see the adversarial pass, item 1.
  - The index `idx_gci_scope_key_version` (encrypted rows only).
- **`crypto.TenantKeys`** (`tenant_keys.go`) replaces `KeyManager` (deleted
  with its tests and two benchmarks) and the key half of `SSEService`.
  - Each version holds a random 32-byte chunk key and an ML-KEM-768 seed,
    wrapped with AES-256-GCM under the master. AAD =
    `"vaultaire-tenant-key"‖0‖tenant‖0‖version‖purpose`, format `[0x01][nonce][ct]`.
  - `Current` creates v1 under
    `pg_advisory_xact_lock(hashtext('tenant_key_versions'), hashtext(tenant))`.
    This is the two-key form with its own namespace (the prompt's one-key
    `hashtext(tenant)` would share the jobs' lock space). Then
    `ON CONFLICT DO NOTHING` and a re-read: the loser never keeps its own
    secrets.
  - `Version(v)`: cached 1 h, 10k entries.
  - `Rotate`: max+1, `retired_at` stamped on the previous.
  - `CurrentVersion` reads past the cache. The current number is cached ≤ 60 s.
  - `Forget`; `RewrapAll`.
  - No HKDF compatibility path (prod has no encrypted bytes; a dev DB's
    encrypted chunks are unreadable).
- **Chunks** (`chunk_encryption.go`, `s3_chunked_put_pool.go`,
  `s3_engine_adapter.go`, `s3_large_get.go`, `gci.go`).
  - `EncryptChunkData(ctx, …)` seals under the current version and returns it.
  - The pool records it on the row (via `insertChunkSQL`) and on the ref.
  - A dedup hit (lookup, or the reused branch of `storeChunkLocked`) copies
    the **row's** version, exactly as it copies `ciphertext_hash`.
  - `resolveChunkDescs`: the row's version is authoritative, the ref's is the
    fallback.
  - `DecryptChunkData(ctx, tenant, version, …)`.
  - The convergent key and the R8-01 nonce are unchanged except for the
    tenant-key input.
- **SSE-S3 0x02** (`sse_s3.go`): `[0x02][version u32 BE][KEM ct][nonce][ct+tag]`.
  The 5-byte header is authenticated as AAD, so a v2 blob relabelled v1 fails.
  `SSEOverheadBytes` = 1121; a 0x01 blob opens as v1.
  - `EncryptBytes` returns the version, which goes onto
    `object_head_cache.encryption_key_version`.
  - The chunked upsert, the chunked copy and `upsertWholeObjectHeadRow` set it
    NULL.
  - `Current` replaced `EnsureTenantKey`.
- **Rotation**: `POST /api/v1/admin/tenants/{tenant_id}/encryption-key/rotate`.
  - Behaviour: admin JWT; the management idempotency middleware on that route,
    so a retried `Idempotency-Key` replays the answer and does not rotate
    twice; 200 / 404 / 503 `encryption_disabled`.
  - Audit row `admin.key_rotate` with `key_version`.
- **`key_reencrypt` job** (`key_reencrypt.go`): every 10 min, boot +4 min,
  30 min ceiling, `KEY_REENCRYPT_MAX_GB_PER_RUN` 50, `JobName`, an
  `onlyScope` test hook.
  1. **Retired addresses:** those past `retire_after` are deleted under the
     chunk's lock. If an index row names the address again, only the retired
     row goes. A failed delete keeps the row.
  2. **Re-encryption:** for each tenant with >1 version, rows below its newest
     (read past the cache), oldest first, until the budget. Under the chunk's
     session advisory lock: re-read the row → `chunkStore.get` → check
     `ciphertext_hash` → open with the recorded version → seal with the newest
     → `chunkStore.put` to `_chunks/<tenant>/<hash>.k<N>` → `UPDATE … WHERE
     storage_key = old AND ciphertext_hash = old` (+ `backend_id`, since the
     put lands on the primary) → retire the old address for 15 min in the
     same transaction → `InvalidateCache`.
  - Never a delete in the same run.
  - A chunk failure is a note. The run is `error` only when a scan fails.
  - Result in `job_runs.result`.
  - Whole SSE-S3 objects are **not** re-encrypted (WP-R2-1).
- **Master re-key**: `ENCRYPTION_MASTER_KEY_PREVIOUS` →
  `checkTenantKeysAtBoot`.
  - Rows that open only under it are re-wrapped: one Info line
    "re-wrapped", `vaultaire_tenant_keys_rewrapped_total`.
  - Rows that open under neither key: one Error line naming up to 50 rows,
    plus the `vaultaire_tenant_keys_unwrap_failures` gauge and the critical
    rule `TenantKeysUnwrapFailing`. Those tenants' reads are 500.
- **Shredding**: `account.EraseRows` deletes `tenant_key_versions`. The
  runner reports `keys_shredded` in `account.erased` and calls `Forget`.
  - `tenant_key_versions` is in `exportExcludedTables` ("key material").
  - `tenant_encryption_keys` (037) is read by nothing now. Its erase/export
    rows stay, and it goes with the D-12 drop.
- **Metrics/rules**: `vaultaire_key_rotation_pending{kind}` (read from the
  tables, 15 s cache), `vaultaire_key_reencrypt_chunks_total{outcome}`;
  `key_reencrypt` added to `PeriodicJobStale`; `TenantKeysUnwrapFailing`
  (critical), `KeyReencryptBacklogStuck` (warning, 24 h).
- **Docs**:
  - New: `docs/ENCRYPTION.md`.
  - Updated: root CLAUDE.md (env rows, the table entry), and the
    `internal/crypto`, `internal/api` and `internal/account` CLAUDE.md files.

## The same bug elsewhere: the audit

- **A short body that ends cleanly**: the plain GET's `io.Copy` (which
  carries the `parallel_get` reader), its native-Range `io.CopyN` (the error
  was ignored outright), `serveRange`, and `/cdn`'s two copies. All of them
  now `cutUnlessClientGone` (Part A, `TestPlainGet_MidBodyBackendFailureCutsTheConnection`:
  before, `read error <nil>` at 1 of 4 MiB).
- **A version assumed instead of recorded**: SSE-S3 (fixed: 0x02 + head
  column), chunk refs (fixed), the chunked copy (copies the source refs with
  their versions; the row stays authoritative). SSE-C uses the customer's key
  and has no version, correctly.
- **Found, not fixed (pre-existing, same class as R3-07): background writers
  store plaintext into `sse_enabled` buckets.** `generatedObjectWriter`
  (GDPR exports, access-log delivery, inventory reports) writes through
  `upsertWholeObjectHeadRow` with `encryption_algorithm = ''`. The `_exports`
  system bucket is created `sse_enabled` whenever a master key is set
  (`account_export.go` `SSEDefault`). So the day the master key goes on, an
  "encrypted" bucket holds plaintext GDPR exports. → WP-R3-3 (or its own
  small WP before the copy flips).

## Adversarial pass on my own code

1. **Could the retired-blob delete destroy a live blob?** Yes, in the first
   draft. The job retires `_chunks/<t>/<h>`, dedup GC sweeps the row (ref 0),
   then a PUT re-stores the chunk at the same plain key. The delete 15 min
   later would have removed the live blob. Fixed: `retired_chunk_blobs`
   carries scope+hash, the delete runs under the chunk lock, and it is skipped
   when an index row names `(backend_id, storage_key)`
   (`TestKeyReencrypt_RetiredAddressLiveAgain_IsKept`).
2. **Can the job "re-encrypt" to a stale version?** Another slot's cached
   current number could lag a rotation by 60 s. The job reads `CurrentVersion`
   past every cache and seals with `EncryptChunkDataVersion`, never `Current`.
3. **Two first writers on two processes** each generated secrets. Only the
   stored row is ever returned (re-read after `ON CONFLICT DO NOTHING`). 16
   writers over 2 stores in `TestTenantKeys_TwoFirstWritersAgree`.
4. **A concurrent GET during the swap** may hold the old desc (old key, old
   hash, old version): the old blob exists for 15 min and the old key version
   is never deleted. A GET holding the new desc reads `.k<N>`. Another
   process's cache is bounded by `gciCacheTTL` (5 min) < 15 min. 23–26 reads
   during the swap, 0 inexact.
5. **Dedup across a rotation**: the second PUT of v1 bytes must not
   overwrite the v1 row or record v2 on the ref (it would then fail to open
   under v2). The ref copies the row's v1, and the row is untouched.
6. **The SSE version field**: is it trusted blindly? It is bound as GCM AAD.
   A relabelled blob fails (test).
7. **A wrong master key** must never serve ciphertext. Chunked and whole
   objects both answer 500, live and in tests.
8. **The AAD binding**: a wrapped chunk key copied onto another tenant's
   row, or across purposes or versions, does not open (test).
9. **Part A's panic** would skip post-handler bookkeeping. That is why the cut
   is made in `loggingMiddleware`, after it; "behind the logging middleware"
   is one of the five client shapes tested.
10. **Not closed:** `KeyVersion` material stays in another slot's memory for
    ≤ 1 h after an erasure (no row routes to it). The re-encrypt job puts the
    new blob on the *primary*: a chunk on `lyve` moves to wherever the primary
    is (intended — placement is the engine's — but the row's backend changes).

## Tests

**Red before / green after.** (`crypto` tests can't compile on the old tree;
the probe above is their before.)

- **Part A:** `TestChunkedGet_LaterChunkOnDeadBackend_Is503BeforeHeaders`,
  `…UnregisteredChunkBackend_Is503NotInternalError`,
  `…MidStreamFailureCutsTheConnection` (5 client shapes incl. HTTP/2),
  `TestPlainGet_MidBodyBackendFailureCutsTheConnection`,
  `TestGCICache_EntryExpires`, `TestCanAsk_ObservesTheBreakerNeverCharges`.
- **Part B, crypto:** `TestTenantKeys_StoredRandomNeverDerived` (also never the
  old HKDF output), `_TwoFirstWritersAgree`,
  `_RotateAddsAVersionAndRetiresThePrevious`, `_WrappedSecretsCannotBeMoved`,
  `TestMasterKeyRewrap_PreviousKeyToCurrent`, `_UnwrapsUnderNeither`,
  `TestSSES3_V2BlobCarriesVersion_V1BlobStillDecrypts`,
  `TestSSEService_DecryptShreddedKey`.
- **Part B, handler level:**
  - `TestChunkedPut_RecordsKeyVersionOnRowAndRef_DedupHitCopiesIt` (before:
    the row column was NULL, every ref 1).
  - `TestChunkedGet_DecryptsWithTheRecordedVersion_AfterRotate`,
    `TestChunkedGet_RowKeyVersionWinsOverRef` (and fail-closed when both are
    wrong).
  - `TestSSES3_GetAfterRotate_UsesRecordedVersion`.
  - `TestKeyReencrypt_MovesChunksToCurrent_OldBlobRetiredThenDeleted`:
    budget = 1 chunk, then through the scheduler with 3 concurrent readers, a
    `job_runs` row, rows at `.k2`, old blobs kept in the grace and deleted
    after it, a fresh process reads.
  - `TestKeyReencrypt_RetiredAddressLiveAgain_IsKept`.
  - `TestKeyRotateEndpoint_AuditAndIdempotent` (503 / 403 / 404 / 200,
    replay, one audit row per rotation).
  - `TestMasterKeyRewrap_PreviousKeyAtBoot`,
    `…UnwrapsUnderNeither_ReadsFailClosed` (GET 500 for chunked and whole).
  - `TestErasure_ShredsKeys_LeftoverBlobUndecryptable`.
- **Rule file:** `TestJobRuleFile_…` now runs with a master key, so
  `key_reencrypt` is in the table, and the new series are on `/metrics`.
- **Removed:** `TestSecurity_KeyRotation` (green while rotation was a no-op),
  `keymanager_test.go`.
- **Gates:** `make lint` 0, gosec 0, `make test-integration` (own DB) green,
  incl. `TestSQLLiteralsMatchSchema`, `TestExportSectionsCoverSchema` and the
  `internal/account` schema walk.

## Live proof (local, 2026-10-12)

Local binary on a private DB migrated through 084, `STORAGE_MODE=local`,
`ENCRYPTION_MASTER_KEY` = A, driven with aws-cli. The chunking threshold is
fixed at 64 MiB, so the "multi-chunk object" is 70 MiB = **37 chunks**, not 3.

| Step | Result |
|---|---|
| PUT 70 MiB (chunked) + 1 MiB `--server-side-encryption AES256` | 37 GCI rows `encryption_key_version = 1`, head row of the SSE object `1`, one `tenant_key_versions` row |
| `POST …/encryption-key/rotate` ×2 with one `Idempotency-Key` | `{"key_version":2}` twice, ONE `admin.key_rotate` row `{"key_version": 2}`; unknown tenant 404 |
| PUT the same 70 MiB as `big2.bin`, the 1 MiB again, a new 3 MB SSE object | `big2.bin`: 37 refs → rows v1, ref v1 (dedup onto the v1 blobs); SSE objects v2 |
| GET all four | SHA-256 equal to the sources (`7f2a5bf0…`, `5286b238…`) |
| `vaultaire_key_rotation_pending` | chunk 37, whole 1 |
| `POST /api/v1/admin/jobs/key_reencrypt/run` | 202; `job_runs`: `ok`, 37 rows, `{"candidates":37,"reencrypted":37,"bytes":73400320}`; 37 rows v2 at `.k2`; 37 in `retired_chunk_blobs`; 74 blobs on disk; GETs exact; pending chunk → 0 (whole stays 1, by design) |
| grace expired (`retire_after` moved back), run again | `retired_deleted: 37`, 0 retired rows, 37 blobs on disk, GETs exact |
| restart with B + PREVIOUS = A | `tenant keys re-wrapped … rows=2 rewrapped=2 unwrap_failures=0`; `vaultaire_tenant_keys_rewrapped_total 2`; GETs exact |
| restart with B alone | `tenant keys checked rows=2 unwrap_failures=0`; GETs exact |
| restart with A alone (the wrong key) | Error line naming `v1, v2`, gauge 2, GET chunked and whole → **InternalError (500)**, never ciphertext |
| throwaway tenant: chunked + SSE object, one chunk blob copied aside, deletion scheduled and run (`POST /api/v1/admin/account-deletion`) | `ok`; its `tenant_key_versions` 1 → **0**; `account.erased` `keys_shredded: 1`; the copied-aside blob under master B: `tenant key version not found` (cannot be opened); the other tenant's objects still exact |

## Prod (read-only, 2026-10-12)

The box `vaultaire-slc`, `sudo -u postgres psql vaultaire`, with the SQL
passed as a file. `/version` = `6beeb56` (before #681's deploy).

| | count |
|---|---|
| `tenant_encryption_keys` rows | 0 |
| `global_content_index` encrypted rows | 0 (of 2,296) |
| `object_head_cache` rows with an `encryption_algorithm` | 0 |
| buckets `sse_enabled` | 0 |
| `tenant_key_versions` table | not yet (084 deploys with this PR) |
| `ENCRYPTION_MASTER_KEY` lines in `.env` | 0 |

## Decisions (taken here, or for Isaac)

1. **Set `ENCRYPTION_MASTER_KEY` on prod: recommended YES** once this PR
   is deployed. The one-way door is closed: versions exist, rotation moves
   chunks, shredding deletes the keys. **Before or on the same day:**
   - WP-R14-6 copy flips.
   - Multipart stays plaintext until WP-R3-3.
   - Generated objects (GDPR exports into `_exports`, delivered logs,
     inventory reports) are plaintext even in SSE buckets (the audit above):
     either fix that first (small) or say so in the copy.
   - Back up the key offline. Losing it loses every encrypted object.
2. Whole-object re-encryption rides with WP-R2-1 (next prompt).
3. `tenant_encryption_keys` goes with the D-12 drop migration.
4. The two #677 bypasses (`generatedObjectWriter allowSync = nil`;
   `placeObject`'s region branch) → WP-R6-1, with the region fan-out and
   `wasabi` target-only.
5. Carried unchanged: the 11 iDrive criticals (silence the ten regions, keep
   `idrive`; Isaac), `wasabi` target-only (→ WP-R6-1), the PG 16 drop on
   2026-10-13, the Sync.com sub-processor sentence, `gey48`.

Taken in code:

- Half-open passes the chunked preflight (see Part A).
- The `object_head_cache.encryption_key_version` and `retired_chunk_blobs`
  scope/hash columns.
- The two-key advisory lock namespace.
- Idempotency on the rotate route.
- The new blob goes to the primary.

## Not done / what I could not prove

- **No prod live proof by design**: the master key stays unset until Isaac
  decides.
- **Not proven on a fixed-bucket backend live**: the local server has one
  backend (`local`). The fixed-bucket shape is covered by the tests
  (`fixedBucketDriver`).
- **A 3-chunk object** was not possible without a code change (threshold
  64 MiB); 37 chunks instead.
- **The 15-min grace** was simulated by moving `retire_after`, not waited
  out.
- **Database backups** keep shredded key rows until they age out (7 days on
  the box, 30 off-box): stated in the runbook, not engineered around.
- **`make deadcode`** not run. `GenerateMasterKey(Hex)` has no product caller
  (tests only), as before.

## [YOU]

- After deploy: install the rule file on the box (`deploy/**` is installed by
  hand):
  `sudo -S sh -c 'install -m 0644 /opt/vaultaire/src/deploy/monitoring/vaultaire-jobs.yml /etc/prometheus/rules/ && systemctl reload prometheus'`.
  The path of the checked-out file depends on the box; see the next line.
- Decide the master key (Decision 1). If yes: `openssl rand -hex 32` on the
  box, keep the key in the password manager, put it in `.env`, then
  `sudo vaultaire-switch restart`, then look for `tenant keys checked … unwrap_failures=0`.

## Post-merge review (2026-10-12, the plan driver)

Both PRs read against the code on `main` (`a05bf41`), CI / Security / Deploy
green on both merge commits, prod `/version` = `a05bf411c91d`, the rule file
installed on the box. The targeted tests re-run on a private database
(`vaultaire_test_r81`) with `-race`: green.

**Every claim above holds.** Checked in particular: the red tests are at the
handler (`key_versioning_test.go` drives the chunked PUT/GET, the job, the
endpoint behind its middleware, the boot re-wrap and the shred); the job never
deletes in the run that retires (`deleteRetired` runs first and selects
`retire_after <= now`, a retirement is `now + 15 min`);
`TestChunkBlobCalls_GoThroughTheChunkStore` stays green; the wrap AAD is
`"vaultaire-tenant-key"‖0‖tenant‖0‖u32 version‖purpose` with two fixed
purposes (no ambiguity); the 0x02 header is bound as GCM AAD; a wrong master
fails closed and `unwrapVersion` falls back to `ENCRYPTION_MASTER_KEY_PREVIOUS`
at read time too; the `.k<N>` storage key is safe against the chunk move
(`chunkKeyIdentity` rejects a 67-character hash → `Unrecognized`, never an
orphan; a retired plain key whose row now points elsewhere is `Unrecognized`
too, never deleted); the Part A preflight skips an empty `backend_id` (legacy
rows keep the fan-out path); the idempotency cache keys on method + path, so
one `Idempotency-Key` on two tenants is `idempotency_key_reuse`, never a
replay. The six deviations taken in code are accepted.

**No P0/P1.** Two P2 and two P3, all handed to Stage 1a Prompt 2 (Part A):

- **P2-a — a chunked GET that outlives the 15-minute grace is cut.**
  `handleChunkedGet` resolves its plan once (`resolveChunkDescs`) and
  `fetchAndVerifyChunk` (`s3_engine_adapter.go:1763`) asks `chunkStore.get`
  for the address it resolved; a miss falls to the legacy prefixes and then
  errors — nothing re-reads the row. After the swap the old address lives
  `keyReencryptRetireAfter` (15 min). A GET still on its plan after that (a
  10 GiB chunked object at 10 MB/s is 17 min; a paced tenant at the 64 KiB/s
  floor needs 17 min per 64 MiB chunk once `egress_throttle` is on; prefetch
  is at most 4 chunks ahead) is cut at its next chunk — honestly, by Part A,
  but a download that worked before the rotation now fails. Item 4 of the
  adversarial pass covers other processes' caches (5 min < 15 min), not a
  stream in flight. Fix: on a miss at the resolved address re-read the row
  past the cache and fetch the new address once (same plaintext hash, so the
  integrity check is unchanged), and lengthen the grace to 1 h.
- **P2-b — the master re-key's boot check misses a row created after it
  ran.** `RewrapAll` runs once at boot; `Version()` opens a row under the
  previous key at runtime but drops `usedPrevious` (`kv, _, err :=`) and
  writes nothing back. The two-slot switch keeps the OLD slot (old master
  only) serving for up to `LONG_OP_TIMEOUT` (900 s) after the new slot's
  check ran; a first-time-encrypting tenant's `createFirst` on the old slot
  in that window wraps v1 under the old master. The new slot opens it through
  the fallback, so nothing is visible; the runbook's step 4 ("check
  `unwrap_failures=0`, remove PREVIOUS, restart") then leaves a row no key
  opens: that tenant's every encrypted read AND write (`Current` → `Version`)
  is 500 until PREVIOUS is put back. The runbook's "the second slot finds
  nothing left to do" is true only for rows older than the first slot's
  check. Fix: re-wrap lazily in `Version()` when the previous key opened the
  row (guarded on the old bytes, as `RewrapAll` does); run `RewrapAll` from
  the `key_reencrypt` job while `HasPrevious()`; runbook step 4 = remove
  PREVIOUS only after a run with it set reports `rewrapped=0` and the old slot
  is stopped.
- **P3-a** — `deleteRetired`'s liveness query (`WHERE backend_id = $1 AND
  storage_key = $2`) has no index on `global_content_index`: one sequential
  scan per retired address per run. Fine at 2,296 rows; an index in the next
  migration.
- **P3-b** — the worker's own finding: `generatedObjectWriter` writes
  plaintext into `sse_enabled` buckets (`_exports` is created `sse_enabled`
  when a master key is set). This is the one item that gates setting the
  master key; it is Part A1 of the next prompt.

**Decision 1 (the master key), the driver's verdict for Isaac:** YES, the day
Part A of Stage 1a Prompt 2 is deployed (generated objects encrypted), with
the WP-R14-6 copy flipped the same day and the multipart caveat stated
("multipart uploads are not yet encrypted at rest" until WP-R3-3), the key in
three places (password manager, the box's `.env`, an offline copy), and **no
tenant-key rotation before P2-a and P2-b are fixed** — both are rotation-time
risks, not key-setting risks.
