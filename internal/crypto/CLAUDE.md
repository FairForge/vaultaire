# internal/crypto

Encryption, key management, and post-quantum cryptography for Vaultaire.

## Key Files

- **ssec.go** — SSE-C (customer-provided keys): stateless AES-256-GCM encrypt/decrypt with customer's 32-byte key. S3 header parsing (algorithm, key, MD5 validation). No DB, no key storage
- **sse_s3.go** — SSE-S3 service: ML-KEM-768 key encapsulation + AES-256-GCM data encryption. Per-tenant keypairs in DB, per-object DEKs via KEM encapsulation
- **chunk_encryption.go** — `ChunkEncryptionService`: per-chunk convergent encryption (AES-256-GCM with HKDF-derived deterministic nonce). Same tenant + same content → same ciphertext (dedup-safe). Ciphertext format: `[nonce 12B][GCM ciphertext+tag]` (28B overhead)
- **encryption.go** — Core Encryptor interface: AES-256-GCM, ChaCha20-Poly1305, Noop implementations. Chunk-level encrypt/decrypt for pipeline
- **keymanager.go** — Multi-tenant HKDF key derivation with version tracking and TTL cache
- **postquantum.go** — ML-KEM-768 via cloudflare/circl (pipeline encryption). SSE-S3 uses Go stdlib crypto/mlkem instead
- **compression.go** — LZ4/Zstd/Snappy compression with auto-detection
- **chunker.go** — Content-defined chunking (FastCDC)
- **pipeline.go** — Full encrypt+compress+chunk pipeline
- **tls.go** — TLS configuration helpers
- **gci.go** — Global content index for deduplication
- **config.go** — Crypto configuration types

## SSE-S3 Architecture (Phase 5.14.4)

**SSEService** provides transparent server-side encryption at rest:

- **Key hierarchy**: ENCRYPTION_MASTER_KEY (env, 32B hex) → encrypts tenant seeds → ML-KEM-768 keypairs per tenant → per-object shared keys via KEM encapsulation → HKDF-derived AES-256 DEKs
- **Encrypted blob format**: `[version=0x01][KEM ciphertext (1088B)][nonce (12B)][AES-GCM ciphertext+tag]`
- **Size overhead**: 1117 bytes per object (1 + 1088 + 12 + 16)
- **Max encryptable size**: 256 MiB (whole-object GCM requires buffering)
- **ETag**: computed on plaintext, not ciphertext (S3 compatibility)
- **object_head_cache.size_bytes**: stores plaintext size (matches AWS SSE-S3 behavior)
- **Tenant seed protection**: AES-256-GCM with masterKey, stored as nonce||ciphertext in DB

**Activation**: set `ENCRYPTION_MASTER_KEY` env var (64 hex chars). When absent, SSE-S3 is disabled gracefully. Per-bucket via `sse_enabled` column, per-request via `x-amz-server-side-encryption: AES256` header.

**Two ML-KEM implementations** coexist:
- `postquantum.go` — cloudflare/circl (pipeline encryption, existing)
- `sse_s3.go` — Go stdlib crypto/mlkem (SSE-S3, new)

## Chunking + Dedup Architecture (Phase 8.3-8.5)

**FastCDC chunking** splits large objects (>64 MB) into content-defined chunks. The **Global Content Index** (GCI) deduplicates chunks across all tenants — identical content is stored once. Since Phase 10, chunking and encryption are no longer mutually exclusive: per-chunk convergent encryption (AES256-CE) is applied when `ENCRYPTION_MASTER_KEY` is set.

**Global chunk store** (Phase 8.5.1): chunks are stored in a shared, tenant-independent container `_global` (const `chunkContainer` in s3_engine_adapter.go), NOT the per-tenant/bucket namespace. Because dedup spans tenants, a chunk first written by tenant A / bucket X must be reachable when tenant B / bucket Y dedups against it — storing in the writer's namespace made cross-bucket/cross-tenant GETs 404. Isolation is preserved at the manifest layer: a tenant reaches a chunk only through its own `tenant_chunk_refs` (queried by `tenant_id`), and `_global` is not addressable via the S3 API (all S3 paths route through `tenant/{id}/{bucket}`).

- **chunker.go** — FastCDC chunker (1 MB min / 4 MB avg / 16 MB max) using `restic/chunker`. `ChunkBytes()` for sync, `Chunk()`/`ChunkContext(ctx)` for streaming (context-cancellable — goroutine exits on ctx.Done). SHA-256 per chunk.
- **gci.go** — `GlobalContentIndex`: DB-backed dedup index with 100K-entry in-memory cache. Batch lookups (`LookupChunks`), ref counting (`IncrementRef`/`DecrementRef`), cache invalidation (`InvalidateCache` — used by the GC sweep, WP-6), tenant chunk manifests (`AddTenantChunkRef`), object metadata (`SaveObjectMetadata`), transactional delete (`DeleteObjectChunks`/`DeleteObjectChunksTx` — the Tx variant releases a displaced manifest atomically with a caller's plain PUT/copy/multipart overwrite), atomic manifest swap on overwrite (`ReplaceObjectManifest`/`ReplaceObjectManifestTx` — releases old refs + installs new manifest + metadata; the Tx variant runs inside the PUT's head-upsert transaction so swap and head cache commit together), chunk insert inside a caller-owned tx (`InsertChunkTx` — pairs with the WP-6 advisory lock; invalidates rather than sets the cache since the caller may roll back). **`IncrementRef` returns `(rowsAffected, error)` (WP-6)** — 0 rows means the chunk row vanished (GC swept it after a stale cache hit); callers MUST re-store instead of proceeding, or the manifest points at deleted data. Both `IncrementRef` and `DecrementRef` cache-delete their entry; `insertChunkSQL`'s ON CONFLICT arm also unmarks (`marked_for_deletion=FALSE, marked_at=NULL`) so re-referencing a marked row rescues it from sweep.

