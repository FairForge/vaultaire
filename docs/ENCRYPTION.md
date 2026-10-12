# Encryption at rest: keys, rotation, shredding

Operator runbook for server-side encryption (WP-R8-1, migration 084). Code:
`internal/crypto/tenant_keys.go`, `sse_s3.go`, `chunk_encryption.go`;
`internal/api/tenant_keys_admin.go`, `key_reencrypt.go`. Review note:
`docs/reviews/WP-R8-1.md`.

**State on prod (2026-10-12): `ENCRYPTION_MASTER_KEY` is not set.** Nothing is
encrypted and none of this runs until it is.

## The key hierarchy as built

```
ENCRYPTION_MASTER_KEY  (env, 32 bytes; never in the database)
 └─ wraps (AES-256-GCM, AAD = tenant ‖ version ‖ purpose) ─┐
                                                            ▼
tenant_key_versions  (one row per tenant per version; the newest encrypts)
 ├─ chunk_key_wrapped  32 random bytes ──► convergent chunk key = SHA-256(chunk_key ‖ plaintext hash)
 │                                          → AES-256-GCM per chunk (nonce from the key + the sealed bytes)
 └─ kem_seed_wrapped   ML-KEM-768 seed ──► per object: encapsulate → shared secret → HKDF → AES-256-GCM
```

- **Both tenant secrets are random and stored**, not derived from the master.
  That is what makes rotation and shredding real. Before 084, the chunk key
  was `HKDF(master, tenant, "v1")`: any process with the master could recompute
  it, every version was the same function of the master, and deleting nothing
  made it unreadable.
- **Every blob records the version it was sealed under.** A chunk records it
  on its index row (`global_content_index.encryption_key_version`, with a copy
  on each ref). A whole SSE-S3 object records it in its blob header (format
  `0x02`: `[0x02][version u32][KEM ct][nonce][ct+tag]`, header authenticated)
  and on `object_head_cache.encryption_key_version`. A read always uses the
  recorded version; a write always uses the newest.
- **Dedup across a rotation.** The same bytes uploaded after a rotation
  dedup onto the existing blob, sealed under the old version, and the new
  reference records that old version. The re-encrypt job moves the blob later.
- **Not encrypted:** multipart uploads stay plaintext until WP-R3-3; SSE-C
  uses the customer's key and never touches this; unencrypted chunks are in the
  shared `_global` dedup scope.

## Turning encryption on (prod)

1. Generate the key on the box and keep a copy offline (in the password
   manager, **not** in the repo). Without it every encrypted object is lost:
   `openssl rand -hex 32`
2. Add `ENCRYPTION_MASTER_KEY=<hex>` to `/opt/vaultaire/configs/.env`.
3. `sudo vaultaire-switch restart`, which restarts both slots zero-downtime.
4. Check the log: `encryption initialized: SSE-S3 … versioned tenant keys`
   and `tenant keys checked rows=0 unwrap_failures=0`.
5. What changes: **new** buckets get `sse_enabled` by default. Objects up to
   256 MiB in an SSE bucket, or sent with `x-amz-server-side-encryption: AES256`,
   are sealed whole. Chunked objects (above the chunking threshold, with the
   `chunking` flag on) are sealed per chunk in the tenant's own dedup scope.
   Existing objects are not touched: they stay plaintext until overwritten.
6. A tenant's first key version is created on its first encrypted write.

## Rotating one tenant's key

```
curl -X POST -H "Authorization: Bearer <admin JWT>" -H "Idempotency-Key: $(uuidgen)" \
  https://stored.ge/api/v1/admin/tenants/<tenant_id>/encryption-key/rotate
→ 200 {"tenant_id":"…","key_version":N}
```

- 404 `tenant_not_found`. 503 `encryption_disabled` means no master key on that
  slot. A retry with the same `Idempotency-Key` replays the answer and does not
  rotate twice. Audit row: `admin.key_rotate` with `key_version`.
- New writes use version N at once on the slot that answered, and within
  60 seconds on the other slot (its cached "current" number expires). A write in
  that minute records the old version, so it stays readable, and the job moves it.
