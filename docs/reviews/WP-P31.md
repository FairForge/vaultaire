# Stage 1a — Prompt 31: keystone K2b, the Private-mode key hierarchy (plan 10.0, F-12, F-13)

*2026-10-22. PRs, one per step, stacked: #900 (step 1, the sealed box's wire pinned — RP37-12), #901 (step 2, the account root), #903 (step 3, the identity), #905 (step 4, spaces, objects, chunks, manifests, shares, and the F-35 measurement), #906 (step 5, WASM + JS + the shared vectors), this docs PR. **No server code, no migration, no flag, no deploy-visible change.** Next migration still **104**. Prompt 30 (Part 0, `internal/…`) ran beside it; nothing here touches `internal/` or `cmd/`.*

## Step 0

- #898 (the WP-P27/P28 post-merge review) was in the merge queue at the start and merged during step 1. It was read from its branch.
- K5: #894 (the probe) and #895 (`internal/recovery`) are **not merged**. Their branches were read:
  - K5-PROBE: the Juicebox secret is at most **128 bytes**, padded to 129.
  - #895's client: `client.Register(ctx, cfg, pin, secret, userInfo, guesses)` takes the secret as opaque bytes, refused over `recovery.MaxSecretLength` (128).
  - Step 2's vault secret (32 bytes) is designed to both.