**PUT flow** (s3_engine_adapter.go `handleChunkedPut`, streaming since Phase 8.4.1):
1. Gate: `gci != nil && size > threshold && !versioned && body unencrypted` — SSE-C bodies never chunk; versioned/suspended buckets keep the plain path (manifests lack version_id); tenant IDs are strings, no UUID gate (WP-C). On chunked-path error, return 5xx (never fall through — the body is consumed).
2. `ChunkContext(ctx, hashingBody)` streams through the MD5 hasher+chunker — peak memory is one chunk (~16 MB) regardless of object size. Per-chunk: `LookupChunk` → if new → `storeChunkLocked` (`pg_advisory_xact_lock(hashtext(scope), hashtext(hash))` + `engine.Put(_global, "_chunks/{sha256}")` + `InsertChunkTx`, one tx — serializes against the GC sweep, WP-6); if existing → `IncrementRef`, and **rows-affected 0 → the chunk vanished → re-store as new** (WP-6). Collect the new manifest refs and accumulate `measuredSize`.
3. `ReplaceObjectManifestTx` + `object_head_cache` upsert (`is_chunked=TRUE`, `measuredSize`) commit in ONE tx via `atomicHeadUpsert` (review-A): release the previous version's chunk refs + install the new manifest + upsert `object_metadata` + head row — a failed install leaves the previous version fully intact. New chunks are ref'd in step 2 *before* this, so a chunk shared between old and new versions never transiently hits ref_count 0. After install, the stale pre-chunking whole-object blob at the key is deleted (best-effort).
4. **Abort compensation (WP-6/F10)**: every chunk processed takes one GCI ref; if the PUT errors before the manifest install commits, a deferred compensator `DecrementRef`s each taken ref on a cancellation-immune context — otherwise aborted PUTs leak increments on shared chunks until reconcile.

