# Permafrost fleet bench — six drives through the production driver (2026-11-05)

`cmd/tools/onedrive-bench` (the driver path: FNV placement, home-first reads, List union, Delete sweep) on SLC,
256 MB objects unless stated. Slots 1–3 = the three fleet accounts; slots 4–6 = one new 5 TB drive in each of the
same three tenants, reached with the same app registration (`TENANT_N_USER` is the only difference).

## Throughput

| Drives | Objects | Concurrency | PUT MB/s | GET MB/s | Errors |
|---|---|---|---|---|---|
| 1 | 16 | 8 | 199 | 235 | 0 |
| 1 | 48 | 24 | 506 | 353 | 19/48 GETs (429/503 on 8 MiB ranges), 10/48 DELETEs (429) — the per-user 3,000 req / 5 min |
| 2, same tenant | 16 | 8 | 186 | 302 | 0 |
| 3 | 24 | 12 | 204 | 297 | 0 |
| 3 | 48 | 24 | 477 | 415 | 0 |
| 6 | 24 | 12 | 228 | 284 | 0 |
| 6 | 48 | 24 | 477 | 657 | 0 |
| 6 | 96 | 48 | **826** | **659** | 0 — 24 GB in 30 s, ≈ 6.6 Gbps of the box's 10 |

Concurrency, not drive count, bounds a burst up to 24 streams; one drive tolerates about 8 streams before the
per-user request limit. Reads of a 256 MB object cost 32 requests at 8 MiB per range.

## Soak — 3 drives (one per tenant), 31 rounds × 48 × 256 MB at concurrency 24, 30 min

372 GB written and 372 GB read: 124 GB per drive per half hour = 5× the documented 50 GB/h ingress and 2.5× the
100 GB/h egress per user, under the 400 GB/h per app per tenant line. **No PUT throttling** (373–518 MB/s per round,
0 errors). Reads failed in 9 of 31 rounds (1–9 objects each) on single 8 MiB ranges: a few 429s early, then
`connection reset` from the download CDN (13.107.136.10 / .138.10); the driver's two attempts did not absorb them.

## Small objects — 1,000 × 1 MB, concurrency 16, two tenants

PUT 21.3 objects/s = **10.6 writes/s per tenant** — the 1,250 request-units/min line at 2 RU per upload. GET 39/s.
DELETE 14.8/s, one 429 in 1,000.

## Other findings

- **Drive selection:** `getDriveID` takes `/users/{upn}/drives[0]`. Two of the three fleet users have two drives,
  `PersonalCacheLibrary` (a hidden system library) and `OneDrive`, and the order differs per account: for two
  accounts the driver's `vaultaire/` tree sits in the PersonalCacheLibrary, for one in OneDrive. The default drive
  (`/users/{upn}/drive`) is the OneDrive library. Select by name, record the id.
- **Recycle bin = quota:** a driver DELETE parks bytes for 93 days against the drive. After the bench the three
  fleet drives held 68 / 23 / 21 GiB in their bins. `POST /personal/<alias>/_api/site/RecycleBin/DeleteAll`
  (SharePoint, app-only) reclaims at once; Graph `driveItem/permanentDelete` answers 204 with the data app.
- **New drives:** created only by the user's own first sign-in (a Temporary Access Pass works, no Authenticator
  registration wall); the SharePoint bulk enqueue did nothing in 8 h. Quota = the tenant's `OneDriveStorageQuota`
  default (1 TB in two tenants until raised to 5 TB through the tenant admin API).
- **Sign-in blocked (`accountEnabled=false`):** app-only drive access unaffected.
- Graph `/users/{id}/drive` from an app without a Files/Sites role answers 404 for existing drives — not an
  existence check. SharePoint `PeopleManager/GetPropertiesFor` → `PersonalUrl` and `/personal/<alias>/_api/site/usage`
  (`StoragePercentageUsed` is a ratio, not a percent) are the truth.

## What changed (WP-FLEET-1 + WP-FLEET-2, 2026-11-05/06; #1167, #1169, #1168)

