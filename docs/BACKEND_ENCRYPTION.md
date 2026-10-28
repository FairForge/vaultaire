# Backend encryption — what each storage backend does with a customer's bytes

RP33-6 (the WP-P26 review; Stage 1a Prompt 27). One row per driver the binary can register (`addDriver` in `cmd/vaultaire/main.go`; `TestBackendEncryptionDoc_EveryDriverHasARow` fails when a driver has no row here, or a row names no driver). It answers three questions before the owner flips `modes_copy` globally (the privacy page's per-mode table renders Standard's sentence from `internal/api/landing/modes.json`):

1. **Transport** — how our process reaches the backend: TLS, loopback (the box itself), or plain HTTP.
2. **At rest** — the vendor's own statement, with the URL and the date it was read; "unknown" when no official page says it. We have verified none of these ourselves: they are the vendors' words.
3. **Can a customer's Standard-mode object land there?** — *default* (a Standard object goes there with no flag or operator action), *flag-gated* (only with a feature flag or an operator setting), *never*.

Our own layer on top: with `ENCRYPTION_MASTER_KEY` loaded, SSE-S3 seals objects before they leave the process (`internal/crypto/stream.go`); **prod has no master key loaded** (`docs/reviews/K3-PROBE.md`), so today the vendors' rows below are the whole story for Standard. Private-mode objects (SSE-C) are sealed with the customer's key before they reach any backend, whatever the row says.

Prod facts read on 2026-10-22 (`/opt/vaultaire/configs/.env` names, the boot log, `object_head_cache`): `STORAGE_MODE=idrive`; registered: `local`, `idrive` + 13 `idrive-<region>`, `lyve`, `geyser`, `r2`, `wasabi`, `permafrost`, `sync` (5 bridges), `blomp`; not registered: `s3`, `quotaless`, `sia`. Head rows: 501 on `idrive`, 2 on `blomp`. `BLOMP_ALLOW_UNSEALED=true`. The box's disk (`nvme0n1p4`, ext4) has no dm-crypt layer (`lsblk`, `dmsetup ls --target crypt` = none).

