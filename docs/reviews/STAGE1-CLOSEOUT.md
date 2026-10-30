# Stage 1 closeout — the engine lane (Stage 1a Prompt 52E)

2026-10-29. Run beside Prompt 52 (WP-TEAM-1) in the Developer-surface lane;
none of Prompt 52's files were edited. Migration **119** (`119_index_hygiene.sql`).
Post-merge review: **RP85**.

| PR | Branch | What | WP |
|----|--------|------|----|
| #1073 | `stage1a-p52e-chunks` | The body is what was signed: chunk signatures + the signed trailer; the trailer / header checksum at the origin | WP-R15-1 (+ the origin trailer check) |
| #1070 | `stage1a-p52e-boot` | The boot DB contract; the migration runner's timeouts; migration 119 | WP-R1-10, WP-R9-3 |
| #1075 | `stage1a-p52e-api` | Idempotency claim-first; multipart conformance; `last_accessed` coalescing | WP-R11-2, WP-R3-6, WP-R9-5 |
| #1076 | `stage1a-p52e-deletions` | The class-B deletions; the Quotaless and generic S3 drivers | WP-R0-1, WP-R7-3 |
| #1077 | `stage1a-p52e-trailer-eof` | A2 — the body is read to its end before a commit (found by PR A's prod proof) | WP-R15-1 |
| #1074 | `stage1a-p52e-user` | The dedicated `vaultaire` service user | F-14's code prerequisite |
| this PR | `stage1a-p52e-docs` | This note, SYNTHESIS, plan, STATUS, CLAUDE.md | — |

## Step 0 answers (what was found before the first edit)

- **aws-cli and boto3 never sign chunks.** A capture of aws-cli 2.28.19
  against a local listener: over TLS it sends `STREAMING-UNSIGNED-PAYLOAD-TRAILER`
  with an `x-amz-checksum-crc64nvme` trailer (`--checksum-algorithm` picks the
  others); over plain HTTP it sends a sha256 payload hash and an
  `x-amz-checksum-*` **header**. boto3 is botocore, the same shapes. The
  signed captures the prompt asked for are therefore minio-go's: `mc cp` over
  HTTP (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) and minio-go's `StreamingSignV4`
  with a CRC32C trailer (`…-PAYLOAD-TRAILER`). minio-go writes the trailer as
  `name:value\n\r\n` before the signature line; the decoder accepts it.
- AWS's documentation pages render only with JavaScript; the published
  example's values were checked by reproducing them: the spec formula over
  the example's canonical request gives the documented seed `4f232c43…` and
  the three chunk signatures `ad80c730…`, `0055627c…`, `b6c6ea8a…` exactly.
- The origin path discarded every trailer: aws-cli's default CRC64NVME was
  never checked at the origin (the edge has its backend check it).
- `.env` on prod is `root:user1 0640` (the prompt said `root:640`), read by
  `user1` for the deploy's migrations, `pg-backup.sh` and `health-check.sh`.
- `/tmp/vaultaire-stripes` (the Sync stripe staging, in use) and
  `/tmp/vaultaire-packs` are `user1` 0700.