**Drive selection (#1167, prod `273e8317`, booted 2026-11-06 00:07 UTC).**

- Before the deploy, prod's `.env` got `TENANT_1/2/3_DRIVE_ID` pinned to the drive that held each slot's `vaultaire/` (backup `.env.bak-fleet-drive-pin-20261105T230754Z`):
  - slot 1: PersonalCacheLibrary `…To0yBofch7MF`
  - slot 2: OneDrive `…SZNUJVnYOYQh`
  - slot 3: PersonalCacheLibrary `…ToPkXGXHDoL0`
- After the deploy:
  - three `permafrost account drive` Info lines named those drives with `pinned: true`;
  - `vaultaire_permafrost_drive_mismatch` read 0 for all three accounts;
  - `vaultaire_permafrost_fleet_mismatch` read 0, and the fingerprint was `match`, `written: 0`.

**What prod's database names on the fleet: nothing.** There are 0 head rows and 0 version rows on `permafrost`/`onedrive`, and the only `vault_parity` row is on Lyve. The trees hold bench data only: the 2026-11-04 `vbench` parity shards, `benchmark-*.bin` from 2025-11, and the fingerprint marker. So `routing_truth` has nothing to sample on `permafrost`. The switch was checked by the tool's verify and by reading the moved objects back through the driver.

**The migration tool (#1169), live on SLC with `.env.bench`.**

- `-selftest -slot 3`: Graph's `quickXorHash` matched the Go port on all 8 vectors (1 B … 1 MiB + 7). The cross-drive `copy` of the scratch folder completed in 6 s and verified. Both scratch folders were `permanentDelete`d and confirmed gone.
- `-dry-run` on the three slots:

| Slot | Where `vaultaire/` is | Files | Folders | Size |
|---|---|---|---|---|
| 1 | PersonalCacheLibrary | 1,296 | 182 | 6.90 GiB |
| 2 | OneDrive | 1,306 | 179 | 6.85 GiB |
| 3 | PersonalCacheLibrary | 1,274 | 177 | 5.97 GiB |

**Slot 3 (fromhis@fromhis.com), the first account moved (item 9).**

1. `-copy`: one Graph `copy` of the folder, finished in 2 min 23 s. Verified: 1,274 files, 177 folders, 6,405,661,665 bytes, every `quickXorHash` equal. 362 requests, 0 throttled, 3 min 4 s in all.
2. `-switch`: verified again, wrote `drive_id` = OneDrive `…7pmIe9H4` on both markers (the old one gained `moved_to`). 369 requests, 21 s.
3. Prod `.env`: `TENANT_3_DRIVE_ID` set to the OneDrive library (backup `.env.bak-fleet-slot3-switch-20261106T000837Z`), then `sudo vaultaire-switch restart`.
4. After the restart: tenant-3's Info line names `…7pmIe9H4`, `drive_mismatch` reads 0 ×3, the fingerprint is `match`.
5. `blob-verify -driver permafrost -mode full` with prod's pins read both bench containers whole through the driver (`…_vbench-viera-vaultaire`, `…_vbench-user1-vaultaire`): 3,314 objects, 7.45 GB, all `ok`, **0 fallback hits**, so every object was found on its home drive (slot 3's now the OneDrive library).

**permanentDelete (#1168, prod `d542e51b`).** The proof (item 13), all with prod's pins:

- `fleet-recyclebin.py` before;
- `run-fleet.sh 1,2,3 24 256 12 permadelete`, using `onedrive-bench` built from #1168: PUT, GET, LIST, then DELETE of 24 × 256 MB at 2,460 MB/s with 0 errors, 0 objects left;
- `fleet-recyclebin.py` after.

| Account | Bin items before | Bin items after | Bin size (both) |
|---|---|---|---|
| vierafun | 1,194 | 1,194 | 68.38 GiB |
| vierapics | 502 | 502 | 23.15 GiB |
| fromhis | 440 | 440 | 20.88 GiB |

**Bin growth 0.** Before #1168 the same round added 24 items and 6 GiB to the bins. Four GETs of the round failed on one 8 MiB CDN range answered 503, which the driver does not retry (WP-FLEET-3's item, as in the soak above).

**Quota gauges.** `vaultaire_permafrost_quota_bytes{kind="deleted"}` reads 73.4 / 24.9 / 22.4 GB: Graph's `drive.quota.deleted` IS the bin, and `remaining` = total − used − deleted. So `PermafrostAccountNearFull` reads (total − remaining) / total. The rule file is installed on the box (`/etc/prometheus/rules/vaultaire-drivers.yml`, backup `.bak-20261106-fleet2`), and the rule loads `inactive`.

**Owed:**

- slot 3's `-retire -confirm-retire` (the old 5.97 GiB tree in its PersonalCacheLibrary) after 24 h green, no earlier than 2026-11-07 00:10 UTC;
- then slot 1 the same way (`-copy` → `-switch` → `TENANT_1_DRIVE_ID` → restart → read-back → 24 h → `-retire`);
- slot 2 needs no move: its `-switch` only records `drive_id`;
- the one-time purge of the three bins (~112 GiB) is the owner's decision (D-F1-6).

## What changed (the WP-FLEET next chain, 2026-11-06/07; #1175–#1183)

The chain is `docs/reviews/WP-FLEET-NEXT-PROMPT.md`. All of it was measured on SLC: the test drives (`stored-fleet-test2@<tenant>`, slots 5 and 6 of `run-fleet.sh`) and the parity lab. No prod fleet account was benched.

### WP-FLEET-3: ride the bursts (#1175 PR A, #1177 PR B)

**The soak, repeated.** Two drives in two tenants, 48 × 256 MB PUT then GET per round at concurrency 24. The driver was built from #1177, plus `onedrive-bench`'s new closing `permafrost metrics:` line, which sums the driver's own counters for the run.

| Run | Rounds | PUT (both drives) | GET (both drives) | Failed objects | `size mismatch: got 0` | 429s absorbed |
|---|---|---|---|---|---|---|
| **Paced**, the defaults (8 req/s, 50 / 100 GB/h per drive, 400 GB/h per tenant), 2 h | 13 | 30.1–30.3 MB/s | 71.2–71.3 MB/s | **0** | **0** | **0** |
| Bytes unpaced, the request bucket still at 8/s, 18 min | 8 | 408–453 MB/s | 130–134 MB/s | 0 | 0 | 0 |
| **Fully unpaced** (`PERMAFROST_ACCOUNT_RPS=0` too), 2 h | 93 | 105–480, mean 401 MB/s | 89–686, mean 473 MB/s | **0** | **0** | **187**: 174 range, 10 fragment, 3 graph; 1 fragment resumed |

- **Acceptance is met:** 0 failed objects with the bursts counted, and never a clean empty body. The 2026-11-06 soak at the same load lost ≥ 20 of 48 objects in 19 of 111 rounds and had 135 `got 0`.
- **The unpaced run had to be repeated.** The prompt's unpaced settings (the three GB/h knobs at 0) leave the request bucket at 8/s. Two drives × 8 requests/s × 8 MiB ranges is 134 MB/s, exactly what those reads measured, and no 429 ever came: the request pacing alone keeps the account under Microsoft's line. The fully unpaced run brings the 2026-11-06 load back, and the ride-through with it.
- **Microsoft's `RateLimit-*` headers appeared** (never seen before). In the fully unpaced run the accounts were held for `Retry-After`s and `RateLimit-Reset`s: 162,599 caller-seconds over 2 h, about 30 s per round per caller, with round 4's 510 s of holds and no throttle counted.
- Paced rates sit a little above the published ones (about 54 / 128 GB/h per drive). Each round is a new process, so each starts with a full one-minute burst bucket; a long-running server takes that burst once.
- Range hedges: 70–93 lost and 0–6 won per paced round (about 5 % extra range requests). Unpaced: about 105–155 lost and 40–63 won, so hedges win about 30 % of the time under load, the straggler cure.
- After the runs the test drives held 0 bytes and 0 items in their recycle bins (`fleet-recyclebin.py --test-only`). About 4.5 TB was written and deleted, and `permanentDelete` (#1168) left nothing.

### WP-FLEET-4: transfer tuning (#1179, fix #1183)

Unpaced, test drives, PR B's binary against main's:

| Shape | #1177 | #1179 | #1179 + #1183 |
|---|---|---|---|
| GET 48 × 256 MB, c24 | 135 MB/s | **460 MB/s** | — |
| GET 8 × 256 MB, c2 | 116 MB/s | 159 MB/s | — |
| PUT 48 × 256 MB, c24 | 440 MB/s | 363 MB/s | — |
| PUT 8 × 256 MB, c2 (a session) | 45.3 / 42.7 / 43.7 MB/s | 33.8 MB/s | 41.1 / 39.2 MB/s |
| PUT 8 × 200 MB, c2 | 41.9 / 39.1 / 39.2 MB/s (a session) | **20.4 MB/s** (one simple PUT over HTTP/2) | 40.7 / 30.3 MB/s (one simple PUT over HTTP/1.1) |

- **Reads:** 32 MiB ranges carry four times the bytes per request under the same request bucket, so a 256 MB read is 8 requests instead of 32.
- **The proof found a regression, fixed in #1183.** The 200 MiB simple PUT went over the Graph client, which is HTTP/2, and Go's HTTP/2 flow control held it to about 10 MB/s per object. It now uses the HTTP/1.1 upload transport and is level with the session path. Its gain is requests, one instead of four, not speed.
- **Not met: "per-object write ≥ 24 MB/s".** No binary got there. One upload stream from SLC to OneDrive writes about 20 MB/s, in a session or in one PUT; aggregate write speed comes from concurrency across objects.

### Part 3: the sealed-only leg rule (#1176)

Measured on prod after deploy: `vaultaire_vault_parity_unprotected_objects{reason="unsealed_leg"}` is 0. Prod's leg is Lyve, and no row names permafrost.

### Part 4: parallel protects (#1178)

Parity lab, built from main, with the leg = the three test drives. 12 × 256 MiB GLACIER objects, sealed under the lab's master key (`ML-KEM-768+AES-256-GCM`), each set protected in one run.

| `VAULT_PARITY_PROTECT_CONCURRENCY` | First row → last row | Job run |
|---|---|---|
| 1 | **192 s** | — |
| 4 | **75 s** (39 % of sequential) | 75 s, 12 protected, 0 partial or failed |

**Not met: "≤ 1/3 of the sequential time".** The speed-up is 2.6×. With four in flight the bound looks like the four parallel Geyser read-backs, not the fleet. The lab tenant is free tier, so its quota was raised in the lab database only, and the sets were deleted through S3 after, their shards with them.

### WP-FLEET-11 and WP-FLEET-10 (#1180, #1182)

- **WP-FLEET-11:** a fake fleet of 300 drives boots in about 3 s (50 ms per Graph call; one after the other it took 45 s). A miss on a settled fleet costs 1 request, where it cost 300. A probe samples ≤ 16 accounts.
- **On prod:** the boot reads `match`, and every `vaultaire_permafrost_fallback_walks_total` series is 0.
- **WP-FLEET-10:** rules `PermafrostHashMismatch` and `PermafrostFallbackWalking` are installed on the box beside `PermafrostThrottling` (all six `Permafrost*` rules `inactive`). Each fires on its forced condition in its promtool test; a live Prometheus saw none, because the bench drives the driver without a server.

### The owed slot moves

- **Slot 3 retired (2026-11-07 01:00 UTC), after a refusal.**
  - The tool refused first: the old library held 1 file, 68 MB of the parity lab's shards, and empty `t-ecbench/ec-*` folders, all written *after* the switch by binaries older than #1167, which took `drives[0]`.
  - `fleet-admin/fleet-fill.py` diffed both trees and Graph-copied the 6 missing items into the OneDrive library. Then `-retire` verified 1,275 files by count, bytes and quickXorHash, and permanently deleted the old tree.
- **Slot 1 moved (2026-11-07 01:24 UTC).**
  - `-copy`: 1,296 files, 191 folders, 7,405,467,690 bytes, every quickXorHash equal, 0 throttled, 13 min. Then `-switch`, `TENANT_1_DRIVE_ID` set to the OneDrive library (backup `.env.bak-fleet-slot1-switch-20261107T012450Z`), and `vaultaire-switch restart`.
  - Read back through the driver with prod's pins: 3,314 objects `ok`, **0 fallback hits**. 7 keys first answered 429 on metadata at concurrency 16 from a pre-pacing `blob-verify` build, and read whole at concurrency 1.
  - **Owed:** `-retire` no earlier than 2026-11-08 01:25 UTC. Rebuild the parity lab's `erasure-bench` (it still takes `drives[0]`) before any run touches slots 1–3, or the old library gets writes again; the lab's `vaultaire` is now main's build.

## Evening additions (2026-11-05 22:00 → 2026-11-06 07:00 UTC) — the gate rehearsals

**Worker ↔ fleet, no origin bytes (a throwaway Worker on the bench account, deleted after; bodies synthesised in the Worker).**
Writes, one upload session, 96 MiB: 20 MiB fragments ≈ 13 MB/s serial or pipelined; **56.25 MiB fragments (180 × 320 KiB) ≈ 25 MB/s**; a streamed fragment needs a known length (`FixedLengthStream`; a plain stream is refused by Graph) and performs like a buffered one. Pipelining pays only against a client's upload: with a client body through the Worker, pipelined 20 MiB fragments wrote 24.6 MB/s against 7.6 read-then-send. Parallel sessions on one drive: 8 × 96 MiB → **171 MB/s** aggregate, 12–16 × 96 → ~123, 24 × 64 → 4 MB/s each (throttled). Cloudflare answered **413 to a 256 MiB request body** (the plan's 100 MB limit).
Reads from the download URL inside a Worker, 96 MiB: 1 range 150–172 MB/s, 4 → 258–278, **8 → 278–351**, 16 → 214–284, 32 → 126–159. The URL stayed valid 60 min and answered 401 at 70.
RS(4,4) decode in plain JavaScript, stripe-wise (4 × 1 MiB in, 4 × 1 MiB out, buffers reused): 64 MiB 0.64 s, 256 MiB 2.4 s, **1 GiB 9.9 s ≈ 110 MB/s**; a whole-object decode of 256 MiB exceeded the isolate (error 1102).

**Six drives as the Vault parity leg THROUGH Vaultaire** (the 2026-11-04 parity lab, same binary, `VAULT_PARITY_LEGS=permafrost`, slots 4–6 = the three new drives): protect 64 MiB 10.6 s / 256 MiB 14.6 s / 1 GiB 53.4 s (unchanged from three drives — Geyser's read-back); parity-serve reads 64 MiB 36–63 MB/s, 256 MiB 71–84, 1 GiB 78–101 with one run at 17 (a straggler shard); **two concurrent 1 GiB 115 MB/s aggregate, four concurrent 298 MB/s** (three drives on 2026-11-04: 62–77).

**Migration primitive:** a Graph cross-drive `copy` of a 3-file sandbox folder from the PersonalCacheLibrary to the OneDrive library of the same user completed in 6.4 s; every size and `quickXorHash` matched.

**Small objects, raw** (1 MiB simple PUTs, one drive, 300 per step): concurrency 8 → 8.8 writes/s, 16 → 17, **32 → 33**, 64 → 25; 1,200 writes, no 429, no `RateLimit-*` header. The 10.6 writes/s per tenant in the morning table was the bench's latency at 8 in flight, not a budget.

**Two-hour soak, two drives in two tenants** (48 × 256 MB PUT then GET per round, concurrency 24; 111 rounds): **666 GB written and 666 GB read per drive** (6.7× / 3.3× the published hourly rates). Clean rounds PUT median 436 MB/s, GET median 642 (both drives). 19 rounds (17 %) lost ≥ 20 of 48 objects to `HTTP 429` on 8 MiB ranges — about one burst every five rounds (gaps 5,3,2,1,5,5,5,10,10,6,5,5,11,6,6,6,4,1), each about one round long, the next round always clean; 44 rounds lost 1–14 objects; 9 rounds had write failures (fragment 429s, the driver's retries exhausted). Error kinds over the soak: 574 range `HTTP 429`, **135 `size mismatch: got 0`** (the driver ended a body cleanly after 0 bytes when its ranges were refused), 63 connect/TLS errors to the `-my.sharepoint.com` host, 9 resets. Microsoft's response to over-rate traffic is periodic bursts, not a cap.

**Driver-level erasure bench with the six-drive fleet** (`cmd/tools/erasure-bench`, RS(4,4), data shards on Geyser LA1, parity on the fleet, read-back hashed; a concurrent soak was loading one of the six drives' tenant, which is what the 429 lines are):

| Object | Protect (4 shards to the fleet) | Read, first 4 of 8 | Read, Geyser lost (fleet alone) | Read, fleet lost (Geyser alone) |
|---|---|---|---|---|
| 256 MiB | 5.2 s (99 MB/s wire) | 127 MB/s | 79 MB/s | 66 MB/s |
| 1 GiB | 12.4–14.9 s (138–166 MB/s wire) | 118 MB/s | 97 MB/s | 62–79 MB/s |
| 1 GiB, **hedged** reads | 12.6–13.0 s | 92–152 MB/s | **124–131 MB/s** | 79–80 MB/s |

Under the neighbouring tenant's 429 burst one shard PUT failed its retries and two parity-only reads failed with "only 3/4" / "only 2/4 shards" — the same lesson as the soak: a burst must be ridden (WP-FLEET-3), and a hedge lifts the fleet-only read by a third (97 → 124–131 MB/s on 1 GiB).

**Multi-leg layouts with the six-drive fleet** (erasure-bench, data on Geyser, nothing else on the fleet; the Sync bridge was not configured in the lab, so the stack is Geyser + Lyve + fleet):

| Layout | Object | Protect (all shards landed) | Read first 4 of N | Lose Geyser | Lose Lyve | Lose fleet |
|---|---|---|---|---|---|---|
| stacked RS(4,8): geyser:4 + lyve:4 + permafrost:4 | 256 MiB | 4.6–6.4 s | 102–107 MB/s | 94–111 | 69–110 | 87–100 |
| stacked, same | 1 GiB | 12.5–13.6 s (fleet slowest, 226–246 MB/s wire) | **140–160 MB/s** | **158–204** | 157–158 | 115–164 |
| spread RS(4,4): geyser:4 + lyve:2 + permafrost:2 | 256 MiB | 4.3–4.4 s | 74–87 | 72–83 | 70–81 | 73–81 |
| spread, same | 1 GiB | 10.7–12.9 s | 105–110 | 92–129 | 73–78 | 77–83 |

The 2026-11-04 verdict holds with six drives: a full parity set on each leg races the legs (any one leg lost still reads at 115–204 MB/s) and costs 2× parity bytes here (3× with Sync); a spread layout needs every leg for a fast read and loses a third of its speed when any one is gone. Every read hashed clean; no 429 with the fleet otherwise idle.

**Single tenant, two drives, 30 min, nothing else on the fleet** (the viera.pics test drives; same round shape as the two-hour soak): 21 rounds, **12 lost ≥ 20 of 48 reads and 7 lost writes** (two rounds wrote nothing); only 2 rounds read clean (690 MB/s). The two-tenant soak's first 30 minutes, at the same per-drive rate, lost reads in 5 of 30 rounds and writes in none. Each drive moved ≈ 420 GB/h, so the tenant carried ≈ 840 GB/h against Microsoft's 400 GB/h per app per tenant: **the per-tenant budget is what binds a dense tenant, and it binds writes too.** Design: pace per tenant, spread any job's drives across tenants.