**DELETE flow** (s3_engine_adapter.go `HandleDelete`):
1. Query `is_chunked` from `object_head_cache`
2. If chunked → `gci.DeleteObjectChunks` (transactional: deletes refs, decrements counts, deletes object_metadata)
3. Chunk data stays until GC (Phase 8.7) — `marked_for_deletion=TRUE` when `ref_count=0`. GC must delete from the `_global` container (using each GCI entry's `storage_key`/`backend_id`), not a tenant namespace.

**GC coherence (WP-6, item 1.7)**: the dedup GC sweep (`internal/api/dedup_gc.go sweepOne`) invalidates the GCI cache before and after each row delete, and holds a session-level `pg_advisory_lock(hashtext(scope), hashtext(hash))` on a dedicated connection across row-delete → blob-delete (row delete commits first; every failure mode leaks at most a blob). The PUT-side `storeChunkLocked` takes the same lock as `pg_advisory_xact_lock`, closing the delete-vs-reref race. GDPR `ExecuteDeletion` releases the tenant's GCI refs set-based inside the deletion tx and marks zero-ref rows for sweep (F5). Advisory locks and cache keys are always keyed on BOTH `(dedup_scope, plaintext_hash)`.

**GET flow** (s3_engine_adapter.go `handleChunkedGet`, Phase 8.5 → streaming in 8.6):
1. `HandleGet` reads `is_chunked` from `object_head_cache`; if set and `gci != nil`, branches before the normal `engine.Get`
2. Preflight: `GetObjectChunks` → ordered manifest; per chunk `LookupChunk` resolves storage key + `BackendID` + `SizeBytes` WITHOUT reading data. A missing index entry → return error → caller falls through (→ NoSuchKey). Done before any byte is written.
3. **Bounded streaming** (8.6): each chunk is read into a bounded buffer (≤ max chunk size, ~16 MB) via `fetchAndVerifyChunk` (`HintBackend` → `engine.Get(_global, "_chunks/{sha256}")`), its SHA-256 verified against the expected plaintext hash, then written to the response. Peak memory is ONE chunk, not the whole object (was a full `bytes.Buffer` in 8.5 — violated "always stream"). Order is `chunk_index` ASC → byte-identical to upload → plaintext ETag matches. Cross-bucket/cross-tenant retrieval works because chunks live in shared `_global`.
4. Range requests touch only chunks overlapping `[start,end]`, located via `tenant_chunk_refs.chunk_offset` + per-chunk `SizeBytes`; the first/last overlapping chunks are trimmed (`skip`/`take`). No whole-object materialization.
5. **Integrity** (8.6): corrupt chunk (sha256 mismatch) → never served. First-chunk corruption → clean 500 (`errChunkIntegrity`, handled here, NOT a fallthrough to 404 — corrupt ≠ missing). Mid-stream failure → body aborted (short read the client detects), status already committed. HEAD needs no chunk-aware logic — `object_head_cache` already holds plaintext size/ETag.

**Constraints**: chunking is mutually exclusive with SSE-C (customer keys) — enforced at the gate since review-D (#343): an SSE-C body that entered the chunked path was stored as chunked ciphertext with AES256-CE stamped over the SSE-C marker, so a keyless GET served raw ciphertext with 200. Versioned buckets are mutually exclusive with chunking (review-A, #341) until manifests are version-aware. SSE-S3 objects >256 MiB route through per-chunk convergent encryption (Phase 10) when `chunkEncSvc` is available. Cross-tenant dedup of encrypted chunks is not supported — same-tenant dedup works via convergent determinism.

**Deterministic chunking (WP-7)**: `NewFastCDCChunker` uses a single fixed polynomial, `DefaultChunkerPolynomial` (`0x2ADD89E3B790BB`). This value is **PERMANENT** — changing it re-defines every chunk boundary, so nothing would dedup against existing chunks and re-chunking to locate data would fail (every stored chunk orphaned). Pinned by `TestFastCDCChunker_PermanentPolynomial`. Before WP-7 each chunker drew a fresh `RandomPolynomial`, so identical content never produced matching chunk hashes and dedup could never hit.

**Tenant-scoped encrypted dedup (WP-7)**: the GCI is partitioned by a `dedup_scope`. Unencrypted chunks use `crypto.GlobalDedupScope` (`"_global"`) — shared across tenants. Encrypted chunks use the **tenant ID** as the scope, and are stored under `_chunks/{tenantID}/{hash}`; their `tenant_chunk_refs.dedup_scope` records this so GET/DELETE/GC resolve the right row. This is why deterministic chunking is safe: identical plaintext from two tenants yields identical plaintext hashes but distinct scoped GCI rows, each encrypted with that tenant's convergent key — so tenant B never receives tenant A's undecryptable ciphertext. `LookupChunk`/`IncrementRef`/`DecrementRef`/`LookupChunks` all take a scope argument; `GCIEntry`/`TenantChunkRef` carry `DedupScope`.

**Ciphertext hash lives on the GCI row**: `global_content_index.ciphertext_hash` (nullable, set for encrypted chunks) is the SHA-256 of the blob actually stored, recorded once at first store. Dedup hits COPY it from the row into the new `tenant_chunk_refs` entry — never recompute it. Recomputing depended on the current request's Content-Type matching the first uploader's (`ShouldCompress` consults Content-Type, so the compression decision could differ) and on zstd output being byte-stable across library versions; either mismatch produced a hash of a blob that was never stored, and the object then failed its integrity precheck on every GET. On read, the GCI row's hash is authoritative; the per-ref copy is a fallback for older rows.

**SSE-S3 ↔ chunk-encryption mutual exclusion (WP-7)**: whole-object SSE-S3 (`sseService`) is skipped when an object will take the chunked per-chunk-encryption path (`chunkEncSvc` set, size > chunk threshold, non-versioned bucket). Otherwise the object would be encrypted twice (SSE-S3 whole-object, then per-chunk) and GET — which only peels the per-chunk layer — would return SSE ciphertext. SSE-S3 is also non-deterministic (random ML-KEM ciphertext + nonce), which would defeat chunk-dedup determinism. Guarded by `TestChunkedEncryption_SkipsWholeObjectSSE`.

**Tables**: `global_content_index`, `tenant_chunk_refs`, `object_metadata` — created by migration **016** (051's CREATEs were no-ops; 016 is the source of truth per review-B #340), TEXT tenant_id at source + migration 058 converts pre-2026-07-18 DBs in place (WP-C). Scope partitioning + `ciphertext_hash` via 054. `object_head_cache.is_chunked` flag.

## SSE-C Architecture (Phase 5.14.8)

**Stateless** customer-provided key encryption. No DB, no key management — the customer sends a raw 256-bit AES key with each PUT/GET/HEAD request.

- **Encrypted blob format**: `[nonce (12B)][AES-GCM ciphertext+tag]`
- **Size overhead**: 28 bytes per object (12 nonce + 16 GCM tag)
- **Max encryptable size**: same 256 MiB limit as SSE-S3
- **ETag**: computed on plaintext (before encryption)
- **Key never stored**: only the algorithm indicator `AES256-SSE-C` in `object_head_cache.encryption_algorithm`
- **Mutually exclusive** with SSE-S3 per object
- **S3 headers**: `x-amz-server-side-encryption-customer-algorithm: AES256`, `x-amz-server-side-encryption-customer-key: <base64>`, `x-amz-server-side-encryption-customer-key-MD5: <base64>`
- **GET/HEAD without key**: returns 403 AccessDenied
- **Multipart uploads**: not yet supported (returns NotImplemented)
- **CopyObject**: not yet supported for SSE-C objects
