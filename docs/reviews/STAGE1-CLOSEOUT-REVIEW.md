# RP85 — the post-merge review of the Stage 1 closeout, engine lane (Stage 1a Prompt 52E)

*Reviewed 2026-10-30 (session RP85, run with RP84). Read adversarially:*
- *the PRs: #1073 (chunk signatures, the origin's trailer and header checksums), #1077 (A2: read the body to its end before a commit), #1070 (the boot DB contract, the migration runner, migration 119), #1075 (idempotency claim-first, multipart conformance, `last_accessed` coalescing), #1076 (the class-B deletions, the Quotaless/S3 drivers), #1074 (the `vaultaire` service user) and the closeout #1079 (`docs/reviews/STAGE1-CLOSEOUT.md`);*
- *against the note's Step 0 answers, decisions 1–13 and residuals.*

## Verdict

**HOLDS. No P0, P1, P2 or P3.** The prod proof already found and fixed the one real gap (#1077: an S3-class driver's single PutObject stopped at the declared length, so the terminal chunk, the signed trailer and the declared checksum were never read for any body ≤ 32 MiB). The review found nothing of that weight.

Two P4s. One comes from how this lane's middleware meets Prompt 52's role check.

Every test the PR bodies name exists by name:
- `TestAWSChunked_TrailerCheckedWhenTheDriverStopsAtTheDeclaredLength`;
- `TestEdgeAdmit_SignedChunkStreamsPassThrough`;
- `TestBoot_DeadDatabaseFailsFast`;
- `TestMigrations_Reapply`;
- `TestMultipartConformance`;
- `TestExportSectionsCoverSchema`;
- `TestBackendEncryptionDoc_EveryDriverHasARow`.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| The chunk verifier | `ChunkVerifier.Chunk` and `.Trailer`: the string to sign as the spec gives it (`AWS4-HMAC-SHA256-PAYLOAD`, date, scope, the previous signature, `sha256("")`, the chunk's hash; the trailer's over the canonical trailing headers), chained from the seed, the terminal chunk included; a chunk with no `chunk-signature` fails; once failed, every later call fails; `hmac.Equal`; the signing key only in the closure | Yes |
| Fail closed | `requestPayload` refuses a header-signed stream whose body carries no verifier (`errAWSChunkedSignature`); `isAWSChunked` keys on `STREAMING-*` OR `Content-Encoding: aws-chunked`; the chunk size is parsed as int32 hex, never negative | Yes |
| The other decoders | the edge (`edgeStreamingOf`) and the data-plane host (`internal/dataplane/awschunked.go`) take only `STREAMING-UNSIGNED-PAYLOAD-TRAILER` with a trailer they verify; a signed stream is handed back to the origin | Yes |
| #1077's drain, every branch | `HandlePut` drains after `placeObject`, ahead of the commit; the chunked branch returns before the drain but the chunker reads to EOF and returns the decoder's errors (they are wrapped errors, never a bare `io.ErrUnexpectedEOF`, so the chunker cannot read them as end of stream), and the declared length is checked after; staged UploadPart reads to EOF; the SSE-C part path drains after the sealer, and a byte past the declared length is refused; CopyObject's source is a backend GET (no client framing) | Yes |
| R3-25 (the note's open question) | `s3_copy.go` passes `WithContentLength(putLength)` (the source size, or the sealed length for SSE-S3 / SSE-C) on the backend PUT; #1075 added the answer's `Content-Length` | **Confirmed**: R3-25 was already fixed before this prompt |
| The boot contract | a dead DSN exits 1 (no "without a database" mode); the unit is `After=postgresql.service`, `Wants=postgresql.service`, `Restart=always`, `RestartSec=5`, so a box reboot where Postgres is late retries the slot instead of leaving it down (the 01:28 reboot proved it) | Yes |
| Migration 119 | the four duplicate indexes dropped and their CREATEs emptied in 005/006 (the repo's drop rule); `idempotency_cache.request_digest` + `claimed`; `multipart_uploads.tagging`; additive for the previous build | Yes |
| Idempotency | the cache is keyed by `(tenant_id, idempotency_key)`, never global; the claim row's ready 409 makes the previous build answer "in progress"; a different body under the same key is 422; secrets blanked at any depth before storing | Yes (RP85-1/2 below) |
| The service user | `vaultaire-switch` installs binaries as the unit's `User=`; `.env` `root:vaultaire 0640`; the cutover's checks (process user, `permission denied` = 0, staging under `data/multipart` 0700) | Yes |
| The deletions | prod had 0 rows on `quotaless` / `s3` / `s3compat` in every routing table before the drivers went | Yes |

## Findings

| ID | Sev | Where | What | Fix |
|---|---|---|---|---|
| RP85-1 | P4 | `internal/api/management_routes.go` / `webhooks_routes.go`: `r.Use(im.Middleware)` on the group, `r.With(s.requireRole(…))` on each route | **The idempotency replay runs before Prompt 52's role check.** chi runs a group's `Use` middleware ahead of a route's `With`. A viewer who presents a developer's `Idempotency-Key` on the same tenant gets the developer's cached answer, for example a direct upload's presigned PUT URL, which is a bucket write. A refused attempt's 403 is also stored under its key. The keys are random and chosen by the client, and the request log does not record them, so this is P4. | Test first, `TestIdempotency_KeyedByTheActingMember`: the same key from two members of one tenant is two entries, and the viewer's call is 403 `role_forbidden`, never a replay. Then store the key as `<acting user id>:<key>` (no schema change; a management key of WP-KEY-1 uses its own id). |
| RP85-2 | P4 | `internal/api/idempotency.go` (redaction) | A retried key create or rotate (also STS and a webhook secret) is replayed as 201/200 with the secret blanked, and nothing in the answer says it is a replay. A client that retries a timed-out create gets a key whose secret it never saw and cannot tell why. | Send `Idempotent-Replayed: true` (Stripe's header) on every replay, and add `"secret_redacted": true` where a secret was blanked. `docs/API.md` § Idempotency: a replay never carries a secret; revoke and create again. |

## The residuals, re-checked

- `yaml.v3` (test-only), the unlinked `engine/{analytics,capacity,load_balancer}.go`, and the dashboard's `quotaless` remnants: accepted. The dashboard remnants are now free to remove, since Prompt 52 has merged.
- A tampered chunk's bytes may already be at the backend address: never committed, queued `orphan_write`. Accepted (decision 2).
- The two migration 118 files: the runner applies both in lexical order. Accepted. The next number is 120.
- The two-session hazard: the rule is now in the cutover runbook and in memory (check ListAgents, `who`, `pgrep vaultaire-switch` before a restart or reboot).

## Ops note (another lane's, found during this review)

`deploy/monitoring/vaultaire-jobs.yml` on the box (`faa2e77b…`) is behind the repo (`bd98b8ae…`). WP-VAULT-2's #1065 / #1066 added `VaultParityScrubStale`, `VaultParityScrubBacklog`, `VaultTapeAuditFailed`, `VaultTapeAuditStale` and `VaultParityRestoreStuck`, and they are not installed. This is the vault lane's [OPS] step: run `promtool test rules` on the box, install, reload Prometheus. This review did not touch the box (several sessions were active).

## Stage 1 still owes (unchanged from the note)

- The owner's ceremonies: F-14 (now possible: the slots run as `vaultaire`) and the translog key.
- The TPM-resident KEK WP.
- WP-DOMAIN-1 part 2 (the `.env` click-list).
- The engine-lane leftovers that waited on Prompt 52's files, now free: WP-R12-4, WP-R12-9, WP-R14-3, WP-R12-11/12, and the Trust lane's 43.1 → 43.2 → 43.3. RP84-3's header guard overlaps WP-R14-3 (e-mail hygiene): do it once, in Prompt 53's Part 0.
