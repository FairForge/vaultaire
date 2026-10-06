# Status — the live pointer

Updated: 2026-10-06. Single writer: the plan driver folds worker results in; a worker PR edits only the line of its own item. This file replaced the Status block at the top of `docs/IMPLEMENTATION_PLAN.md` as what `/next` and `/next-step` read first, so code PRs stop touching the 2,900-line plan. The plan keeps the rules, the order, the decisions and the phases; `docs/reviews/SYNTHESIS.md` keeps the owner checklist and the decision table.

## Prod (read-only facts, 2026-10-06 15:20 UTC)

- main @ `12a29e3` deployed 2026-10-05 19:17 UTC. `STORAGE_MODE=idrive` (the new reseller account, primary since 2026-10-04 15:08 UTC; `wasabi` registered, dormant). 17 backends healthy. `SIGNUPS_ENABLED=false`.
- Not set on prod: `EMAIL_PROVIDER` (LogSender: a password reset sends nothing), `ENCRYPTION_MASTER_KEY`, `STRIPE_*`, `SYNTHETIC_CHECK_*`.
- Prometheus loads `vaultaire-{alerts,backends,tls}.yml` only; `auth`, `synthetic`, `egress`, `jobs`, `routing` are not installed.
- Backups: on the box, `0664`, 7 days, nothing leaves the host (WP-R9-7). HAProxy: no `/metrics` deny; `tune.h2.initial-window-size 262144`.
- Flags: `signups` off (env); `egress_throttle`, `smart_demotion`, `quota_checkout`, `house_overview` OFF; `vault_parity` ON for the bench tenant only; `chunking` on (code default).
- Migrations: `078_bucket_cors.sql` latest. **Next: 079.**

## Launch

- Date of record: 2026-10-31 (plan). The 2026-10-06 plan review recommends opening signups the day the owner checklist is done and making the public push Black Friday, 2026-11-27 — Isaac's call.
- Owner checklist: SYNTHESIS "Pre-launch checklist", plus the 2026-10-06 review's launch table: Stripe endpoint + six prices, an e-mail provider (Email Service proven), the synthetic tenant + the five rule files, backups off-box, 2FA on both Cloudflare logins + rotation of the 2026-10-03 credentials + the Lyve root key off the data plane + the `/metrics` deny, the DMCA designated agent + an NCMEC runbook, the flags (`egress_throttle` exemptions then on, `signups`), decisions D-19/D-21/D-23/D-25/D-26, the master key, `0.0.0.0/0` refused.

## Worker queue (one item per session; done = merged, deployed, verified, this file updated)

1. ~~**GitHub churn**~~ — **done 2026-10-06, #601** + the merge-queue rule on the main ruleset (SQUASH, ALLGREEN, up to 5 built / 5 merged, min 1, 60 min check timeout): `gh pr merge --auto --squash` now enqueues, the PR is tested on the merge branch and merged there; `paths-ignore` on `deploy.yml`; dependabot `golang.org/x/*` group; this file.
2. ~~**Bucket CORS**~~ — **done 2026-10-06** (`internal/api/s3_cors.go`, migration 078): `PutBucketCors` / `GetBucketCors` / `DeleteBucketCors` in the AWS shape, default none; the OPTIONS preflight answered before SigV4 (by bucket name across tenants); the matching rule's headers on every status of the real request; `docs/API.md` + the changelog. Live proof against `s3.stored.ge` after the deploy is the next thing to do.
3. **`whoami`** — `GET /api/v1/whoami` signed with the key (SigV4): tenant id, key id, permissions, bucket scope, expiry, whether it is an STS token — so an edge gateway can partition its cache per tenant (plan 42.7).
4. **Zero-downtime deploy** — two instances (`vaultaire@8000` / `vaultaire@8001`) behind HAProxy with a drain-and-switch in `deploy.yml`; one active at a time (the pending-TOTP secret and the auth cache are per process); `docs/DEPLOY.md`.
5. Parked: **WP-R11-3** webhook outbox (asked 2026-10-02, unanswered; returns with the first app that purges an edge copy on delete). Old queue items 6 (encryption track), 7 (WP-R2-1), 12 (edge follow-ups): the 2026-10-06 review recommends post-launch; await Isaac.
6. Then the FULL-PLAN TRACK of the plan, Stage 1a first (WP-R2-1, WP-R8-1, WP-R3-1, WP-R6-1); the app-plane asks (the outbox, pooled organisation quotas 42.1) may jump the queue.

## Lanes

- **Engine**: this repository, one worker prompt per turn, the plan driver reviews every PR before the next prompt.
- **Apps**: other repositories (snapshelter, hearth, storedpics) against prod's public API as ordinary tenants; an engine ask becomes a queue item here.
- **Lab**: a local worktree + `lab/<topic>` branch off `origin/main`, own test DB (`vaultaire_test_<name>`) and port; findings in a new per-topic file; never a checkout in the main checkout.

## Known stale claims (fix with the next docs PR that touches the file)

- Plan `:889` "5.12.2 R2 driver DROPPED" — it shipped in #466 and is registered on prod. Plan `:2506` and `:1898` still say "demand-gated". Plan `:2904` names 073 as the latest migration.
- Landing: "30-day minimum per object" (D-21 says drop), the Performance-tier egress line (`landing.src.html:622`), "free unlimited egress" on wind-down (`:686`). SYNTHESIS prod facts: the h2 window is 256 KiB, not 4 MiB; the iDrive-regions row is both DONE and open.
- `.private/TIER_STRATEGY.md` prices ($3.99 Standard, $6 Performance, $6.99 Wasabi); `.private/CLAUDE.md` "Lyve DROPPED"; `.private/LAUNCH_EXECUTION_SEQUENCE.md` Stage 6 "Aug 31".
