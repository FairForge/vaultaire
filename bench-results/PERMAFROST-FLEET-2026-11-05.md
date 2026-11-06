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
