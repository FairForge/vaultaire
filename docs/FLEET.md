# The OneDrive fleet (`permafrost`) — operator page

*Written 2026-11-06 from the measurements of 2026-11-04/05 (`bench-results/VAULT-PARITY-LEGS-CF-2026-11-04.md`, `bench-results/PERMAFROST-FLEET-2026-11-05.md`). The driver's internals are `internal/drivers/onedrive_README.md`; the work packages are `docs/IMPLEMENTATION_PLAN.md` § P7. This page is the rules an operator needs, with the number behind each one.*

## What the fleet is

A set of Microsoft 365 OneDrive for Business drives, one per licensed user, in tenants Stored administers, reached by the `permafrost` driver through Microsoft Graph with one app registration per tenant (certificate or secret, app-only). Each drive is **5 TB** when the tenant's `OneDriveStorageQuota` default says so (a tenant left at the 1 TB default provisions 1 TB drives — two of three tenants were, until 2026-11-05). A plan whose only storage plan is OneDrive Lite gives 5 GB: the 5 TB seats are the ones carrying `SHAREPOINTENTERPRISE` (SharePoint Plan 2 — today the Dynamics 365 bundle; renewal mid-2028).

**What it is for:** the Vault parity leg (four Reed-Solomon shards per object, any four of eight rebuild it), the cold half of Standard, operator backups. **What it is never:** the only copy of anything (one company controls every tenant), a store for small customer files (shards and packs only), a hot tier.

## The limits, as measured (not as published)

