# internal/recovery — keystone K5, the recovery-vault interface

The Juicebox protocol (docs/reviews/K5-PROBE.md, pinned to the MIT
juicebox-sdk at `2e0f544e`): a short PIN plus a threshold of realms rebuild
a ≤ 128-byte secret; each realm counts wrong guesses on its own and destroys
its record at the limit. Contract lines 1, 7, 9, 12; ENGINE.md "first ten" #6;
plan F-19, 10.0.3.

| Package | What | Linked into |
|---|---|---|
| `recovery` | the messages + their CBOR (`types.go`, `cbor.go`), the primitives (`crypto.go`), Noise NK (`noise.go`) | the realm binary, the client, and (PR 4) nothing of Vaultaire but the token minting |
| `recovery/realm` | the state machine `Realm.Handle`, `Store` (`MemoryStore`, `BoltStore`) | `cmd/recovery-realm` only — **never** `cmd/vaultaire` (a realm belongs on another box: the attested one of 41.4) |
| `recovery/client` | `Register` / `Recover` / `Delete` across N realms, `Transport` | clients, tests, labs |

## Rules

- **Copy:** until two realms under different control exist, say "attempt-limited
  by our realm". Never "hardware attempt limits" (F-19, Signal's lesson: one
  operator can restore a snapshot and reset the counter). This applies to every
  comment, doc, dashboard string and PR text.
- **Clean-room.** The AGPL juicebox-software-realm is run as a black box in labs, never read
  or copied. The MIT SDK and the paper are the sources. Where the paper's prose
  and the code disagree, the code wins (the probe lists each case).
- **Byte compatibility is tested, not assumed.** `TestRecovery_GoldenVectors_MatchJuiceboxSDK`
  checks every vector in `testdata/vectors.json` (written by `testdata/gen`,
  Rust on the MIT crates, deterministic). `TestRecovery_SDKTranscript_EveryDerivationAndEncoding`
  re-derives everything the real SDK client sent and received in the lab
  (`testdata/sdk_transcript.json`). Change a primitive, a label or an
  encoding, and these go red.
- **The guess is spent before the answer.** `Realm.Handle` commits the
  Recover2 increment (`Store.CompareAndSwap`, durable when it returns) before
  the evaluation can leave. A store error after the commit means no answer,
  and the guess is still spent (`TestRecovery_CrashBetweenIncrementAndResponse`).
  Concurrent guesses race on the revision, never past the allowance
  (`TestRecovery_ConcurrentRecover2_NeverExceedsAllowedGuesses`, both stores).
- **NoGuesses erases.** The record keeps its state and two counts, and every
  share, key, tag, commitment and ciphertext is zeroed (`Record.HoldsSecretMaterial`).
  It happens at the next Recover1/Recover2 after the last guess, or at a wrong
  tag with none left. The last guess stays usable, as in Juicebox.
- **BoltStore erasure is logical.** bbolt frees pages without zeroing them.
  The realm's volume must be encrypted with a key that does not rest on that
  disk.
- **What a realm learns:** a tenant name (`stored`), an opaque subject,
  and ciphertext/shares. Never a PIN, a secret, a user id or an e-mail.
  The subject is minted by Vaultaire (PR 4) as an HMAC of the user id.
- Thresholds follow the SDK: the recover threshold is a strict majority, and the
  register threshold is at least the recover threshold. Share index = 1-based
  position after sorting realms by id.

## Regenerating the vectors

`cd testdata/gen && GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf GIT_CONFIG_VALUE_0=git@github.com: CARGO_NET_GIT_FETCH_WITH_CLI=true cargo run --release > ../vectors.json`.
The SDK repository's `artifacts` submodule URL is SSH, so the env lines rewrite
it to HTTPS. Output is deterministic: a rerun must compare equal.
