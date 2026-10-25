# Encryption at rest: keys, rotation, shredding

Operator runbook for server-side encryption (WP-R8-1, migration 084). Code:
`internal/crypto/tenant_keys.go`, `sse_s3.go`, `chunk_encryption.go`;
`internal/api/tenant_keys_admin.go`, `key_reencrypt.go`. Review note:
`docs/reviews/WP-R8-1.md`.

**State on prod (2026-10-12): `ENCRYPTION_MASTER_KEY` is not set.** Nothing is
encrypted and none of this runs until it is. Since Stage 1a Prompt 2A (the
WP-R8-1 follow-ups) it is set by the on-box ceremony below, never in `.env`.

## The key hierarchy as built

```
ENCRYPTION_MASTER_KEY  (32 bytes; a TPM-sealed systemd credential on prod — never in .env, the database or anywhere off the box)
 └─ wraps (AES-256-GCM, AAD = tenant ‖ version ‖ purpose) ─┐
                                                            ▼
tenant_key_versions  (one row per tenant per version; the newest encrypts)
 ├─ chunk_key_wrapped  32 random bytes ──► convergent chunk key = SHA-256(chunk_key ‖ plaintext hash)
 │                                          → AES-256-GCM per chunk (nonce from the key + the sealed bytes)
 ├─ kem_seed_wrapped   ML-KEM-768 seed ──► per object: encapsulate → shared secret → HKDF → AES-256-GCM
 └─ (chunk_key) ──► HKDF-SHA256(info "stored/credential/v1") → AES-256-GCM ──► api_keys / sts_tokens.secret_wrapped (108)
```

- **Both tenant secrets are random and stored**, not derived from the master.
  That is what makes rotation and shredding real. Before 084, the chunk key
  was `HKDF(master, tenant, "v1")`: any process with the master could recompute
  it, every version was the same function of the master, and deleting nothing
  made it unreadable.
