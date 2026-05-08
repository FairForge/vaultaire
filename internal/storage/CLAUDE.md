# internal/storage

Advanced storage primitives for the data pipeline. These are building blocks for Tier 2 features (chunking, dedup, compression, tiering) — not the driver interface.

Note: The driver interface contract is `engine.Driver` in `internal/engine/interface.go`, not in this package.

## Files

| File | Type | Purpose |
|------|------|---------|
| `chunking.go` | `ContentChunker` | FastCDC-style content-aware chunking with rolling hash. Configurable min/max chunk size. `Split(data) → []Chunk` where each `Chunk` has `Data`, `Hash` (SHA-256), `Offset`, `Size`. |
| `dedup.go` | `Deduplicator`, `DedupStore` | Block-level dedup via SHA-256 hashing. `CheckBlock(data) → (hash, isNew)`. In-memory store with reference counting and dedup ratio stats. |
| `delta.go` | `DeltaEncoder`, `VersionStore` | Delta encoding between versions (XOR + gzip). `VersionStore` maintains chains: `Store` → `Update` (stores delta vs previous) → `GetVersion(id)` reconstructs from chain. |
| `garbage_collector.go` | `GarbageCollector` | Block lifecycle tracking with TTL-based expiration (default 7 days). `FindOrphaned()`, `FindExpired()`, `Cleanup() → bytes reclaimed`. |
| `tiering.go` | `TieringEngine`, `TierManager` | Access-pattern classification: ≥3 accesses in 24h = Hot, ≥1 in 7d = Warm, else Cold. `TierManager` moves data between tiers based on policy. |

## Connection to Plan

These primitives are the foundation for:
- Phase 8: FastCDC Chunking + Global Dedup (uses `chunking.go` + `dedup.go`)
- Phase 9: Compression (extends the pipeline after chunking)
- Phase 10: Convergent Encryption (operates on chunks from Phase 8)
- Phase 11: Erasure Coding (operates on encrypted chunks)
- Phase 7: Smart Tiering (uses `tiering.go` patterns, but the production tiering engine will be in `internal/engine/`)
