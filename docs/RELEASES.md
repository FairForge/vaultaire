# Releases: SBOM and signatures (WP-SUPPLY-1)

Stage 1a Prompt 41 Part A — the supply-chain tail of keystone K4. Builds are
reproducible (`scripts/build-release.sh`, `scripts/build-client.sh`, RP37-9) and
every server and client build's sha256 is in the transparency log
(`server_build`, `client_build` — `docs/TRANSPARENCY.md`). What a third party could
still not do: see what is INSIDE a build, and check that a downloaded artefact
was published by us without consulting the log. Both are closed here, dark where
a key is needed: the code and the verification ship now, the signing key is an
owner ceremony (the `TRANSLOG_SIGNING_KEY` pattern).

## The SBOM

`make sbom` writes one **CycloneDX 1.6 JSON** document per build the log names
into `bin/sbom/` (`scripts/sbom.sh`):

| Document | Of | How |
|---|---|---|
| `vaultaire.cdx.json` | the server binary of the release recipe (linux/amd64, CGO off) | `cyclonedx-gomod bin` over the binary: the module list and go.sum hashes **as linked** (`debug/buildinfo`), the binary's own sha256/sha512 in `metadata.component.hashes`, the Go toolchain as the `std` component |
| `sdk-wasm.cdx.json` | the WebAssembly client (`sdk/wasm`, js/wasm) | `cyclonedx-gomod app` for GOOS=js GOARCH=wasm — in a throwaway git repository made of `git archive HEAD`, because the tool reads the main module's version from git and go-git reads neither a worktree nor a shallow checkout the same way; the version is then set to HEAD's commit |
| `sdk-js.cdx.json` | the JS binding (`sdk/js/e2ee/index.mjs`, published as-is) | one file component with its sha256 and the manifest's version |

`cyclonedx-gomod` is **pinned** (`CYCLONEDX_GOMOD_VERSION`, v1.12.0 of 2026-08-22)
and built for the host into a temporary `GOBIN`, never a PATH binary (the
Makefile's golangci-lint / lint-wasm pattern).

**Reproducible.** Two runs on the same commit are byte-identical: no serial number,
no timestamp, the main component's version is HEAD's commit everywhere it appears
(the component, its purl and bom-ref, the dependency graph's refs), and the tool's
own host-dependent binary hashes are dropped from `metadata.tools`.
`scripts/repro-check-sbom.sh` generates twice, seconds apart, and fails unless
every document is the same bytes (with a directory argument, unless the shipped
ones are too) — `make repro-check-sbom`; the Deploy job runs it after the binary's
own repro gate (`deploy.yml` "SBOMs", with the binary it ships as
`SBOM_SERVER_BINARY`). **go.sum's truth:** `scripts/sbom-check.sh` (`make sbom-check`)
fails unless every module the server SBOM names is a `<module> <version> h1:…` line
of `go.sum` whose h1 decodes to the SBOM's SHA-256, and `std` is go.mod's toolchain;
a dependency bump, a removed module, a moved hash or a toolchain change without the
SBOM (or the reverse) fails — `scripts/sbom_test.sh` (CI "Deploy scripts") proves
each refusal on fixtures.

## Release signing

`cmd/tools/release-sign` (`internal/release`): one **detached signed note** per
artefact, the format of the transparency log's checkpoints
(`golang.org/x/mod/sumdb/note`, Ed25519) by a key named `stored.cloud/release/v1`:

```
stored.cloud/release/v1
name vaultaire-linux
sha256 <64 hex>
size <bytes>

— stored.cloud/release/v1 <base64: key hash + signature>
```

Why this and not minisign/signify: the note format is already the project's
(`translog-keygen`, `translog-verify`, the published checkpoint verifier keys), its
verifier is the Go module checksum database's own code, the key hash in the
signature line names the key that signed, and a reader that holds the log's keys
holds these. The statement's `name` is what the artefact was called when signed;
verification checks the **bytes** (sha256 and size) and reports the name, so a
download a browser renamed still verifies. A tampered byte, a wrong key, a
truncated or damaged signature, or a statement of another format is refused
(`internal/release/release_test.go`, `cmd/tools/release-sign/main_test.go`).

```bash
# the owner's ceremony (once; the public half is published, the private half sealed)
umask 077
release-sign keygen -out /root/release.key > release.vkey

# signing — on the machine that holds the key, never GitHub Actions
RELEASE_SIGNING_KEY_FILE=/root/release.key release-sign sign vaultaire-linux vaultaire.cdx.json e2ee.wasm index.mjs sdk-wasm.cdx.json sdk-js.cdx.json
# → <file>.sig beside each; all or none

# verifying — offline, nothing but the tool and the public key
release-sign verify -vkey @release.vkey vaultaire-linux vaultaire-linux.sig
# and against the transparency log: the sha256 must be a build the log lists
release-sign verify -vkey @release.vkey -log https://trust.stored.cloud/translog -log-vkey @translog.vkey vaultaire-linux vaultaire-linux.sig
release-sign verify -vkey @release.vkey -log … -log-vkey … -client sdk-wasm e2ee.wasm e2ee.wasm.sig
```

**The key.** `sign` reads it like the log's signing key: the systemd credential
`release_signing_key`, then `RELEASE_SIGNING_KEY_FILE`, then `RELEASE_SIGNING_KEY`
— a source that is set but unusable is a refusal, no source is a refusal that says
so (`ErrNoSigningKey`), and nothing else changes: dark until the ceremony. The key
never lands in GitHub Actions and never in the repository; `keygen` writes the
signer key to a 0600 file that must not exist and prints only the verifier key.
**Where the public key is published** (the ceremony's second half): in every GitHub
release body, and on the trust host (`trust.stored.cloud`) beside the transparency
log's verifier key — serving it there is a residual of Prompt 41 (the trust host's
route list is closed by test and a key file must exist first).

## The release flow

1. Build reproducibly: `GOOS=linux GOARCH=amd64 scripts/build-release.sh vaultaire-linux`
   and `scripts/repro-check.sh vaultaire-linux`; the clients with
   `scripts/build-client.sh` (their rows in `internal/translog/clientbuilds/manifest.json`
   — the Deploy job rebuilds every row from its source commit).
2. SBOMs: `SBOM_SERVER_BINARY=vaultaire-linux scripts/sbom.sh bin/sbom`,
   `scripts/sbom-check.sh`, `scripts/repro-check-sbom.sh bin/sbom` (the Deploy job
   does the same and uploads them as the `sbom` artifact).
3. Sign (owner): `release-sign sign` over the binary, each client artefact and each
   SBOM → the `.sig` triplets.
4. The GitHub release: attach `<artefact>`, `<artefact>.cdx.json` and `<artefact>.sig`
   for each; the body names the transparency-log entry indexes of the
   `server_build` / `client_build` rows and the verifier key.
5. A reader: `release-sign verify` (offline), or with `-log` against the published
   checkpoint (`translog-verify` for the log itself).

A released client's SBOM is regenerated from its pinned source commit:
`SBOM_TARGETS=sdk-wasm,sdk-js scripts/sbom.sh <dir>` in a checkout of that commit
with the current script (sdk-e2ee-v0.2.0, source commit `4a8aa618…`, got its two
SBOMs attached on 2026-10-25 this way; its signatures wait for the key ceremony).

## What CI does and does not do

The Deploy job builds the SBOMs, holds them to go.sum and proves them
reproducible (keyless, cheap). It **never signs**: a signing key in CI is a key
anyone who can push a workflow holds.
