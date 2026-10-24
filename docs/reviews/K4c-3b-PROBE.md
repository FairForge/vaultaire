# K4c-3b probe — is the SDK key core's WASM build byte-reproducible?

*Stage 1a Prompt 36 (2026-10-23), before any design. Plan 43.6 records "client build hashes (web, desktop, mobile, SDK — clients refuse an unlisted build)"; `docs/surfaces/TRUST.md` item 3 says the first entries are the master-key ceremony and every client build hash. The only client artefact a customer would run today is the SDK key core: `sdk/wasm` (sdk/go/e2ee compiled with `GOOS=js GOARCH=wasm`) and its binding `sdk/js/e2ee/index.mjs`.*

## The question

Can a third party rebuild `e2ee.wasm` from a commit and get the published bytes — the RP37-9 property the server binary already has (`scripts/build-release.sh`)?

## The probe

Two fresh copies of `origin/main` (`bc004600576eaeebc96acdd8bb85a9530febb34d`), extracted with `git archive` (no `.git`), each with its OWN `GOCACHE` and `GOPATH`, built seconds apart on the Mac (arm64 host; the target is `js/wasm`, host-independent for pure Go):

```sh
GOTOOLCHAIN=go1.26.10 CGO_ENABLED=0 GOFLAGS= GOOS=js GOARCH=wasm \
    go build -trimpath -buildvcs=false -ldflags=-s -o e2ee.wasm ./sdk/wasm
```

(The flags are the Makefile's `sdk-wasm` recipe plus `-buildvcs=false` and the pinned toolchain — exactly `build-release.sh`'s rules.)

Result — **byte-identical**:

```
07685b5f6dba6a5419a9c64ef3d524839f71a1452ef79ee21193628163dcc911  e2ee-1.wasm
07685b5f6dba6a5419a9c64ef3d524839f71a1452ef79ee21193628163dcc911  e2ee-2.wasm
cmp: identical
```

The Deploy job's gate (`scripts/repro-check-clients.sh`) then rebuilt the same commit on its own fresh copy and matched the manifest hash, which is the recurring form of this probe.

## What would make the bytes differ

- **The toolchain.** A different Go version is a different binary; the recipe pins `GOTOOLCHAIN` to go.mod's `toolchain` line (go1.26.10), and the entry records it.
- **Embedded paths.** `-trimpath` removes the build directory and GOPATH; `-buildvcs=false` keeps a dirty work tree or the VCS state out.
- **`-ldflags`.** `-s` (the Makefile's) drops the symbol table; changing linker flags changes bytes, so the recipe is one reviewed script, named in the entry.
- **`wasm_exec.js`.** Not part of the artefact: it is the toolchain's own loader shim, shipped beside the wasm and fully determined by the toolchain version the entry records (`$(go env GOROOT)/lib/wasm/wasm_exec.js`). A loader that pins the wasm hash should pin its shim the same way (K4c-3c, with the first app).
- **CGO / host.** `CGO_ENABLED=0` always; `js/wasm` has no host-specific code paths in our tree.

## What a client needs to identify itself

A loader (and an auditor) needs, per release: the **client** name (a closed list — `sdk-wasm`, `sdk-js`), the **version**, the **source commit** (40 hex: where to rebuild from), the **sha256** of the artefact as published, the **toolchain**, and the **recipe** (the script path). That is exactly the `client_build` entry's allow-list, and the manifest row's (`internal/translog/clientbuilds/manifest.json`).

`sdk/js/e2ee/index.mjs` is published as-is (its "build" is a byte copy), so its row records `toolchain: "none"` and its sha256 is the source file's: `4766be3dba6d856dea354cbb2c8c90a024eb585c51234d0f3271e906e9f9d7d0` at the same commit.

## Verdict

Reproducible today, with the pinned recipe. K4c-3b proceeds: the entry kind, the manifest, the Deploy gate, the boot appender and `translog-verify -client`.
