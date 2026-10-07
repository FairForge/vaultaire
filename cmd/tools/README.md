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
| `geyser-restore-probe` | probe (timed customer path) | `VAULTAIRE_BENCH_ACCESS_KEY` / `VAULTAIRE_BENCH_SECRET_KEY` (a customer key; else `AWS_*`) — the customer endpoint only, no backend creds | Waits for an archive object to leave Geyser's landing zone for tape: every visit (hourly; `-once` for cron) is one HEAD and one 1-byte GET; while readable it hashes the object once (the baseline); the first time the GET is refused (403 `InvalidObjectState`, or 503 + `Retry-After`) it runs the customer path with timestamps — GET refused, RestoreObject, poll `x-amz-restore`, GET — compares the bytes (SHA-256 baseline, else the MD5 ETag) and appends one JSON line to `-report`. State per object in `-state-dir`; a restore that times out is resumed by the next visit (WP-VAULT-1, `docs/reviews/WP-VAULT-1.md`) | current (2026-10-05; hourly on SLC for `tier-archive-20261004/obj8.bin` and `vault-bench-20261004/v256.bin`) |
| `geyser-test` | probe | `GEYSER_*` | Round-trip through the Geyser driver | current |
| `geyser-admin-test`, `geyser-smoke` | probe | `GEYSER_*` | Local-only scratch probes (gitignored, not tracked) | local |
| `lighthouse-bench` | bench | none (public gateway) | Lighthouse/IPFS gateway evaluation | historical (not in the stack) |
| `loadtest` | bench | server URL + keys | Second load harness over `internal/loadtest`. The harness of record is `tests/load/` — decision D-7 (delete both) | frozen |
| `onedrive-bench` | bench | `TENANT_N_*` | The driver-path OneDrive/permafrost benchmark (`-files`, `-size-mb`, `-concurrency`) | current |
| `permafrost-v2`, `permafrost-v3` | bench | `TENANT_N_*` | Raw Graph API benchmarks behind `.private/PERMAFROST_TESTING_RESULTS.md` (v3 = HTTP/1.1 + Range) | historical |
| `pixeldrain-bench` | bench | `PIXELDRAIN_API_KEY` | CDN option evaluation (`internal/drivers/pixeldrain_README.md`) | historical |
| `routing-truth` | probe (read-only plan) | `DB_*` or `DATABASE_URL`; optional `DATA_PATH`, `IDRIVE_*`, `TENANT_N_*` | Head rows per recorded backend and tenant, a sample of each class asked of its driver (signed HEAD / stat / Graph), the other routing tables, and the plan per class — writes nothing (WP-R7-5, `docs/reviews/WP-R7-5.md`) | current (2026-10-03) |
| `uloz-bench` | bench | `ULOZ_LOGIN`, `ULOZ_AUTH_TOKEN` | Uloz.to evaluation (own `CLAUDE.md`) | historical (not in the stack) |
| `webdav-bench` | bench | `WEBDAV_PASSWORD` (env only); `-url` (default the Sync bridge `http://127.0.0.1:4918`), `-user` (`sync`) | A WebDAV endpoint for Stored's workloads through the real `WebDAVDriver` (+ raw WebDAV where the driver hides it): small objects, large streams, ranges, consistency, listing, the Vault parity I/O pattern, opt-in limits probes; every read sha256-verified; bridge RSS/CPU from `/proc` — see below | current (2026-10-06, PR #614) |
| `validate` | probe | server URL + keys | S3 conformance drive against a running server; `scripts/validate-backends.sh` cycles backends through it | current |

Deleted in Review R15: `quotaless-bench`, `quotaless-bench-v2`, `quotaless-debug`,
`quotaless-full-bench` (the Quotaless account is dead and the driver is slated
for removal — WP-R7-3). `tools/geyser-grabber` (a browser extension that
captures Vail console calls) stays under `tools/`; it is not Go.

## `webdav-bench`

Benchmarks a WebDAV server for Stored's workloads — first the Sync.com bridge
(`sync-webdav`, `internal/drivers/webdav_README.md`). Object traffic goes
through the real driver (`drivers.NewWebDAVDriver`, tenant `-tenant` in the
context), so MKCOL caching, the post-PUT size check (a Depth 0 PROPFIND after
every known-length PUT), Range handling and the PROPFIND walk are on the
measured path; raw WebDAV requests are used where the driver hides what is
measured. Data comes from a seeded ChaCha8 generator, streamed (1 GiB is never
in memory); **every read is byte-verified** (sha256) and a difference is
printed as `!!! MISMATCH` (exit status 2; other errors exit 1).

