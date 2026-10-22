# Stage 1a — Prompt 28: keystone K4, the transparency log (plan 43.6, F-16)

*2026-10-22. PRs: #879 (PR 1, the library), #880 (PR 2, the store, **migration 103**), #884 (PR 3, the producers, the read side and `translog-verify`), this docs PR. Also #885 (K2 crypto core: the owner's ChaCha + post-quantum request of the same session, see the end). Next migration: **104**. Prompt 27 ran in parallel (#881 merged, #882 open); nothing here touches `internal/modes`, `internal/payments`, `internal/billing` or the S3 write path.*

## What was built

- **PR 1 (#879, merged 14:12 UTC, `f35b15cd`).** `internal/translog`, pure Go, no database: an RFC 6962 / 9162 Merkle log, checkpoints as C2SP signed notes, inclusion and consistency proofs, and the c2sp tlog-tiles paths. Origin `stored.cloud/translog/v1`: the checkpoint's first line and the signing key's name.
- **PR 2 (#880, merged 15:01 UTC, `bb0a6643`).**
  - Migration 103: `translog_entries`, `translog_hashes` and `translog_checkpoints`, append-only by trigger (UPDATE, DELETE and TRUNCATE raise).
  - The typed entry kinds `server_build` and `key_ceremony`, each with an allow-list of fields.
  - The signing key, read in the master key's precedence (credential `translog_signing_key`, then `_FILE`, then the env variable; a source set but unusable is Fatal).
  - `logstore`: one append transaction under an advisory lock.
  - The `translog_checkpoint` job (10 min). It signs only after proving the log extends the newest checkpoint.
  - Metrics, the rules `vaultaire-translog.yml`, the job added to `PeriodicJobStale`, and `cmd/tools/translog-keygen`.
- **PR 3 (#884, merged 16:06 UTC, `83ef6c91`).**
  - The producers:
    - one `server_build` entry per distinct (git sha, binary sha256), appended at `Start` in the background;
    - `POST /api/v1/admin/translog/ceremony`: admin JWT plus a fresh TOTP code, which is consumed once; audited `admin.translog_ceremony`.
  - The read side: `/translog/checkpoint` and `/translog/tile/*`, behind `translog_public` (default OFF = 404). A signed or presigned request on those paths goes to the S3 API.
  - `translog.Client` and `cmd/tools/translog-verify`, which checks the signature, inclusion, and consistency with the checkpoint it saved last.

## Library choice

| Need | Library | Version | Licence |
|---|---|---|---|
| Hashes, stored hashes, roots, proofs, tiles | `golang.org/x/mod/sumdb/tlog` | v0.41.0 | BSD-3 |
| Signed notes, Ed25519 note keys | `golang.org/x/mod/sumdb/note` | v0.41.0 | BSD-3 |
| Checkpoint body | `github.com/transparency-dev/formats/log` | v0.1.1 | Apache-2.0 |
| Proof **verification** | `github.com/transparency-dev/merkle/proof` | v0.0.2 | Apache-2.0 |

There is no hand-written Merkle code. Every proof that x/mod produces is verified by transparency-dev's independent implementation. `TestTranslog_KnownAnswer_RFC6962Vectors` uses transparency-dev's testonly root vectors for sizes 0 to 8.

## Red tests and mutations

Each behaviour started from a red test; the red runs are kept with the PRs.

| PR | Red first | Load-bearing mutation → test that went red |
|---|---|---|
| 1 | `TestTranslog_KnownAnswer_RFC6962Vectors`, `_InclusionProofVerifies`, `_ConsistencyProof_RewriteDetected`, `_CheckpointIsC2SPSignedNote`, `_TilePaths_C2SP`, `_TileData`, `_EntryBundle`, `_StoredHashesAreWhatTheReaderNeeds` | — (the library's KATs) |
| 2 | `TestTranslog_EntriesNeverNameATenant`, `_EntryValidation`, `_SigningKeySources`, `_ConcurrentAppends_Contiguous`, `_AppendDedupKey`, `_AppendRefusedEntryCountsAFailure`, `_TablesAreAppendOnly`, `_CheckpointSignsWhenTheTreeGrew`, `_CheckpointWithoutAKeySignsNothing`, `_CheckpointRefusesARewrittenHistory`, `TestTranslogStateCollector`, `TestTranslogSigningKeyOrFatal_RefusesTheBootAndNeverLogsTheKey` | no advisory lock → `ConcurrentAppends_Contiguous`; e-mail rule dropped → `EntriesNeverNameATenant`; malformed key read as "no key" → `SigningKeySources`; `checkExtends` accepts everything → `CheckpointRefusesARewrittenHistory`; an INSERT outside logstore → `OnlyTheStoreWritesTheLogTables`; Fatal turned into an error → `SigningKeyOrFatal_…` |
| 3 | `TestTranslogHandler_CheckpointTilesAndCacheHeaders`, `TestTranslogClient_InclusionAndConsistency`, `TestTranslogVerify_RewrittenHistoryFails` (library and CLI), `TestTranslog_BuildEntryOncePerBinary`, `TestTranslogCeremonyRoute_AdminMFAAudited`, `TestTranslogPublic_FlagOffIs404_OnServes_SignedGoesToS3` | consistency check skipped (library, CLI) → `RewrittenHistoryFails`; tile bound dropped → `CheckpointTilesAndCacheHeaders`; a partial tile cached → same; no build dedup → `BuildEntryOncePerBinary`; TOTP replay allowed → `CeremonyRoute_AdminMFAAudited`; flag ignored → `FlagOffIs404…`; a signed request not passed to S3 → same |

Every mutation turned its test red. Guards that other packages now enforce:
- `TestTranslogTablesAreInfrastructure` (account schema walk: never erased);
- `translog_hashes.hash` in `exportNeverColumns`;
- no `translog_` retention policy;
- the OpenAPI drift guard (the ceremony route is documented).

## Departures

- **D-P28-1.** The ceremony's MFA code travels in the JSON body (`mfa_code`) and is checked with `ValidateCode` + `ConsumeTOTPCode`. There is no separate step-up endpoint, so one request is one ceremony.
- **D-P28-2.** `server_build` is appended in the background at `Start` with a 1 min timeout and never blocks the boot. A failure is counted in `vaultaire_translog_append_failures_total{kind="server_build"}` (`TranslogAppendFailing`) and retried at the next boot.
- **D-P28-3.** The read routes are matched before the S3 catch-all, and only for unsigned requests. A signed or presigned request on `/translog/...` reaches the S3 API (RP35-1).
- **D-P28-4.** The flag is global only (no per-tenant row): the log is one public object.

## Prod proof

All reads were made on the box: sudo password on stdin, the journal read as JSON, `psql` as postgres. Baseline before the work: `a2d388f` on 8000, 0 errors.

- **#880, `bb0a664`** (slot 8001):
  - The 3 tables and 6 triggers exist.
  - Boot Info line: `translog signing key: none — entries append, no checkpoint is signed`.
  - Every `vaultaire_translog_*` series is exported at 0, including `signing_key_loaded 0`.
  - 0 `"level":"error"` lines in 30 min.
  - First job run `ok`, detail `empty`.
- **#884, `83ef6c9`** (slot 8001, 16:07 UTC, public `/version` checked by the Deploy run):
  - `translog_entries` holds **1 row**: idx 0, `server_build`, git_sha `83ef6c91d649`, binary_sha256 `cdd30ba6…2b59e`. That equals `sha256sum /opt/vaultaire/bin/vaultaire-8001`.
  - Why exactly 1: bb0a664 had no producer, so the count equals the distinct producer binaries deployed (one).
  - The hash rows are consistent with size 1; 0 checkpoints.
  - `job_runs`: `translog_checkpoint | ok | no signing key | {"size":1,"checkpoint_size":0,"signed":false,"root":"b7e6f20e…7420c","reason":"no signing key"}`.
  - With `translog_public` OFF, `/translog/checkpoint` and `/translog/tile/0/000.p/1` answer **404**, on the slot and at `https://stored.ge`.
  - An unauthenticated ceremony POST answers **401**.
  - `vaultaire_translog_size 1`, both `append_failures_total` series 0, `signing_key_loaded 0`.
  - **0 `"level":"error"` lines in 90 min**, across both switches.
- **K2 #885, `d70e5b4`**: Deploy, CI, Security and CodeQL all green. The build has no server code path.

## Rules installed

`promtool check rules` (2 + 9 rules) and `promtool test rules` (both test files in one directory) passed on the box. The files were installed into `/etc/prometheus/rules` and Prometheus was reloaded:

| File | sha256 |
|---|---|
| `vaultaire-translog.yml` (new) | `acd8ff49812fc3ea4a2efd78d0d0fb7486de292c2ef54ece6937920a408a89b9` |
| `vaultaire-jobs.yml` | `9daa8a878f4549f8c7abd0760dd4a5088f9223633c5245ce9d9247e5e401aea1` |

After the reload: **17 groups, 103 rules**, all `health=ok` after the first evaluation. `TranslogCheckpointStale`, `TranslogAppendFailing` and `PeriodicJobStale` are inactive.

## Residuals

- **RP35-1 (P4).** `/translog/checkpoint` and `/translog/tile/*` shadow an **anonymous** GET of a public customer bucket named `translog`. Signed and presigned requests still reach S3. Fix when the log moves to trust.stored.cloud (K4b): serve it only on that host.
- **RP35-2 (P4).** Cloudflare's "S3 API is never cached" rule covers `/translog/*`, so full tiles marked `immutable` are not edge-cached. This is harmless at today's size; revisit with trust.stored.cloud.
- **RP35-3 (note).** The K2 WASM is 3.8 MB raw and 1.1 MB gzip (the Go runtime). TinyGo or a split build is a later K2 concern, not a blocker.
- **RP35-4 (note).** The signing key and the verifier key are not on prod: every checkpoint waits for the owner's ceremony. Until then the log appends and nothing signs, by design.
- **RP35-5 (note).** The first ceremony entry must be appended after the key is loaded, so that the first signed checkpoint covers it. The order is in `docs/TRANSPARENCY.md`.

## K4b (not in this prompt)

The external witness and `cmd/tools/translog-witness`; OpenTimestamps anchoring; the Rekor mirror; trust.stored.cloud (with RP35-1 and RP35-2); `admin_read` entries (an operator read of tenant metadata, with an opaque subject); client build hashes (K2's WASM and the CLI); the fTPM quote in `server_build`; `audit_logs` roots.

## Owner items (not acted on)

1. **The signing-key ceremony** (`docs/TRANSPARENCY.md` § Ceremony). It can share the F-14 master-key session.
2. **Publish the verifier key outside our infrastructure** before the first signed checkpoint (a signed tag, a gist, a post), then record it in § Verifier key.
3. **Flip `translog_public`** once a checkpoint is signed.

## K2: the client crypto core (#885, same session)

The owner asked for ChaCha and post-quantum encryption "in the wasm and areas it makes sense".

**What changed:**
- `sdk/go/e2ee`:
  - `0x01`: an XChaCha20-Poly1305 box.
  - `0x02`: a convergent chunk under the space key. The key comes from HKDF; the nonce comes from HMAC over the plaintext. The v1 bytes are pinned.
  - `0x03`: an HPKE sealed box (Go 1.26 `crypto/hpke`: MLKEM768-X25519 "X-Wing" + HKDF-SHA256 + ChaCha20-Poly1305).
- `sdk/wasm` and `sdk/js/e2ee`, proven byte-identical to native Go in node (`TestE2EE_WASMMatchesNative`).

**What did not change:** the server's Standard mode stays AES-256-GCM + ML-KEM-768. ChaCha is not post-quantum; it only wins without AES hardware.

**Escrow:** `docs/ENCRYPTION.md` now asks for age ≥ 1.3 from upstream (Ubuntu's apt has 1.2.1), with at least two `age1pq` recipients.

**Research correction:** the research note's "X-Wing is TLS-only" was wrong and is corrected; that line was about X25519Kyber768Draft00.

**Mutations:**
| Mutation | Test that went red |
|---|---|
| nonce from the key alone | `ConvergentWithinASpace_NeverAcross`, `DeterministicNonceSeparatesMessages` |
| chunk key ignores the space | `ConvergentWithinASpace_NeverAcross` |
| box AD not bound | `Box_SealOpen` |
| sealed-box AD not bound | `SealedBox_RoundTripAndBinding` |
| WASM chunk not convergent | `E2EE_WASMMatchesNative` |