- **Every blob records the version it was sealed under.** A chunk records it
  on its index row (`global_content_index.encryption_key_version`, with a copy
  on each ref). A whole SSE-S3 object records it in its blob header and
  on `object_head_cache.encryption_key_version`. The format is **`0x04`**
  since WP-R8-3 A.2 (`internal/crypto/stream.go`; `0x03` from WP-R8-5 until
  then, the same layout): `[0x04][0x01][version u32][KEM
  ct 1088][segment_size u32][nonce_prefix 7]`, then the object in 1 MiB
  segments, each AES-256-GCM under a per-object DEK with AAD = the header ‖
  the object's tenant, bucket and key, and a nonce of `prefix ‖ index ‖
  last` — sealed and opened as it streams, one segment in memory, so there
  is no size limit and a `Range` reads only the segments it spans. **A blob
  copied to another key's address does not open** (the first segment fails:
  the GET answers 500 before any byte, and the whole-object integrity check
  would cut it if it did); under `0x03` the AAD was the header alone and such
  a blob decrypted and was served as the other key (R8-10). Blobs written
  before (`0x03`, `0x02`: `[0x02][version u32][KEM ct][nonce][ct+tag]`,
  `0x01`: no version) still open; prod held none when 0x04 shipped. A read
  always uses the
  recorded version; a write always uses the newest.
- **Dedup across a rotation.** The same bytes uploaded after a rotation
  dedup onto the existing blob, sealed under the old version, and the new
  reference records that old version. The re-encrypt job moves the blob later.
- **Every writer into an `sse_enabled` bucket seals or refuses** (WP-R8-1
  follow-up A1): PutObject; the generated objects — the GDPR export into
  `_exports`, a delivered access log, an inventory report — sealed exactly as a
  PUT, at any size since WP-R8-5 (only a process without the key refuses one:
  `vaultaire_generated_object_refused_total{kind,reason="sse_unavailable"}`);
  CopyObject of a plaintext source (sealed, at any size; a chunked
  plaintext source 501) and of a sealed source (opened as it streams and
  sealed again for its destination — it was 501); a direct upload (refused — its bytes never pass
  through us; at complete the plaintext already on R2 is removed). A process
  with no master key refuses every write into an `sse_enabled` bucket (503)
  and every encrypted read (500) — R8-04's rule both ways. The `/cdn` path
  opens SSE-S3 objects (it streamed the ciphertext before).
  A multipart upload is placed exactly as a PUT of its bytes (WP-R3-1,
  2026-10-13, `internal/api/placement.go`): chunked and chunk-encrypted when
  a PUT would chunk, sealed whole at any size on a placement that cannot
  chunk (versioned, region-pinned, GLACIER / DEEP_ARCHIVE, RESILIENT,
  PUBLIC, SYNC — over 256 MiB that was a 413 until WP-R8-5), **503** without
  the key — at CreateMultipartUpload and again at Complete. The
  `x-amz-server-side-encryption: AES256` header asks for the same as an
  `sse_enabled` bucket, on a PUT and on CreateMultipartUpload (kept on
  `multipart_uploads.sse_requested`, 089): sealed, or 503 without the key.
  **Public objects are sealed too** (WP-R8-5 decision A5): a bucket made
  public keeps the one rule; nothing at the edge reads R2 raw (Sippy pulls
  through the S3 API, the Workers through `/cdn`), and `/cdn` opens a sealed
  object as it streams — one ML-KEM decapsulation and one AES-GCM pass over
  the bytes per hit (measured in `docs/reviews/WP-R8-5.md`); the bytes cached
  at the edge are the plaintext, as before. A chunked source copied
  into a placement that cannot chunk is re-stored whole there, sealed when
  the destination encrypts.
- **Not encrypted:** the staged parts of an open multipart upload, on the
  box's own disk (`/tmp/vaultaire-multipart`) between UploadPart and Complete
  (≤ 48 h, the reaper; WP-R3-5 moves the directory); multipart objects
  completed before 2026-10-13; SSE-C uses the customer's key and never touches
  this; unencrypted chunks are in the shared `_global` dedup scope; objects
  written before the key was set stay plaintext until overwritten.

## S3 secrets wrapped under the tenant's key (plan 43.7, migration 108)

SigV4 needs the secret itself, so `api_keys.secret_key` and
`sts_tokens.secret_key` were plaintext: a dump of one table signed as every
tenant through the front door. Since Stage 1a Prompt 42 (PR B):

- **The wrap.** `crypto.TenantKeys.SealCredential` / `OpenCredential`
  (`internal/crypto/credential_wrap.go`) — the ONE seam, which the key broker
  (43.7, F-20) later replaces: the key is HKDF-SHA256 of the tenant version's
  chunk key (no salt, info `stored/credential/v1`), AES-256-GCM, format
  `[0x01][nonce 12][ciphertext+tag]`, AAD `"vaultaire-credential" ‖ 0 ‖
  tenant ‖ 0 ‖ key id ‖ 0 ‖ version (u32 BE)` — a wrapped secret moved to
  another row, key or tenant never opens; a name holding a NUL is refused.
  No new key and no new `tenant_key_versions` column (contract rule 4: no
  key outside the one hierarchy). The format is pinned by a vector computed
  with Python's `cryptography` (`TestCredentialSeal_GoldenAndRefusals`).
- **Writers** (registration's primary, a new key, a rotation's successor, an
  STS mint) store `secret_wrapped` + `secret_key_version` when the server has
  a master key — and still the plaintext until the `credential_plaintext_drop`
  flag (PR C). A seal that fails refuses the write.
- **Readers** (`auth.LookupCredential`, `Auth.PrimaryPair`; every `auth.Auth`
  is built in `Server.s3Auth`): wrapped present → it is opened, and only it;
  a failure is an Error line, `vaultaire_credential_unwrap_failures_total{table}`
  and a **500** — never a fallback to the plaintext column. Wrapped absent →
  the plaintext.
- **The backfill** is a pass of the `key_reencrypt` job (every 10 min, ≤ 1,000
  rows a run): live rows (`api_keys` not revoked, `sts_tokens` not expired)
  holding only the plaintext are sealed, opened back and compared, then written
  guarded on what was read (a row another process wrapped meanwhile is left).
  Not at lookup: a write on the SigV4 path is a failure mode on the hottest
  read, and a key nobody presents would never be wrapped. Result fields
  `credentials_wrapped`, `credentials_rewrapped`, `credential_failures`
  (`credentials_raced`) in `key_reencrypt.result`.
- **No master key = nothing wrapped** (prod until F-14): the writers write the
  plaintext only and the pass does not run. After the ceremony and one
  restart, the first run(s) wrap every live row.
- The process no longer loads any secret into memory at boot (and keeps none
  for a key it creates): every credential decision reads its row per request.
- **The drop (PR C, flag `credential_plaintext_drop`, default OFF, global
  only).** On: the writers store no plaintext where they wrapped (api_keys,
  sts_tokens, the tenants mirror — a server with no master key still writes
  the plaintext: a secret is never stored nowhere), and each `key_reencrypt`
  run nulls the plaintext of every row whose wrap opens back to exactly it,
  of every dead row (a revoked key, an expired token: none authenticates), and
  the tenants mirror once the live primary has none
  (`credentials_plaintext_dropped`, `mirrors_plaintext_dropped`). A wrap that
  does not open keeps its plaintext (and is counted in `credential_failures`).
  **Turning it on is refused** — 409 `master_key_absent` without a master key,
  409 `credentials_not_wrapped` (with the count) while a live row holds only
  the plaintext — on the admin API and the dashboard alike; nothing is
  written. The owner's order: the F-14 ceremony, one deploy (or restart), a
  `key_reencrypt` run reporting the backlog wrapped, then the flag. After it
  a pg_dump holds no usable S3 secret without the master key
  (`TestDump_RestoredWithoutTheMasterKeyHasNoUsableSecret`). A rollback to a
  build before PR B is then impossible; `vaultaire-switch` refuses one
  (`docs/DEPLOY.md` § Zero-downtime deploys, "Rollback past the wrapped S3 secrets").

## Setting the master key (the on-box ceremony)

The key is generated ON the box, sealed to the box's TPM and OS install with
`systemd-creds`, handed to the service by systemd, and escrowed as three
encrypted shares. Its plaintext never leaves the box: not a laptop, not a
password manager, not a chat, not `.env`. What it protects against is the
storage vendors, a leaked vendor key and a backend backup — not us: the
service unwraps tenant keys to serve every GET, so whoever controls the host
or the database can read Normal-mode data. "Can't read it" exists only
client-side (SSE-C today, Vault mode F-12 later); the copy says "encrypted at
rest with keys we hold", never "zero-knowledge".

**Step 0, immediately before the ceremony: clear the test data** (owner
decision 2026-10-13). The key encrypts new writes only — no job ever
encrypts an object stored before it, and only buckets created after it get
`sse_enabled` — so whatever is stored at that moment stays plaintext for
ever. On 2026-10-13 prod held only test data (4 tenants, ~19 GB, 0 Stripe
customers, nothing encrypted): two old bench accounts, the bench tenant, and
the retired LET demo (letplay; embedded nowhere). Done last, right before the
key, so nothing plaintext lands in between.

1. **Erase the three accounts** through the account-deletion runner (every
   object deleted on its recorded backend, every backend swept, rows erased,
   key rows shredded, an `account.erased` audit row each):

   ```sql
   UPDATE users u
      SET status = 'pending_deletion', deletion_scheduled_at = NOW() - INTERVAL '1 minute',
          deletion_reason = 'pre-master-key test data wipe'
     FROM tenants t
    WHERE t.email = u.email
      AND t.id IN ('tenant-357e3a63382e091f',   -- old bench, 11 GB
                   'tenant-f4294518dc0a7a8f',   -- old bench, 5.7 GB
                   'tenant-cb05d8eeb2093573');  -- LET demo (letshow), 67 MB
   ```

   then `POST /api/v1/admin/jobs/account_deletion/run` (admin JWT) or wait
   for the 04:30 UTC run. Done when `job_runs` says `ok` for
   `account_deletion` and each tenant has its `account.erased` row; a tenant
   it defers (a backend unreachable) fails the run on purpose — run it again.
2. **Empty the bench tenant and drop its buckets** (`tenant-14a623b16b3f7012`,
   the `VAULTAIRE_BENCH_*` key): for every bucket, delete every version and
   delete marker (`aws s3api list-object-versions` → `delete-objects`), then
   `aws s3 rb`. The account and its keys stay; buckets recreated after the
   key are `sse_enabled`.
3. **Check:** `SELECT count(*) FROM object_head_cache` and
   `SELECT count(*) FROM object_versions WHERE storage_key IS NOT NULL` → 0.
   What is left is unreachable and goes on schedule: displaced blobs after
   the `retired_blobs` grace (24 h), chunk blobs after dedup GC's 7-day grace
   (`dedup_gc`, 02:30 UTC). Each vendor's own deletion timing applies to
   their copies.
4. Outside the service: the letplay app (Maxko) and `~user1/letshow` on the
   box can go too (no cron writes to the demo any more, checked 2026-10-13).

Prerequisites (once): the build of WP-R8-5 Part A (streaming SSE: no size
refused for encryption, sealed copies, sealed parity) deployed (`/version`)
— it includes Stage 1a Prompt 2A;
`deploy/systemd/vaultaire@.service` of the same commit installed;
`apt install libtss2-rc0 ssss` (systemd-creds needs the TPM library —
`systemd-creds has-tpm2` must say `yes`, not `partial`) and **age ≥ 1.3.0**
from upstream (the release binary checked against its published checksum, or
`GOOS=linux GOARCH=amd64 go install filippo.io/age/cmd/...@v1.3.2` on a laptop and copied): Ubuntu
26.04's `age` package is 1.2.1, which has no post-quantum keys; three age
recipients whose identities are offline (a hardware token via
`age-plugin-yubikey`, a paper identity in a safe, a third holder), their
public strings at hand. **Post-quantum escrow (2026-10-22):** the shares
leave the box and are kept for years, so they are the one place a
"harvest now, decrypt later" attacker could store what we encrypt today. At
least TWO of the three recipients are hybrid post-quantum `age1pq1…` keys
(`age-keygen -pq`: ML-KEM-768 + X25519 through HPKE, the same suite as the
Private key core). A hardware token stays classical (`age-plugin-yubikey` is
P-256, and age refuses to mix a PQ and a classical recipient in one file) —
which is safe in a 2-of-3 split: one share alone reveals nothing, so a
quantum attacker holding only the classical share learns nothing. Decide whether the slots move off `user1` to a dedicated service user
first (the credential directory is readable by the unit's user only; `user1`
is also the deploy SSH identity) — the ceremony works either way.

Run it in one root shell on the box (`sudo -i`; the sudo password typed at
the prompt, never on a command line), logged (`script -q /root/master-key-ceremony-$(date -u +%F).log`
— the log is the record; the key is never echoed into it):

```sh
umask 077
install -d -m 0700 -o root -g root /etc/vaultaire/credentials
K=$(openssl rand -hex 32)                     # lives in this shell only

