# Lyve Cloud 2 Driver — Ops Manual

Status (2026-07-29): **technically ready; two contract questions outstanding.**
Reseller authorization — the one hard legal blocker — is **resolved** (we are an
authorized reseller). Prefix-based multi-tenancy, previously believed broken, is
**working** (see *Prefix scoping DOES work*), so Lyve bucket counts are not a
tenant ceiling.

Lyve was "dropped" in July 2026 partly on a perf verdict that turned out to be
a benchmarking artifact (see *The bucket-homing trap* below); the driver is
fully functional and benchmarked on both the direct and engine paths. What
remains before it can back a customer-facing tier is **two questions for
Seagate** — account status and post-interim price — plus the absence of any
durability warranty. Read *Contract terms* immediately below before designing
anything customer-facing on Lyve.

## Contract terms (extracted 2026-07-29 — READ THIS FIRST)

Sources: `~/Downloads/LYVE CUSTOMER AGREEMENT.pdf` (58pp) and
`LYVE SERVICES TERMS.pdf` (65pp, version 2024-12-10), both dated 2025-08-16.
They had sat unread since acquisition; these are the operative terms, not the
API guide. **Not legal advice — quotes are verbatim so they can be checked.**

### 1. Reselling requires Seagate's prior written authorization

> "Company's resale of the Services to its customers is conditioned on
> Seagate's **prior written authorization** and Company's compliance with the
> Solution Provider plan terms… **Resale includes use of the Services for
> purposes of providing the Solution Provider's services, offerings, or
> solutions … to its resellers or end-user business customers.**"

That definition covers exactly what stored.ge does: storing paying customers'
data on Lyve and selling it as our product **is resale**, even though we never
resell a Lyve login.

**Status: we are an authorized reseller** (owner, 2026-07-29), so this
requirement is satisfied and is *not* a blocker. Two things still worth doing,
because the API view disagrees with that: `RSGetUserInfo` reports us as
customer **v01** under reseller **global** with *customer-level* RS access,
and reseller-level calls (`RSLiveBilling`, `RSListCustomer`,
`RSCustomerDetails`) return Unauthorized. So (a) keep the written
authorization and the Solution Provider Plan acceptance on file with the
launch records, and (b) ask Seagate whether the account should be re-scoped to
reseller level — today's API access does not reflect reseller status, which
also blocks the billing reconciliation we want.

### 2. There is no durability warranty — at all

> "The warranties in this Agreement do not apply to Company Data or any other
> data, **or any data integrity or loss**, or costs related to retrieving and
> returning any data. **Seagate does not warrant the complete security,
> accessibility, or inalterability of Company Data.**"

No durability figure ("eleven nines" or otherwise) appears anywhere in either
document. The only durability language we have ever had is our own qualitative
note, "Seagate-grade durability" — which is marketing, not a commitment. Any
customer-facing durability claim backed by Lyve would be unsupported.

### 3. The SLA is uptime-only, credit-only — and excludes non-paid accounts

Monthly Uptime = 100% − Error Rate, averaged over 5-minute intervals
(request-based; intervals with no requests count as 0% error):

| Monthly Uptime | Service Credit |
|---|---|
| < 99.5% and ≥ 99.0% | 10% |
| < 99.0% and ≥ 95.0% | 25% |
| < 95.0% | 100% |

Credits are applied against amounts owed, are **not refundable**, are the
**sole remedy**, and must be claimed in writing by the end of the second month
after they accrue, with our own request logs attached. Excluded: force
majeure, our own or third-party causes, planned downtime, and any period of
suspension. Note the implied commitment threshold is only **99.5%**, and that
a credit against a **$0** invoice is worth exactly nothing.

### 4. Our free arrangement is almost certainly a "Non-paid Service Account"

> "…evaluation accounts including, but not limited to, 'Evaluation', 'Proof of
> Concept', 'Trial', 'Try-to-buy', **or similar non-paid offers** (each, a
> 'Non-paid Service Account'). Non-Paid Service Account deployments are
> **time-bound**… Seagate reserves the right to **immediately suspend** the
> account… Seagate shall **delete all the Company Data** from the Non-paid
> Services Account **approximately 30 days** after the earlier of termination
> or expiration. **Non-paid Service Accounts are not covered by the Lyve Cloud
> Service Level Requirements.**"

We pay $0, so "similar non-paid offers" fits us unless Seagate says otherwise.
If so: no SLA, time-bound by definition, suspension possible on expiry, and a
**~30-day data-deletion clock**. This is the contractual form of the warning
already in the strategy docs ("free deals end", "never the sole copy") — and
it is sharper than that phrasing implies, because the deletion is automatic
rather than a negotiation. **Confirm our account's status with Seagate in
writing** — it is the single cheapest de-risking action available.

### 5. Other terms worth knowing

- **Termination for inactivity:** no active subscription for 6 months → Seagate
  may terminate on **10 days' notice**.
- **Termination for cause:** either party, on material breach, **30 days** to
  cure.
- **Warranty claims** must be raised through the Portal support menu within
  **30 days** of discovery; SLA procedures must be exhausted first.
- **No fee-change clause** was found in either document — pricing lives in the
  Order, which we do not have a copy of. So the post-interim rate is
  genuinely unknown; the `$6.37/TB` in `cmd/vaultaire/main.go:144` is our own
  modelling constant, never a quoted or invoiced price.
- **Infrequent Access supplement** (we are not adopting IA — recorded for
  completeness): 180-day minimum retention, **128 KB** minimum object size,
  monthly retrievals capped at average objects stored, US-Central and APAC
  only, no cross-DC replication by default.

