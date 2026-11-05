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
