# Changelog

What's new on stored.ge. Ship requests and bug reports are credited — tell us
what you need and it shows up here.

---

## 2026-07-20 — Customer docs are live

- **Real documentation at [/docs](/docs).** Getting Started (signup to first
  upload in two minutes), a full rclone guide, and an honest FAQ covering
  pricing, trust, and the technical details. The interactive API reference
  moved to /docs/api and now carries the right name and support address.

## 2026-07-20 — New homepage

- **The real stored.ge front page is live.** Tier ladder (Vault archive →
  Standard → Performance), honest measured numbers, full pricing with the
  Vault packs, and a straight-answers section covering the questions most
  storage providers dodge. The old pre-launch placeholder page is gone.
- **The footer now links everything.** Terms, privacy, acceptable use, DPA,
  GDPR, BAA, EU Data Act, the status page, this changelog, and abuse
  reporting — all reachable from the homepage at last.

## 2026-07-20 — Live-iteration kit

- **Runtime feature flags.** New capabilities now roll out flag-dark and get
  enabled per account, so early adopters can opt in before a feature is on
  for everyone. Kill-switches let us turn a misbehaving subsystem off in
  seconds — no redeploy, no downtime.
- **This changelog.** Public, updated with every deploy. Entries credit the
  person who asked for the change.

## 2026-07-20 — Storage engine hardening

- **Failed backend writes now fail loudly.** If a storage backend rejects a
  write, the upload returns an error your client retries — data is never
  silently parked somewhere it doesn't belong.
- **Abandoned multipart uploads are reaped.** Uploads idle for 48 hours are
  aborted automatically and their parts cleaned up, and a single upload's
  in-flight parts are capped at 50 GiB.

## 2026-07-19 — Deduplication + garbage collection

- **Dedup GC coherence.** The nightly garbage collector, the dedup cache, and
  concurrent uploads can no longer race each other — shared chunks are
  reference-counted with database-level locking end to end.
- **Deploy auto-rollback.** A deploy that fails its health check now rolls
  back to the previous binary automatically.