# 1. Seal it for systemd (TPM + this OS install; no PCR binding — an OS
#    upgrade must not lock it).
printf %s "$K" | systemd-creds encrypt --with-key=tpm2+host \
    --name=encryption_master_key - /etc/vaultaire/credentials/encryption_master_key.cred
systemd-creds decrypt --name=encryption_master_key \
    /etc/vaultaire/credentials/encryption_master_key.cred - | cmp - <(printf %s "$K") && echo sealed-ok

# 2. Escrow: 2-of-3 Shamir shares, each encrypted to a different offline
#    recipient. Only the encrypted shares leave the box.
printf %s "$K" | ssss-split -t 2 -n 3 -x -q > /root/shares.txt
i=0; for r in age1pq1RECIPIENT_ONE age1pq1RECIPIENT_TWO age1yubikey1RECIPIENT_THREE; do
  i=$((i+1)); sed -n "${i}p" /root/shares.txt | age -r "$r" -a > /root/master-key-share-$i.age
done
shred -u /root/shares.txt
unset K
```

3. Copy `master-key-share-{1,2,3}.age` to three places (one per holder; they
   are useless alone), then `shred -u /root/master-key-share-*.age`.
4. Uncomment the `LoadCredentialEncrypted=encryption_master_key:…` line in
   `/etc/systemd/system/vaultaire@.service`, `systemctl daemon-reload`.
5. `sudo vaultaire-switch restart` (zero downtime: the idle slot starts with
   the key, takes the traffic, the old slot is drained and stopped).
6. Check the new slot's log: `master key: credential source=credential`,
   `encryption initialized: SSE-S3 …`, `tenant keys checked rows=0
   unwrap_failures=0`. No `ignored` Warn (an `ENCRYPTION_MASTER_KEY` left in
   `.env` would be named there — remove it). A credential that is present but
   unreadable or malformed refuses the boot (`encryption master key: refusing
   to start`): the switch then leaves the active slot untouched.
7. What changes: **new** buckets get `sse_enabled`; objects in such a
   bucket, or sent with `x-amz-server-side-encryption: AES256`, are sealed
   whole at any size (streaming, `0x04`) where they are not chunked; chunked objects are sealed per chunk in the tenant's own
   dedup scope; a tenant's first key version is created on its first
   encrypted write. Existing objects are not touched.
8. **Prove the identity binding (WP-R8-3 A.2), with the bench tenant:** PUT
   two objects A and B of the same size with
   `x-amz-server-side-encryption: AES256`; on the backend, with the
   backend's own key pair, copy A's blob (`object_head_cache.storage_key` of
   A) over B's address; GET B through Stored → **500 before any byte** (the
   `encrypted object does not open` Error line names B) — never A's bytes
   under B's ETag; GET A → 200 byte-equal; delete both. Then one
   `aws s3 cp` (UploadPartCopy) of a sealed object of 100 MB (RP12-3).

`ENCRYPTION_MASTER_KEY_FILE=<path>` (and `ENCRYPTION_MASTER_KEY` itself) still
work — precedence credential > file > env, each `_PREVIOUS` the same — for
labs and tests. Prod uses the credential.

**Recovery (box lost or TPM reset):** a logged ceremony — two holders decrypt
their shares on the new box (`age -d -i <identity> share-N.age`), `ssss-combine
-t 2 -x` gives the key, it is sealed for the new box exactly as in step 1, and
the use is recorded in the transparency report (43.4). Re-split any time (new
holders) without touching data: run step 2 again from a recovered key.

**Suspected exposure of the key:** re-key (below) with the exposed key as
PREVIOUS. No data is re-encrypted; the exposed key opens nothing once every
row is re-wrapped and the backups older than the re-key have aged out.

## Rotating one tenant's key

```
curl -X POST -H "Authorization: Bearer <admin JWT>" -H "Idempotency-Key: $(uuidgen)" \
  https://stored.ge/api/v1/admin/tenants/<tenant_id>/encryption-key/rotate
