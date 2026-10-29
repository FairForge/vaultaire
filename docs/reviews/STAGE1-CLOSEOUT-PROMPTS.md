# Stage 1 closeout — the engine lane beside the Stripe layer

*Written 2026-10-29 by the RP82 session (owner: "start Prompt 52 and give me another prompt I start at the same time"). The plan's order puts the Stripe layer "first after Stage 1a"; the chain moved to Phase 40 at Prompt 44 and to WP-STRIPE at Prompt 49 with Stage 1 items still open. The plan's lane rule allows one deep code lane and one surface lane at once: **Prompt 52 (WP-TEAM-1) is the surface lane; this prompt is the engine lane, run at the same time.***

*What Stage 1 still owes, read 2026-10-29 against `origin/main`, prod and `docs/reviews/SYNTHESIS.md`:*
- *owner / ops: the F-14 master-key ceremony (prod has no credential; the service runs as `user1`), the translog signing-key ceremony, WP-DOMAIN-1 part 2 (prod still answers as stored.ge);*
- *code: the rows of SYNTHESIS table B still `open` — WP-R15-1, WP-R11-2, WP-R3-6, WP-R1-10, WP-R9-3, WP-R9-5, WP-R7-3, WP-R0-1, and the ones that collide with Prompt 52 (WP-R12-4, WP-R12-9, WP-R14-3, WP-R12-11/12); the Trust lane's 43.1–43.3; the dedicated service user and the TPM-resident KEK of the F-14 row.*

*This prompt takes the engine-lane code that does not touch Prompt 52's files. The rest is listed at the end.*

## Stage 1a Prompt 52E — Opus edition

You are closing the plan's Stage 1 ("Foundations") in the Vaultaire repository, in the **Engine lane**, while another session builds Prompt 52 (WP-TEAM-1) in the Developer-surface lane at the same time. Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`; your own test DB per the repo's rules. **Migration 119** (Prompt 52 holds 118).

**Coordination with Prompt 52 (hard rules):**
- Do NOT edit these files:
  - `internal/auth/auth.go`, `internal/auth/members.go`, `internal/auth/apikey.go`;
  - `internal/api/management_routes.go`, `internal/api/platform_*.go`;
  - `internal/dashboard/**`;
  - `internal/api/account_export_sections.go`;
  - `internal/account/**`, `internal/audit/**`, `internal/email/**`.

  If a fix needs one of them, stop that step, note it, and move on.
- Rebase on `origin/main` before every PR. If Prompt 52 merged first, re-run the touched packages' tests after the rebase.
- Migration 119 only. If you need none, say so in the closeout.