- Baseline on `origin/main` `b58a5335`, `GOTOOLCHAIN=go1.26.10`: the 8 tests of `sdk/go/e2ee` pass, including `TestE2EE_WASMMatchesNative` (node v24.6.0, 0.62 s). WASM `-trimpath -ldflags=-s`: **3,844,776 B, 1,102,770 B gzip -9** (RP35-3's 3.8 / 1.1 MB).

## The hierarchy as built

```
password ──Argon2id (params + salt in the header)──▶ KEK ─┐
                                                          ├─ 0x04 password wrap ─┐
recovery key (24 BIP-39 words) ── 0x01 box ───────────────┼──────────────────────┼─▶ MASTER KEY (random, on the device)
   ▲                                                      │                      │      │
   └─ 0x01 box under the master key (shown again)         │                      │      ├─ 0x01 identity/enc ─▶ X-Wing private key
   └─ 0x01 box under the VAULT KEY ◀── K5 realms (PIN) ───┘                      │      ├─ 0x01 identity/sig ─▶ Ed25519 seed (alg ‖ seed)
                                                                                 │      └─ 0x01 space-key ‖ id ─▶ SPACE KEY (owner)
member's X-Wing key ── 0x05 signed share (HPKE X-Wing + Ed25519) ──────────────────────────────────────────────▶ SPACE KEY (member)
SPACE KEY ── 0x01 object-key ‖ space ‖ object ─▶ OBJECT KEY ── 0x01 manifest ‖ space ‖ object ‖ version ─▶ MANIFEST v1
MANIFEST: per chunk (keyed id, SHA-256, chunk key, size, sealed size) ─▶ 0x02 chunk under ChunkKey(space key, SHA-256)
```

| Construction | Wire / AD label | Inputs | What the server sees | What it can never see |
|---|---|---|---|---|
| box | `01 ‖ nonce(24) ‖ ct ‖ tag`, AD = `01 ‖` the caller's AD | key, plaintext, purpose | the ciphertext and its length | the plaintext, the key |
| chunk | `02 ‖ nonce ‖ ct ‖ tag`, AD = `02`; key = HKDF(space key, `…/chunk-key` ‖ SHA-256), nonce = HMAC(key, `…/chunk-nonce` ‖ pt)[:24] | space key, chunk | the sealed chunk, its length (= len + 41), which chunks in one space are equal | the bytes, SHA-256(pt), the key |
| chunk id | HMAC-SHA256(HKDF(space key, `…/chunk-id`), SHA-256(pt)) | space key, chunk hash | an opaque 32-byte id, stable within a space | SHA-256(pt); whether two spaces hold the same chunk |
| sealed | `03 ‖ enc(1120) ‖ ct ‖ tag`; HPKE base X-Wing + HKDF-SHA256 + ChaCha20-Poly1305, info = `…/sealed/` ‖ **03** ‖ info | recipient public key, plaintext, info, ad | the message | the plaintext |
| password wrap | `04 ‖ kdf 01 ‖ mem KiB(4) ‖ t(4) ‖ p(1) ‖ salt(16) ‖ box`, box AD = `…/master-wrap/kek` ‖ header | master key, password, params | the parameters and the salt (it must hand them back) | the password, the KEK, the master key |
| recovery wrap | box under the recovery key, AD `…/master-wrap/recovery` | recovery key, master key | a 73-byte blob | the recovery key, the master key |
| recovery key wrap | box under the master key, AD `…/recovery-key` | master key, recovery key | a 73-byte blob | the recovery key |
| vault wrap (D-P31-1) | box under the vault key, AD `…/recovery-wrap/vault` | vault key, recovery key | a 73-byte blob; the realms see only their Juicebox share | the vault key, the recovery key |
| identity | enc = X-Wing (seed 32 / pub 1216); sig = `alg ‖ key` (0x01 Ed25519); signature = `alg ‖ sig` over lp(`…/sig/` ‖ context) ‖ msg | — | both public keys | both private keys |
| identity wraps | boxes under the master key, AD `…/identity/enc`, `…/identity/sig` | master key, private keys | two blobs | the private keys |
| fingerprint | SHA-256(`…/fingerprint` ‖ lp(enc pub) ‖ lp(sig pub)); text = base32 of its first 160 bits, 8 × 4 | public identity | it can compute it (it holds both public keys); nothing publishes it yet | — |
| space key (owner) | box under the master key, AD `…/space-key` ‖ lp(space id) | master key, space id, space key | the blob, the space id | the space key |
| share | `05 ‖ sender fp ‖ recipient fp ‖ be32 n ‖ SealTo(recipient, space key, info "share" ‖ lp(space id), ad = both fps) ‖ Sign(sender, "share", lp(space id) ‖ all before it)` | sender identity, recipient public identity, space id, space key | who shared with whom (two fingerprints), when | the space key; it cannot make a member accept a key it chose |
| object key | box under the space key, AD `…/object-key` ‖ lp(space) ‖ lp(object) | space key, ids, object key | the blob, the ids | the object key |
| manifest v1 | box under the object key, AD `…/manifest` ‖ lp(space) ‖ lp(object) ‖ be64 version; plaintext `01 ‖ lp(space) ‖ lp(object) ‖ be64 version ‖ previous(32) ‖ compressor ‖ be64 total ‖ be32 n ‖ n × (id ‖ SHA-256 ‖ key ‖ be32 size ‖ be32 sealed size)` | object key, the manifest | its length (99 B + the two ids + 108 B per chunk), the version, the chain by hash | the chunk list, keys, hashes, sizes inside |

`…/` = `stored.cloud/e2ee/v1/` (D-P31-2). `lp` = be16 length ‖ bytes.

**Per construction, the one sentence (no "zero-knowledge"; WASM never zeroes a key — RP37-15, no claim made):**
- The **password wrap** tells us the Argon2id costs and the salt, and we can try passwords against it offline. The floor (64 MiB, t ≥ 3) is what makes each guess cost.
- The **recovery** and **vault** wraps are boxes we cannot open.
- A **realm** holding the vault key's share learns nothing it can open. It never sees the wrapped recovery key, which only our server stores. The PIN, limited by the realms' guess count, stands between a thief of our blobs and the recovery key.
- A **share** shows us which two fingerprints shared a space and when, never the key.
- A **chunk id** lets us dedup within a space and nothing else.
- A **manifest** shows us an object's version count and its manifest's size.
- A **sealed chunk's length** shows us its plaintext length, and so can confirm a file known elsewhere (F-35).

## Departures

- **D-P31-1 — the vault secret is a separate random 32-byte vault key that wraps the recovery key, not the recovery key itself.** Deleting the server's vault-wrap blob ends every registration of that key, including at a realm that failed to forget (the realms only ever hold a key that opens one blob we store). The size is 32 B, inside K5's 128. The SDK hands the bytes to the K5 client; no K5 code is called.
- **D-P31-2 — the labels keep the package's prefix `stored.cloud/e2ee/v1/`.** The prompt wrote `stored/e2ee/v1/…`. The pinned 0x02 golden already uses `stored.cloud/e2ee/v1/chunk-key`, and one prefix serves the whole package.
- **D-P31-3 — the sealed golden is the `sealed` section of `testdata/vectors.json` from step 1**, not a separate `sealed_golden.json` folded in later: one vectors file throughout.
- **D-P31-4 — the manifest carries each chunk's key and SHA-256 inside its one box.** There is no box per entry: the manifest's box under the object key is their wrap, which saves 41 B and a nonce per chunk. The hash is there because `OpenChunk` checks it and a reader with only the object key cannot recompute the key's derivation.
- **D-P31-5 — `OpenShare(me, share, sender, spaceID)` takes the sender's whole public identity and the expected space id.** The prompt's form was `(myPriv, share, trustedSenderSigPub)`. The fingerprint is over both keys, and the space id is signed and in the HPKE info, so a share cannot be moved to another space.
- **D-P31-6 — format bytes: 0x04 = the password wrap, 0x05 = the share.** The manifest is a 0x01 box with its own version byte inside.
- **D-P31-7 — the Argon2id floor is m ≥ 64 MiB and t ≥ 3.** There is also an upper bound of 1 GiB, t ≤ 16, p ≤ 16, so a header from the server cannot exhaust a device. Both are refused before anything is derived.
- **D-P31-8 — the fingerprint's text is base32 of 160 bits.** Ente uses BIP-39 words of SHA-256(public key). Words are a UI choice; the bytes are fixed here.
- **D-P31-9 — F-35 gains a fourth option, a per-space chunker polynomial.** It is measured below and implemented nowhere, and it conflicts with contract line 6.

## Argon2id measurements (step 2)

Apple M1 Pro, 16 GB, `golang.org/x/crypto/argon2` v0.58.0, go1.26.10, best of 3; node v24.6.0 for WASM. Peak RSS is the process maximum from `/usr/bin/time -l`; node's includes the runtime (~45 MB idle).

| m | t | p | native | node / WASM | peak RSS native | peak RSS node |
|---|---|---|---|---|---|---|
| 64 MiB | 2 | 1 | 92 ms | 180 ms | 72.6 MB | 141.6 MB |
| **64 MiB** | **3** | **1** | **140 ms** | **268 ms** | **72.7 MB** | **145.2 MB** |
| 128 MiB | 3 | 1 | 300 ms | 538 ms | 140.0 MB | 211.4 MB |
| 256 MiB | 2 | 1 | 411 ms | 728 ms | 274.3 MB | 346.9 MB |
| **256 MiB** | **3** | **1** | **614 ms** | **1,093 ms** | **274.4 MB** | **345.7 MB** |
| 512 MiB | 2 | 1 | 828 ms | 1,477 ms | 543.1 MB | 614.7 MB |
| 1 GiB (libsodium SENSITIVE) | 4 | 1 | 3,469 ms | 6,288 ms | 1,079.9 MB | 1,135.8 MB |
| 256 MiB | 3 | 4 | 159 ms | — | 274.7 MB | — |

- **`Sensitive` = 256 MiB, t=3, p=1.** About 1 s in a browser on a fast laptop and ~350 MB at peak, which is the most a mobile browser tab can be asked for. 512 MiB doubles both; 1 GiB takes 6.3 s and 1.1 GB.
- **`Interactive` = 64 MiB, t=3, p=1** is the floor: it is RFC 9106's second recommended option, at p=1.
- **p=1** because WebAssembly runs one thread. Four lanes made native 4× faster (159 ms) for the same memory-time cost to an attacker: more lanes would only widen the gap between a browser and a native machine, never raise the cost of a guess.
- Mobile Safari and low-end Android were not measured: RP39-13.

## Mutation table

Every mutation was applied to committed code and the package's tests run under go1.26.10 with node present; then reverted.

| # | Mutation | Red |
|---|---|---|
| M1 | format byte out of the box AD (seal + open) | `Box_GoldenBytes`, `Box_RelabelledFormatDoesNotOpen`, `Chunk_ConvergentWithinASpace_…` (the chunk golden) |
| M2 | `labelSealed` changed | `SealedBox_GoldenOpens` |
| M3 | label dropped from HPKE info | `SealedBox_GoldenOpens` |
| M4 | format byte out of HPKE info | `SealedBox_GoldenOpens` |
| M5 | Argon2 time ↔ memory | `Argon2id_KnownAnswer`, `PasswordWrap_GoldenBytes` |
| M6 | Argon2 lanes ignored | `Argon2id_KnownAnswer` |
| M7 | no floor on open | `PasswordWrap_RefusesParametersUnderTheFloor` |
| M8 | header out of the box AD | `PasswordWrap_GoldenBytes`, `…RefusesParametersUnderTheFloor` |
| M9 | recovery label = recovery-key label | `KeyWraps_GoldenOpen` |
| M10 | vault label changed | `KeyWraps_GoldenOpen` |
| M11 | mnemonic checksum not verified | `RecoveryMnemonic_Refusals` |
| M12 | checksum from the wrong byte | `RecoveryMnemonic_BIP39Vectors`, `_Refusals` |
| M13 | fingerprint over the X-Wing key only | `Fingerprint_CoversBothKeys`, `Identity_GoldenBytes` |
| M14 | signature without its context | `Identity_GoldenBytes` |
| M15 | both identity wraps under one label | `Identity_GenerateWrapOpen`, `Identity_GoldenBytes` |
| M16 | `Verify` ignores the alg byte | `Signature_Refusals` |
| M17 | share signature not verified | `Share_RefusesAnotherSigner`, `_AnotherRecipient`, `_AnotherSpace` |
| M18 | share recipient not checked | `Share_RefusesAnotherRecipient` |
| M19 | share sender field not checked — **survived the first run**; `Share_RefusesASenderFieldThatIsNotTheSigner` added | `Share_RefusesASenderFieldThatIsNotTheSigner` |
| M20 | space id not signed | `Share_GoldenOpens` |
| M21 | fingerprints out of the share's HPKE AD | `Share_GoldenOpens` |
| M22 | chunk id unkeyed (= SHA-256) | `ChunkID_GoldenKeyedPerSpace`, `Manifest_GoldenBytes` |
| M23 | `OpenChunk` skips the hash | `Chunk_OpenRefusesAPoisonedChunk` |
| M24 | manifest AD without the version | `Manifest_GoldenBytes`, `_RefusesAnotherVersion`… (5 tests) |
| M25 | manifest AD without the object | `Manifest_RefusesASpliceFromAnotherObject`… (5 tests) |
| M26 | no rollback check | `Manifest_DetectsRollback` |
| M27 | no chain check | `Manifest_DetectsRollback` |
| M28 | no fork check | `Manifest_DetectsRollback` |
| M29 | no total-size check | `Manifest_RefusesTruncation`, `_Rules` |
| M30 | no inner-id check | `Manifest_RefusesASpliceFromAnotherObject` |
| M31 | no sealed-size check | `Manifest_RefusesReorderedChunks` |
| M32 | version-1 / previous rule gone | `Manifest_Rules` |
| M33 | WASM `newShare` sender/recipient swapped | `E2EE_WASMMatchesNative` |
| M34 | WASM `openManifest` ignores the seen head | `E2EE_WASMMatchesNative` |
| M35 | WASM `deviceOnlyChunkHash` returns an id | `E2EE_WASMMatchesNative` |

**Independent recomputation** (`sdk/go/e2ee/testdata/vectors_check.py`, run by hand, 49 checks, all ok): every deterministic golden — box, Argon2id (OpenSSL 3.6 `ARGON2ID` and libsodium; OpenSSL needs no `threads` option, it has no pool here), password wrap, the key wraps, identities (libsodium Ed25519), fingerprints, chunk id (HKDF/HMAC from the standard library), the bound wraps, both manifest versions and their chunks, and the share's layout and Ed25519 signature. Not recomputable here: the HPKE X-Wing layer of the sealed box and of the share (no X-Wing in libsodium); those are pinned from the open side only (RP39-10). BIP-39: trezor/python-mnemonic's published 256-bit vectors (MIT), all 8.

**Wordlist licence:** BIP-39's English wordlist, vendored byte for byte as `sdk/go/e2ee/bip39_english.txt`, sha256 `2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda` (pinned by `TestMnemonic_WordlistIsBIP39`). BIP-39's header says `License: MIT` ("This BIP falls under the MIT License"), checked 2026-10-22 against `bitcoin/bips` master; the MIT text sits beside it in `bip39_english.LICENSE`. **No new module dependency**: `x/crypto/argon2` is in the x/crypto already required; Ed25519, HKDF, HMAC and HPKE are the standard library.

## Length leakage (step 4.5) → F-35

Tool: `sdk/go/e2ee/testdata/leakage` (out of `./...`; `go run ./sdk/go/e2ee/testdata/leakage -list files.txt [-compare-pol P]`). The frozen chunker (restic Rabin `0x2ADD89E3B790BB`, 1 MiB / 20 bits / 16 MiB), sealed length = len + 41 in every space.

**Corpora:**
- (1) **Public files**: this repo's `git archive` tarball, the 9 Go 1.26.x darwin-arm64 toolchain zips of the module cache, and the node 24.6 binary — 11 files, 712.6 MiB, 383 chunks.
- (2) **The repo's files** as one object each: 1,997 files, 23.4 MiB.
- (3) **A photo set**: 2,000 JPEG/HEIC/raw sampled (seed 31) from a local library of 81k — 1,185.6 MiB, 2,071 chunks, only 6 multi-chunk. Aggregates only, no name leaves the machine.

H2 = collision entropy of a non-final chunk's stored length under the chunker's model (min + geometric with mean 2^20). "Bits" = chunks × H2; ≥ 40 bits ≈ unique among 10^12 objects.

| corpus | policy | stored overhead | worst padding | H2 / chunk | distinct lengths | anonymity set (median) | multi-chunk ≥ 40 bits | median bits |
|---|---|---|---|---|---|---|---|---|
| public | none (today) | 0.00 % | — | 21.0 | 356 | 1 | 100 % | 756 |
| public | Padmé | 1.27 % | 4.9 % | 5.8 | 77 | 1 | 100 % | 209 |
| public | power-of-two | 47.87 % | 99.8 % | 1.0 | 7 | 1 | 0 % | 36 |
| repo files | none | 0.33 % (the 41 B on small files) | — | — | 1,757 | 1 | — | — |
| repo files | Padmé | 2.27 % | 9.1 % | — | 159 | **22** | — | — |
| repo files | power-of-two | 45.40 % | 100 % | — | 16 | 352 | — | — |
| photos | none | 0.01 % | — | — | 2,018 | 1 | 100 % (6) | 147 |
| photos | Padmé | 1.16 % | 3.1 % | — | 87 | **56** | 50 % | 41 |
| photos | power-of-two | 47.64 % | 100 % | — | 6 | 1,122 | 17 % | 7 |

**A second polynomial** (`0x30d14e33f73647`, drawn with `chunker.RandomPolynomial`, irreducible) cut over the same corpora:
- **0 of the 11 public files** keep their sequence; 9 of 383 single lengths coincide.
- Every single-chunk file keeps its one length, which is its size: 1,995 / 1,997 repo files and 1,994 / 2,000 photos.

**What it says:**
- **Today, across spaces, the bytes and the ids never converge; the lengths do.** Any file of two or more chunks (≳ 2 MiB) that is public anywhere can be confirmed in any Private space by its sealed lengths alone, with no key: median 756 bits for the public set.
- **Padmé** costs ~1–2 % and turns small objects into crowds of 22–56 in a 2,000-object corpus. It does **not** save large files: 5.8 bits a chunk means ≥ 7 chunks is still unique.
- **Power-of-two** costs ~46–48 % and still leaves a 40-chunk file unique.
- **Only boundaries that depend on a secret defeat the confirmation of large files.** That is restic's own defence, a random polynomial per repository. Per space, it costs 0 bytes and keeps dedup inside the space, but it is a different chunker identity from the server's frozen one (contract line 6; 10.3's shared namespace and 38.1's write-avoidance assume the server's boundaries).

**F-35 (owner):** none / Padmé / power-of-two / per-space polynomial / per-space polynomial + Padmé.
- **Recommend: per-space polynomial + Padmé**, both a NEW chunk format (the 0x02 wire and the frozen identity are untouched until the owner picks). Cost: ~1–2 % stored bytes; line 6 amended to "frozen for Standard and the public namespace; a Private space cuts with its own secret polynomial".
- Residual: an object's total stored size, the number of chunks, timestamps and access.
- **modes.json Private leakage sentence (today):** "We see how many pieces each file has and how long each piece is; for a file that is public elsewhere, those lengths can show that you have it."

## F-34 — a post-quantum signature for identities (owner)

The envelope is in place: `alg` 0x01 = Ed25519; **0x02 is held for Ed25519 + ML-DSA-65**, both of which must verify. Its key would be `02 ‖ ed25519 pub(32) ‖ ML-DSA-65 pub(1,952)` and its signature `02 ‖ 64 ‖ 3,309`. A hybrid identity has a new fingerprint; nothing signs with 0x02 and it is refused today.

| Option | Cost |
|---|---|
| **(a) Go 1.27 for the repo → `crypto/mldsa`** (standard library, FIPS 204) | The toolchain bump for everything: server, CI, the deploy builder, the full suite re-run; the D-24 kind of step. No new dependency or new trust. Expect WASM +100–200 KB (not measured); a share grows from 1,303 B to 4,612 B. |
| (b) `cloudflare/circl` `sign/mldsa/mldsa65` (BSD-3) on 1.26 now | A third-party cryptographic module in the client's trust base and the supply chain, with its pinning and audit; a later swap to the standard library (the wire is FIPS 204 either way). |
| (c) Ed25519 only, until a need | A share's signature is checked when the member opens it, usually soon after it is made. A forger needs a quantum computer **then**, so this is not harvest-now-decrypt-later. But a share left unopened for years, or a key-transparency entry (K4b) verified years later, would be forgeable by one. |

**Recommend (a), taken when the repo moves to 1.27 for its own reasons; (c) until then.** The alg byte makes it additive.

## Residuals

| # | Sev | Residual |
|---|---|---|
| RP39-1 | P3 | **Passwords are not Unicode-normalised.** The same password typed as composed and as decomposed characters derives two KEKs. Decide NFC (x/text, already indirect) or NFKC **before the first account**: normalising later changes every KEK. |
| RP39-2 | P3 | **Sender-side trust of the recipient key.** A member refuses a share not addressed to them, but a sender whose client was handed the server's public key for "bob" seals to the server. The fingerprint comparison (UX) or key transparency (K4b) must exist before Shared spaces ship. |
| RP39-3 | P4 | **One manifest is one box**, held in memory: ≤ 2^22 chunks (≈ 8 TiB at 2 MiB), ~108 B per chunk (450 MB at the cap). A larger object needs a manifest tree. |
| RP39-4 | P3 | **Rollback detection needs a remembered head.** A fresh device trusts its first sight. Per-space signed heads, or the heads in K4b, close it. |
| RP39-5 | P4 | **Paranoid flag (random chunk keys):** its chunk id must be random too (a keyed hash of the plaintext still shows equality within the space). Not wired. |
| RP39-6 | note | **WASM never zeroes a key**, and Go's GC may copy one. No claim is made, and none may be (RP37-15). |
| RP39-7 | P4 | **Removing a member** needs a new space key and a re-wrap of the object keys (an epoch). Not built. |
| RP39-8 | note | **The vault flow's server side** (storing the vault wrap, deleting it on PIN change) waits for K5's API (#897). |
| RP39-9 | note | **WASM +594 KB** (4,438,502 B, 1,232,885 gzip). RP35-3 carried (TinyGo / split later). |
| RP39-10 | note | **The HPKE layer cannot be recomputed outside Go here.** The 0x03 and 0x05 goldens are pinned open-side only. |
| RP39-11 | P4 | **Only `CompressorNone`.** A compressor is a new id, and `OpenChunk` then decompresses before hashing. |
| RP39-12 | — | **The chunker in WASM, pinned to the golden boundaries**, remains. |
| RP39-14 | note | `docs/ENCRYPTION.md` § Setting the master key still says "Vault mode F-12 later" for the client-side hierarchy; "Vault" names the tape tier (contract line 1). One word, outside this PR's rows: for the next docs pass. |
| RP39-13 | P3 | **Argon2id is measured on an M1 Pro only.** Mobile Safari's and low-end Android's memory ceilings for 64 / 256 MiB in WASM are unmeasured: measure before the browser path (10.0.2) picks a profile. |

## What remains of K2

- The chunker in WASM pinned to the golden boundaries.
- SRP / OPAQUE + passkeys (10.0.5; **not in this prompt**).
- The browser upload path (10.0.2).
- The paranoid-flag wiring (RP39-5).
- Key transparency for fingerprints (K4b).
- F-34, F-35 (owner).
