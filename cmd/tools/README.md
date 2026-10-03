# cmd/tools — operator probes and benchmarks

Nothing in this directory is part of the product. The product binary is
`cmd/vaultaire` only — `go list -deps ./cmd/vaultaire` never lists a
`cmd/tools` package, `make build` builds only `cmd/vaultaire`, and the
Security workflow's gosec run excludes this tree (operator-run CLIs whose
"tainted" inputs are the operator's own flags and environment).

Every tool reads credentials from **environment variables** (typically
`source .env.bench`, a gitignored file) — none reads a credential file from
the repository. Build with `go build -o <name> ./cmd/tools/<name>`; `make
clean` removes the resulting binaries from the repo root.

| Tool | Kind | Needs | What it does | Status |
|------|------|-------|--------------|--------|
| `backend-matrix` | probe | any of `IDRIVE_*`, `LYVE_*`, `GEYSER_*`, `R2_*`, `TENANT_N_*`; `MATRIX_LOCAL_DIR` optional | Pairwise interop: PUT on backend A, GET/Exists/Delete on every backend, prints a matrix | current (R7 used it) |
| `bench` | bench | `.env.bench` backend creds | Original single-backend throughput bench | superseded by `bench-compare`; kept for the JSON shape in `bench-results/` |
| `bench-compare` | bench | `.env.bench` | Cross-provider matrix (`-smoke`, `-only`, `-skip`); the tool `scripts/bench-vaultaire.sh` drives | current |
| `dedup-migrate` | one-off migration | `DB_*`, backend creds | Rewrites whole objects as chunked manifests. **Do not run**: races the GC and ignores the chunking flag (R8-06) — decision D-14 (delete or build-tag + fix) | frozen |
| `erasure-bench` | bench | backend creds, `PIXELDRAIN_API_KEY` optional | Reed–Solomon layouts across backends (`bench-results/ERASURE-*.md`) | current (2026-09-25/27) |
| `geyser-cloudsync-probe` | probe | `GEYSER_*` console creds, `GEYSER_MFA_CODE[_FILE]` | Exercises the Vail cloud-sync (RestoreToCloud) API; polls a public Lyve bucket for the landed object | current |
| `geyser-console-probe` | probe | `GEYSER_*` console creds | Login / MFA / token state for the Vail console (`-state` file, 0600) | current |
| `geyser-test` | probe | `GEYSER_*` | Round-trip through the Geyser driver | current |
| `geyser-admin-test`, `geyser-smoke` | probe | `GEYSER_*` | Local-only scratch probes (gitignored, not tracked) | local |
| `lighthouse-bench` | bench | none (public gateway) | Lighthouse/IPFS gateway evaluation | historical (not in the stack) |
| `loadtest` | bench | server URL + keys | Second load harness over `internal/loadtest`. The harness of record is `tests/load/` — decision D-7 (delete both) | frozen |
| `onedrive-bench` | bench | `TENANT_N_*` | The driver-path OneDrive/permafrost benchmark (`-files`, `-size-mb`, `-concurrency`) | current |
| `permafrost-v2`, `permafrost-v3` | bench | `TENANT_N_*` | Raw Graph API benchmarks behind `.private/PERMAFROST_TESTING_RESULTS.md` (v3 = HTTP/1.1 + Range) | historical |
| `pixeldrain-bench` | bench | `PIXELDRAIN_API_KEY` | CDN option evaluation (`internal/drivers/pixeldrain_README.md`) | historical |
| `routing-truth` | probe (read-only plan) | `DB_*` or `DATABASE_URL`; optional `DATA_PATH`, `IDRIVE_*`, `TENANT_N_*` | Head rows per recorded backend and tenant, a sample of each class asked of its driver (signed HEAD / stat / Graph), the other routing tables, and the plan per class — writes nothing (WP-R7-5, `docs/reviews/WP-R7-5.md`) | current (2026-10-03) |
| `uloz-bench` | bench | `ULOZ_LOGIN`, `ULOZ_AUTH_TOKEN` | Uloz.to evaluation (own `CLAUDE.md`) | historical (not in the stack) |
| `validate` | probe | server URL + keys | S3 conformance drive against a running server; `scripts/validate-backends.sh` cycles backends through it | current |

Deleted in Review R15: `quotaless-bench`, `quotaless-bench-v2`, `quotaless-debug`,
`quotaless-full-bench` (the Quotaless account is dead and the driver is slated
for removal — WP-R7-3). `tools/geyser-grabber` (a browser extension that
captures Vail console calls) stays under `tools/`; it is not Go.