| Dimension | Microsoft's page | What the fleet actually did (2026-11-05) | Rule |
|---|---|---|---|
| Streams per drive | 3,000 requests / 5 min per user | ~8 parallel streams or ~24 parallel upload sessions before 429s | **8 streams per drive**; 16 sessions per drive from the edge is the knee (171 MB/s), 24 collapses |
| Write volume per drive | 50 GB/h | 666 GB in 2 h (6.7×) with 9 of 111 rounds hit by a 429 burst | pace at the published rate; writes are not the problem |
| Read volume per drive | 100 GB/h | 666 GB in 2 h (3.3×); **a one-minute 429 burst about every five minutes**, 19 of 111 rounds lost most objects, the next round always clean | pace at the published rate as the good-tenant posture; the DRIVER must ride a burst (4+ attempts, full `Retry-After`, a hedge) — WP-FLEET-3 |
| Small writes per tenant | 1,250 RU/min (2 RU per upload) | 1,200 × 1 MiB PUTs on one drive at 33/s, zero 429s, no `RateLimit-*` header | small objects are latency-bound (~1 s each), not budget-bound; pack them anyway |
| Per app per tenant | 400 GB/h in and out | **two 5 TB drives in ONE tenant, 30 min, nothing else running: 12 of 21 rounds lost ≥ 20 of 48 reads and 7 of 21 rounds lost writes, against 5 of 30 and 0 of 30 for one drive per tenant at the same per-drive rate** (each drive ≈ 420 GB/h, so ≈ 840 GB/h on the tenant) | **the tenant budget is the one that bites first in a dense tenant**: pace per TENANT at 400 GB/h each way, spread a job's drives across tenants, never saturate one tenant's drives together |
| `RateLimit-*` headers | sent at ≥ 80 % of a budget | never seen at the published rates; **seen in the fully unpaced 2026-11-07 soak** (holds with no 429) | the driver holds the account below 20 % (WP-FLEET-3); still defensive, not the primary control |
| Download URL | "may expire within minutes" | 206 at +60 min, 401 at +70 | fetch per read, never cache; a long stream re-fetches and resumes by `Range` |
| Request body at the edge | — | Cloudflare 413 at 256 MiB (the plan's 100 MB limit) | edge writes are whole bodies ≤ 100 MB; larger = one upload session per piece + a manifest |
| Upload session | fragments < 60 MiB, multiples of 320 KiB, sequential | the largest fragment (180 × 320 KiB) is fastest from a local body (~25 MB/s per session vs ~13 at 20 MiB); pipelining helps only to overlap a client's upload | one session = one object (or piece); parallelism comes from sessions, not fragments |
| Hashes | `quickXorHash` only on Business | returned on every simple PUT, on the final fragment, on every later GET; Go port with live vectors in `internal/drivers/quickxor.go` (#1169) | verify every write and every migration copy by it |
| Recycle bin | 93 days, counts against quota | 68 / 23 / 21 GiB of deleted shards on the fleet drives after one bench day | every driver delete is `permanentDelete` since #1168; the bin is `vaultaire_permafrost_quota_bytes{kind="deleted"}`; a one-time purge is the SharePoint `RecycleBin/DeleteAll` (operator script, admin app) |
| Drive selection | `/users/{upn}/drives` lists several; order arbitrary | two of three fleet users list a hidden `PersonalCacheLibrary` first | the driver picks the drive named `OneDrive`; `TENANT_N_DRIVE_ID` pins it; the marker records it (#1167) |

## Throughput to plan with

- **From the origin (SLC, the driver):** 6 drives × 48 streams = 826 MB/s PUT / 659 GET, 0 errors — the 10 Gbps box is the next ceiling. One object ≈ 20–25 MB/s (one session); aggregate comes from concurrency across objects.
- **From the edge (a Worker, no origin bytes):** reads 171 MB/s per object whole, ~300 MB/s as 8 ranges; writes ~25 MB/s per session, 171 MB/s per drive with 8 sessions.
- **As the Vault parity leg through Vaultaire** (lab, same binary as prod): protect 64 MiB 10 s / 256 MiB 15 s / 1 GiB 54 s — bound by Geyser's read-back (~20 MB/s per object), not by the fleet; parity-serve reads of 1 GiB 78–137 MB/s single stream (one straggler run in ~10); **four concurrent 1 GiB rebuilds: 62–77 MB/s aggregate on three drives, 298 MB/s on six** — rebuild capacity scales with drive count.
- **Driver-level, the fleet alone as the parity leg (erasure-bench, six drives):** protect 1 GiB in 12–15 s at 138–166 MB/s on the wire; a parity-only read of 1 GiB 97 MB/s, **124–131 MB/s hedged** (a slow shard range re-requested on a fresh connection — the straggler cure).
- **Beside the other legs (erasure-bench, six drives):** a full parity set on each of Geyser, Lyve and the fleet reads a 1 GiB object at 140–160 MB/s and still 115–204 with any one leg gone; a spread of shards across legs reads at 73–129 and needs every leg. Stack, never spread (the 2026-11-04 verdict, confirmed).
- **Rebuild at the edge:** a Worker decodes RS(4,4) at ~110 MB/s in plain JavaScript, stripe by stripe (a whole-object decode exceeds the 128 MiB isolate); shard reads are not the bottleneck.

## Provisioning a drive (the recipe that works)

The SharePoint bulk pre-provisioning request (`CreatePersonalSiteEnqueueBulk`, what `Request-SPOPersonalSite` does) answered 200 and created nothing in 8 hours. **A drive exists about one minute after the user's own first sign-in.** The unattended recipe (`fleet-admin` scripts on SLC; the tool is WP-FLEET-7):

1. Create the user (Graph, the tenant's admin app), `usageLocation` set, a random password nobody keeps.
2. Assign the 5 TB SKU with every plan but `SHAREPOINTENTERPRISE` + `SHAREPOINTWAC` disabled.
3. Mint a **Temporary Access Pass** (Graph, a Global Admin's DELEGATED token — the one step needing the admin's browser, once per tenant per session; the TAP policy must be enabled for all users).
4. Sign in once as the user with the pass (headless browser) and open `https://<initial>-my.sharepoint.com/`: the site is created; a password sign-in instead hits the Authenticator-registration wall and never gets there.
5. Confirm with SharePoint, not Graph: `PeopleManager/GetPropertiesFor` → `PersonalUrl` is `/personal/<alias>/`, and `/personal/<alias>/_api/site/usage` answers 200 (`StoragePercentageUsed` is a ratio: quota = `Storage` ÷ ratio). Graph `/users/{id}/drive` from an app without a Files role answers 404 for existing drives.
6. `accountEnabled = false` — the app's access is unaffected; the user has no sign-in surface.
7. Append the slot (`TENANT_N_*`, `TENANT_N_DRIVE_ID`); never reorder slots. Under placement version 2 (WP-FLEET-5) a drive appended to an existing tenant moves about 1/d of that tenant's files and nothing elsewhere; `PERMAFROST_FLEET_ACCEPT` blesses the new order and `permafrost-rebalance` moves what changed home.

Before any of it, per tenant: `OneDriveStorageQuota` = 5242880 (MB) via the tenant admin REST (`PATCH /_api/SPO.Tenant`, Int64 as a string), sharing internal-only, TAP policy on.

## Terms and consent

The OneDrive service description excludes "system back-ups and departmental and organizational level data" and "the assignment of a per user license to a bot, department, or other non-human entity"; the Product Terms exclude "commercial hosting services to third parties". **Microsoft's consent for Stored customers' data on the fleet is in place (owner, 2026-11-07).** Sealed parity shards and cold copies may be held on fleet users provisioned for that purpose. No terms question remains; what remains is the posture below and the sizing rule.

## The limits, as published and re-read 2026-11-07

Microsoft Learn, *Avoid getting throttled or blocked in SharePoint Online* (page dated 2025-10-02, updated 2026-08-10):

| Scope | Limit | Scales with licences? |
|---|---|---|
| Per app per tenant, ingress | **400 GB/h** | **no** — "no license bound" |
| Per app per tenant, egress | **400 GB/h** | **no** |
| Per app per tenant, requests | 1,250 RU/min, 1.2 M RU/day at 0–1,000 licences (2,500/min at 1,001–5,000 …) | yes; 300 users stays in the bottom band |
| Tenant, all apps | 18,750 RU / 5 min (bottom band) | yes |
| Per user | 50 GB/h in, 100 GB/h out, 3,000 requests / 5 min | — |
| RU cost | download-URL fetch 1; create / update / delete / upload 2; `$expand=permissions` 5 | — |

Three sentences from that page decide the design: **"SharePoint Online does not return or support IETF RateLimit headers … honor the Retry-After header"** (the headers the 11-07 soak saw are not a contract; fixed buckets at the published rates are the design, not a conservative choice); **"Don't create separate AppIDs where the applications essentially perform the same operations … applications running against the same tenant share the same resource bucket"** (never a second data app per tenant); **"Well-decorated traffic will be prioritized"** (`ISV|Stored|permafrost/<sha>`, #1177). Persistent overuse is a block: permanent 503 and a Message Center notice.

The OneDrive service description (2026-07-23) lists Business plans at **1 TB per user** and E3/E5 at 1 TB "can be increased to 5 TB"; the 25 TB-by-support ladder is gone from the page ("eligible enterprise and government cloud customers should work with their Microsoft representatives"). Our 5 TiB drives are SharePoint Plan 2 (the Dynamics bundle) plus the tenant default; 5 TB is the ceiling to plan on.

## Sizing a tenant

The pipe is **per tenant and fixed**; the bucket is **per user and additive**. How many 5 TB users sit behind one 400 GB/h pipe is therefore the one sizing decision:

| Users per tenant | Capacity | Re-protect a lost tenant from Geyser | What binds |
|---|---|---|---|
| 1 (2026-11) | 5 TiB | ~4 days (the user's 50 GB/h in) | the user |
| **8** | 40 TiB | ~4 days | the user — 8 × 50 GB/h = the tenant's 400: the tenant cap never trips |
| **16** | 80 TiB | ~8 days | the tenant |
| 300 | 1.5 PB | **~156 days** | the tenant — five months with that tenant's shards single-copy |

**Rules:** keep **8 users per tenant** by default and never more than **16**; add a tenant rather than user 17. Keep **≥ 4 tenants** so RS 4+4's shards sit on four tenants (shard *j* on tenant *j*, WP-FLEET-5): a rebuild reads four pipes, and a lost tenant loses one shard of four, not the copy; a 5th tenant is the spare (or a fifth parity shard, RS 4+5). 5 × 8 = 40 drives ≈ 200 TiB ≈ 100 Vault customers at 2 TB each, repairable in four days, behind 2 TB/h each way — more than the business will fill. Fewer tenants is fine while the data is small (3 tenants: shards 0 and 3 share one), more users per tenant never is. Pipe scales with tenants, capacity with users; tenants are expensive to own (a GA, a licence bundle with its own lifecycle date, two app registrations with expiring secrets, a fingerprint slot, a probe), users are cheap to add (~1 min each, § Provisioning).

**Economics:** the free seats are the whole case. A paid 5 TB seat (SharePoint Plan 2, ~$10/user-month list — verify) is ~$2/TB-month, Vault's list price; Geyser is $1.55. The fleet grows only on promotional seats, and every tenant's bundle has a lifecycle date (today 2028-06-05 and 2028-08-02).

## The step plan (2026-11-07)

| Step | What | What it buys / gate |
|---|---|---|
| 0 | Close the chain: #1185; slot-1 `-retire` (dry-run first, ≥ 2026-11-08 01:25 UTC); the bins' one-time purge. **F-14 (the master key):** the sealed-only rule means only sealed bytes land on the fleet — without a master key, only SSE-C objects qualify. F-14 is the owner click that unlocks the fleet for every other customer | the fleet correct at 3 × 1; the gate for everything below |
| 1 | The fleet as Vault's leg: `VAULT_PARITY_LEGS=permafrost,…` (or the stacked RS 4+8 set with Lyve while its promo lasts); `vault_parity_serve` ON for the bench tenant → tenant zero → global; restore p50/p99 measured | Vault's second copy at $0 and restores in seconds; 15 TiB covers the first ~7–15 customers |
| 2 | **WP-FLEET-5** placement over (tenant, drive): rendezvous ranking, shard *j* → tenant *j*, drive by rendezvous inside the tenant, `ListDir` over the spread set, the rebalance tool (`docs/reviews/WP-FLEET-5-PROMPT.md`) | growth stops remapping everything; a rebuild spans four tenants; a lost tenant loses one shard. **Precondition for any fourth drive** |
| 3 | WP-FLEET-6 (certificates, licence + lifecycle + secret-expiry gauges) with WP-FLEET-11's fleet file | operable at 40–80 drives; the 2028 dates are a metric |
| 4 | Provision 8 users per tenant on the three tenants (WP-FLEET-7's recipe) | 24 drives ≈ 120 TiB ≈ 60 Vault customers, repair ≤ 4 days |
| 5 | Tenants 4 and 5 — only those carrying the promotional bundle (candidates: stored.ge, teamviera.com, vaultaire.cloud, oligrace.co; SKUs unchecked); else 16 users on three tenants | 200 TiB on a true shard-per-tenant layout, or 240 TiB on three |
| 6 | The cold half of Standard on the fleet (`smart_demotion`, sealed ≥ 64 MiB only) — **not before 2027**: Lyve is free until mid-2028 and the fleet ends the same summer, so the window in which it saves money is about zero; the fleet's capacity is Vault's first | decided with step 7 |
| 7 | The 2028 leg: build an archive leg (AWS Deep Archive via the S3 driver, or Azure Archive; ~$1/TB-month, 180-day minimum) in 2027-H1; price Vault's term against it; **re-protect the fleet's shards onto it before the bundles lapse** (400 TiB at 2 TB/h ≈ 8 days; an unlicensed drive is read-only at day 60, archived at day 93) | no single-copy window in summer 2028 |
| 8 | BYO-M365: a business customer's own tenant holds their Vault copy — the same placement, pacing and provisioning engine, no seats, consent or cliff of ours | the scalable form of the fleet; year 2 |

Dates: Dynamics lifecycle 2028-06-05 (fromhis.com) and 2028-08-02 (viera.fun, viera.pics); data-app secrets 2027-10-02 / 2027-10-08 / 2028-04-18; admin certificates 2028-11-04; Lyve's promo mid-2028.

## Good-tenant posture

The operator's part is to behave like a well-made client: one app registration per tenant and never more for the same job; pace at the published per-user and per-tenant rates even though the soaks took several times more; honour `Retry-After` fully and treat a 429 burst as the budget's signal, not a fault; decorate the `User-Agent`; keep ≥ 5 licensed users per tenant (the 5 TB entitlement) and every drive licensed (an unlicensed drive goes read-only at 60 days and archived at 93); no per-file customer keys (path ≤ 400, ~50k items per folder); shards, packs and generated names only.

## What a Vault write and read look like on the fleet, end to end

1. PUT of a GLACIER object → Geyser (tape) takes the bytes; the row records `geyser`.
2. The `vault_parity` job reads the object back from Geyser and writes four shards to the fleet (`<tenant>__parity/<digest>/<etag>/p0..3`), each on the account its name hashes to; the row is `complete` when all four landed. 1 GiB: ~54 s after the write.
3. A GET while the object is on tape, or while Geyser refuses, reads the four shards (any four of the eight data+parity shards rebuild) and serves the object with `x-vaultaire-served-from: parity`; with `vault_parity_serve` the parity copy is read FIRST. 1 GiB: 10–13 s at ~100 MB/s, first byte ~1–3 s.
4. A DELETE erases the shards (`permanentDelete`) and the row. Nothing waits in a bin.
5. Loss of a drive (unlicensed, suspended, deleted): the object is still on tape and on three other shards' drives; the job re-protects. Loss of every drive: the object is on tape, a recall away.

## Open at the time of writing

- WP-FLEET-1 slot moves: **slot 3 retired** (2026-11-07). **Slot 1 is moved and read back:** prod reads the OneDrive library, and its `-retire` is owed no earlier than 2026-11-08 01:25 UTC. Slot 2 needs no move. **Owner:** the one-time purge of the fleet drives' bins.
- WP-FLEET-3 (`docs/reviews/WP-FLEET-3-PROMPT.md`) is built: PR A #1175, where a refused range fails the body and ranges and fragments get 6 attempts (8 under a job) with Retry-After honoured to 90 s; and PR B, which paces at the published rates per account and per TENANT, holds the whole account on a 429, hedges a slow range, resumes a fragment from its session, sends `ISV|Stored|permafrost/<sha>`, and adds the `PermafrostThrottling` rule (`internal/drivers/onedrive_README.md` § Throttle Handling). PR C, the soak: paced, 13 rounds, 0 throttles; fully unpaced, 93 rounds, 187 429s absorbed, **0 failed objects, 0 clean-empty bodies** (`bench-results/PERMAFROST-FLEET-2026-11-05.md` § What changed). Microsoft's `RateLimit-*` headers were seen for the first time.
- The driver at scale is built (WP-FLEET-11): `PERMAFROST_FLEET_FILE`, one credential and transport set per tenant, home-only reads once the fingerprint is settled, and a sampled probe. List and ListDir stay the fleet's union until WP-FLEET-5's placement over (tenant, drive).
- **Placement over (tenant, drive) is built (WP-FLEET-5 PR A, 2026-11-07; `internal/drivers/onedrive_placement.go`):** version 2 places a shard `p<j>` on a tenant of column *j* mod `PERMAFROST_PLACEMENT_SPREAD` (4) by rendezvous on its folder, and on one drive of that tenant by rendezvous on the folder, and any other file by its full path over every tenant and drive — four parity shards on four tenants, a parity folder's shards on one drive per tenant, a folder of plain files spread (prod's dry run showed the folder rule alone would stack every bench file on one drive), an appended tenant moving an eighth of the shards on a 4 → 5 step and an appended drive 1/d of its tenant's files only; the marker records `placement_version`, the tenant grouping and a rebalance `placement_target`; the second look of an unplaced fleet is the previous placement's home, computed (two requests at any size — the 8-account walk is gone); `ListDir` of a `<digest>/<etag>` folder asks the four drives placement names. Prod's markers are version 1 and stay authoritative untouched. **PR B** = `cmd/tools/permafrost-rebalance` (dry run → run → commit); until it lands an accepted fleet keeps its second look.
- Observability (WP-FLEET-10): the System page's Permafrost Fleet card, and the rules `PermafrostThrottling`, `PermafrostHashMismatch` and `PermafrostFallbackWalking`.
- Unsealed Vault objects' shards: **the sealed-only leg rule is in** (#1176, D-F-SEAL-1). The fleet takes ciphertext shards only unless `PERMAFROST_ALLOW_UNSEALED`; until F-14, that means SSE-C objects only.
- Protects run up to `VAULT_PARITY_PROTECT_CONCURRENCY` (default 4) at once, since D-F-PROT-1. One protect is bound by Geyser's ~20 MB/s read-back, not by the fleet.
- Transfers (WP-FLEET-4, #1179 + #1183): 32 MiB ranges (reads 135 → 460 MB/s under the same request bucket), one simple PUT for a re-readable body ≤ 200 MiB over HTTP/1.1, double-buffered 180 × 320 KiB fragments, every write checked by quickXorHash. One upload stream writes about 20 MB/s; the prompt's 24 MB/s per object was not reached.
