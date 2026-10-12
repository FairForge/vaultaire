# Consumer surface, charter ("the Apple of storage")

*Added 2026-10-12. Lives at stored.cloud/photos (stored.pics redirects there) and in the apps. Contract: `docs/ARCHITECTURE_CONTRACT.md`. Plan homes: 42.3 Stored, Phase 10.0 (the Ente model, F-12, F-13), 41.1 to 41.2, 43.5; decision F-19.*

**Who it is for.** A person who will never read documentation. They want their photos and files safe forever, beautiful, shareable with family, findable, and recoverable when they forget a password.

**The promise, in one sentence.** Only you can see your photos. Not us, not advertisers, not an AI. And you will never lose them.

**Default mode.** **Private**, once the recovery story is mainstream-grade (F-19): a passcode recovery vault with hardware attempt limits (the Apple iCloud Keychain escrow pattern; Juicebox, WhatsApp's Backup Key Vault and Signal's SVR3 are the open references), trusted contacts (Ente's "Legacy"), passkeys, and recovery nudges before protection is on. **Before that**, the default is recoverable Standard with "Advanced protection" as the opt-in that demands a recovery method first, which is exactly Apple's shape. The user never sees a mode; the product decides per data type.

**The copy rule.** Outcomes only. No keys, phrases or the word "encryption" on screen before the user asks. Never "zero-knowledge".

**Non-goals.** S3 semantics, API documentation, modes as settings, server-side scanning of private content (the north star's CSAM posture: public and CDN content is scanned, private is reactive).

**What this crowd punishes.** Lost accounts, missing magic (faces, "dog at the beach", memories, previews everywhere, shared albums, links that open without an app), and friction.

**Metrics.** Recovery success rate; account lockout rate; day-30 retention; upload success on mobile networks; time-to-first-shared-album; support tickets that needed content we cannot see (should be zero, by design).

**The first ten.**

1. The product is Ente's audited clients on our engine (42.3 Stored, `~/fairforge/stored.pics`), brought back on the box when this lane opens.
2. Keystone consumed: the SDK key core (10.0.1) in the Go SDK and as WASM in the browser client (10.0.2).
3. SRP or OPAQUE sign-in and passkeys (10.0.5, F-13).
4. The recovery vault (F-19), first realm on the attested box, plus trusted contacts (10.0.3).
5. On-device ML: CLIP search and face grouping (41.2), the feature list this crowd checks.
6. Sharing: sealed-box album shares, public links with the key in the fragment (10.0.6), casting.
7. Blind indexes for dates and sizes (41.1) so the timeline works without reading.
8. The web client's trust story: published build hashes in the transparency log, a verify extension later.
9. Family plans on pooled quotas (42.1).
10. Numbered accounts, demand-gated (43.5).
