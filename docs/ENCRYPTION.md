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
 └─ kem_seed_wrapped   ML-KEM-768 seed ──► per object: encapsulate → shared secret → HKDF → AES-256-GCM
```

- **Both tenant secrets are random and stored**, not derived from the master.
  That is what makes rotation and shredding real. Before 084, the chunk key
  was `HKDF(master, tenant, "v1")`: any process with the master could recompute
  it, every version was the same function of the master, and deleting nothing
  made it unreadable.
- **Every blob records the version it was sealed under.** A chunk records it
  on its index row (`global_content_index.encryption_key_version`, with a copy
  on each ref). A whole SSE-S3 object records it in its blob header (format
  `0x02`: `[0x02][version u32][KEM ct][nonce][ct+tag]`, header authenticated)
  and on `object_head_cache.encryption_key_version`. A read always uses the
  recorded version; a write always uses the newest.
- **Dedup across a rotation.** The same bytes uploaded after a rotation
  dedup onto the existing blob, sealed under the old version, and the new
  reference records that old version. The re-encrypt job moves the blob later.
- **Every writer into an `sse_enabled` bucket seals or refuses** (WP-R8-1
  follow-up A1): PutObject; the generated objects — the GDPR export into
  `_exports`, a delivered access log, an inventory report — sealed exactly as a
  PUT (above 256 MiB refused, never written plaintext: the export goes
  `failed` with a reason the dashboard shows, a log or report is a Warn and a
  job note, `vaultaire_generated_object_refused_total{kind,reason}`);
  CopyObject of a plaintext source (sealed; above 256 MiB 413; a chunked
  plaintext source 501); a direct upload (refused — its bytes never pass
  through us; at complete the plaintext already on R2 is removed). A process
  with no master key refuses every write into an `sse_enabled` bucket (503)
  and every encrypted read (500) — R8-04's rule both ways. The `/cdn` path
  opens SSE-S3 objects (it streamed the ciphertext before).
- **Not encrypted:** multipart uploads stay plaintext until WP-R3-1/WP-R3-3
  (the known caveat; the public copy says so); SSE-C uses the customer's key
  and never touches this; unencrypted chunks are in the shared `_global` dedup
  scope; objects written before the key was set stay plaintext until
  overwritten.

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

Prerequisites (once): the build of Stage 1a Prompt 2A deployed (`/version`);
`deploy/systemd/vaultaire@.service` of the same commit installed;
`apt install libtss2-rc0 ssss age` (systemd-creds needs the TPM library —
`systemd-creds has-tpm2` must say `yes`, not `partial`); three age recipients
whose identities are offline (a hardware token via `age-plugin-yubikey`, a
paper identity in a safe, a third holder), their public `age1…` strings at
hand. Decide whether the slots move off `user1` to a dedicated service user
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
i=0; for r in age1RECIPIENT_ONE age1RECIPIENT_TWO age1RECIPIENT_THREE; do
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
7. What changes: **new** buckets get `sse_enabled`; objects up to 256 MiB in
   such a bucket, or sent with `x-amz-server-side-encryption: AES256`, are
   sealed whole; chunked objects are sealed per chunk in the tenant's own
   dedup scope; a tenant's first key version is created on its first
   encrypted write. Existing objects are not touched.

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
  cannot re-read (an older build during a switch). Watch the backlog drain:
  `vaultaire_key_rotation_pending{kind="chunk"}` → 0, and
  `GET /api/v1/admin/jobs` → `key_reencrypt.result`. Rule:
  `KeyReencryptBacklogStuck`.
- **Whole SSE-S3 objects are not re-encrypted yet.** Each keeps opening under
  its recorded version. `vaultaire_key_rotation_pending{kind="whole"}` counts
  them and falls only as they are overwritten or deleted. Their re-encryption
  (write the new blob, then swap) lands with WP-R2-1. An in-place rewrite is
  the R2-05/R2-15 class of bug.
- The old version's key row stays (retired, `retired_at` set): it is needed
  to read whatever is still sealed under it. Deleting a retired version's row
  while anything is sealed under it destroys that data.

## Rotating the master key

No data is re-encrypted: only the wrapping of each tenant key row changes.

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
  includes a stale copy the sweep did not reach, a chunk blob in the shared
  `_global` container (dedup GC deletes it later), a retired chunk address, a
  backend's own versioning or backup, and a copy someone took of a blob.
  Without the key row it cannot be opened, master key or not.
- **Not covered:**
  - Plaintext objects. Everything stored before encryption was turned on,
    multipart uploads (until WP-R3-3), and unencrypted buckets are deleted,
    not shredded.
  - Database backups: the key rows are in every dump taken before the
    erasure (7 days on the box, 30 days off-box). Until those age out, the old
    master key plus an old dump can open the blobs.
  - Another slot's process memory: unwrapped keys are cached there for at most
    an hour and are not reachable through the API (no row routes to them).

## Where to look

| What | Where |
|---|---|
| Key rows | `SELECT tenant_id, key_version, created_at, retired_at FROM tenant_key_versions ORDER BY 1,2;` |
| Chunk backlog | `vaultaire_key_rotation_pending{kind="chunk"}` |
| Whole-object backlog | `vaultaire_key_rotation_pending{kind="whole"}` |
| Job | `GET /api/v1/admin/jobs` → `key_reencrypt`; `vaultaire_key_reencrypt_chunks_total{outcome}` |
| Old addresses awaiting delete | `SELECT * FROM retired_chunk_blobs ORDER BY retire_after;` |
| Boot check | `journalctl -u 'vaultaire@*' \| grep -E 'tenant keys\|master key'` |
| Re-key progress | `key re-encrypt run master_rekey=true rewrapped=… unwrap_failures=…` in the log; `key_reencrypt.result` on the jobs page |
| Chunked GETs that followed a moved chunk | `vaultaire_chunked_get_rereads_total{outcome}` |
| Generated objects refused (not stored plaintext) | `vaultaire_generated_object_refused_total{kind,reason}` |
