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
| Standard | **$3.99/TB/mo** (annual) / $4.99 (monthly) | General storage, auto-tiered |
| Performance | $5.99/TB/mo | Always-hot, ~24 ms reads |
| Vault packs (archive) | from **$1/TB/mo** | Tape-backed deep archive |

No API/request fees, ever. No minimum storage duration. Egress is free up to 3×
your stored volume each month, then $0.01/GB (9× cheaper than AWS).

**How is $3.99/TB sustainable? Is this VC-subsidized?**
No. Three things make the math work: (1) **tiering** — cold data migrates to tape
that costs us a fraction of hot storage; (2) **deduplication** — backup workloads
dedupe 30–50%, and we bill logical bytes while paying for physical; (3) **zstd
compression** on every chunk. Blended cost on realistic workloads lands well under
our price. If you store incompressible, unique, constantly-hot data, that belongs on
the Performance tier (still cheaper than B2).

**Will my price change at renewal?**
No renewal-price roulette. The rate you sign up at is the rate you pay.

## Trust & reliability (the honest part)

**Do you have an SLA?**
Not a contractual one yet — we won't sell one we can't back. Target is 99.5%+, there's
a public [status page](/status), and incident reports are written by a human.
Contractual SLA credits ship when the second site comes online.

**Isn't a single server a single point of failure?**
For the *control plane*, yes — the API runs on one dedicated box (HA-proxied,
monitored, daily off-site DB backups, tested restores). Your **data** is not on that
box — it lives on enterprise S3 and tape providers. If the server dies, your data is
intact and the control plane restores from backup (target: under an hour). A hot
standby in a second city is the first thing new revenue buys.

**What if you disappear? / bus factor of one?**
Three answers: (1) It's standard S3 — `rclone sync` your data out anytime, egress
is free, there's no proprietary format and no lock-in. (2) Your data lives on
established providers that don't depend on us. (3) The core engine is
[open source](https://github.com/FairForge/vaultaire). Documented wind-down
commitment: minimum 60 days' notice and free unlimited egress before any shutdown.

**Should I trust a brand-new provider with my only copy?**
No — follow 3-2-1. We should be your second or third copy, not your only one. That's
true of any provider, including the big ones. 30-day full refund, no questions.

## Technical

**How fast is it?**
~320 MB/s sustained multipart upload from a datacenter host; from home you'll
saturate your uplink first. Sub-1ms HEAD/metadata (served from cache). Range-GET
passthrough for video seeking and partial restores.

**How does encryption work?**
Encrypted at rest by default (SSE-S3). Want to hold your own keys? Use SSE-C, or
client-side encryption (rclone crypt works great). Client-side encryption disables
dedup on that data — everything else still works.

**A privacy note on deduplication:**
Dedup runs over encrypted chunks via convergent encryption (key derived from content
hash). Known limitation: someone who already possesses an exact file could confirm it
exists in the store. If that's in your threat model, use SSE-C or client-side
encryption and you keep dedup on everything else.

**Cold data retrieval — is it instant?**
Hot data is <50ms. Data aged to tape spools to cache on first read — seconds to a
couple of minutes for first byte, longer for multi-TB restores. Need everything hot
forever? That's the Performance tier.

**Which S3 features are supported?**
Multipart, versioning, Object Lock (governance + compliance/WORM), presigned URLs,
CORS, range + conditional requests, tagging, batch delete, ListObjectsV2, scoped API
keys (per-bucket, IP allowlist, expiry), and STS temporary credentials.

## Support

**How do I get help?**
Email [support@stored.ge](mailto:support@stored.ge) — founder-direct, target
response under 4 hours. For abuse reports, see [/abuse](/abuse).