- **Chunks:** the `key_reencrypt` job runs every 10 minutes and works through up
  to `KEY_REENCRYPT_MAX_GB_PER_RUN` (50) GB per run. For each chunk it reads
  the blob, checks its ciphertext hash, opens it under the old version, seals it
  under N, and writes it to a **new** key `_chunks/<tenant>/<hash>.k<N>`. It then
  swaps the index row under the chunk's lock and keeps the old address for 15
  minutes in `retired_chunk_blobs`, because a reader may have resolved it. A
  later run deletes it. Watch the backlog drain:
  `vaultaire_key_rotation_pending{kind="chunk"}` → 0, and
  `GET /api/v1/admin/jobs` → `key_reencrypt.result`. Rule:
  `KeyReencryptBacklogStuck`.
- **Whole SSE-S3 objects are not re-encrypted yet.** Each keeps opening under
  its recorded version. `vaultaire_key_rotation_pending{kind="whole"}` counts
  them and falls only as they are overwritten or deleted. Their re-encryption
  (write the new blob, then swap) lands with WP-R2-1. An in-place rewrite is
  the R2-05/R2-15 class of bug.
- The old version's key row stays (retired, `retired_at` set): it is needed
  to read whatever is still sealed under it. Deleting a retired version's row
  while anything is sealed under it destroys that data.

## Rotating the master key

No data is re-encrypted: only the wrapping of each tenant key row changes.

1. `openssl rand -hex 32` gives the new key. Keep the old one at hand.
2. In `.env`: `ENCRYPTION_MASTER_KEY=<new>` and
   `ENCRYPTION_MASTER_KEY_PREVIOUS=<old>`.
3. `sudo vaultaire-switch restart`. Each slot's boot re-wraps every row that
   opens only under the previous key. One Info line: `tenant keys re-wrapped
   under the new master key … rewrapped=N`, and
   `vaultaire_tenant_keys_rewrapped_total`. The second slot finds nothing left
   to do. Rows opening under neither key produce an Error line naming them
   (`tenant vN`), the gauge `vaultaire_tenant_keys_unwrap_failures`, and the
   critical rule `TenantKeysUnwrapFailing`. Stop there and find the right key.
4. Check `unwrap_failures=0`, then remove `ENCRYPTION_MASTER_KEY_PREVIOUS` and
   restart again (the boot check now reads `tenant keys checked … unwrap_failures=0`).
5. Destroy the old key only after the database backups older than the re-key
   (7 days on the box, 30 days off-box) have aged out: they hold rows wrapped
   under it.

A wrong master key never serves ciphertext: every encrypted read of an
affected tenant answers 500 (R8-04's rule).

## What shredding does and does not cover

An account erasure (`internal/api/deletion_runner.go`) deletes the tenant's
`tenant_key_versions` rows inside `account.EraseRows`, after the objects are
deleted and the backends swept. The `account.erased` audit row counts them as
`keys_shredded`, and the process that ran the erasure drops its cached copies.

- **Covered:** every byte sealed under those keys, wherever it still is. That
  includes a stale copy the sweep did not reach, a chunk blob in the shared
  `_global` container (dedup GC deletes it later), a retired chunk address, a
  backend's own versioning or backup, and a copy someone took of a blob.
  Without the key row it cannot be opened, master key or not.
- **Not covered:**
  - Plaintext objects. Everything stored before encryption was turned on,
    multipart uploads (until WP-R3-3), and unencrypted buckets are deleted,
    not shredded.
  - Database backups: the key rows are in every dump taken before the
    erasure (7 days on the box, 30 days off-box). Until those age out, the old
    master key plus an old dump can open the blobs.
  - Another slot's process memory: unwrapped keys are cached there for at most
    an hour and are not reachable through the API (no row routes to them).

## Where to look

| What | Where |
|---|---|
| Key rows | `SELECT tenant_id, key_version, created_at, retired_at FROM tenant_key_versions ORDER BY 1,2;` |
| Chunk backlog | `vaultaire_key_rotation_pending{kind="chunk"}` |
| Whole-object backlog | `vaultaire_key_rotation_pending{kind="whole"}` |
| Job | `GET /api/v1/admin/jobs` → `key_reencrypt`; `vaultaire_key_reencrypt_chunks_total{outcome}` |
| Old addresses awaiting delete | `SELECT * FROM retired_chunk_blobs ORDER BY retire_after;` |
| Boot check | `journalctl -u 'vaultaire@*' \| grep 'tenant keys'` |
