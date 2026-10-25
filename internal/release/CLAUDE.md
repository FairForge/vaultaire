# internal/release

Release artefact signatures (WP-SUPPLY-1, Stage 1a Prompt 41 Part A; `docs/RELEASES.md`). One detached signed note per file — `golang.org/x/mod/sumdb/note`, the transparency log's format, Ed25519, the key named `stored.cloud/release/v1` (`Name`):

```
stored.cloud/release/v1
name <base name when signed>
sha256 <64 hex>
size <bytes>

— stored.cloud/release/v1 <base64 key hash + signature>
```

- `GenerateKey` / `NewSigner` / `NewVerifier` — note keys named `Name` only (the log's key is refused; errors quote nothing of a key).
- `Sign(signer, path)` → the signature bytes; `Verify(verifier, path, sig)` → the `Statement` once the signature verifies AND the file's sha256 and size are the statement's (`ErrMismatch` otherwise; a renamed download verifies — the name is reported, not enforced); `Open` verifies the signature alone; `ParseStatement` is strict (four lines, this order, 64 lower-hex, a canonical non-negative size, a name with no separator) — anything else `ErrFormat`.
- `LoadSigningKey(getenv, readFile)` — the signing key by precedence: the systemd credential `release_signing_key`, `RELEASE_SIGNING_KEY_FILE`, `RELEASE_SIGNING_KEY` (the master key's / translog's rules): set but unusable = `ErrSigningKeySource`; none = `ErrNoSigningKey` (dark until the owner's ceremony; the key never lands in CI or the repository).

The tool is `cmd/tools/release-sign` (`keygen`, `sign`, `verify [-log]`; `-log` uses `translog.FindBuild`). Tests: `release_test.go` (round trip; a tampered bit, an appended byte, the wrong key, a truncated or flipped signature, seven malformed statements under a valid signature; key names; the loader's precedence and refusals).
