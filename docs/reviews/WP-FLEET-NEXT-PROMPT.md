# WP-FLEET, the next chain — ride the bursts, tune the transfer, seal the leg, parallel protects, scale the driver — Opus edition

*Written 2026-11-06 after the gate rehearsals (`docs/FLEET.md`, `bench-results/PERMAFROST-FLEET-2026-11-05.md` § Evening additions, plan § P7). Line numbers are from `origin/main` at `a849133a` (2026-11-06) — re-grep before editing. WP-FLEET-1 and -2 are shipped (#1167, #1169, #1168, #1172); this chain is everything the fleet needs before volume that does not depend on the tenant inventory the owner has deferred (WP-FLEET-0, -5, -6, -7, -8, -9 wait for it).*

You are working in the Vaultaire repository (`/Users/viera/fairforge/vaultaire`). The main checkout is on another session's branch with uncommitted files: never `git checkout` or `git stash` it. One worktree per part from `origin/main` (`git worktree add ../vaultaire-<part> origin/main`). The rules of `CLAUDE.md` hold: TDD (red test first), `make lint` + `make test` green before each push, PRs through the merge queue, never `--admin`, no AI attribution lines, migrations additive, nothing from `research_notes/`, `reports/`, `.private/` committed. Prod = `ssh vaultaire-slc`; sudo password in the Mac Keychain item `vaultaire-slc-sudo` piped to `sudo -S` on the ssh stdin. Never print a secret or a pre-authenticated URL. Each part is its own PR series; merge one part before starting the next unless they touch disjoint files. Read `docs/FLEET.md` first — it is the measured truth every decision below rests on.

## Why this order: what the soak failures mean

The two-hour and single-tenant soaks did not break any drive. Every failure was Microsoft answering `HTTP 429` on 8 MiB download ranges or on upload-session fragments — a one-minute burst about every five minutes above the published per-user rates, and a near-continuous one when two drives in ONE tenant ran flat out (the per-app-per-tenant 400 GB/h budget). The data was intact every time; the next round read it at full speed. What the failures mean for production as it stands:

- a customer GET served from the fleet during a burst would be CUT by the API's integrity reader (a short body never ends cleanly) and the client retries — an availability blip, never wrong bytes;
- a Vault rebuild from parity during a burst fails ("only 3/4 shards") and falls back to tape — minutes to hours instead of seconds;
- a parity protect whose shard PUT fails leaves the row `partial` and the job retries next run — a longer unprotected window, no loss;
- **one real defect:** the driver returned a clean 0-byte body 135 times under a burst (`size mismatch: got 0`) — inside the parity rebuild that is a shard that decodes to garbage unless caught; the rebuilder's length/ETag checks are the only guard today.

A real drive loss (a licence lapse, a suspended tenant, Microsoft removing a drive) costs one shard per object that homed there; any four of eight shards rebuild, tape holds the data, and the job re-protects onto the remaining drives. The fleet is designed to lose drives; it is not yet designed to ride a throttle. That is Part 1.

## Part 1 — WP-FLEET-3: ride the bursts (`docs/reviews/WP-FLEET-3-PROMPT.md`)

Execute that prompt as written (PR 0 red tests → PR A the body-fails fix + attempts → PR B pacing/hedge/resume/metrics → PR C the soak repeated). Two additions since it was written:

- **D-F3-5a** the per-TENANT bucket is the primary control, per-drive buckets second (the single-tenant soak: 12 of 21 rounds lost, writes too). Default `PERMAFROST_TENANT_GBH` 400 each way; the bucket is keyed on the tenant id, shared by every slot in that tenant.
- **D-F3-10** token renewal pauses after a transient error from `login.microsoftonline.com` (`odTenant.cred` refresh path): one renewer, the others keep the valid token, no per-request storm (the Share-one-Drive 3.10.5 pattern).

Acceptance is the prompt's PR C: the same soak shape with 0 failed objects, bursts counted, never a clean empty body.

## Part 2 — WP-FLEET-4: transfer tuning

Decisions:
- **D-F4-1** Read ranges on the fleet 32 MiB (`odRangePart`, `onedrive.go:65`; 64 MiB for objects ≥ 1 GiB): a 256 MB object costs ≤ 8 requests instead of 32 (the per-user request budget is what one drive hits first).
- **D-F4-2** Simple PUT (`PUT …:/content`) up to **200 MiB** (`simpleUploadMax`, line 56, is 4 MiB today; Microsoft allows 250 MB): one request per shard of an object ≤ 800 MB. Above it the session, whose fragments become **58,982,400 B** (180 × 320 KiB; `odChunkSize` 60 MiB sits ON the bound).
- **D-F4-3** The session is **double-buffered**: two pooled fragment buffers, one in flight while the next is read from the source (`odChunkPool`, `odFragment`, `putChunk` line ~1419 fill-then-send today). Measured: a pipelined session from a client body 24.6 MB/s vs 7.6 serial; from a local body the largest fragment ~25 vs ~13 — so both changes, together.
- **D-F4-4** Write verification: the final fragment's (or the simple PUT's) item carries `file.hashes.quickXorHash`; the driver hashes the body as it streams (`internal/drivers/quickxor.go`, #1169) and a mismatch is `engine.ErrObjectChanged`-class failure of the Put (the shard is `permanentDelete`d), counted `vaultaire_permafrost_hash_mismatch_total{account}`.
- **D-F4-5** Session resume by `nextExpectedRanges` is Part 1's (D-F3-3); do not duplicate.

Red tests: on the fake Graph, a 200 MiB body is one request; a 300 MiB body is 6 fragments of 58,982,400 B + remainder; the second fragment is sent before the third is fully read (a slow source proves the overlap); a corrupted hash fails the Put and deletes the item; a 256 MB read makes ≤ 8 range requests. Proof: `onedrive-bench` 48 × 256 MB c24 on the test drives — per-object write ≥ 24 MB/s, requests per object down 4×; record in the bench file.

## Part 3 — the sealed-only leg rule (the plaintext gate)

`internal/api/vault_parity.go:393–396`: a cleartext-transport leg (`blomp`) and `sia` are skipped for an UNSEALED object unless `AllowCleartextLegs` / `AllowSiaLeg`; `permafrost` (TLS) is not — so four RS shards of a plaintext Vault object are a complete readable copy under the data app's credential. Decision **D-F-SEAL-1:** the fleet joins that rule — an unsealed object's shards are not written to `permafrost` unless `PERMAFROST_ALLOW_UNSEALED=true` (a Warn at boot, like the other two); an SSE-S3 or SSE-C object's shards (ciphertext) go as today. With no master key on prod (F-14 pending) this means the fleet protects only SSE-C Vault objects until the key ceremony — the correct state. `vaultaire_vault_parity_unprotected{reason="unsealed_leg"}` joins the existing reasons; the operator page and `docs/BACKEND_ENCRYPTION.md`'s permafrost row say so. Red test: an unsealed vault object with `permafrost` as the only leg is `unprotected{reason}` not `complete`; with the env flag it protects; a sealed one protects regardless.

## Part 4 — parallel protects (the Vault job's throughput)

Measured: one protect reads the object back from Geyser and writes four shards at ~20 MB/s (1 GiB ≈ 54 s) on three drives AND on six — the fleet is not the bound, the sequential protect is (`vault_parity.go` protects candidates one at a time within `MaxObjectsPerRun` / `MaxBytesPerRun` and the `ProtectRate` estimate, lines 287–293, 539–567, 820–828). Decision **D-F-PROT-1:** `VAULT_PARITY_PROTECT_CONCURRENCY` (default **4**, 1–16): up to N protects in flight per run, each with its own size estimate against the run's deadline, the byte budget shared; shards of concurrent objects naturally land on different drives (placement), and Part 1's per-tenant bucket keeps the sum under the tenant line. Red test: four small candidates protect in about the time of one on a fake Geyser with a fixed per-read latency; the budget is still respected; a failing protect does not stop the others. Proof on the lab (`~/parity-lab`, `VAULT_PARITY_LEGS=permafrost`, slots 4–6): 12 × 256 MiB objects protected in ≤ 1/3 of the sequential time. Record in `docs/reviews/WP-VAULT-2.md`'s successor note or the bench file.

## Part 5 — WP-FLEET-11: the driver at scale

Measured facts: `NewOneDriveFleetDriver` (line 179) reads `TENANT_1..15` (line 181) and builds a credential + three HTTP transports PER SLOT (line 214); `tenantsFor` (299) walks every account after a home miss (`Get` 445, `Exists` 901, `Delete` 587); `List` (643), `ListDir` (778), `RemoveEmptyDir` (810), `RemoveContainerTree` (713) union or sweep every account; `HealthCheck` (946) probes every account `odProbeConcurrency` (8, line 82) at a time inside the probe's 15 s. At 900 drives a miss is 900 Graph calls and the probe cannot finish. Decisions:
- **D-F11-1** A fleet file replaces the env slots past 15: `PERMAFROST_FLEET_FILE` (JSON: `tenants: [{tenant_id, client_id, certificate (systemd credential name or file), drives: [{index, user, drive_id}]}]`), the env slots still read for backward compatibility and refused when both are set; indexes are append-only and the marker records `(index, tenant, drive_id)`.
- **D-F11-2** One token source and one transport set per TENANT shared by its drives (`odTenant` → `odAccount{tenant *odTenantAuth, user, driveID}`).
- **D-F11-3** Placement-authoritative reads: once every marker matches (`FleetState` `match`), `Get`/`Exists`/`Delete` ask the home drive only — a miss is a miss (the WP-R6-1 rule); the fallback walk stays only while the state is `mismatch`/`unknown` and is capped at 8 accounts, counted `vaultaire_permafrost_fallback_walks_total`.
- **D-F11-4** `List`/`ListDir` ask the drives the placement can name for the prefix (a parity folder lives on one drive; the reconcile walks per drive with `/delta` in WP-9) and fall back to the union only on an explicit `-all` (the sweep).
- **D-F11-5** Probe sampling: every drive probed within an hour, `k = ceil(drives / 240)` per 15 s interval, the tenant token checked every interval; the fingerprint read with `$batch` (20 per call).
Red tests on a 300-drive fake fleet: boot under 20 s, a miss costs one request, the probe's work per interval is bounded, a 15-env-slot config still boots. No prod change in this part until the owner's inventory adds drives.

## Part 6 — WP-FLEET-10: observability for what Parts 1–5 added

Per-account gauges from the probe (quota kinds exist since #1168; add throttles, paced seconds, hash mismatches, fallback walks, protect concurrency in use), rules `PermafrostThrottling`, `PermafrostHashMismatch`, `PermafrostFallbackWalking`; the System page's fleet card lists accounts with drive, quota used/bin/total, throttles in the last hour, licence state (WP-6 brings the licence itself). Every alert fired once in a lab.

## Operator steps owed from WP-FLEET-1 (do these first, they need no code)

- Slot 3 (`fromhis`): 24 h after its switch (2026-11-06 00:08 UTC) with `routing_truth` present-count unchanged and `drive_mismatch` 0 → `permafrost-migrate-drive -slot 3 -retire -confirm-retire`.
- Slot 1 (`vierafun`, ~6.9 GiB in the PersonalCacheLibrary): `-dry-run` → `-copy` → `-switch` → `TENANT_1_DRIVE_ID` to the OneDrive library in prod `.env` (backup first) → `sudo vaultaire-switch restart` → routing truth green 24 h → `-retire -confirm-retire`.
- **Ask the owner** before: the one-time purge of the fleet drives' bins (69.7 / 24.5 / 22.6 GiB on 2026-11-06), deleting the seven test users, any prod env knob beyond the defaults.

## When a step does not go as written

Each part's own prompt or section says what to do; the standing rules: never serve or store a shorter body as a success; never widen a default without a measurement in the PR; a prod `.env` change always has a dated backup; stop and ask the owner for anything outside this document.
