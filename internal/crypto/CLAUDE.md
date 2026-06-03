# internal/crypto

Encryption, key management, and post-quantum cryptography for Vaultaire.

## Key Files

- **ssec.go** — SSE-C (customer-provided keys): stateless AES-256-GCM encrypt/decrypt with customer's 32-byte key. S3 header parsing (algorithm, key, MD5 validation). No DB, no key storage
- **sse_s3.go** — SSE-S3 service: ML-KEM-768 key encapsulation + AES-256-GCM data encryption. Per-tenant keypairs in DB, per-object DEKs via KEM encapsulation
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

## Chunking + Dedup Architecture (Phase 8.3-8.4)

**FastCDC chunking** splits large objects (>64 MB, unencrypted) into content-defined chunks. The **Global Content Index** (GCI) deduplicates chunks across all tenants — identical content is stored once.

- **chunker.go** — FastCDC chunker (1 MB min / 4 MB avg / 16 MB max) using `restic/chunker`. `ChunkBytes()` for sync, `Chunk()` for streaming. SHA-256 per chunk.
- **gci.go** — `GlobalContentIndex`: DB-backed dedup index with 100K-entry in-memory cache. Batch lookups (`LookupChunks`), ref counting (`IncrementRef`/`DecrementRef`), tenant chunk manifests (`AddTenantChunkRef`), object metadata (`SaveObjectMetadata`), transactional delete (`DeleteObjectChunks`).

**PUT flow** (s3_engine_adapter.go `handleChunkedPut`):
1. Gate: `gci != nil && size > threshold && no encryption`
2. Parse tenant UUID (skip chunking if non-UUID tenant)
3. `ReadAll` through MD5 hasher → `ChunkBytes` → batch `LookupChunks`
4. For each chunk: if new → `engine.Put("_chunks/{sha256}")` + `InsertChunk`; if existing → `IncrementRef`
5. `AddTenantChunkRef` per chunk, `SaveObjectMetadata`, `object_head_cache` with `is_chunked=TRUE`

**DELETE flow** (s3_engine_adapter.go `HandleDelete`):
1. Query `is_chunked` from `object_head_cache`
2. If chunked → `gci.DeleteObjectChunks` (transactional: deletes refs, decrements counts, deletes object_metadata)
3. Chunk data stays until GC (Phase 8.7) — `marked_for_deletion=TRUE` when `ref_count=0`

**GET flow**: not yet wired — chunked objects are not retrievable until Phase 8.5 (download pipeline).

**Constraints**: chunking is mutually exclusive with SSE-S3/SSE-C in this phase. Convergent encryption (Phase 10) will enable per-chunk encryption + dedup.

**Tables**: `global_content_index`, `tenant_chunk_refs`, `object_metadata` (migration 051). `object_head_cache.is_chunked` flag.

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
