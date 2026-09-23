# Changelog

What's new on stored.ge. Ship requests and bug reports are credited — tell us
what you need and it shows up here.

---

## 2026-09-22 — Quota pricing: pick your TB, flat rate, every tier

- **Every tier is now sold as a quota.** You choose a size in whole TB (any
  size), pay a flat per-TB rate, and resize up or down whenever you like
  (prorated). The quota is a hard cap — writes past it return a clear quota
  error until you resize — so your bill never moves on its own. No meters,
  no overage charges. Vault already worked this way; now Standard does too.
- **Standard is $4.49/TB/mo annual prepaid, $4.99 monthly.** About 15% of
  your quota stays on hot storage; data idle for 14+ days moves to tape and
  comes back hot within minutes when read. New **pin-hot add-on** ($3/TB/mo)
  for data that must never demote.
- **Egress allowances are keyed to your quota**: Standard egress is free up
  to 0.5× your quota per month, then throttled rather than billed. Vault
  restores are free up to 1× your quota per month; beyond that they queue,
  never billed. (The old "3× stored" and "$2.99/TB" overage lines are gone.)
- **Performance tier is parked** until after launch — it returns at $6.99/TB/mo.
  Until then, pin-hot on Standard covers always-hot needs.

## 2026-09-16 — Simpler Vault pricing: $2/TB, any size

- **Vault is now linear.** The prepaid pack ladder (Vault1–Vault18) is gone;
  the tape-backed archive tier is simply **$2.00/TB/mo annual prepaid** or
  $2.55/TB/mo monthly, at whatever allocation size you want. 5 TB of tape
  archive is $10/mo billed yearly. Packs punished small archives and made
  you buy sizes you didn't need — linear pricing fixes both.
- **The limits, stated plainly** (they're what make the price honest): one
  durable tape copy — we're the offsite leg of *your* 3-2-1; 30-day minimum
  per object; free restores up to 1× your stored bytes each month (run a
  restore drill monthly, free), then $2.99/TB; restored copies stay hot for
  7 days. Never retrieval fees, never egress fees.
- **Founders slots.** The first 100 TB sold go at $1/TB/mo, annual prepaid,
  3–10 TB per account, lifetime rate, numbered slots. Hard-capped — when
  they're gone, they're gone.
- **Launch date is October 31, 2026.**

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