- `vaultaire-switch`'s `RUN_USER=user1` was used for two things — the staged
  upload's owner and the installed binaries' owner — so the user change broke
  it (fixed: `DEPLOY_USER` + the unit's `User=`).
- The four duplicate indexes of R9-17 are exactly as named, on the test DB and
  on prod (16 kB each, 244–558 scans): `idx_api_keys_key_id`,
  `idx_tenants_access_key`, `idx_tenants_email`, `idx_users_email`.
- Prod 2026-10-29: 0 rows on `quotaless` / `s3` / `s3compat` in
  `object_head_cache`, `object_versions`, `smart_demotions`, `retired_blobs`,
  `global_content_index`, `vault_parity`; no `QUOTALESS_*`, `S3_*`,
  `S3COMPAT_*` in `.env`; `STORAGE_MODE=idrive`.
- The `s3` registration was `S3CompatDriver` (hard-wired to Quotaless's
  `us.s3compat.cloud`), not `S3Driver`; `S3Driver` was `QuotalessDriver`'s
  base and two bench tools' raw client. All three are Quotaless code.
- R3-25 (`WithContentLength` on the copy's backend PUT) was already fixed
  (`putLength`); the prompt's "CopyObject's answer has Content-Length" was
  done as the answer header.

## Decisions

1. **The chunk verifier rides on the request body** (`auth.StreamChunkVerifier`,
   as `payloadVerifyReader` does for a payload hash): the one verifier attaches
   it after the seed verifies; the signing key lives only in its closure. A
   header-signed stream that reaches a handler without one is refused (fail
   closed). Presigned, `UNSIGNED-PAYLOAD` and the unsigned trailer are unchanged.
2. **Bytes are streamed, then failed** — a chunk's bytes are handed on as they
   arrive and the read that ends the chunk fails on a mismatch; the upload
   fails before its commit and the address that was sent the body is queued
   `orphan_write` (placeObject's rule). A driver that stores the bytes despite a
   failed read is refused after the fact (`src.failed()`). A signature failure
   counts in `vaultaire_auth_failures_total{reason="chunk_signature_mismatch",key_known="true"}`.
3. **A declared trailer that never arrives is 400** `XAmzContentChecksumMismatch`.
4. **No "without a database" mode.** A dead DSN refuses the boot; a server
   with no DB authenticated every request as the test tenant. The `NewServer`
   prologue has one 6 s deadline (worst case ≈ 11 s with `connect_timeout` 5 s)
   and returns at once on a failure (`Server.BootError`).
5. **Migration 119 carries all three schema changes of this prompt** (the
   index drops, `idempotency_cache.request_digest` + `claimed`,
   `multipart_uploads.tagging`): one migration, and a merged migration is never
   edited. The dropped indexes' CREATE was emptied in 005/006 (the repo's rule
   for drops, as 100 did — the deploy re-applies every file); this is the one
   kind of edit to a merged migration the repo allows.
6. **`request_digest`, not `request_hash`**: a `*hash*` column is a
   secret-looking name for `TestExportSectionsCoverSchema`, whose list lives in
   Prompt 52's `account_export_sections.go`; the digest of a request is no secret.
7. **Idempotency claims live in `idempotency_cache`** (no new table: a tenant
   table would need `internal/account`, Prompt 52's). A claim row stores a ready
   409 `idempotency_in_progress`, so the previous build — which replays any row
   — answers "in progress" during a switch. A claim older than 10 min is a dead
   process's and is taken over. The redaction list: `secret`, `secret_key`,
   `secretAccessKey`, `secret_access_key`, `session_token`, `sessionToken`,
   `token` (string values, any depth).
8. **`multipartMinPartBytes`** is a variable only so the api package's
   fixtures (64 KiB parts) keep working; `TestMain` lowers it,
   `TestMultipartConformance` holds the production 5 MiB.
9. **One `last_accessed` coalescer per database** (every per-request adapter
   shares it), armed by a timer on the first pending touch — no idle goroutine —
   bounded at 20,000 keys (an early flush), written by one
   `UPDATE … FROM unnest(…)` that never moves a value back.
10. **The Quotaless code is three files**, deleted together with their
    registration, auto-detection, TCP probe, canary entry, tenant walkers and
    store ids. `cmd/tools/backend-matrix` and `cmd/tools/erasure-bench` moved
    to `NewWasabiDriver`, `NewR2Driver` and `NewFixedBucketS3Driver` (B2): their
    keys are now `t-<tenant>/<container>/<key>` like the product's.
    `cmd/tools/dedup-migrate` lost its two Quotaless branches.
    `cmd/tools/bench-compare` (raw HTTP, no driver) still knows Quotaless's
    endpoints and stays.
11. **`.env` is `root:vaultaire 0640` with `user1` in the group**, so the
    deploy, the backups and the health check keep reading it.
12. **`vaultaire-switch` installs the binaries as the unit's `User=`** (read
    with `systemctl show`), so a rollback to `user1` needs no script edit; the
    staged upload is still `user1`'s. The unit and the switch are hand-installed
    on the box: merging PR E changes nothing until the cutover.

13. **The terminal chunk is read before a commit** (#1077, found on prod):
    the S3-class drivers' single PutObject reads exactly the declared length
    (`putSinglePartIfSmall`), so for every body ≤ 32 MiB the decoder never
    reached its terminal chunk — the last chunk signature, the signed trailer
    and the declared checksum went unchecked. `HandlePut` drains the body after
    the backend write; the SSE-C part path after the sealer. The local driver
    reads to EOF, which is why the unit tests passed: the new red test uses the
    existing `exactLengthDriver`.

## Residuals

- `yaml.v3` stays in `go.mod`: four `internal/api` tests parse the alert
  rules with it (test-only; not in the product binary). `wazero` went.
- `engine/{analytics,capacity,load_balancer}.go` are still present and
  unlinked (D-9 names only the replicator).
- The dashboard (`internal/dashboard/**`, Prompt 52's) still has a `quotaless`
  locality row and a comment naming `s3`/`quotaless` storage modes: harmless,
  for whoever next edits those files.
- A crash loses at most a minute of `last_accessed` touches (an object read in
  that minute looks idle a minute longer; the demotion job counts in days).
- A tampered signed chunk's bytes may already be at the backend address when
  the read fails: never committed, queued `orphan_write`, collected after the
  grace.
- Prompt 52 (#1068) and WP-VAULT-2 (#1065) both landed a migration 118
  (`118_members.sql`, `118_vault_parity_scrub.sql`); the runner's lexical
  order applies both.
- `cmd/tools/stall-s3` bounds its chunk-size allocation (CodeQL alert 119,
  open on main since 2026-10-17, counted against #1075).
- A two-session hazard: a planned reboot by another session met this
  cutover. Ops that restart slots or the box should check `who` /
  `pgrep vaultaire-switch` first; the cutover's runbook now does.

## Open questions

- None blocking. The R3-25 reading (decision list, Step 0) is for RP85 to
  confirm.

## Prod proofs

All on 2026-10-29/30 UTC, bench tenant (`tenant-14a623b16b3f7012`, bucket
`tuning-bench`, the iDrive primary), through Cloudflare + HAProxy, the
objects removed afterwards.

**PR A + A2 (build `37f018d0`).** A Go client sending mc's signed-chunk shape
and aws-cli 2.28's `STREAMING-UNSIGNED-PAYLOAD-TRAILER` shape (headers as
captured), re-signed with the bench key; aws-cli itself for the normal uploads:

| Request | `stored.ge` | `s3.stored.cloud` | After |
|---|---|---|---|
| signed chunks, as signed | 200 | 200 | HEAD 200 |
| signed chunks, one payload byte flipped | **403 SignatureDoesNotMatch** | **403** | HEAD 404; `retired_blobs` `orphan_write` |
| aws-cli trailer, CRC32 right | 200 | 200 | HEAD 200 |
| aws-cli trailer, **CRC32 wrong** | **400 XAmzContentChecksumMismatch** | **400** | HEAD 404; `orphan_write` |
| `aws s3 cp` (default CRC64NVME trailer) and `--checksum-algorithm SHA256` | 200, 200 | — | stored |

The first run (build `7a4c7429`, PR A alone) stored the wrong-CRC body
through iDrive (200) while the local driver refused it: that is #1077.

**PR B.** A lab boot of the binary with a dead DSN exits 1 in 1 s
(`database unreachable at boot — refusing to start`); against a live
database it boots `healthy`. On prod #1070 deployed cleanly (migration 119
applied by the runner with the new `PGOPTIONS`).

**PR C.** aws-cli against a `/verify` server (`VAULTAIRE_ENDPOINT=https://s3.stored.cloud`):
list-parts pages (`IsTruncated`, `NextPartNumberMarker`), auto-pagination,
`EntityTooSmall` for a 5,000,000-byte first part, then success with
`Location https://…`, the Create tag set on the object, CommonPrefixes.
Deployed with `c74c711c`.

**PR E — the cutover (2026-10-30 01:27–01:33 UTC).** The new
`vaultaire-switch` installed, `vaultaire-user-install.sh` run (user created,
user1 in the group, `data/`, `bin/`, `.env` `root:vaultaire 0640`, the unit's
`User=vaultaire`), then `vaultaire-switch restart` twice. **Another session's
planned reboot (kernel/systemd updates, 01:28:04) landed four seconds into the
first restart**: the site answered 503 / no answer for about a minute while the
box rebooted; slot :8000 came back as `vaultaire` from the already-installed
unit. Once that session reported the box free, the two restarts ran: both slots
came up as `vaultaire`, active :8000, both binaries `vaultaire:vaultaire`.
Checks: `/health` ok (21/21 backends); the process user `vaultaire`; a
bench-key PUT, GET and DELETE through `stored.ge` and `s3.stored.cloud`; a
32 MiB `aws s3 cp` (multipart staging under `data/multipart`, `vaultaire` 0700);
`translog_checkpoint` `ok` at 01:33:20 on the new process ("no signing key", as
before the ceremony); `retired_blobs` `ok`; `permission denied` in
`journalctl -b -u 'vaultaire@*'`: 0. Backups of the old unit and switch:
`/root/*.bak-p52e-20261030T012756Z`.

## Stage 1 still owes

- **Owner, on the box:** the F-14 master-key ceremony (`LoadCredentialEncrypted=`
  for `vaultaire@.service` — possible now that the slots run as `vaultaire`);
  the translog signing-key ceremony.
- **A WP:** the TPM-resident KEK follow-up.
- **WP-DOMAIN-1 part 2:** the prod `.env` click-list.
- **After Prompt 52 merges** (its files): WP-R12-4 (audit rows for the
  dashboard's mutations), WP-R12-9 (OAuth PKCE + consent), WP-R14-3 (e-mail
  hygiene), WP-R12-11/12; the Trust lane's 43.1 inventory page (it walks the
  export's maps, which Prompt 52 extends), then 43.2 disassociation and 43.3
  retention clocks — the owner confirms 43.3's targets first.
- **This prompt's leftovers:** the dashboard's Quotaless remnants (above);
  `engine/{analytics,capacity,load_balancer}.go` (D-9's other three, if the
  owner wants them gone).
