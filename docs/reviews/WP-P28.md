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
- **K2 #885, `d70e5b4`**: Deploy, CI, Security and CodeQL all green on its own merge commit. The build has no server code path.

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
- **RP35-6 (P3, fixed by #888).** The Security workflow on `83ef6c9` (#884) failed: gosec G705 (XSS taint) at `internal/translog/serve.go:119`, the read handler's `w.Write`. It is a false positive (a signed note or tile bytes, non-HTML content types, `nosniff`); #888 adds a `#nosec G705` with that reason, and its gosec job passes. It was missed because the ground rule's `make gosec` never really ran: the Mac had no `gosec` on PATH, and `go run …gosec@latest` skips SSA ("type errors") in every package there, so taint rules never run locally. The Security job on the PR is the only real gosec gate; check it before arming.

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

## Post-merge review (2026-10-22, meta-review)

State: prod `ea969b1b`, 0 `"level":"error"` in 6 h; `translog_entries` = **8 rows, all `server_build`, for 6 git shas** (below); `translog_checkpoint` `ok` every 10 min, `no signing key`, size 8, root `4e6b8110…`; `translog_public` has no row (OFF); `vaultaire-translog.yml` on the box = `acd8ff49…` (matches). Also read here, out of this prompt but merged meanwhile: #883 (target-orphans census), #886 (a failed write queues its address on every backend that was sent the body), #891 (Geyser deletes name `VersionId=null`), #892 (Geyser buckets created never-versioned).

**What holds (K4).** Migration 103 re-applies cleanly (run twice more on a migrated DB: NOTICEs only, still 6 triggers, no rows); nothing needs to delete (erasure, export, retention skip the tables); the append lock + `MAX+1` + dedup in one transaction, entry and hashes committed together; two slots serialised by the job lock and `ON CONFLICT (size) DO NOTHING`; origin = key name; `tile/<L>/<N>`, `tile/entries/<N>`, `.p/<W>` (1–255), the x-prefixed groups, only the canonical form parsed, a width past the checkpoint 404, only full tiles `immutable`, a short read 503; the client pins origin + key name, refuses a smaller tree and a same-size fork, authenticates every tile against the root (`tlog.TileHashReader`), checks the printed body's leaf hash; `DisallowUnknownFields` + a sealed `Entry`; an admin without MFA 403, the code consumed once; dedup backed by a unique partial index; the key sources and their Fatal; every `append_failures_total{kind}` series at 0 from boot. **K2:** primitives used correctly — format byte in the AEAD AD of 0x01/0x02, HKDF info = label ‖ SHA-256(pt), HMAC nonce, two spaces never share a key or ciphertext; HPKE base mode via `NewSender`/`NewRecipient`, the 1120-byte encapsulation length checked; "X-Wing" is the accurate name (KEM 0x647a, the X-Wing combiner); 6.1 M fuzz execs over every `Open*` with no panic; randomness from `crypto/rand`, no caller nonce, plaintext only after authentication. **Drivers:** LA1 (`stored3lib-…`, Suspended) listed 2026-10-22 23:40 UTC: 15 versions, **0 with a non-null version id**, so #891's "no older version can surface" holds on prod; #886's queue-on-sent is bounded to the write's own fresh address.

| # | Sev | Finding | Evidence |
|---|---|---|---|
| **RP37-9** | P3, proven on prod | **Builds are not reproducible, so no `server_build` entry can be recomputed by anyone.** Two shas have two binaries each: `c2a7fa698eb2` → `dca5b82a…` (dispatch, 16:59) and `eba0b280…` (push, 17:21); `d464454ea785` → `701bb916…` and `c6c81090…`. The ldflags stamp the wall-clock build time and nothing pins `-trimpath`/CGO/VCS stamping; a third party holding the commit can never reach either hash. The log's first entry kind is unverifiable until this is fixed (the worker noted it as K4b; it is small and it is what makes K4's only entries mean something) | `select convert_from(body,'UTF8') from translog_entries` |
| RP37-10 | P3, proven | **Free text in a permanent public log.** A `key_ceremony` `statement` is ≤ 2,000 bytes checked only against six shape regexes: "Installed by Isaac Viera with Jane Doe of 12 Main Street", "…from isaacv17 at gmail dot com" and a bare 16-hex tenant id all pass `Encode`. The triggers refuse DELETE, so a name appended by mistake can never be erased (GDPR). The claim "no entry can name a tenant, a person, an e-mail" (`entries.go:18`) is false as written | Throwaway test (removed): all three accepted |
| RP37-11 | P3 | **A database restore makes the key equivocate.** The 03:00 dump holds `translog_checkpoints` with the entries; after a restore the newest checkpoint row is the restored one, `checkExtends` passes, and the job signs a DIFFERENT tree at sizes it already signed and published (up to 24 h of entries). Only an outside verifier with a saved checkpoint notices; `docs/TRANSPARENCY.md:79` says a restore shows as "does not extend", which a full restore never triggers. And when "does not extend" does fire, it surfaces only as `TranslogCheckpointStale` (warning, > 1 h + 15 m) — the one event the log exists to expose does not page | `logstore.go:244-299`, `vaultaire-translog.yml:28-38` |
| RP37-12 | P3, proven (mutation) | **The sealed box's wire format is not pinned.** Changing `labelSealed`, or dropping it (no domain separation), leaves every test green: the HPKE KAT calls `openSealed` below the label. The relabel test passes for the wrong reason (the `msg[0]` check, not the AD binding). Byte 0x03 is checked but not bound into HPKE's info. Nothing is sealed yet, so the wire can still change for free — after the first share it cannot | Mutations M2/M3 green, M1 caught only by the chunk golden test |
| RP37-13 | P4 | Translog tests flake on the shared global log: each test signs with its own random key; another package's append + sign between this test's Append and Checkpoint → `unchanged`, the served note is the other key's → "note has no verifiable signatures" (`internal/api/translog_read_test.go:94-124`; `logstore_test.go:191-208` the same window). A merge-queue ejection costs 20 min | Deterministic interleaving shown |
| RP37-14 | P4 | The ceremony route consumes the TOTP code before `Encode` validates the entry (a typo burns the code); no dedup key or `Idempotency-Key`, so a client retrying after an unseen commit appends a second, permanent entry. The comment says refusals are audited; a non-admin's 403 (`requireAdmin`) writes none | `translog_read.go:188-195`, `middleware.go:11-18` |
| RP37-15 | note (K2b design) | What the next K2 layer must carry: shares are HPKE **base** mode — unauthenticated, so the server (which holds every member's public key) can seal a key it chose to a member or substitute its own public key: a share needs a sender signature and an out-of-band / logged recipient key; the only chunk id the API offers is `chunkHash` = plain SHA-256(pt) — sent to the server it confirms a known file across every tenant: the id must be keyed under the space; `OpenChunk` does not check SHA-256(pt) against the hash that derived the key (dedup poisoning by a space member); ciphertext length = len(pt)+41 and the chunker is a public polynomial, so a known file's length sequence matches across spaces ("two spaces never converge" is true of bytes, not lengths); chunks carry nil AD, so the manifest must bind order, length, space, object, version and resist rollback; WASM never wipes keys (no claim made — none may be made) | Agent review of #885 |
| RP37-16 | note | Builds of K4: dedup means a rollback to an older binary appends nothing — the log shows which builds ever started, not which one runs; the app role owns the tables, so `DISABLE TRIGGER` / `session_replication_role` passes the guard (protection = the outside consistency check, as documented) | design |
| RP37-17 | note (ops) | **Geyser LA1 was unreachable from SLC for two hours**: 188 `backend health check failed` (`request send failed` / deadline on the signed HeadBucket) from 17:38:43 to 19:38:56 UTC, breaker closed throughout, 0 error lines, healthy since. Unknown whether `BackendDown`-class rules fired and whether a Vault write in the window failed; the delete-method matrix of #891 (20:55 UTC) left 3 null delete markers on LA1 (`deltest-20261022/la1/m4`, `m6-never`, `m8`) and #892's live proof a bucket `la2rot2` on LA2 | journal, `list-object-versions` |

RP35-1…5 carried. → **Prompt 30 = Part 0 (with WP-P27's RP37 rows)**; **Prompt 31 = K2b** (the key hierarchy in `sdk/go/e2ee`, RP37-12 and RP37-15 inside it). No migration in either; next migration **104**.