### What this means in practice

Lyve is excellent as what the strategy docs already make it: an internal
buffer, restore-staging target, and a never-sole second copy, where the free
arrangement is upside and its collapse is survivable. Turning it into a
customer-facing tier inverts the risk — customer data would sit on a backend
with no durability warranty, quite possibly no SLA, a resale restriction we
may not satisfy, and a deletion clock we do not control. The gating questions
are for Seagate, not for the benchmark suite. Reseller authorization is
**resolved** (see §1); two remain, and both are cheap to ask:

1. Is our account a **Non-paid Service Account**, and when does it expire?
   This is the sharpest one — it decides whether an SLA exists at all and
   whether a 30-day deletion clock applies.
2. What is the **post-interim rate**, and is there a minimum term or commit?
   The `$6.37/TB` we model is above every tier's selling price, so the answer
   determines whether Lyve can back a tier at all or stays an internal buffer.

Worth pairing with the account-scope question in §1 — reseller-level API
access would also unlock the billing reconciliation (`RSLiveBilling`) we
currently can't run.

## Launch-tier readiness — technical side (assessed 2026-07-29)

Separate from the contract question above: *if* the contract clears, here is
what the code and the platform actually support.

### Capacity limits, measured

| Limit | Measured | Notes |
|---|---|---|
| Bucket count | **no cap found at 211** (402 account-wide via `rs-bucket-stats`) | created 99/100 in one run; the single failure was transient, not a cap |
| Bucket creation latency | **p50 3.4 s, p95 5.2 s, max 8.0 s** | slow. Fine for admin provisioning, far too slow to sit in a signup request |
| `ListBuckets` latency | **~540 ms, flat** at 111 and 211 buckets | does not degrade with bucket count, but is slow for a control-plane call — don't put it on a request path |
| IAM sub-users | 25 bulk-created, no cap hit | never pushed higher |
| Concurrency | 128 workers, **0 errors**, 602 ops/s | see benchmark table |

**Bucket-count limits never bind us.** On the proxied path the driver keys
objects as `t-{tenant}/{container}/{artifact}` inside **one** bucket per region
(`lyve.go:78`, `getBucket()` → `stored-{region}`), so tenant separation is
Vaultaire's, in Postgres, and Lyve sees a single credential. And on the
*direct*-credential path, **prefix scoping works** via resource ARNs (see
*Prefix scoping DOES work* below) — so per-tenant buckets are not needed there
either. The 3.4 s bucket-creation cost is therefore an admin-provisioning
detail, not a per-tenant signup cost.

### Quirks that are harmless to us, and why

- **`If-None-Match: *` overwrites instead of 412.** Harmless: conditional
  requests are implemented at our API layer (`internal/api/conditional.go`)
  against our own metadata, and `engine.Driver` has no conditional-write path
  at all, so backend conditional semantics are never relied on.
- **No S3 event notifications.** Drain/replication scheduling must poll or
  track writes — already the design.
- **Bucket tagging silently dropped; `PutBucketLogging` unsupported.** We use
  neither.

### Real technical constraints

- **No multi-region replication on our account** — `replication-policy` is
  silently ignored. Data lives in exactly one region. Geo-redundancy must be
  client-side (server-side cross-region `CopyObject` works, no client egress)
  and **doubles stored bytes**. Any customer-facing durability or
  regional-failover story has to account for this.
- **Cross-region access costs ~28 ms/op warm** (39 ms home vs 67 ms wrong
  region) — the homing trap, quantified.
- **Lifecycle expiration is unverified** — canary due Jul 31; use our own
  reaper until it's confirmed.

### Code gaps if it becomes a selectable tier

The driver itself needs nothing — it already implements the full `Driver`
contract plus `RangeGetter` and parallel multipart, is registered in
`cmd/vaultaire/main.go` on `LYVE_ACCESS_KEY`, and is already treated as
durable/failover-eligible. The gaps are in the surfaces around it:

