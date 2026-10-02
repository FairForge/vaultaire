# WP-R8-4 — the chunker's identity is on record

**Worker session, 2026-10-02. PR #557, branch `wp/R8-4-chunker-identity`, from `main` @ #556.**
Closes R8-11, R8-18, R10-32 and SYNTHESIS table B row WP-R8-4. No migration (`pipeline_config` has
been a JSONB column of `object_metadata` since 016/051; it was NULL on every row).

**Decision applied by default — status quo, needs Isaac's confirmation:** the chunker stays what is
shipped: restic's Rabin fingerprint chunker, 1 MiB minimum, **20 average bits (≈2 MiB real average)**,
16 MiB maximum, polynomial `0x2ADD89E3B790BB`. Changing the average later (`SetAverageBits(22)` for
the 4 MiB the docs claimed) cuts new uploads at other boundaries: nothing stored would ever
deduplicate against them again — a one-time reset of dedup for everything stored. The record built
here is what makes such a change possible to do knowingly, one day: every chunked object says which
chunker cut it.

## What was built

### The identity (`internal/crypto/chunker.go`)

One constant set, `ChunkerIdentity` / `DefaultChunkerIdentity()`:

| Part | Value | Pinned by |
|---|---|---|
| library | `github.com/restic/chunker` | — |
| version | `v0.4.0` (`ChunkerLibraryVersion`) | `TestChunkerIdentity_MatchesGoMod` reads go.mod: a bump has to change the constant |
| polynomial | `0x2ADD89E3B790BB` (`DefaultChunkerPolynomial`, WP-7) | `TestRabinChunker_PermanentPolynomial` |
| min / average bits / max | 1 MiB / **20** / 16 MiB | `TestRabinChunker_GoldenBoundaries` |

`TestRabinChunker_GoldenBoundaries`: a fixed 24 MiB pseudo-random input (PCG with fixed seeds — a
specified generator, the same bytes on every Go version) must cut at exactly eleven recorded offsets
with the recorded sizes and hashes, through both the sync and the streaming path. A dependency bump
that moves a boundary, a different mask, minimum or polynomial fails the build (each tried: 21
average bits and a version string that is not go.mod's — both red). `go.mod` carries the pin as a
comment on the require line.

**The split mask is passed explicitly now** (`SetAverageBits(ChunkerAverageBits)` on every library
chunker): it was the library's default and nothing of ours, and the old constructor's "4 MiB average"
argument was accepted, validated and never used (R8-11). That argument is gone
(`NewRabinChunker(min, max)`); `ChunkExpectedAverage` (2 MiB = the minimum plus the expected ~1 MiB
to a 20-bit boundary) is the documented number, and `PipelineConfig.ChunkAvgSize` in the presets
says it.

**The type is named for what it is**: `FastCDCChunker` → `RabinChunker`, `DefaultFastCDCChunker` →
`DefaultChunker`, `ChunkingFastCDC` ("fastcdc") → `ChunkingRabin` ("rabin"). No `pipeline_config` row
was ever written with the old name, so no stored value changes meaning.

### The record (`PipelineConfig.Chunker`, `object_metadata.pipeline_config`)

`crypto.RecordedPipeline(identity, encrypted)` is what a chunked object records: what the pipeline
DID — the chunker's identity, dedup scope (cross-tenant unless encrypted), zstd per chunk, convergent
AES-256-GCM when the master key is set. Written by the chunked PUT (`chunkedPipeline`), carried by the
chunked copy from its source (`copiedPipeline`; a source without a record gets the default chunker's
— the only one that ever cut anything), and by the frozen `dedup-migrate` tool. `PipelineConfig` is
a `driver.Valuer`/`sql.Scanner` (JSON); `GetObjectMetadata` reads it back. Reads never consult it:
the index rows describe each chunk. R8-18 asked for it in `pipeline_config`; it is there.

### One helper for the threshold and the floor (`internal/api/chunked_object.go`)

`chunkThreshold()` (64 MiB default; the inline default in `HandlePut` is gone), `chunkedObjectFloor`
(the chunked PUT's and the chunked copy's floor — it was the SQL literal `'standard'` in two places
and `usage.FloorStandard` in three; R8-18 / R10-32). It is a constant on purpose:
`TestChunkedObjectFloor_IsTheFloorOfEveryClassThatChunks` walks every storage class and asserts that
each class `storageClassDisablesChunking` lets through bills on that floor — the day chunking is
opened to the attic, that test names the one place to derive the floor from the class.

## No behaviour change for stored data

The chunker cuts exactly as before (the golden boundaries were generated with the code on `main`
before the explicit `SetAverageBits` call and pass after it); hashes, scopes, keys and the one address
of WP-R8-7 are untouched; the floor written is the same `standard`; `pipeline_config` goes from NULL
to a JSON document on new and re-written chunked objects only. Prod's 127 rows stay as they are (their
object is in the dead iDrive account, WP-R8-7). Nothing was found that needs a change to keep old
objects readable.

