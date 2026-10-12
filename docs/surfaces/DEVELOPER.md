# Developer surface — charter ("the Stripe of storage")

*Added 2026-10-12. Lives at stored.cloud/developers, docs.stored.cloud, s3.stored.cloud and the management API. Contract: `docs/ARCHITECTURE_CONTRACT.md`. Plan homes: Phases 5.10, 5.11, 26, 31, 42.1, 18; WP-SSEC-1; WP-DOMAIN-1.*

**Who it is for.** A developer, an ops person, a backup tool author, a reseller. They read documentation, they script, and they punish surprises.

**The promise, in one sentence.** An S3 endpoint that never surprises, a management API that reads like Stripe's, documentation that answers before you ask, and a platform you can resell.

**Default mode.** **Standard**, first-class. Server-side features read content here on purpose: transforms, rules, functions, events, search, CDN. **Private** is offered, never imposed: the SDK (Private spaces reachable through S3 as ciphertext), SSE-C with the "SSE-C required" bucket flag, and a guide to client-side tools (restic, rclone crypt, Kopia, Duplicati, Veeam). **Public** for public buckets, served through the content domain and the edge.

**The copy rule.** Standard is "encrypted at rest with keys we hold", with the per-mode table from `modes.json` on the page. Never "zero-knowledge".

**Non-goals.** Consumer UX, photo features, the voice of the home page. Nothing here hides a mode; everything here documents one.

**Metrics.** Conformance pass rate (versitygw BATS and our suite); documented operations / total operations in OpenAPI; time-to-first-successful-PUT for a new key; 4xx caused by our divergence from S3; docs search zero-result rate; status-page incidents with a post-mortem.

**The Stripe standard, and where we are.** Idempotency keys (shipped), webhooks (shipped, outbox pending WP-R11-3), API version header (shipped), sandbox / test-mode tenants (not built — 26.1), SDKs in the languages that matter (26.6, not built), a docs portal with a changelog (26.5 → docs.stored.cloud), an API explorer (26.7), a status page (5.14.12 → status.stored.cloud), per-space quotas and the plan ladder (42.1), resellers (Phase 18).

**The first ten.**

1. WP-DOMAIN-1: s3.stored.cloud and docs.stored.cloud named in every example before launch; stored.ge served forever.
2. The status page on status.stored.cloud.
3. The client-side encryption guide (zero code).
4. WP-SSEC-1 and the "SSE-C required" flag.
5. Sandbox tenants (26.1) with a test-mode key prefix.
6. The docs portal with a changelog and the OpenAPI as its source (26.5).
7. SDKs: Go first (the key core ships inside it), then TypeScript and Python (26.6).
8. Smart buckets: rules, then functions (Phase 31, decision F-1).
9. The plan ladder and pooled quotas (42.1).
10. Reseller surface (Phase 18) once two resellers exist.