Read before editing:
- `docs/reviews/SYNTHESIS.md` § B, the rows named below (each row's files, size and the finding ids);
- `docs/reviews/R15-tests-ci-synthesis.md` (the accepted deviation behind WP-R15-1);
- `internal/api/s3_engine_adapter.go` (`awsChunkedReader`, line ~205, and its two callers: PutObject ~1148, UploadPart in `s3_multipart.go` ~353);
- `internal/auth/sigv4.go` (the seed signature, `canonicalRequestV4`, the signing-key derivation);
- `internal/api/edge_ingest.go` (`edgeStreamingOf`: the edge admits only `STREAMING-UNSIGNED-PAYLOAD-TRAILER`; every other stream is the origin's);
- `internal/api/idempotency.go`;
- `internal/database/postgres.go`, `cmd/vaultaire/main.go`;
- `.github/workflows/{deploy,ci}.yml`, `Makefile`;
- `deploy/systemd/vaultaire@.service`, `deploy/scripts/vaultaire-switch`, `docs/DEPLOY.md`.

### PR A — the body is what was signed (integrity). Branch `stage1a-p52e-chunks`.

1. **WP-R15-1, chunk signatures.** Test first:
   - `TestAWSChunked_SignedChunksVerified`: AWS's published example ("Transferring Payload in Multiple Chunks": the example secret, 64 KiB chunks, the documented seed and chunk signatures) and a raw aws-cli or boto3 capture against a known test secret are each accepted byte-identical;
   - the same body with one payload byte flipped is 403 `SignatureDoesNotMatch` and nothing is committed (no head row; the address queued `orphan_write` where bytes reached a backend);
   - a chunk whose `chunk-signature` is missing is refused the same way;
   - `STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER` verifies `x-amz-trailer-signature` too.

   Then:
   - the verifier hands the request a chunk checker built from the seed signature and the scoped signing key. A closure in `auth`; the secret never leaves `auth`; the key is never logged.
   - `awsChunkedReader` checks each chunk's `HMAC(signingKey, "AWS4-HMAC-SHA256-PAYLOAD\n"+date+"\n"+scope+"\n"+prevSig+"\n"+sha256("")+"\n"+sha256(chunk))` as it reads, the previous signature chained, and fails the read on a mismatch (`errAWSChunkedSignature` → 403).
   - Presigned and `UNSIGNED-PAYLOAD` requests are unchanged.
   - Add `TestEdgeAdmit_SignedChunkStreamsPassThrough` (a signed-chunk stream is the origin's at the edge, as today).

   Verify: `go test -race ./internal/auth/ ./internal/api/ -run 'AWSChunked|SigV4|EdgeAdmit'`.
2. **The trailer checksum at the origin.** Today `awsChunkedReader` discards trailing headers, so the `x-amz-checksum-*` trailer that aws-cli ≥ 2.23 sends on every upload (`STREAMING-UNSIGNED-PAYLOAD-TRAILER`) is never checked on the origin path. (The edge path has its backend check it: `edgeVerifiedTrailers`.)

   Test first: `TestAWSChunked_TrailerChecksumVerified`:
   - for crc32, crc32c, crc64nvme, sha1 and sha256 (golden values from the AWS SDK), a correct trailer stores the object;
   - a wrong one is 400 `XAmzContentChecksumMismatch` (`ErrXAmzContentChecksumMismatch` exists) with nothing committed;
   - an `x-amz-checksum-<alg>` request header on a plain PUT is checked the same way.

   Then: compute the declared algorithm over the decoded bytes as they stream (beside the MD5 tee of WP-R8-3) and compare at the terminal chunk, before the commit.

   Verify: `go test -race ./internal/api/ -run 'Checksum|AWSChunked'`. Also prove it end to end with aws-cli against a `/verify` server (default `cp`, and `--checksum-algorithm SHA256`).

### PR B — boot and migrations. Branch `stage1a-p52e-boot`.

3. **WP-R1-10, the boot DB contract.** Test first: `TestBoot_DeadDatabaseFailsFast` (an unreachable DSN: `NewPostgres` + the `NewServer` prologue return an error within 12 s; today it logs "connected"). Then:
   - `PingContext` with a 10 s bound in `NewPostgres`;
   - a `bootCtx` deadline on the `NewServer` prologue;
   - `main` exits non-zero, so `vaultaire-switch` keeps the old slot.

   CLAUDE.md's "never pinged at boot (R9-06 / WP-R9-4)" sentence changes with it.
4. **WP-R9-3, the migration runner.** `PGOPTIONS='-c lock_timeout=5s -c statement_timeout=600s'` in `deploy.yml`, `ci.yml` and the Makefile's `test-db`.

   Migration `119_index_hygiene.sql`:
   - `DROP INDEX IF EXISTS` for each duplicate index the R9 finding names. Re-check each against `pg_indexes` on a migrated test DB first: same table, same columns, same predicate. A name you cannot prove is a duplicate stays.
   - The rule "a new index on a big table is `CREATE INDEX CONCURRENTLY` in its own file, outside a transaction" goes into `internal/database/migrations/README` (create it if absent).

   Test: the reapply test passes twice over 001–119.

### PR C — API correctness. Branch `stage1a-p52e-api`.

5. **WP-R11-2, idempotency.** Today the same key with a different body replays the first answer, and cached bodies hold key secrets for 24 h.

   Test first, `TestIdempotency_ClaimFirstAndBodyHash`:
   - two concurrent requests with one key: one runs, the other waits or gets 409 `idempotency_in_progress`;
   - the same key with another body is 422 `idempotency_key_reused`;
   - a cached answer of a route that returns a secret stores no secret: the replay answers the original status with the secret fields blanked and `"idempotent_replay": true`, never the secret.

   Then: migration 119 adds `idempotency_cache.request_hash` (sha256 of method + path + body), a claim row inserted before the handler runs, and a redaction list of the response fields that carry a secret (find them: every key-create, key-rotate and STS answer). Change only `internal/api/idempotency.go` and the migration.
6. **WP-R3-6, conformance.** Test first, against aws-cli's and boto3's own request shapes (pinned as fixtures):
   - ListParts (`max-parts`, `part-number-marker`, `IsTruncated`, `NextPartNumberMarker`);
   - ListMultipartUploads (`key-marker`, `upload-id-marker`, `max-uploads`, `prefix`, `delimiter`);
   - Complete with a non-last part under 5 MiB is 400 `EntityTooSmall`;
   - `Location` in Complete's answer uses the configured endpoint's scheme;
   - CopyObject's answer has `Content-Length`;
   - `x-amz-tagging` on Create is kept on the upload row and applied at Complete.

   Then the handlers in `s3_multipart.go` / `s3_copy.go`.
7. **WP-R9-5, the `last_accessed` touch.** Test first: `TestLastAccessed_CoalescedPerKey` (100 GETs of one key in a second = at most one UPDATE; the value lands within 60 s; a process stop flushes). Then an in-process map flushed every 60 s in one `UPDATE … FROM unnest(...)`; the edge read commit's touch goes through the same map.

### PR D — dead code. Branch `stage1a-p52e-deletions`.

8. **WP-R0-1** (D-1, D-2, D-4, D-7: delete, the R0 recommendation; the owner starting this prompt is the go). Before and after, keep `go list -deps ./cmd/vaultaire` and `make deadcode`.
   - Delete `internal/{k8s,container,global,ha,slo,integrations}`, `internal/engine/{sla,disaster_recovery}.go`, `internal/drivers/wasm.go`, the legacy `internal/api/metrics.go`, and `cmd/loadtest` + `internal/loadtest` if still present.
   - D-9: `replicator.go` too, per R6.
   - Then `go mod tidy` (`yaml.v3` and `wazero` go if nothing else imports them).
   - Anything still imported by the product binary stays, with the importer named in the closeout.
   - Fix the plan lines that name the deleted files.
9. **WP-R7-3, the Quotaless and generic S3 drivers.** Checked on prod 2026-10-29: 0 head rows and 0 version rows on `quotaless` / `s3` / `s3compat`, and no `QUOTALESS_*` / `S3_ACCESS_KEY` in the prod `.env`. Re-check both before deleting.
   - Delete `internal/drivers/quotaless.go` and the `S3Driver` with its registration, auto-detection (`config.DetectStorageMode`, today iDrive > Wasabi > Quotaless > S3 > Geyser > local) and the env rows in CLAUDE.md.
   - The fixed-bucket S3 driver (iDrive, Wasabi, Lyve, Sia, R2) is NOT this driver: keep it.
   - A `cmd/tools` program that used `S3Driver` is moved to the fixed-bucket driver or deleted, named in the closeout.
   - Test: the boot routing check still passes with the registry it now has.

### PR E — the dedicated service user (F-14's code prerequisite). Branch `stage1a-p52e-user`.

10. **The unit and the files.** On prod today the slots run as `user1` (`/etc/systemd/system/vaultaire@.service`); `/opt/vaultaire`, `data`, `bin` are `user1`; `configs/.env` is `root:640`.

    Change `deploy/systemd/vaultaire@.service` to `User=vaultaire` / `Group=vaultaire` (a system user, no shell, no home). Add `deploy/scripts/vaultaire-user-install.sh`, idempotent, which:
    - creates the user;
    - chowns `data/` (multipart staging, translog high-water, the local driver's tree) and the binaries' directory to it;
    - makes `.env` `root:vaultaire 640`;
    - keeps `user1`'s Sia user units untouched.

    `vaultaire-switch` and the sudoers entries stay as they are unless the user change breaks one (then fix it and say why).

    `docs/DEPLOY.md` gets the cutover and the rollback (`User=user1`, the old ownership). **The cutover on prod** comes after the PR deploys, at a quiet hour:
    1. the install script;
    2. `sudo vaultaire-switch restart` (one slot, then the other);
    3. verify: `/health` ok; a bench-key PUT, GET and DELETE through `stored.ge` and through `s3.stored.cloud`; the multipart staging path writable (a 32 MiB `aws s3 cp`); the next `translog_checkpoint` run `ok`; no `permission denied` in `journalctl -u 'vaultaire@*'`.

    A failure rolls back at once. The master-key credential (`LoadCredentialEncrypted=`) stays the owner's ceremony; this PR only makes it possible.

### PR F — docs.

- `docs/reviews/STAGE1-CLOSEOUT.md`: the decisions, residuals, the prod proofs and what Stage 1 still owes.
- SYNTHESIS table B rows flipped with their PRs.
- The plan's Stage 1 row: what is left, by owner.
- STATUS, CLAUDE.md (the boot contract, the deleted drivers and env rows, the service user, migration 119), the ledger row.
- The post-merge review is **RP85**.

**Still owed after this prompt** (write them into the closeout's "Stage 1 still owes"):
- the F-14 ceremony and the translog signing-key ceremony (owner, on the box);
- the TPM-resident KEK follow-up WP;
- WP-DOMAIN-1 part 2 (the prod `.env` click-list);
- after Prompt 52 merges: WP-R12-4 (audit rows for the dashboard's mutations), WP-R12-9 (OAuth PKCE + consent), WP-R14-3 (e-mail hygiene), WP-R12-11/12, and the Trust lane's 43.1 inventory page (it walks the export's maps, which Prompt 52 extends), then 43.2 disassociation and 43.3 retention clocks (the owner confirms 43.3's targets first).

**Done means:**
- PRs A–F merged and deployed;
- on prod: a tampered signed-chunk body is 403, and a wrong CRC trailer from aws-cli is 400, each with nothing committed;
- a dead DSN fails the boot in a lab slot;
- the service runs as `vaultaire` with every check above green;
- SYNTHESIS table B's engine rows are closed.

## Stage 1a Prompt 52E — chain edition

**Stage 1 closeout, engine lane, beside Prompt 52 (WP-TEAM-1).** Migration 119; never edit Prompt 52's files (the list in the Opus edition). In order, test first each:
1. **WP-R15-1:** verify `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` chunk signatures, and the signed trailer, in `awsChunkedReader` from the seed signature and a signing-key closure that never leaves `auth`.
2. **Trailer checksums:** the origin verifies `x-amz-checksum-*` trailers and headers (crc32, crc32c, crc64nvme, sha1, sha256). Today they are discarded on the origin path.
3. **WP-R1-10:** the boot pings the database (10 s) and fails fast.
4. **WP-R9-3:** `statement_timeout` beside `lock_timeout`; the proven duplicate indexes dropped (migration 119); the CONCURRENTLY rule.
5. **WP-R11-2:** idempotency claim-first plus a body hash (422 on reuse with another body); no secret in a cached body.
6. **WP-R3-6:** multipart conformance.
7. **WP-R9-5:** the coalesced `last_accessed`.
8. **WP-R0-1:** the class-B deletions, D-1/D-2/D-4/D-7/D-9.
9. **WP-R7-3:** the Quotaless and generic S3 drivers (0 rows on prod).
10. **The dedicated `vaultaire` service user:** the unit, the install script, the cutover on prod with its rollback. It is F-14's last code prerequisite.

Closeout `docs/reviews/STAGE1-CLOSEOUT.md` with "Stage 1 still owes". Review RP85.

ultrathink