## Adversarial pass

- **A dependency bump.** go.mod's version ≠ the constant → `TestChunkerIdentity_MatchesGoMod` red;
  a bump that keeps the boundaries passes the golden test and only the constant changes; one that
  moves them is red where it matters.
- **"Deterministic" against platform.** The golden input is `math/rand/v2` PCG with fixed seeds —
  specified, not "the default source"; the boundaries are what the library computes on the bytes,
  with no platform-dependent step.
- **The old name in stored data.** `pipeline_config` was NULL everywhere (R8-11 found it; prod
  confirmed in WP-R8-7's read); `ChunkingAlgo` values never reached a row.
- **A copy of an object cut by a future chunker** carries the source's record, not the current
  default (`copiedPipeline` copies the source's when it names a chunker).
- **The constant floor.** Not derived from the class: pinned instead, with the test that says when
  deriving becomes necessary. Deriving today would hide that the chunked copy ignores the destination
  bucket's class (it reserves and bills `standard` whatever the destination's tier — pre-existing,
  noted, not changed).

## Tests

`internal/crypto/chunker_identity_test.go` (golden boundaries, identity = what the chunker does,
go.mod pin, from-config), `internal/api/chunked_object_test.go` (floor coupling, threshold, the record
on PUT, the record on copy, the default for an unrecorded source); the renamed existing suites;
`go test -race ./...`, `make lint` 0, `make gosec` 0.

## Docs made true

`internal/crypto/CLAUDE.md`, `internal/api/CLAUDE.md`, `docs/ARCHITECTURE.md`, the plan's Phase 8
heading and 8.1 sketch, Phase 9.3 row, two test comments, four benchmark names. Historical review
documents keep the words they were written with.

## [YOU]

- **Confirm the decision**: keep the 2 MiB average (status quo). If you want 4 MiB instead, say so
  before customer data accumulates — it is a one-time dedup reset, and with the record in place the
  change is `ChunkerAverageBits = 22` + regenerating the golden boundaries + a new identity on new
  objects; old objects keep reading (their chunks and manifests do not depend on the average).
- Nothing to install, no env, no migration.

## Post-merge review (plan driver, 2026-10-02)

Read against the note: `crypto/chunker.go`, `config.go` (`Value`/`Scan`, `RecordedPipeline`),
`gci.go` (the upsert already wrote `pipeline_config`; it was nil everywhere), `api/chunked_object.go`,
the two write sites. A nil `*PipelineConfig` reaches `database/sql` as NULL (value-receiver `Valuer`
on a nil pointer is the documented exception). No finding. One note: `NewChunkerFromConfig` builds
the chunker from `min`/`max` and the constant `ChunkerAverageBits`, not from the recorded
`Chunker.AverageBits` — fine while there is one identity; the day the average changes, a config-built
chunker must take its bits from the record.
