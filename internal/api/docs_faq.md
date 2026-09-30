# Frequently Asked Questions

## The basics

**What is stored.ge?**
S3-compatible object storage. One endpoint, one API, and an engine that routes your
data across enterprise S3 (hot) and tape libraries (cold) based on access patterns.
Anything that speaks S3 works: aws-cli, rclone, restic, boto3, JuiceFS, Cyberduck.

**What's the endpoint and region?**
Endpoint `https://stored.ge`, region `us-east-1`, path-style addressing. See
[Getting Started](/docs/getting-started).

**Is there a free tier?**
Yes — 5 GB free, no credit card. Enough to test every feature end to end.

**Where do I get my keys?**
They're shown once at signup. Need another? Dashboard → API Keys → Generate Key —
the secret is shown once; copy it then.

## Pricing

**How much does it cost?**

| Tier | Price | Best for |
|------|-------|----------|
| Standard | **$4.49/TB/mo** (annual) / $4.99 (monthly) | General storage, auto-tiered |
| Performance | $6.99/TB/mo — launches after Oct 31, not for sale on day one | Always-hot, ~24 ms reads |
| Vault (archive) | **$2.00/TB/mo** (annual, prepaid) / $2.55 (monthly) | Tape-backed archive, any size |

**Every tier is sold as a quota.** You pick a size in whole TB (any size), pay the
flat per-TB rate, and resize up or down whenever you like (prorated). The quota is a
hard cap — a write past it returns a clear quota error until you resize — so the
bill never moves on its own: no meters, no overage charges.

No API/request fees, ever. No retrieval fees. No minimum storage duration on
Standard; Vault has a 30-day per-object minimum (a third of Wasabi's 90). Egress
is free up to 0.5× your quota each month; beyond that throughput may be throttled,
never billed — no surprise egress bills. Vault restores are free up to 1× your quota
each month — test your restores monthly at no cost — and restores beyond that are
queued, never billed.

**What stays hot on Standard?**
The Smart schedule: about 15% of your quota lives on hot storage; data idle for 14+
days moves to tape. Reading it brings it back hot — within minutes while the archive
still holds it on its staging disk (roughly two weeks), a few hours once it has been
written to tape — and it then stays hot until it goes idle again (14 days), or sooner
if your hot allowance is full. The schedule is switched on per account by us — until it
is on for yours, everything you store on Standard simply stays hot. Need specific data to never demote?
The **pin-hot add-on** is $3/TB/mo for the pinned amount.

**How is $4.49/TB sustainable? Is this VC-subsidized?**
No. The tiering math alone is a real margin: at full fill, ~15% on hot storage and
~85% on tape blends our cost to about $1.94/TB against $4.49 revenue. On top of
that, **deduplication** (backup workloads dedupe 30–50%) and **zstd compression**
on every chunk are upside — your quota counts the bytes you see (logical), the
savings are ours; that's how the price works, not a fee you pay. If you store
incompressible, unique, constantly-hot data, pin it hot ($3/TB/mo) or wait for the
Performance tier after launch.

**Will my price change at renewal?**
No renewal-price roulette. The rate you sign up at is the rate you pay.

## Trust & reliability (the honest part)

**Do you have an SLA?**
Not a contractual one yet — we won't sell one we can't back. Target is 99.5%+, there's
a public [status page](/status), and incident reports are written by a human.
Contractual SLA credits ship when the second site comes online.

**Isn't a single server a single point of failure?**
For the *control plane*, yes — the API runs on one dedicated box (HA-proxied,
monitored, daily DB backups; off-host backup copies and a rehearsed restore are on
the pre-launch list). Your **data** is not on that box — it lives on enterprise S3 and
tape providers. If the server dies, your data is intact and the control plane restores
from backup (target: under an hour). A hot standby in a second city is the first thing
new revenue buys.

**What if you disappear? / bus factor of one?**
Three answers: (1) It's standard S3 — `rclone sync` your data out anytime, egress
is never billed (a full exit past your monthly allowance is throttled, not charged),
there's no proprietary format and no lock-in. (2) Your data lives on
established providers that don't depend on us. (3) The core engine is
[open source](https://github.com/FairForge/vaultaire). Documented wind-down
commitment: minimum 60 days' notice and free unlimited egress before any shutdown.

**Should I trust a brand-new provider with my only copy?**
No — follow 3-2-1. We should be your second or third copy, not your only one. That's
true of any provider, including the big ones. 30-day full refund, no questions.

## Technical

**How fast is it?**
~320 MB/s sustained multipart upload from a datacenter host (measured 2026-07);
from home you'll saturate your uplink first. Sub-1ms HEAD/metadata (served from cache). Range-GET
passthrough for video seeking and partial restores.

**How does encryption work?**
Everything is encrypted in transit (TLS 1.2+, HSTS). At rest you have two options
today: SSE-C (you send the key with each request; we encrypt with it and keep
nothing) or client-side encryption (rclone crypt, restic — we store ciphertext and
can't read a byte). Server-managed encryption at rest (SSE-S3, AES-256-GCM) is built
and tested but **not yet enabled in production**; when it is, it will be on by default
for every bucket and this answer will say so. Client-side encryption disables dedup on
that data — everything else still works.

**A privacy note on deduplication:**
Dedup is content-addressed: identical chunks are stored once, per account for
encrypted data and shared across accounts for plain data. Known limitation: someone
who already possesses an exact file could confirm it exists in the store. If that's in
your threat model, use SSE-C or client-side encryption and you keep dedup on everything
else.

**Cold data retrieval — is it instant?**
Hot data is <50ms. Data aged to tape spools to cache on first read — seconds to a
couple of minutes for first byte, longer for multi-TB restores. Need specific data
hot forever? The pin-hot add-on ($3/TB/mo) keeps it from ever demoting; a full
always-hot Performance tier launches after October 31.

**Which S3 features are supported?**
Multipart, Object Lock (governance + compliance/WORM, legal hold), presigned URLs,
CORS, range + conditional requests, tagging, batch delete, ListObjectsV2, scoped API
keys (per-bucket, IP allowlist, expiry), STS temporary credentials, and versioning in
the sense of version history + delete markers (retrieving a *previous* version's bytes
is not yet available — `GET ?versionId=<old>` answers 501 rather than the wrong bytes).
The full list is the [OpenAPI spec](/openapi.json).

## Support

**How do I get help?**
Email [support@stored.ge](mailto:support@stored.ge) — founder-direct, target
response under 4 hours. For abuse reports, see [/abuse](/abuse).