- **`STANDARD_IA → lyve` routing is already live** (`internal/engine/storage_class.go`),
  but **unreachable** — no `tier_preference` value maps to `STANDARD_IA`, so no
  customer can select it. Note this directly contradicts
  `docs/IMPLEMENTATION_PLAN.md:871` ("Don't route customer `STANDARD_IA` to
  Lyve"); **the code and the plan disagree today** and one of them must change.
- **Cost tracking would bill Lyve at $0** — `internal/usage/cost_tracker.go`
  has no `lyve` key, and `admin_costs.go` omits it from the cost/egress/order
  maps.
- **Health check is commented out** (`internal/api/server.go`) and points at a
  stale `lyvecloud.seagate.com` host.
- **`bucketRegionDriver` hardcodes `"idrive-" + region`**, and `IsValidRegion`
  is an iDrive-only registry — Lyve's region names differ, so bucket-region
  routing cannot currently express a Lyve tier.
- **`TestLanding_NoDeadProduct` asserts the landing page never contains
  "Lyve"** — putting Lyve in customer-facing copy **fails CI** until that test
  is revised.
- Auto-detect omits Lyve (explicit `STORAGE_MODE=lyve` works, which is what
  the E2E bench harness uses).

## Platform architecture (Lyve Cloud 2 / RSTOR)

Seagate's current platform (endpoints `s3.<region>.global.lyve.seagate.com`)
is the RSTOR-derived "Lyve Cloud 2". API reference PDF: *Lyve Cloud Object
Storage API User Guide* (extracted highlights below). Key facts:

- **Path-style requests only**, SigV4 (or V2) signing.
- **Every bucket is homed in the region(s) of its replication policy.** The
  policy is set at bucket creation (`PUT /bucket?replication-policy=US-WEST-1`,
  comma-separated list); with no policy the bucket is homed in the region
  whose endpoint created it (the PDF claims "replicated to every region" by
  default — account behavior says otherwise; trust the probe, not the PDF).
- **Any regional endpoint accepts requests for any bucket and transparently
  proxies to the bucket's home region.** No error, no redirect — just
  ~500 ms/op added latency for cross-US proxying.
- Check an object's home: `GET` it with header `x-rstor-replication-status:
  true`; the response header lists the region(s) holding it.
- Our account: all 7 regions available, `ReplicationEnforcement: false`
  (query `POST /?Action=RSAvailableRegions&Version=2010-05-08` on
  `iam.global.lyve.seagate.com`, service `iam`, root S3 creds).
- Multipart uploads must be **completed in the DC where they were initiated**
  (fine for this driver — it pins one regional endpoint).
- SSE is on at rest (`X-Rstor-Sse-Encryption-Status: true` on responses).
- Known wart: malformed-auth PUTs hang ~9 s then return 502 (healthy paths
  return fast 403s). Reads/writes with valid auth are unaffected.

### Quick probes (curl ≥7.75)

```bash
# Where does this object live?
curl -sS -o /dev/null --aws-sigv4 "aws:amz:us-west-1:s3" --user "$AK:$SK" \
  "https://s3.us-west-1.global.lyve.seagate.com/BUCKET/KEY" \
  -H "x-rstor-replication-status: true" -D - | grep -i rstor

# Create a bucket homed in a specific region (do this from that region's endpoint)
curl -sS --aws-sigv4 "aws:amz:us-west-1:s3" --user "$AK:$SK" -X PUT \
  -H "Content-Length: 0" "https://s3.us-west-1.global.lyve.seagate.com/BUCKET"
```

## Admin (RS) API — tested 2025-11-28, still valid

Account identity (`RSGetUserInfo`): we are customer **v01** under reseller
**global** (root user isaacv17@gmail.com) — customer-level RS access, not
reseller-level. Three service hosts, each with its own signing service:
`iam.global.lyve.seagate.com` (service `iam`),
**`sts.global.lyve.seagate.com` (service `sts`)** — this host exists and
works (geo-DNS to the nearest DC; the 2025 note claiming "STS = IAM host"
was wrong — STS actions 400 on the IAM host), and
`s3.<region>.global.lyve.seagate.com` (service `s3`).

### Endpoint list (Seagate support, 2026-07-29) — one correction

Seagate support supplied the official endpoints. All check out except STS:

| Service | Seagate's answer | Verdict |
|---|---|---|
| S3 | `https://s3.<region>.global.lyve.seagate.com` | correct |
| Console | `https://console.global.lyve.seagate.com` | correct — **live, and new to us** (see below) |
| IAM | `https://iam.global.lyve.seagate.com` | correct |
| STS | `https://iam.global.lyve.seagate.com` | **wrong — STS is not on the IAM host** |

Re-verified 2026-07-29: `AssumeRole` against `iam.global…` returns
`400 IAM: Missing Action or Version param` under **either** signing service
(`iam` or `sts`) — the IAM vhost does not route STS actions. It succeeds only
on `sts.global.lyve.seagate.com` signed with service **`sts`** (200,
`AssumeRoleResult`); that same host signed as `iam` returns 401. Seagate's own
console config agrees with us (`userEndpoint: 'https://sts.global…'`), so
treat the support answer as a slip and keep using the `sts.` host.

Note `console`, `iam`, and `sts` are three vhosts on **one IP**
(134.204.253.1, `dfw01.geo.lyve.seagate.com`), which is likely why they get
conflated. Also live: **`s3.global.lyve.seagate.com`** — a *non-regional* S3
endpoint (not in the support list, but what the console itself uses);
`list-buckets` there returns the full account.

Measured from SLC against a **west-homed** bucket (HEAD, one pooled
connection, 6 requests):

| Endpoint | cold (incl. DNS+TCP+TLS) | warm (reused conn) |
|---|---|---|
| `s3.us-west-1.global…` | 128 ms | **39 ms** |
| `s3.global…` | 128 ms | **39 ms** |
| `s3.us-east-1.global…` | 219 ms | **67 ms** |

So the global alias is exactly equivalent to naming the home region — no
penalty, no benefit — while a *wrong*-region endpoint costs ~28 ms/op warm on
the same object. That is the homing trap quantified, and it confirms the
alias does not escape it. The warm 39 ms also independently reproduces the
38 ms warm HEAD in the benchmark table below.

Measure this way or not at all: naive one-connection-per-request loops report
250–770 ms and swing wildly by DC, because they measure TLS setup, not the
service. The driver pools connections (`TunedHTTPClient`), so warm is the
number that describes production.

Their advice to drive IAM with a **dedicated admin user's** access key rather
than the root key is worth taking — it matches the lockout risk already noted
below under the RS actions we don't touch.

## LC2 console — reachable and enumerable (2026-07-29)

`https://console.global.lyve.seagate.com` returns 200: an Angular SPA
(`gui_version v0.19.92`) whose runtime config is world-readable at
**`/assets/config.js`** — no auth needed. That file is the fastest way to see
what our account actually has switched on, and it is how the two findings
below were discovered. Auth is Auth0-backed (`externalAuth0: true`,
`auth.lyve.seagate.com`), so a Geyser-style programmatic login is a bigger
lift than Geyser's was — not attempted yet.

Flags worth knowing (fetch the file again rather than trusting this list to
stay current):

| Flag | Value | Why it matters |
|---|---|---|
| `storageClassEnabled` / `storageConfiguration` | `true`, STANDARD → STANDARD_IA | a colder class exists — see below |
| `disableMultitenancy` | `false` | multitenancy is on for our account |
| `rprotectEnabled` | `true` | ransomware-protection feature present |
| `sessionDurationSeconds` | `900` | matches the 15 min `AssumeRole` default |
| `bucketCorsEnabled` | `false` | console won't manage CORS for us |
| `hasTransporter` | `false` | the `rramp.global.lyve.seagate.com` migration service is off for our account |

## Object Lock / WORM — fully enforced (probed 2026-07-29)

Previously skipped on the theory that a lock makes the test bucket
undeletable. It doesn't have to: **set a ~3-minute `retain-until` and clean up
after it lapses.** That makes WORM safe to probe, and it now is. All of the
following was verified live and the bucket fully removed afterward.

| Probe | Result |
|---|---|
| `create-bucket --object-lock-enabled-for-bucket` | works; `get-object-lock-configuration` → `Enabled` |
| PUT with `COMPLIANCE` + retain-until | accepted; retention reads back exactly |
| DELETE a COMPLIANCE version in-window | **AccessDenied — even with root credentials** |
| DELETE a GOVERNANCE version, no bypass | **AccessDenied** |
| DELETE a GOVERNANCE version, `--bypass-governance-retention` | succeeds |
| PUT with `--object-lock-legal-hold-status ON` | accepted; reads back `ON` |
| DELETE under legal hold | **AccessDenied** |
| legal hold → `OFF`, then DELETE | succeeds |
| DELETE a COMPLIANCE version *after* retain-until lapses | succeeds |

So Lyve implements real S3 WORM: COMPLIANCE is genuinely un-overridable while
the window is open (root can't escape it), GOVERNANCE is overridable only with
the explicit bypass flag, and legal hold is an independent, releasable lock.
COMPLIANCE is time-bound rather than permanent — the object deleted normally
once the window closed, which is what makes the short-window technique safe.

This is the concrete backing for the Vault immutability pitch (3-2-1-0, the
"0" being zero-errors/immutable copies) and it composes with the write-only
credentials proved above: ingest creds that can PUT but not GET/DELETE/LIST,
writing into a COMPLIANCE-locked bucket, is a ransomware-resistant path with
no override short of waiting out the retention.

Caveat: `rprotectEnabled: true` in the console config suggests Seagate also
ships a *proprietary* ransomware-protection feature distinct from S3 Object
Lock. Not probed — the S3-standard path above is what the driver would use,
and it works, so the proprietary one is only worth chasing if it offers
something the standard API can't express.

## Storage classes — STANDARD_IA works (probed 2026-07-29)

**Not a tier we intend to use** (decision 2026-07-29 — no IA in the lineup);
recorded only so the capability isn't re-discovered later.

Undocumented in the API guide we extracted, but **live on our account**:

- `put-object --storage-class STANDARD_IA` is accepted, and `list-objects-v2`
  reports the class back as `STANDARD_IA` (STANDARD objects stay `STANDARD`).
- A lifecycle rule with `Transitions: [{Days: 30, StorageClass: STANDARD_IA}]`
  is accepted by `put-bucket-lifecycle-configuration` and reads back intact.
- Console config declares the only legal move is **STANDARD → STANDARD_IA**;
  `STANDARD_IA` has an empty transition list, so there is no colder class and
  no documented path back up.

Verified with a throwaway bucket (created, both classes PUT, lifecycle set and
read back, then fully deleted incl. versions). Both objects also came back
`ServerSideEncryption: AES256` with a `VersionId`, consistent with the
account-wide SSE and versioning notes above.

We never priced it, and won't: IA's usual penalties (minimum object size,
minimum storage duration, per-GB retrieval fee) land badly on the Vault path,
which is read rarely but read *in full*. If that calculus ever changes, get
those three terms from Seagate before designing around a transition.

**Working with our root S3 creds:**

```bash
# Per-bucket size/objects/REPLICATION POLICY for the whole account — THE
# homing audit tool (S3 endpoint, service s3):
curl -s --aws-sigv4 "aws:amz:us-west-1:s3" --user "$AK:$SK" \
  "https://s3.us-west-1.global.lyve.seagate.com/?rs-bucket-stats"

# Account info / regions / billing (IAM endpoint, service iam, POST form):
curl -s -X POST --aws-sigv4 "aws:amz:us-east-1:iam" --user "$AK:$SK" \
  -d "Action=RSGetUserInfo&Version=2010-05-08" https://iam.global.lyve.seagate.com/
# Also working: Action=RSAvailableRegions (7 regions, ReplicationEnforcement:false),
# Action=RSListBillingData&From=<ISO>&Till=<ISO> (per-region daily
# upload/download/delete bytes + UsedSpace + ObjectCount — usable for COGS
# reconciliation), and standard IAM CreateUser/ListPolicies (sub-user mgmt).
```

**STS works** (2026-07-29): `AssumeRole` on `sts.global.lyve.seagate.com`
(sign service `sts`, `Version=2011-06-15`, RoleArn/RoleSessionName required
but ignored) returns temp credentials that work against S3 with the session
token. The 2025-11 failure was just the wrong host.

**Not available to us** (reseller-only or unsupported): `RSLiveBilling`,
`RSListCustomer`, `RSCustomerDetails`, `RSWhitelist*`, `RSSAML*`
(Unauthorized / BadRequest / not supported).

## API coverage sweep (2026-07-29) — everything in the LC2 guide checked

Every customer-scoped action in `docs/references/lyve-cloud-2-api-en_US.pdf`
has now been live-verified except the deliberate skips below.

**Verified working** (beyond everything already listed above): full IAM
policy versioning (CreatePolicyVersion/List/Get/SetDefault/Delete — version
ids are non-standard strings like `vH92BC9A0V5YE`, not `v2`),
ListAttachedUserPolicies, ListEntitiesForPolicy; STS AssumeRole end-to-end;
bucket encryption retrieve (AES256 default confirmed at bucket level);
**bucket policy set/delete with real anonymous access** (public buckets
work; `rs-info` reflects `isPublic`); object-tag delete;
`response-content-type`/`-disposition` GET overrides; ListMultipartUploads
+ ListParts; **browser-style POST form uploads** (SigV4 policy via
`generate_presigned_post` → 204); Content-MD5 enforcement (BadDigest on
mismatch); trailing-slash empty-directory keys; delimiter `/` listing.

**Broken / quirks found in the sweep:** bucket *tagging* is accepted but
silently dropped (PutBucketTagging 200 → GetBucketTagging NoSuchTagSet —
object tagging works fine); PutBucketLogging → InvalidRequest (logging
likely unsupported); GetBucketPolicyStatus → NotImplemented;
`GET /speedtest/*` → 404 NoSuchBucket on LC2 (v1 relic in the doc);
`?metadata` on a bucket just returns a listing. Plus the earlier finds:
replication-policy param ignored, `If-None-Match: *` overwrites, SigV2
presigned PUT 403s.

**Deliberately untested:** root-auth mutators (ChangePassword, UpdateLoginProfile, RSSetUserInfo,
RSResetPassword/GetPasswordResetToken, RS*TFA, RSSetUserAuthMethod —
lockout risk on the root account); RSLogin (needs console password);
reseller-only management (RSCreate/ModifyCustomer,
RSSetCustomerServiceAccessLevel).

## Feature matrix (probed live from SLC, 2026-07-28)

Everything below was verified against real us-west-1/us-east-1 buckets, all
probe buckets deleted afterward. Object Lock / WORM is covered separately
below (2026-07-29) — it *is* now tested, safely.

**Works, full S3 semantics:**

| Feature | Notes |
|---|---|
| Versioning | Enable/suspend, old-version GET, delete markers, marker removal restores. Purge all versions before bucket delete. |
| Server-side copy | 16 MB: same-bucket 0.6 s, cross-bucket 0.7 s, **cross-region 1.9 s** (west→east, zero client bandwidth) |
| Presigned URLs | Standard SigV4 (`aws s3 presign`) AND native `POST /b/k?rs-presign=` + `ExpirationDate` (≤720 h) → JSON `{"link": ...}` |
| Lifecycle | Expiration rules set/get/delete (don't add lock-adjacent rules) |
| Object tagging | `x-amz-tagging` on PUT + Put/GetObjectTagging |
| Bucket CORS | Set/get/delete |
| SSE-S3 | On by default account-wide (AES256 on every response) |
| SSE-C | Correct enforcement — GET without the key is rejected |
| Checksums | `--checksum-algorithm SHA256` honored, ChecksumSHA256 returned |
| Conditional GET | `If-None-Match` → 304 |
| IAM sub-users | Default-deny until policy attached; CreateUser/CreateAccessKey/Delete* all work (delete keys before user or DeleteConflict) |
| `GET /<bucket>?rs-info` | `{bucketSize, replicationPolicy, versionStatus, isPublic}` |

**Broken / unavailable — the gotchas that matter:**

- **Multi-region replication is NOT available to our account.** The
  `replication-policy` bucket-create parameter is silently ignored as a
  query param and as a header (bucket homes in the creation region no
  matter what you pass), and 400s as a form body. `ReplicationEnforcement:
  false` from RSAvailableRegions suggests we may choose — reality says no.
  Geo-redundancy must be client-side: write the home region, then async
  **server-side cross-region CopyObject** to a second-region bucket (works,
  no client egress).
- **No atomic create:** `PUT` with `If-None-Match: *` on an existing key
  returns 200 and overwrites (real S3 would 412). Do not build
  concurrency control on Lyve conditional writes.
- `GET /speedtest/download` returns ~200 bytes, not a stream — dud.
- Stale IAM sub-users exist from earlier experiments (`stored-*@stored.ge`
  ×6, `servertest`) — left in place, audit before deleting. The 2025 test
  users were cleaned up 2026-07-28.

## Extended probes (2026-07-29)

**APAC regions work** (smoke, from SLC, correctly-homed buckets — latency is
RTT-bound, so APAC-local clients would see local latencies):

| Region | warm PUT/GET/HEAD p50 | cold dial p50 (p95) |
|---|---|---|
| ap-southeast-1 (Singapore) | 337 / 216 / 215 ms | 2.1 s (9.0 s) |
| ap-northeast-1 (Tokyo) | 332 / 216 / 214 ms | 1.2 s (4.1 s) |

Lyve is the only backend in our lineup with an APAC footprint.

**IAM capacity / per-tenant credentials:** bulk-created 25 sub-users with no
cap hit (~1.2 s per create — provision async), bucket-scoped managed policies
(CreatePolicy → AttachUserPolicy) enforce correctly, and the full
create→scope→key→delete lifecycle is clean. No documented user/bucket caps
in the API guide. This makes **per-tenant direct S3 credentials** viable:
Vaultaire as control plane, tenants reading/writing Lyve directly.

**Presigned offload:** 256 MB via presigned GET = **130 MB/s** single-stream,
unauthenticated — full egress offload with zero Vaultaire bandwidth.
Presigned PUT works **only with SigV4** URLs (SigV2 presigned PUTs 403;
SigV2 presigned GETs work). Direct client upload is therefore possible.

**Cross-region replication at scale:** 256 MB server-side copy west→east in
5.5 s (**~47 MB/s** — the 16 MB number was overhead-dominated), and
**UploadPartCopy works cross-region** (32 MB parts, completed MPU verified),
so objects of any size can replicate server-side in parallel parts.

**Lifecycle canary:** bucket `lyve-lifecycle-canary` (us-west-1) has a 1-day
expire-all rule and one object planted 2026-07-29 — check whether it
actually expired before relying on lifecycle for retention. Delete the
bucket after the check.

## Account API v2 — unreachable, but NOT a dead platform (re-probed 2026-07-29)

Spec: `docs/references/lyve-account-api-v2-en_US.pdf` (dated 3/1/24, original
Lyve Cloud console era). The API is unreachable for us, but the earlier
"the original platform is wound down" reading was **wrong** — corrected here.

**It is specific hosts that don't answer, not a dead platform.** Observed
behaviour, from two vantage points (a residential Mac and SLC):

| Hostname | Reachable |
|---|---|
| `api.lyvecloud.seagate.com` | TCP 443 blackholed (no SYN-ACK) |
| `console.lyvecloud.seagate.com` | TCP 443 blackholed |
| `s3.<region>.lyvecloud.seagate.com` | **live, serves our data** (legacy alias — see below) |
| `s3.<region>.global.lyve.seagate.com` | live (what we use) |
| `console`/`iam`/`sts.global.lyve.seagate.com` | live |

**Do not read the IP range as the cause** (an earlier revision of this file
did, and was wrong). It is true that `api.`/`console.lyvecloud` land in
`192.55.0.0/16` (ARIN `NetName: SEAGATE-1`), but so do serving hosts: from
SLC, `s3.global.lyve.seagate.com` resolves via `sjc01.geo.lyve.seagate.com`
to **192.55.8.1** and answers normally. Seagate runs production storage DCs
and unreachable control-plane hosts inside the same registration, so the
netblock predicts nothing.

Two related traps when probing this: the names are **geo-DNS**, so the same
hostname resolves to different DCs from different places (`s3.global` →
134.204.253.1 `dfw01` from a Mac, 192.55.8.1 `sjc01` from SLC) — never treat
one resolution as canonical. And the `.lyvecloud` control-plane names are
**wildcard DNS**: `v01.console.lyvecloud.seagate.com` and a nonsense label
both resolve and both blackhole, so a "resolves!" result proves nothing and
there is no per-account API host to find.

All we can honestly say is that those two hosts refuse connections everywhere
we've looked. Firewalled per-host, retired listener, or IP-allowlisted are all
consistent with the evidence; we can't distinguish them from outside.

**The `lyvecloud.seagate.com` S3 names are legacy aliases of the platform we
already use — one account, one namespace.** Verified 2026-07-29: our current
key authenticates against `s3.us-east-1.lyvecloud.seagate.com` and
`list-buckets` there returns the **identical** bucket set as
`s3.us-west-1.global.lyve.seagate.com`. There is no separate "LC1 account"
holding stranded data. Their DigiCert cert (CN matches, genuine Seagate)
**expired 2026-07-19**, so the legacy names now need `--no-verify-ssl` — an
unmaintained alias on a live fabric. Don't build on them; use `global.`.

Practical consequence is unchanged: the Account API's conveniences —
**expiring service accounts** above all — are not available to us, so
per-tenant credentials are LC2 IAM users plus our own rotation, or STS
AssumeRole temp creds. The doc stays as the reference for what to rebuild on
the IAM path. What changed is the prognosis: the platform is plainly alive, so
if Seagate ever opens those hosts to us (or we get reseller-level access) it
is a config change on their side, not a resurrection.

### Account inventory (`?rs-bucket-stats`, 2026-07-29)

**402 buckets account-wide, 299 of them completely empty**, 103 holding
33.0 GB / 20,676 objects — accumulated benchmark and probe litter from
Aug 2025 onward. Largest: `stored-us-east-1` (8.9 GB), five
`lyve-test-*` buckets at 2.24 GB each, `vaultaire-test-1757847421` (3.3 GB).
Note `list-buckets` on a regional endpoint returned only 111 — it is
region-scoped, while `rs-bucket-stats` covers the whole account, so **audit
with `rs-bucket-stats`, not `list-buckets`.** Harmless while Lyve is on the
free interim, but this is the cleanup list if it ever goes metered (and the
299 empties should go regardless — they are pure namespace noise).

## IAM credential-scoping probes (2026-07-29, live from SLC)

Script pattern: create user → create policy → attach → create key → wait
~8-10 s for async provisioning → test → full cleanup. Results:

| Probe | Result |
|---|---|
| **Write-only creds** (`s3:PutObject` only) | **ENFORCED** — PUT ok; GET, DELETE, LIST all denied. Ransomware-resistant backup ingest credentials work today. |
| Object-level prefix scoping (`Resource: bucket/tenant-a/*`) | **ENFORCED** — PUT/GET own prefix ok, PUT other prefix denied |
| **`s3:prefix` Condition on ListBucket** | **Not implemented** — allow-with-condition denies every variant. But conditions are the wrong tool; see below. |

### Prefix scoping DOES work — via Resource ARNs, not Conditions (2026-07-29)

**This supersedes the earlier "per-tenant staging buckets are the
multi-tenancy unit" conclusion.** That conclusion was drawn from testing only
`s3:prefix` *Conditions*, which LC2 does not implement at all — the LC2 API
guide documents **zero** IAM condition keys, and its own policy examples scope
with wildcard resource ARNs (`"Resource":"arn:aws:s3:::abc-bucket*"`). Scope by
**resource ARN** and prefix isolation works today:

```json
{"Version":"2012-10-17","Statement":[{
  "Effect":"Allow",
  "Action":["s3:ListBucket","s3:GetObject","s3:PutObject","s3:DeleteObject"],
  "Resource":["arn:aws:s3:::BUCKET/tenant-a/*"]}]}
```

Measured with that policy on a sub-user (reproduced 4×, 15 s propagation):

| Operation | Result |
|---|---|
| `list-objects-v2 --prefix tenant-a/` | **OK** |
| `list-objects-v2 --prefix tenant-b/` | **DENIED** |
| GET / PUT under `tenant-a/` | **OK** |
| GET / PUT under `tenant-b/` | **DENIED** |
| `list-objects-v2` with **no prefix** | **OK — returns every key in the bucket** ⚠ |

So **per-tenant buckets are not required**, and Lyve bucket counts never
become a tenant ceiling. Note `ListBucket` must be granted on the *object*
ARN (`bucket/tenant-a/*`), which is not how AWS models it — on AWS, ListBucket
takes the bucket ARN. Granting only the bare bucket ARN gives the inverse:
root listing works, prefixed listing is denied. The wildcard form
`bucket/tenant-a*` (no slash) is **rejected** by `CreatePolicy`; use
`bucket/tenant-a/*`.

**The one hole: an unprefixed LIST returns every key in the bucket.** An
explicit `Deny` on `s3:ListBucket` for the bare bucket ARN does *not* close it
(verified twice). The tenant still cannot read or write anything outside its
prefix — this is a **key-name disclosure, not a data leak** — but it does mean
a tenant holding raw Lyve credentials can enumerate other tenants' object
names. Three ways to live with that, in order of preference:

1. **Don't hand out Lyve credentials at all.** Presigned URLs work for both
   GET and PUT (SigV4, `addressing_style: path` — verified 200; the earlier
   "presigned PUT 403s" note was an artifact of `aws s3 presign`, which only
   ever emits GET URLs). Vaultaire already holds the listing in Postgres, so
   it can serve LIST itself and hand out presigned URLs for data — zero
   disclosure, zero per-tenant IAM to manage, and still zero SLC bandwidth on
   the data path.
2. **Make key names non-revealing** (opaque/hashed keys), which renders the
   enumeration useless.
3. **Accept it** where tenants are not mutually untrusted.

None of this affects the **proxied** path we run today, where Vaultaire holds
one credential and keys objects `t-{tenant}/…` inside one bucket — tenant
isolation there is ours, in Postgres, and was never dependent on Lyve IAM.

No S3 event notifications exist on LC2 (word absent from the guide) —
drain/replication scheduling must poll or track writes. ALPN: the S3
endpoint negotiates **http/1.1** (h2 refused) — the Geyser-style h2
single-connection trap cannot occur on Lyve.

## Vault-tier pairing (Lyve staging ↔ Geyser tape), 2026-07-29

Target data path — SLC carries control plane only:
customer → Lyve (presigned PUT / scoped short-lived creds, 0× SLC budget) →
Geyser pulls via console `cloudSync` (0× SLC) → tape; restore =
`RestoreToCloud` into a Lyve-targeted integration → customer presigned GET
(130 MB/s measured, 0× SLC). Both server-side legs gate on ONE unknown:
whether Geyser's `AWS` integration type takes a custom endpoint (or the
`WASABI` type accepts Lyve post-acquisition). Next console session: `GET
/api/supportedregions/wasabi|oracle` + probe `CreateCloudIntegration`
validation. Hand Geyser only a read-only, expiring, staging-scoped service
account — never root creds. Fallback if refused: SLC-mediated drain (2×
budget; customer→Lyve stays 0×, so worst case still halves the 4× buffered
path). Staging cleanup via lifecycle expiration once the canary verdict is
in (due Jul 31); until then use our own reaper.

## Product fit

- **Best fit: second-vendor DR/replication target for the Standard tier** —
  real S3 semantics (versioning, copy, presign), strong concurrency
  (778 MB/s ingest), Seagate-grade durability, and server-side cross-region
  copy for a cheap geo story. Wire as a `ReplicationDriver` target.
- **EU data residency option** (eu-west-1, eu-central-1 already in our
  account) without onboarding a new vendor — and the only APAC footprint
  (Singapore, Tokyo) in the stack.
- **Direct-access data plane** (candidate, needs pricing confirmation):
  per-tenant sub-users + scoped policies + SigV4 presigned URLs would let
  bandwidth-heavy tenants hit Lyve directly with Vaultaire as control plane
  only. Verify our contract's egress/API pricing first — Lyve markets no
  egress fees, which would make presigned offload free bandwidth.
- **Not** a primary hot tier (iDrive is cheaper with faster single-stream
  GET) and **not** archive (Geyser is ~4× cheaper per TB). Lyve ≈
  $6.37/TB (cost map in `cmd/vaultaire/main.go`).

## Prior art & where the old tooling lives

- `~/fairforge/vaultaire-benchmark/lyve-*.sh` — Nov 2025 API sweeps:
  `lyve-correct-endpoints.sh` (the canonical RS/IAM test, correct global
  endpoints — results in `lyve-correct-test-20251128-123805.log`),
  `lyve-full-api-test.sh` + `lyve-rs-commands-test.sh` (older, use the
  RETIRED `s3.<region>.lyvecloud.seagate.com` v1 endpoints; SSE-S3/SSE-C
  test sections still useful as recipes). Scripts contain hardcoded root
  creds — do not commit them.
- `.private/lyve-advanced-test.js` (+ report JSON) — Node SDK perf suite
  (versioning, tagging, lifecycle, CORS, presign, multipart tuning
  configs), Oct 2025, v1 endpoints.
- `docs/references/lyve-cloud-2-api-en_US.pdf` — the LC2 API guide,
  committed in-repo (same doc as the Downloads copies).
- Seagate legal/portal docs in `~/Downloads`: LYVE CUSTOMER AGREEMENT,
  LYVE SERVICES TERMS, `lyve-management-portal-en_US.pdf` (console admin).
- `test-lyve-live.sh` (deleted from repo Sep 2025) — trivial pre-auth
  smoke, superseded; recover with `git show d997410:test-lyve-live.sh`.
- SLC: `~/vaultaire-bench/bench-lyve-west.sh` (E2E smoke wrapper),
  `drivers-test-linux` (integration-test binary), full/smoke JSONs in
  `~/vaultaire-bench/bench-results/`.

## The bucket-homing trap (root-caused 2026-07-28)

The July "us-west-1 write degradation" (warm 4 KB PUT p50 ~580 ms, 2 ops/s,
cold dial ~740 ms) was **entirely** the proxy path: the shared bench bucket
was homed in US-EAST-1, so every us-west op crossed the country. Controlled
A/B from SLC (warm connection): west-homed bucket via west endpoint
**50–80 ms**; east-homed via east 60–90 ms; east-homed via west 500–700 ms.
Even the cold-dial asymmetry was this artifact (that workload includes a PUT).

Rules that follow:

1. **Create every bucket through the endpoint of the region it should live
   in** (or pass `replication-policy` explicitly).
2. **Never benchmark or serve a bucket through a non-home endpoint** unless
   you intend to measure the proxy path.
3. E2E-through-Vaultaire numbers cannot assess backend health while
   `engine/failover.go` can land PUTs locally — verify objects actually
   reached Lyve with a direct probe.

## Driver specifics (`lyve.go`)

- Region default **us-west-1** (closest to SLC; `LYVE_REGION` overrides —
  also defaulted in `cmd/vaultaire/main.go` and `scripts/bench-vaultaire.sh`).
- Bucket layout: one `stored-<region>` bucket per region, keys
  `t-<tenant>/<container>/<artifact>`. `stored-us-west-1` and
  `stored-us-east-1` exist and are verified correctly homed (2026-07-28).
- Uploads go through the shared parallel multipart uploader
  (`s3ParallelUploadInput`, 16 MiB parts × 8 concurrent) with full metadata
  passthrough; small objects are a single PutObject.
- `List` paginates (ListObjectsV2 paginator) — no 1000-key truncation.
- `HealthCheck` = HeadBucket, which deliberately also catches a missing
  `stored-<region>` bucket before the engine can fail over writes to disk.
- Not in `STORAGE_MODE` auto-detect; enable explicitly with
  `STORAGE_MODE=lyve` + `LYVE_ACCESS_KEY`/`LYVE_SECRET_KEY`.

## Benchmarks (SLC, 2026-07-28, direct via bench-compare, west-homed bucket)

Full run: `bench-results/lyve-uswest-full-0728.json` on the SLC box.
**Refresh 2026-07-29** (`lyve-uswest-0729-refresh.json`): numbers hold or
improve — concurrent ingest **820 MB/s** (was 778), sustained upload
**446 MB/s** steady across all 60 s windows, concurrent download 443 MB/s,
multipart 256 MB 159 MB/s with h1 ≈ h2 as expected (server is h1-only at
ALPN), 64 MB single-stream GET 101 MB/s / PUT-h1 98 MB/s.

| Workload | Result |
|---|---|
| cold dial + 1 KB PUT | p50 170 ms |
| warm 4 KB PUT / GET / HEAD | p50 50 / 42 / 38 ms |
| 64 MB PUT / GET single-stream | 112.6 / 95.0 MB/s |
| 256 MB multipart (16 parallel parts) | 213.7 MB/s |
| concurrent ingest / download (20 s) | 778 / 496 MB/s |
| sustained upload 60 s | 442 MB/s, steady (no Geyser-style collapse) |
| burst 500 small files | 194 ops/s |
| worker escalation 8→128 | 602 ops/s at 128 workers, **0 errors** |
| read-after-write / overwrite / list consistency | 20/20, 10/10, 10/10 |

Comparison vs iDrive (launch hot tier, 2026-07 numbers): iDrive wins
single-stream 64 MB GET (225 vs 95 MB/s); **Lyve wins multipart (214 vs
183 MB/s) and concurrency (ingest 778 vs 512, download 496 vs 349 MB/s)**
and has no rate-limit errors up to 128 workers. Integration test
(`TestLyveDriver_Integration`, env-guarded) passes from SLC including a
20 MB multipart round-trip.

Bench gotchas:
- `bench-compare -only lyve` matches **all 7** regional endpoints (~15 min);
  use `-only lyve-us-west`.
- Per-region bench buckets `vbench-lyve-<region>` are pre-provisioned and
  correctly homed; the legacy shared `vbench-user1-lyve` bucket is
  **east-homed** — do not reuse it for west benches.
- Set `AWS_DEFAULT_REGION` when using aws-cli on SLC or every op burns ~2 s
  on IMDS probes.