→ 200 {"tenant_id":"…","key_version":N}
```

- 404 `tenant_not_found`. 503 `encryption_disabled` means no master key on that
  slot. A retry with the same `Idempotency-Key` replays the answer and does not
  rotate twice. Audit row: `admin.key_rotate` with `key_version`.
- New writes use version N at once on the slot that answered, and within
  60 seconds on the other slot (its cached "current" number expires). A write in
  that minute records the old version, so it stays readable, and the job moves it.
- **Chunks:** the `key_reencrypt` job runs every 10 minutes and works through up
  to `KEY_REENCRYPT_MAX_GB_PER_RUN` (50) GB per run. For each chunk it reads
  the blob, checks its ciphertext hash, opens it under the old version, seals it
  under N, and writes it to a **new** key `_chunks/<tenant>/<hash>.k<N>`. It then
  swaps the index row under the chunk's lock and keeps the old address for an
  hour in `retired_chunk_blobs`. A later run deletes it. A chunked GET that
  resolved the old address and finds it gone re-reads the chunk's row once and
  continues at the new blob (`vaultaire_chunked_get_rereads_total{outcome=moved}`),
  so a download longer than the grace is not cut; the hour covers a reader that
  cannot re-read (an older build during a switch). Only `ready` index rows are
  candidates and only a `ready` row is swapped (WP-R8-2, migration 093): a
  `pending` row's blob is still being written by the chunk store, which seals
  it under the version current at that moment. Watch the backlog drain:
  `vaultaire_key_rotation_pending{kind="chunk"}` → 0, and
  `GET /api/v1/admin/jobs` → `key_reencrypt.result`. Rule:
  `KeyReencryptBacklogStuck`.
- **Whole SSE-S3 objects and retained versions** (WP-R2-1 C2,
  `key_reencrypt_whole.go`): the same job, after the chunks, on what is left
  of the SAME per-run budget. Each blob — a head row's or a retained
  version's (`object_versions.storage_key`) — is read from the backend its row
  records, checked (the header names the row's version; the plaintext
  matches the ETag — after the write, as both stream), re-sealed under N as
  `0x04` for its own key (a `0x03`/`0x02`/`0x01` blob too: a rotation is what
  upgrades the format; no format-only pass is scheduled — prod held none) and written to a **new** address
  `<key>@<26>` on the same backend. One transaction (head-row lock, then
  version lock) re-points every row still naming the old address at the old
  version; the ETag does not change. The old address goes to `retired_blobs`
  as `reencrypted` (24 h grace, then the `retired_blobs` collector deletes it
  after its re-check) — never overwritten, never deleted in the run. A write
  or delete that changed the rows first wins: the new blob is queued
  `orphan_write`. **Skipped and left pending, by design** (the run's note and
  `key_reencrypt.result.objects_skipped`): objects on the tape backend
  (`archive` — a read would be a restore), an open Smart-demotion row
  (`demotion`), a `vault_parity` row (`parity` — the shards are computed from
  the stored ciphertext, so re-sealing would need the parity redone; vault
  objects are on tape anyway), the `_exports` system bucket (`export`, 7-day
  objects), and a backend that is unregistered or behind an open breaker
  (`unavailable`). `vaultaire_key_rotation_pending{kind="whole"}` counts
  blobs (head rows + retained versions; the current version is the head row's
  blob, counted once) and therefore never reaches 0 while skipped objects
  exist — which is why `KeyReencryptBacklogStuck` watches `kind="chunk"`
  only. Metric: `vaultaire_key_reencrypt_objects_total{outcome}`.
- **S3 secrets** (plan 43.7): the same job re-wraps every live wrapped
  secret below N (opened under its version, sealed under N, checked, written
  guarded on the old wrap): `vaultaire_key_rotation_pending{kind="credential"}`
  → 0, `credentials_rewrapped` in the run's result. A wrap that does not open
  is counted in `credential_failures` and left as it is (it already answers
  500).
- The old version's key row stays (retired, `retired_at` set): it is needed
  to read whatever is still sealed under it. Deleting a retired version's row
  while anything is sealed under it destroys that data.

## Rotating the master key

No data is re-encrypted: only the wrapping of each tenant key row changes.
The wrapped S3 secrets (108) are untouched too: they hang off the chunk key's
bytes, which a re-wrap keeps (`TestTenantKeys_CredentialRoundTripAndShred`).

1. On the box, as in the ceremony: hand the current key over as the previous
   one, and seal a new key as the current one (then escrow the new key —
   ceremony step 2/3):

   ```sh
   systemd-creds decrypt --name=encryption_master_key /etc/vaultaire/credentials/encryption_master_key.cred - \
     | systemd-creds encrypt --with-key=tpm2+host --name=encryption_master_key_previous - \
         /etc/vaultaire/credentials/encryption_master_key_previous.cred
   K=$(openssl rand -hex 32)
   printf %s "$K" | systemd-creds encrypt --with-key=tpm2+host --name=encryption_master_key - \
     /etc/vaultaire/credentials/encryption_master_key.cred.new
   # … escrow $K (ceremony step 2), then:
   mv /etc/vaultaire/credentials/encryption_master_key.cred.new /etc/vaultaire/credentials/encryption_master_key.cred
   unset K
   ```

   Uncomment the `encryption_master_key_previous` line in the unit,
   `systemctl daemon-reload`.
2. `sudo vaultaire-switch restart`. The new slot's boot re-wraps every row
   that opens only under the previous key (one Info line: `tenant keys
   re-wrapped under the new master key … rewrapped=N`;
   `vaultaire_tenant_keys_rewrapped_total`). Rows opening under neither key
   produce an Error line naming them (`tenant vN`), the gauge
   `vaultaire_tenant_keys_unwrap_failures` and the critical rule
   `TenantKeysUnwrapFailing`: stop there and find the right key.
3. **The old slot keeps serving after that check** — between the new slot's
   boot and the switch, and while it drains (up to `LONG_OP_TIMEOUT`, 900 s).
   A tenant's first encrypted write there wraps its key under the OLD master,
   and a row the new slot re-wrapped is unreadable to the old slot (an
   uncached load there is a 500, never ciphertext). The new slot catches such
   rows twice: at their first use (re-wrapped where it opens them) and in
   every `key_reencrypt` run while PREVIOUS is set (the run re-wraps first:
   `key re-encrypt run master_rekey=true rewrapped=N unwrap_failures=M`,
   `GET /api/v1/admin/jobs` → `key_reencrypt.result`).
4. **Remove PREVIOUS only after** `vaultaire-switch restart` has returned (it
   stops the old slot) **and** a `key_reencrypt` run that started after that
   (`POST /api/v1/admin/jobs/key_reencrypt/run`, then the jobs page) — or a
   restart with PREVIOUS still set — reports `rewrapped=0 unwrap_failures=0`.
   Then comment the `_previous` line out, `systemctl daemon-reload`,
   `sudo vaultaire-switch restart`, and check `tenant keys checked …
   unwrap_failures=0`. Removing PREVIOUS after only the boot check (the old
   runbook) could leave a row no key opens: that tenant's every encrypted
   read and write 500 until PREVIOUS is put back.
5. Destroy the old key (its credential file and its escrow shares) only after
   the database backups older than the re-key (7 days on the box, 30 days
   off-box) have aged out: they hold rows wrapped under it.

A wrong master key never serves ciphertext: every encrypted read of an
affected tenant answers 500 (R8-04's rule).

## What shredding does and does not cover

An account erasure (`internal/api/deletion_runner.go`) deletes the tenant's
`tenant_key_versions` rows inside `account.EraseRows`, after the objects are
deleted and the backends swept. The `account.erased` audit row counts them as
`keys_shredded`, and the process that ran the erasure drops its cached copies.

- **Covered:** every byte sealed under those keys, wherever it still is. That
  includes the wrapped S3 secrets of the account (108: a dump taken before
  the erasure holds `secret_wrapped` bytes that no key opens after it —
  `TestCredentialWrap_ErasureShredsTheWrappedSecret`), a stale copy the sweep did not reach, a chunk blob in the shared
  `_global` container (dedup GC deletes it later), a retired chunk address, a
  backend's own versioning or backup, and a copy someone took of a blob.
  Without the key row it cannot be opened, master key or not.
- **Not covered:**
  - Plaintext objects. Everything stored before encryption was turned on,
    multipart uploads completed before WP-R3-1 (2026-10-13), and unencrypted
    buckets are deleted, not shredded.
  - Database backups: the key rows are in every dump taken before the
    erasure (7 days on the box, 30 days off-box). Until those age out, the old
    master key plus an old dump can open the blobs.
  - Another slot's process memory: unwrapped keys are cached there for at most
    an hour and are not reachable through the API (no row routes to them).

## Client-side keys (Private mode, the SDK key core)

Everything above is **Standard** mode: keys we hold. **Private** mode is the other hierarchy (keystone K2, `sdk/go/e2ee`, plan 10.0, F-12): every key below is made on the customer's device, and the server stores only what is wrapped under keys it does not have. It is one Go package compiled unchanged to WebAssembly (`sdk/wasm`, `sdk/js/e2ee`); no surface makes a key, a wrap or a derivation of its own. Nothing here runs on the server for customer data. It is built and tested (K2b, 2026-10-22, `docs/reviews/WP-P31.md`) and used by no surface yet.

```
password ─ Argon2id (memory, iterations, lanes, salt: in the wrap's header) ─▶ KEK
                                                                               │
                                                     0x04 password wrap ───────┤
recovery key (32 B, shown once as 24 BIP-39 words) ── box "master-wrap/recovery"┼─▶ MASTER KEY (32 B, random, on the device)
  ◀── box "recovery-key" under the master key (shown again when signed in)     │
  ◀── box "recovery-wrap/vault" under the VAULT KEY ◀── the K5 realms, PIN      │
                                                                               ├─ box "identity/enc" ─▶ X-Wing private key   (public key on the server)
                                                                               ├─ box "identity/sig" ─▶ Ed25519 seed         (public key on the server)
                                                                               └─ box "space-key"‖id ─▶ SPACE KEY (the owner)
a member's X-Wing key ── 0x05 share: HPKE X-Wing seal + the sender's Ed25519 signature ─▶ SPACE KEY (a member)
SPACE KEY ─ box "object-key"‖space‖object ─▶ OBJECT KEY ─ box "manifest"‖space‖object‖version ─▶ MANIFEST v1
MANIFEST v1: the ordered chunks (keyed id, SHA-256, chunk key, sizes), the total, the previous manifest's hash
chunk key = HKDF(space key, SHA-256(chunk)) ─▶ 0x02 chunk (dedups within the space, never across)
```

- **Password change** re-wraps the master key under a new KEK; **recovery** opens the master key with the recovery key (the 24 words, or the vault key the realms release under the PIN) and sets a new password. Neither touches a space, an object or a chunk.
- **Argon2id parameters** travel in the wrap. `Sensitive` (256 MiB, t=3, p=1) is the default; `Interactive` (64 MiB, t=3, p=1) is the floor, and a wrap under it is refused before any derivation, so the server cannot hand a device weaker parameters.
- **What the server holds and learns:** the wraps (it opens none), both public keys of each identity, which fingerprints shared which space and when, every ciphertext, and of each chunk its keyed id and its length.
  - The length is len + 41 in every space, and the chunker is public. So a file of two or more chunks that is public elsewhere can be **confirmed** in a Private space by its lengths alone. The fix is the owner's decision **F-35** (measured in WP-P31).
  - It never sees a password, a key, a plaintext chunk hash, a name or a path.
- **No claim beyond that:** no "zero-knowledge" (that waits for an external audit, contract line 8), and no "keys are wiped" (WebAssembly never zeroes memory).
- **Still to build:** SRP/OPAQUE login (10.0.5), the browser upload path (10.0.2), the chunker in WASM, the paranoid flag (random chunk keys), key transparency for fingerprints (K4b). A post-quantum signature waits for **F-34**; the envelope's `0x02` is held for it.

## Where to look

| What | Where |
|---|---|
| Key rows | `SELECT tenant_id, key_version, created_at, retired_at FROM tenant_key_versions ORDER BY 1,2;` |
| Chunk backlog | `vaultaire_key_rotation_pending{kind="chunk"}` |
| Whole-object backlog | `vaultaire_key_rotation_pending{kind="whole"}` |
| Job | `GET /api/v1/admin/jobs` → `key_reencrypt`; `vaultaire_key_reencrypt_chunks_total{outcome}`, `vaultaire_key_reencrypt_objects_total{outcome}` |
| Old addresses awaiting delete | chunks: `SELECT * FROM retired_chunk_blobs ORDER BY retire_after;` — whole objects: `SELECT * FROM retired_blobs WHERE reason = 'reencrypted' ORDER BY retire_after;` |
| Boot check | `journalctl -u 'vaultaire@*' \| grep -E 'tenant keys\|master key'` |
| Re-key progress | `key re-encrypt run master_rekey=true rewrapped=… unwrap_failures=…` in the log; `key_reencrypt.result` on the jobs page |
| Chunked GETs that followed a moved chunk | `vaultaire_chunked_get_rereads_total{outcome}` |
| Generated objects refused (not stored plaintext) | `vaultaire_generated_object_refused_total{kind,reason}` |