| Driver | Transport | At rest — the vendor's statement (URL, read 2026-10-22 unless noted) | Standard object can land there |
|---|---|---|---|
| `local` | loopback (the box's filesystem, `DATA_PATH=/opt/vaultaire/data`) | **Not encrypted.** Our own NVMe, ext4, no dm-crypt layer (checked on the box 2026-10-22) | **never** as a placement on prod (target-only, never a fall-over destination — `engine.targetOnlyBackends`). But every multipart part is staged on the same disk (`MULTIPART_STAGING_DIR=/opt/vaultaire/data/multipart`) until Complete or the reaper (≤ 48 h): plaintext at rest on our disk meanwhile |
| `s3` | TLS | "Starting January 5, 2023, all new object uploads to Amazon S3 are automatically encrypted" (SSE-S3, AES-256) — https://docs.aws.amazon.com/AmazonS3/latest/userguide/default-encryption-faq.html | **never** on prod (not registered); *default* only where `STORAGE_MODE=s3` |
| `lyve` | TLS ("client applications can only connect using HTTPS", TLS 1.2) | "The data is always encrypted at rest using one of two server-side methods … AES 256-bit encryption at rest" — https://help.lyvecloud.seagate.com/en/security.html | *default* for the RESILIENT class, and for every write while it is the primary (it was 2026-10-11 → back to `idrive` by 2026-10-22) |
| `quotaless` | TLS (`S3COMPAT_INSECURE_TLS` can turn off verification) | **unknown** — https://quotaless.cloud/ says "privacy-focused", nothing on encryption | **never** on prod (not registered, dormant) |
| `sia` | loopback HTTP to `s3d` (`http://127.0.0.1:9000`); `s3d` → the Sia hosts over the Sia protocol | Client-side: "The SDK encrypts the file before any byte leaves your application … Providers only ever see encrypted shards" — https://sia.tech/. Our layer: the SIA tier is sealed-only unless `SIA_ALLOW_UNSEALED` | **flag-gated** (the SIA tier + its flag); not registered on prod |
| `geyser` | TLS (`https://la1.geyserdata.com`) | **Not encrypted by the vendor on our account.** Geyser's tape encryption is a paid add-on and ours is off (price book and account read 2025-11-14: `dualCopy/compression/encryption` all false — memory `project_geyser_capabilities`, `internal/drivers/geyser_README.md`; SSE-S3 answers `ServerSideEncryptionConfigurationNotFoundError`). geyserdata.com answers 403 to an automated read | ***default*** for the archive classes (GLACIER / DEEP_ARCHIVE = the `vault` floor); the Smart tier's demotion is flag-gated (`smart_demotion`, OFF) |
| `idrive` | TLS | "all transfers are secured with TLS protocol and offers encryption at rest" — https://www.idrive.com/s3-storage-e2 ; whether it is ON by default is **unknown** (the e2 FAQ and features pages say nothing; the security page answers 403) | ***default*** — the primary on prod |
| `idrive-<region>` | TLS | as `idrive` (same vendor, same account) | ***default*** for a bucket pinned to that region |
| `r2` | TLS | "All objects stored in R2, including their metadata, are encrypted at rest … automatic, do not require user configuration" (AES-256) — https://developers.cloudflare.com/r2/reference/data-security/ | **never** — public buckets only (the PUBLIC class); a Standard object never lands there |
| `wasabi` | TLS | "all stored files are encrypted automatically using FIPS-197 certified AES-256 bit encryption" — https://www.wasabi.com/security | **never** on prod today (dormant: no fall-over, no auto-select); *default* only with `STORAGE_MODE=wasabi` |
| `permafrost` | TLS (Microsoft Graph + its download CDN) | "Microsoft online services encrypt all data at rest and in transit" (BitLocker + service encryption; the Microsoft 365 page, not OneDrive-specific) — https://learn.microsoft.com/en-us/compliance/assurance/assurance-encryption | **flag-gated**: the Vault parity leg only when `sync` is not registered or the operator reorders `VAULT_PARITY_LEGS`; no storage class resolves to it |
| `sync` | loopback HTTP to the `sync-webdav` bridge on the box; the bridge encrypts client-side, then TLS to Sync.com | Client-side end-to-end: "Sync includes end-to-end encryption and zero-knowledge authentication features" — https://www.sync.com/security/ (the vendor's words; the bridge runs on our box, so the key is ours) | ***default*** for the Vault parity of every `vault`-floor object (`vault_parity` ON globally on prod, Sync first — four RS parity shards, any four of eight rebuild the object); the SYNC tier is flag-gated (`sync_backend` + an operator tier) |
| `blomp` | **plain HTTP** (the Swift storage URL `http://swiftproxy.acs.ai.net:8080`, no TLS on any port — `internal/drivers/swift_README.md`) | **unknown** — https://blomp.com/ and /cloud-storage say "three copies … redundancy and security", nothing on encryption | **flag-gated** (`blomp_backend` + a bucket with tier `blomp`; one bench bucket, 2 head rows on prod). With `BLOMP_ALLOW_UNSEALED=true` (prod today) the bytes cross the internet in cleartext and sit unencrypted as far as we know. **Owner gate RB-3: set `BLOMP_ALLOW_UNSEALED` off before any customer** — then a write there is SSE-S3 (needs the master key) or refused |

## Sealed by default (`standard_sealed_default`)

Sealed by default: **no** — the flag `standard_sealed_default` (WP-EDGE-4 PR C, Stage 1a Prompt 48; global only, default OFF; `TestBackendEncryptionDocMatchesFlagDefault` holds this cell to the flag's default). ON, every Standard-mode write is sealed before it reaches any row of the table above: at the origin a body under the chunk threshold is SSE-S3 sealed whole (`placeWholeSealed`), a chunked one chunk-encrypted; at the edge the flag implies `edge_codec` for every tenant (the data-plane host seals each chunk as a 0x06 blob under the tenant's codec key). Public buckets stay as they are (meant to be read) and Private (SSE-C) is sealed by the customer's key already. A write that cannot be sealed — no master key on the process — is 503, nothing stored, never plaintext. Turning it on is refused (409) without a master key (`master_key_absent`) or while a tenant has no key version (`tenant_keys_missing`, with the count). It is flipped after the F-14 ceremony; then the Standard sentence of `modes.json` is true on every row, whatever the vendor does at rest.

## Second copies (`edge_mirror`, `edge_rs`)

A mirror copy (WP-EDGE-4) is the primary's bytes at the same address on its leg (`MIRROR_LEGS`, default `lyve,idrive`): a Standard object's copy is exactly as encrypted as the primary — the leg's row above applies to it (a sealed object's copy is the same ciphertext, a codec chunk's copy the same 0x06 blob). An RS shard (`edge_rs`, dark) is a stripe of the object's bytes plus parity: the leg's row applies the same way. With `standard_sealed_default` ON every copy and shard is ciphertext.

## What this changed

Standard's pre-key sentence in `modes.json` said "Encrypted in transit. At rest your files sit on our storage providers' disks under the providers' own encryption, and we can read them." The table shows it false for backends a Standard object reaches **by default**: `geyser` (no vendor encryption on our account — every archive-class object), `idrive` (at rest unconfirmed — the primary), and our own disk for multipart parts; and for `blomp` (flag-gated, plain HTTP, unsealed on prod). The sentence now says what is true on every row:

> Not encrypted by us at rest yet: your files sit on our storage providers' disks and tapes as we send them, encrypted there only where the provider does so, and we can read them.

It changes again when the master key is loaded (then the `requires_master_key` sentence renders: "Encrypted at rest with keys we hold").

## Owner items this table feeds

- `modes_copy` global flip: read this table and the privacy page's per-mode table first.
- RB-3: `BLOMP_ALLOW_UNSEALED` off before customers.
- Geyser: the encryption add-on ($0.33/TB list, $0.28 wholesale) or the master key (SSE-S3 seals before the bytes leave the box) — either makes the archive row true for "encrypted at rest".
- iDrive: ask the vendor whether e2 encrypts at rest by default on the reseller account (no public page says it).
- The box's disk: multipart staging is plaintext at rest on an unencrypted NVMe for up to 48 h.
