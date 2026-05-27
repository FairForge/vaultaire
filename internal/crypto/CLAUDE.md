# internal/crypto

Encryption, key management, and post-quantum cryptography for Vaultaire.

## Key Files

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
