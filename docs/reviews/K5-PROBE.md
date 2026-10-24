# K5 probe — the Juicebox protocol as its code defines it

*Stage 1a Prompt 29, keystone K5 PR 1 (docs + test data only), 2026-10-22. Feeds PR 2 (`internal/recovery`: the protocol core, the realm state machine, the client), PR 3 (`cmd/recovery-realm`) and PR 4 (the dark token endpoint). Contract: `docs/ARCHITECTURE_CONTRACT.md` line 1 (least access), line 7 (recovery ceremonies are in the transparency log), line 9 (recovery is part of encryption), line 12 (an opaque id, never an e-mail); `docs/surfaces/ENGINE.md` "the first ten" #6 (the recovery-vault interface); plan F-19 and 10.0.3; research `docs/research/ATTESTED_PLANE_AND_RECOVERY_2026-10-12.md` §1.*

**Copy rule, binding on every K5 PR:** one realm under our control is not hardware-enforced (F-19, Signal's lesson: the operator can restore a snapshot and reset the counter). Until two realms under different control exist, the words are "attempt-limited by our realm", never "hardware attempt limits".

## Sources read, and what was not

| Repository | Commit | Licence | How it was used |
|---|---|---|---|
| `github.com/juicebox-systems/juicebox-sdk` | `2e0f544e3fd0bd0997a034f73c8efb6fade3690c` (tag 0.3.7, 2026-07-31) | MIT | **Read**: `whitepaper/whitepaper.typ` and the Rust crates `marshalling`, `oprf`, `secret_sharing`, `noise`, `networking`, `realm/api`, `realm/auth`, `sdk`, `cli/demo`, `cli/tokens`. Built `cli/demo` and `cli/tokens`. The vector generator depends on these crates at this commit |
| `github.com/juicebox-systems/juicebox-software-realm` | `2e485aac584a6c9ada1fb9f156dc4d7cad3ab338` (2024-08-14) | **AGPL-3.0** | **Run only**: built with `go build ./cmd/jb-sw-realm` and started as a separate process (memory provider); only its README was read, for the flags. **No line of its code was read or copied.** Everything we write is clean-room from the paper and the MIT SDK |

Wherever the paper's prose and the code disagree, **the code wins**: the SDK is what every Juicebox client (Rust, Swift, Android, JS — all bridges over the same Rust core, `rust/sdk/bridge/{ffi,jni,wasm}`) actually sends.

## Prose vs code: the disagreements, resolved

| Item | Paper (`whitepaper.typ`) says | The code does | Source |
|---|---|---|---|
| Argon2 memory | "16 KiB of memory" | **16 MiB**: `m_cost(1024 * 16)` (KiB units) | `rust/sdk/src/pin.rs:54` |
| Encryption key MAC label | `MAC(32, seed, "Encryption Key", scalar)` | `"User Secret Encryption Key"`, each input length-prefixed (`be4`) | `rust/sdk/src/types.rs:266` |
| Commitment MAC label | `"Encrypted Secret Commitment"` | `"Encrypted User Secret Commitment"`, length-prefixed | `rust/realm/api/src/types.rs:422` |
| MAC inputs | concatenated values | every input is `be4(len) ‖ bytes`, the label included | `types.rs:372-385, 416-437` |
| Recover2 response fields | `allowedGuesses`, `attemptedGuesses` | `num_guesses`, `guess_count` (the count **after** this guess) | `rust/realm/api/src/requests.rs:252`; client `recover.rs:336` |
| Recover2 response contents | blindedResult, proof, signed key, commitment, counts | the same six, named `oprf_signed_public_key, oprf_blinded_result, oprf_proof, unlock_key_commitment, num_guesses, guess_count` | `requests.rs:252-266` |
| Recover3 request | `version`, `unlockKeyTags_i` | `version`, `unlock_key_tag` (one tag per realm) | `requests.rs:268` |
| Recover3 response | encryptionKeyScalarShare, encryptedSecret, commitment | the same, plus the error variants `VersionMismatch`, `NotRegistered`, `BadUnlockKeyTag { guesses_remaining }`, `NoGuesses` | `requests.rs:275-288` |
| The secret | "a secret of their choosing" | at most **128 bytes**, padded to 129 (`len ‖ secret ‖ zeros`), so every ciphertext is 145 bytes | `rust/sdk/src/types.rs:114, 170-196` |
| The OPRF signing message | `GenerateSignature(signingKey, PublicKey(key), realm.id)` | Ed25519 over `be2(16) ‖ realm_id ‖ be2(32) ‖ public_key`; verified with `verify_strict` | `rust/realm/api/src/signing.rs:64, 71` |
| Threshold | "any threshold ≤ n is valid; > n/2 recommended" | the SDK **refuses** a configuration whose `recover_threshold ≤ n/2` | `rust/sdk/src/configuration.rs:89` |

## Primitives, pinned to source

| Primitive | Exactly | Source |
|---|---|---|
| Group | ristretto255 (curve25519-dalek 4.1.3) | everywhere |
| Random scalar | 64 random bytes reduced mod ℓ (`Scalar::random` = `from_bytes_mod_order_wide`) | dalek |
| Random point | 64 random bytes through `RistrettoPoint::from_uniform_bytes` | dalek; `secret_sharing/src/lib.rs` |
| Hash to group | `RistrettoPoint::hash_from_bytes::<Sha512>(input)` = `from_uniform_bytes(SHA-512(input))` | `rust/oprf/src/lib.rs:236, 276` |
| OPRF (2HashDH) | blind `B = H(x)·r`; evaluate `B·k`; finalize `(B·k)·r⁻¹`; output `SHA-512("Juicebox_OPRF_2023_1;" ‖ input ‖ compress(H(x)·k))` (64 bytes; the input's length is not prefixed) | `oprf/src/lib.rs:236-310` |
| DLEQ (Chaum–Pedersen, Fiat–Shamir) | prove: `t` random; `v_t = t·G`, `w_t = t·U`; `c = Scalar::from_hash(SHA-512("Juicebox_DLEQ_2023_1;" ‖ U ‖ V ‖ W ‖ v_t ‖ w_t))` (compressed points; `G` omitted); `β_z = t + k·c`. Verify: `v_t = β_z·G − c·V`, `w_t = β_z·U − c·W`, recompute `c`, constant-time compare. Proof = `{c, beta_z}` | `oprf/src/dleq.rs:50-120` |
| Shamir | over scalars **and** over points (the T-OPRF combines blinded results as points); `threshold − 1` random coefficients drawn in order; share `i` (1-based) = Horner `((…(a_{t−1})·i + a_{t−2})·i …)·i + secret`; recovery = Lagrange at 0, `λ_i = Πⱼ xⱼ / Πⱼ (xⱼ − xᵢ)`; duplicate indices → error | `secret_sharing/src/lib.rs:56-122` |
| Share index | the realm's 1-based position after sorting the configuration's realms by id (bytewise) | `rust/sdk/src/configuration.rs:104, 116` |
| PIN KDF | Argon2id v0x13, output 64 bytes, salt = `be4(16) ‖ version ‖ be4(len(userInfo)) ‖ userInfo`; `Standard2019`: m = 16 384 KiB, t = 32, p = 1; `FastInsecure` (tests only): m = 8 KiB, t = 1, p = 1. Output `[0:32]` = access key (the OPRF input), `[32:64]` = encryption-key seed | `rust/sdk/src/pin.rs:47-108` |
| Unlock key | `SHA-512(oprf output)`: `[0:32]` = **unlock key commitment**, `[32:64]` = **unlock key** | `rust/sdk/src/types.rs:327-337` |
| MAC | keyed BLAKE2s (RFC 7693 keyed mode, `Blake2sMac<U16>` / `Blake2sMac256` — the digest length is a parameter, not a truncation) | `realm/api/src/types.rs`, `sdk/src/types.rs` |
| Unlock key tag (16 B) | `BLAKE2s-128(key = unlock key; be4(14) ‖ "Unlock Key Tag" ‖ be4(16) ‖ realm_id)` | `realm/api/src/types.rs:372` |
| Encrypted secret commitment (16 B) | `BLAKE2s-128(key = unlock key; be4(32) ‖ "Encrypted User Secret Commitment" ‖ be4(16) ‖ realm_id ‖ be4(32) ‖ share ‖ be4(145) ‖ encrypted_secret)` | `realm/api/src/types.rs:416` |
| Encryption key (32 B) | `BLAKE2s-256(key = seed; be4(26) ‖ "User Secret Encryption Key" ‖ be4(32) ‖ scalar)` | `sdk/src/types.rs:266` |
| AEAD | ChaCha20-Poly1305 (RFC 8439), **nonce = 12 zero bytes** (safe: the key is fresh per registration), no AAD, plaintext = the 129-byte padded secret | `sdk/src/types.rs:119, 131-156` |
| Signature | Ed25519 (a fresh signing key per registration, discarded after) | `realm/api/src/signing.rs` |
| Transport encryption | `Noise_NK_25519_ChaChaPoly_BLAKE2s`, empty prologue; HKDF = HMAC-BLAKE2s (`SimpleHmac`); nonce = 4 zero bytes ‖ le64(counter) | `rust/noise/src/lib.rs:23-215`, `client.rs`, `server.rs` |

## The messages

`SecretsRequest` (`requests.rs:123`) — what a realm acts on:

| Message | Fields (size) | Forward secrecy required (`needs_forward_secrecy`) |
|---|---|---|
| `Register1` | — | no (may ride in the Noise handshake) |
| `Register2` | `version` (16), `oprf_private_key` (scalar 32, this realm's SHARE), `oprf_signed_public_key {public_key (32), verifying_key (32, Ed25519), signature (64)}`, `unlock_key_commitment` (32), `unlock_key_tag` (16), `encryption_key_scalar_share` (scalar 32), `encrypted_secret` (145), `encrypted_secret_commitment` (16), `policy {num_guesses: u16}` | **yes** |
| `Recover1` | — | no |
| `Recover2` | `version` (16), `oprf_blinded_input` (point 32) | **yes** |
| `Recover3` | `version` (16), `unlock_key_tag` (16) | **yes** |
| `Delete` | — | no |

`SecretsResponse` (`requests.rs:157`): `Register1(Ok)`, `Register2(Ok)`, `Recover1(Ok {version} | NotRegistered | NoGuesses)`, `Recover2(Ok {oprf_signed_public_key, oprf_blinded_result, oprf_proof {c, beta_z}, unlock_key_commitment, num_guesses, guess_count} | VersionMismatch | NotRegistered | NoGuesses)`, `Recover3(Ok {encryption_key_scalar_share, encrypted_secret, encrypted_secret_commitment} | VersionMismatch | NotRegistered | BadUnlockKeyTag {guesses_remaining} | NoGuesses)`, `Delete(Ok)`. Inside Noise every response is a `PaddedSecretsResponse {unpadded_length: u16, padded_bytes: [436]}` so sizes do not leak which variant was sent (`requests.rs:170-205`).

The envelope (`requests.rs:20-80`): `ClientRequest {realm: RealmId (16), auth_token: string, session_id: u32, kind: HandshakeOnly | SecretsRequest, encrypted: Handshake {handshake: {client_ephemeral_public (32), payload_ciphertext}} | Transport {ciphertext}}`; `ClientResponse = Ok(Handshake {handshake: {server_ephemeral_public (32), payload_ciphertext}, session_lifetime: Duration} | Transport {ciphertext}) | Unavailable | InvalidAuth | MissingSession | SessionError | DecodingError | PayloadTooLarge | RateLimitExceeded`.

## The realm: states and transitions

Record key = (tenant name, `sub`) (paper § Realm State). States **NotRegistered**, **Registered**, **NoGuesses**. Pinned from the paper's realm pseudocode, the SDK's expectations (`recover.rs`) and, black-box, the software realm's answers (lab below):

| Request | NotRegistered | Registered | NoGuesses |
|---|---|---|---|
| `Register1` | `Ok`, no change | `Ok`, no change | `Ok`, no change |
| `Register2` | → Registered, `guess_count = 0` | **overwritten**, `guess_count = 0` | → Registered, `guess_count = 0` |
| `Recover1` | `NotRegistered` | `guess_count ≥ num_guesses` → **NoGuesses** (erase), answer `NoGuesses`; else `Ok {version}` | `NoGuesses` |
| `Recover2` | `NotRegistered` | `guess_count ≥ num_guesses` → **NoGuesses** (erase), `NoGuesses`; version ≠ → `VersionMismatch` (no guess spent); else **`guess_count += 1` (the attempt is counted here)**, evaluate, prove, answer `Ok {…, num_guesses, guess_count}` | `NoGuesses` |
| `Recover3` | `NotRegistered` | version ≠ → `VersionMismatch`; tag ≠ (constant time) → `BadUnlockKeyTag {guesses_remaining = num_guesses − guess_count}` and, when that is 0, → **NoGuesses** (erase); tag = → **`guess_count = 0`** (the reset), answer `Ok {share, encrypted_secret, commitment}` | `NoGuesses` |
| `Delete` | `Ok` | → NotRegistered, `Ok` | → NotRegistered, `Ok` |

- **`attemptedGuesses` increments in Recover2 and only there.** A Recover3 does not spend a guess (lab probe: a wrong tag before any Recover2 answered `guesses_remaining: 2` of 2): a tag is a 128-bit MAC under the unlock key, which only a correct PIN yields, so there is nothing to brute-force there.
- **It resets on a correct Recover3**, and only there. The SDK's client never sends Recover3 after a wrong PIN: it compares the finalized commitment locally (`recover.rs:194`) and returns `InvalidPin {guesses_remaining = min over realms of num_guesses − guess_count}`.
- **The last guess is usable.** After the N-th Recover2 the record stays Registered (`guess_count = N`) so a correct PIN can still finish Recover3; the self-destruct happens at the next Recover1/Recover2 (or a wrong tag at 0 remaining). The demo shows it: two wrong guesses, then the right PIN gets `InvalidPin {0}` — Recover1 answered `NoGuesses`.
- **Our realm (PR 2) departs on purpose in two places**, neither visible on the wire: (1) the Recover2 increment is committed **durably before** the response leaves (a crash or a failed store after the commit still spends the guess); (2) entering NoGuesses **erases** the secret material — OPRF key share, signed public key, unlock key commitment and tag, encryption scalar share, encrypted secret and its commitment — and keeps only the state and counts, never a flag over intact bytes.

## The wire

- **Encoding:** CBOR through serde (`ciborium` 0.2.1, `rust/marshalling/src/lib.rs`). Structs are maps keyed by the field names in declaration order; a unit enum variant is its name as a text string (`Recover1` = `68 52 65 63 6f 76 65 72 31`); a variant with data is a one-entry map `{name: data}`; newtype structs (`SessionId(u32)`, `RegistrationVersion`, the secrets) are their inner value; every fixed byte array, scalar and compressed point is a CBOR byte string (`58 20 ‖ 32 bytes` for a scalar or point); `Duration` is `{"secs": u64, "nanos": u32}`; every length and integer in the shortest form.
- **HTTP:** `POST <realm address>/req` (the address must end in `/`; `Url::join("req")`, `networking/src/rpc.rs:68`), body = the CBOR request, response body = CBOR. Headers sent by the SDK: `X-Juicebox-Version: 0.3.7`, `User-Agent: JuiceboxSdk-Rust/0.3.7`; no content type. A non-2xx response whose body still decodes as the expected response is used as one; otherwise the status maps: 401 → `InvalidAuth`, 426 → `UpgradeRequired`, 429 → `RateLimitExceeded`, anything else → `Transient` (`sdk/src/request.rs:45-58`). Requests are at most 2 048 bytes (`BODY_SIZE_LIMIT`).
- **Two transports, chosen per realm by the client's configuration** (`sdk/src/request.rs:255-270`):
  - *software realm* (no `public_key` in the configuration): the bare `SecretsRequest` CBOR, `Authorization: Bearer <jwt>`, the bare `SecretsResponse` back (unpadded). This is what the AGPL software realm speaks.
  - *Noise realm* (`public_key` = the realm's 32-byte X25519 static key): a `ClientRequest` envelope carrying the JWT **inside** the body (`auth_token`) and the request Noise-encrypted to the realm's key. A request that needs forward secrecy is never put in the handshake: the client first sends `HandshakeOnly` (empty payload), gets the handshake response (empty payload) and a `session_lifetime`, then sends `Transport {ciphertext}` with the same `session_id` (a random u32 chosen by the client). `Register1`, `Recover1` and `Delete` may ride in the handshake (`kind = SecretsRequest`); their answer comes back in the handshake response payload. Every decrypted answer is a `PaddedSecretsResponse`. `MissingSession` makes the client open a new session (up to 5 attempts, `request.rs:326`); a session unused for `session_lifetime` is dropped by the client.
- **Where Noise sits for us (decision for PR 3):** our realm speaks **only the Noise transport**, `POST /req`, under TLS on the network or plain HTTP on loopback in a lab. TLS terminates wherever the network needs it; Noise ends inside the realm process, so nothing in front of the realm (a proxy, a load balancer, later the host of the attested VM) sees a PIN-derived value or a share. The realm's Noise public key is the key the client pins (`public_key` in the configuration, returned by PR 4's endpoint) and the `key_fingerprint` of its `key_ceremony` entry in the transparency log.

## Authentication (JWT)

From `rust/realm/auth/src/{creation.rs,validation.rs}` and the paper § Authentication:

- Header `{"alg": "HS256", "kid": "<tenant>:<version>", "typ": "JWT"}`; `version` is a u64; the realm looks the key up by `kid`. RS256 and EdDSA are also accepted by Juicebox's validator; **ours accepts HS256 only**.
- Claims `iss` = the tenant name (must equal the `kid`'s tenant; alphanumeric, `^(test-)?[a-zA-Z0-9]+$`), `sub` = the persistent user id the realm keys the record by, `aud` = the realm id as 32 lower-case hex characters (a single string), `nbf`, `exp`, `iat`, `scope` = `"user"`. Juicebox's own tokens live 10 minutes.
- The validator refuses a token whose `exp − nbf` exceeds **86 400 s** or that lacks either claim (`LifetimeTooLong`), allows 5 s of clock skew, and a token over 1 000 bytes.
- For us the tenant is the **realm's** customer — `stored` — never a Vaultaire tenant id, and `sub` is the opaque `base64url(HMAC-SHA256(recovery_subject_key, "stored/recovery/v1" ‖ user_id))` (contract line 12: a realm learns a tenant name, an opaque `sub` and ciphertext, nothing else). The client should pass the same opaque `sub` as the SDK's `userInfo` (the per-user salt input), so the salt is stable across devices and names nobody.

## The lab (local only, 2026-10-22)

Two AGPL software realms (`jb-sw-realm -id 0101…01 -port 9181` and `-id 0202…02 -port 9182`, memory provider, `TENANT_SECRETS='{"test":{"1":"an-auth-token-key"}}'`), each behind a recording HTTP proxy, and the MIT demo client (`rust/cli/demo`, register 2-of-2 with `Standard2019`) with tokens from `rust/cli/tokens create`. The demo ran clean: register → wrong PIN (`InvalidPin {1}`) → right PIN (`teyla21`) → two wrong PINs (`InvalidPin {1}`, `{0}`) → right PIN (`InvalidPin {0}`: self-destructed) → register a second secret → recover (`presso42`) → delete. 18 exchanges per realm, captured byte for byte in `internal/recovery/testdata/sdk_transcript.json` with 23 hand-made probes against realm 1:

| Probe | Software realm answered |
|---|---|
| no `Authorization` | 401 (JSON body) — checked **before** the version header |
| no `X-Juicebox-Version` | 426 `SDK upgrade required` (the SDK maps it to `UpgradeRequired`); `0.2.0` was accepted |
| token for the other realm (wrong `aud`), unknown `kid` version, wrong key | 401 |
| `exp − nbf` = 86 401 s | **accepted** — the software realm does not enforce the SDK validator's 24 h window. **Ours does** (401) |
| body `0xff` | 400 |
| Recover1/Recover2 never registered | `NotRegistered` |
| Register2 replayed | `Ok` (overwrite) |
| Recover2 / Recover3 with a wrong version | `VersionMismatch`, no guess spent |
| Recover3, wrong tag, before any Recover2 | `BadUnlockKeyTag {guesses_remaining: 2}` (no guess spent) |
| Recover2 → wrong tag → Recover2 → wrong tag | `guess_count` 1, remaining 1; `guess_count` 2, remaining 0 |
| Recover1 after that; Recover3 with the RIGHT tag after that | `NoGuesses`; `NoGuesses` (the record is gone) |
| Delete, then Recover1 | `Ok`, then `NotRegistered` |

The Noise transport cannot be exercised against the software realm (it speaks only the bare transport); PR 3's lab runs the same MIT demo client against OUR realm with `public_key` set, which is the Noise path end to end.

## Golden vectors

- `internal/recovery/testdata/vectors.json` — produced by `internal/recovery/testdata/gen` (a small Rust program, `Cargo.lock` committed, depending only on the MIT crates at `2e0f544e` and the RustCrypto/dalek crates at the versions that commit's lock pins; `cargo run --release > ../vectors.json`, deterministic — two runs compare equal). Everything random is drawn from a recorded **entropy tape** (`SHA-512(label ‖ be32(counter))` blocks, consumed front to back), so the Go side replays the same draws. Sections: `oprf` (8 vectors: keys, blinded input/output, the DLEQ proof, the output), `shamir` (scalar and point sharing for 1-of-1, 2-of-2, 2-of-3, 3-of-5), `pin_hashing` (both modes; the `Standard2019` vector is the SDK's own test constant), `registration` (one 2-of-3 registration: every derived value per realm), `noise` (two NK handshakes + four transport messages each), `wire` (every `SecretsRequest`, `SecretsResponse` + its padded form, `ClientRequest` and `ClientResponse` variant as CBOR). The SDK's three `pub(crate)` derivations (PIN hash, encryption key + cipher, unlock-key split) are re-stated in the generator from `pin.rs`/`types.rs` and asserted against the SDK's own test constants before anything is written.
- `internal/recovery/testdata/sdk_transcript.json` — the lab capture above: the real SDK client's bytes. PR 2's test re-derives from it, with the known PIN and `userInfo`, every value the SDK computed (access key, root OPRF key from both shares, commitment, both tags, both signatures, both DLEQ proofs, the encryption scalar, the decrypted secret `teyla21`) and re-encodes every message to the same bytes.

## What PR 2–4 take from this

- PR 2 `internal/recovery`: `types.go` = the messages above with their CBOR (a small hand-written codec for exactly this subset — no CBOR dependency); `crypto.go` = the primitives table on `golang.org/x/crypto` (argon2, blake2s, chacha20poly1305, curve25519, hkdf) and one ristretto255 module (`github.com/gtank/ristretto255`, BSD-3-Clause: `SetUniformBytes` is the `from_uniform_bytes` both hash-to-group and random points need). Test `TestRecovery_GoldenVectors_MatchJuiceboxSDK` = every vector in both files, byte for byte.
- PR 3 `cmd/recovery-realm`: the Noise transport only; HS256 by `kid`; `aud` = our realm id; 401 for a missing, expired, unknown-`kid`, wrong-`aud` or wider-than-86 400 s token; `X-Juicebox-Version` not required (we serve SDK ≥ 0.3); sessions keyed by (tenant, `sub`, `session_id`) so one user cannot reach another's Noise session.
- PR 4: the tokens it mints carry `iss = stored`, `kid = stored:<version>`, the opaque `sub`, `aud` = each realm's hex id, `scope = user`, a window well under 86 400 s.