| Suite | Default | Measures |
|---|---|---|
| `small` | yes | 4 KiB / 64 KiB / 1 MiB × concurrency 1 / 8 / 32, `-n` (200) objects per cell: PUT, GET, Exists, Delete — ops/s, MB/s, p50/p95/p99 |
| `large` | yes | 16 MiB / 256 MiB / 1 GiB × concurrency 1 / 4 (= objects per cell): PUT and GET MB/s, GET time to first byte |
| `range` | yes | one 256 MiB object, 100 random reads of 64 KiB and of 4 MiB (`GetRange`): latency, bytes verified |
| `consistency` | yes | `-consistency-n` (50) rounds: PUT v1 → GET, PUT v2 → GET, DELETE → GET (not found). A stale read is polled every 50 ms (≤ 30 s) and reported with the time it took to resolve |
| `listing` | yes | 1,000 then 10,000 tiny files in one folder (`-list-counts`, capped by `-list-max`): driver `List` and one raw `PROPFIND Depth: 1`, each timed and counted |
| `parity` | yes | Vault's RS leg as I/O only: for 64 MiB / 256 MiB / 1 GiB with k=4 (shard = size/4), write m=4 shards concurrently, read them back concurrently, a degraded read of k shards; then 1,000 × 1 MiB shards at concurrency 16 in one folder vs a 2-level hex fan-out (`aa/bb/name`) |
| `limits` | **opt-in** | Sync's documented limits: total path length 200 / 248 / 249 / 300 / 1000 chars (`-limits-paths`; does PUT succeed, does GET read back), names with `: ? * < > \| " \`, trailing `.`, leading/trailing space, `%`, `#`, `+`, unicode, emoji, `CON`/`NUL`/`AUX`, `.DS_Store`, `desktop.ini`, `~$x` (PUT status/body, read back, listed). `-limits-folder` adds the 50,000-files-per-folder probe: up to `-limits-folder-max` (50,001) zero-byte files at `-limits-conc` (64), progress every 1,000, the first failure's index, status and body. Slow |
| `crossbridge` | **opt-in**, needs `-urls` (≥ 2) | Cross-bridge staleness: `-crossbridge-n` (20) rounds, `-crossbridge-conc` (4) at a time, each pairing bridge i with j (every pair in turn): PUT v1 through i (single-server driver), poll j every `-crossbridge-poll` (250 ms) until it serves those bytes; PUT v2 through i, wait until j serves v2; DELETE through i, wait until j answers not found. Rows `visible new / overwrite / delete` = the waits (p50/p95/max); a wait past `-crossbridge-timeout` (10 m) is an error. Live 2026-10-07: new 1.5–13 s, overwrite ~30 s (once 310 s), delete ~30 s |
| resources | always | `/proc/<pid>` of `-proc` (`sync-webdav`) once a second when it runs on this host: peak RSS, mean/peak CPU; size of `-spill-dir` (the bridge's `--upload-temp-dir`): peak and at the end |

Everything is written under `<-root>/t-<tenant>/run-<timestamp>/` (default
`_bench/t-bench/…`) and that folder is deleted at the end (`-cleanup=false`
keeps it); each case drops its own folder when it finishes, so the bridge's
spill directory holds one case at a time. The password is read from
`WEBDAV_PASSWORD` only — there is no flag — and is never printed. Output: a
table on stdout, the full report (rows, mismatches, errors, consistency
events, limits probes, resources) as JSON to `-out`. Sizes take `4KiB`,
`16MiB`, `1GiB` or bytes; every list flag is comma-separated (`-h` for all).

Stalls never hang a suite: the driver is built with the server's bounds
(`-idle-timeout` 60s, `-put-timeout` 2m per 64 MiB, `-max-concurrency` 8,
`-retries` 3 — `internal/drivers/webdav_resilience.go`), and `-op-timeout`
(30m, 0 = off) cancels any one operation and any raw request but the run
folder's recursive DELETE. A stalled op is an error on its row; the summary
line `stalls: upload N, download N …; retries N; ops cut off by -op-timeout N`
(and `stalls` in the JSON) counts them. The 2026-10-06 run hung 45+ minutes on
a 256 MiB PUT the bridge stopped reading after 62 KB — that is what these
bounds are for.

On SLC (the bridge listens on localhost only):

```bash
GOOS=linux GOARCH=amd64 go build -o webdav-bench ./cmd/tools/webdav-bench
scp webdav-bench vaultaire-slc:/tmp/
# on the box, as a user that can read the bridge's credentials:
WEBDAV_PASSWORD=$(sudo grep ^SYNC_WEBDAV_PASSWORD= /opt/vaultaire/configs/.env | cut -d= -f2-) \
  /tmp/webdav-bench -run small,large,range,consistency,parity -spill-dir /var/lib/sync-webdav/spool -out results.json
# the limits probes (opt-in; -limits-folder takes a long while):
WEBDAV_PASSWORD=… /tmp/webdav-bench -run limits -limits-folder -out limits.json
# several bridges of one folder, through the multi-bridge driver the server uses
# (one key per bridge by HRW; -large-concurrency = SYNC_WEBDAV_LARGE_CONCURRENCY):
WEBDAV_PASSWORDS=$(sudo grep ^SYNC_WEBDAV_PASSWORDS= /opt/vaultaire/configs/.env | cut -d= -f2-) \
  /tmp/webdav-bench -urls "$(sudo grep ^SYNC_WEBDAV_URLS= /opt/vaultaire/configs/.env | cut -d= -f2-)" \
  -run small,large,crossbridge -out multi.json
```

`-urls a,b,…` (with `WEBDAV_PASSWORDS`, comma-separated, same order) drives
`drivers.NewMultiWebDAVDriver` instead of the single-server driver; the raw
client uses the first bridge. The stall/retry summary sums every bridge.

`main_test.go` runs every suite in tiny sizes against `golang.org/x/net/webdav` (and `-urls` + `crossbridge` against three servers on one file system)
(httptest + Basic auth), checks cleanup and that the password never appears
in the output, and that a server corrupting GET bodies is reported as a
mismatch, and that a PUT the server stops reading fails its op (driver idle timeout, or `-op-timeout` with it off) and is counted — so the tool cannot rot.
