# Changelog

What's new on stored.ge. Ship requests and bug reports are credited — tell us
what you need and it shows up here.

---

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
