# Dashboard plan: the house comes inside

*Drafted 2026-09-27 from the landing-page work (PRs #479–#493). Status: Phase 0 shipped (#498), Phase 1 shipped behind `quota_checkout` (#504), Phase 2 shipped behind `house_overview` (#505), Phase 3 shipped (this PR). Owner: Isaac. Estimates are working days for one person and assume the landing sources/generator pattern.*

## Why

The public site now sells storage as a house: the floor is the tier, pieces are whole TB, the receipt is the plan. The moment someone signs in, that story stops. The dashboard still speaks the older product (Vault packs by size, a metered "Standard", a "Current Plan: Free" card) in the older visual language (purple, system fonts, generic cards). A visitor who arrives with a house in hand finds nothing that recognises it.

The plan below makes the dashboard the second half of the same product, in this order: keep the promise made at signup, make the money model match what the site sells, then bring the look in line. Everything here builds on what exists (chi router, `html/template`, PostgreSQL sessions, Stripe service, the quota tables) and on the decisions already taken (QUOTA-SOLD: every tier sold as a whole-TB quota at flat per-TB rates; Standard $4.49/TB annual, Vault $2/TB annual; pin-hot add-on $3/TB; Performance parked until after launch).

## What exists today (so the plan is honest)

| Area | Today | Gap against the site |
|---|---|---|
| Signup | `/register` form → tenant + first key → credentials shown once | Knows nothing about the house (fixed in the handoff PR: intent is stored on the tenant) |
| Billing | Stripe Checkout per **pack** (`vault3/9/18/36`, `standard` metered), `tenants.plan` string, portal link | The site sells whole-TB quotas; no quantity checkout, no per-floor plan, prices hard-coded in `server.go` |
| Quota | `tenant_quotas` (tier, limit, used), enforced on PUT, free tier 5 GB | One tier per tenant; a house with an attic needs two quotas (Standard + Vault) |
| Overview | Buckets, objects, usage numbers, onboarding checklist | Numbers, not the house; no "where is my stuff" |
| Buckets | List, objects, settings (tier preference `auto/performance/standard/archive/resilient`) | Tier names differ from the site's floors |
| Admin | Tenants, revenue, costs, backends, flags, waitlist, abuse, support | Waitlist page doesn't show the per-tier demand the site now captures |
| Look | Purple palette, system fonts, no dark mode | Site is pixel/oat/charcoal with a light/dark toggle |

## Principles

1. **One vocabulary.** Downstairs = Standard, attic = Vault, everywhere a customer looks. Internal tier ids stay as they are.
2. **One price file.** `internal/api/landing/prices.json` (already embedded as `landing.Get()`) is the only place a dollar figure lives; Stripe price ids map to it, never the other way round.
3. **Whole TB, flat rate.** No meters shown to a quota customer. Usage is shown as fullness of the house, not as a bill.
4. **Nothing surprising.** Resizing is prorated; downgrades never delete; the attic's minutes-to-restore is stated wherever a Vault object is shown.
5. **Same engineering rules as the site:** sources in the repo, generated assets stamped and guarded, browser checks in CI, accessibility 100.

## Phases

### Phase 0: keep the promise (2 days) — *the handoff PR does most of this*

- Store the house on the tenant at signup (`intent_std_tb`, `intent_vault_tb`, `intent_room`) and on waitlist rows (`plan_std_tb`, `plan_vault_tb`, `room`). **Done in the handoff PR.**
- Registration page shows "Your house: 6 TB downstairs, 1 TB attic · $28.94/mo" and carries it through. **Done.**
- Billing page shows the house as the proposed plan with a link back to the room, until a subscription exists. **Done.**
- Admin waitlist page shows TB per floor and a total, so launch-day demand is visible per tier. **Done.**
- Onboarding checklist gains a first step: "Your plan is waiting on Billing" when an intent exists and no subscription. **Done (Phase 1 PR).**

### Phase 1: quota checkout (4 days) — *the money model* — **shipped dark**

The site sells whole TB; billing must too. *Shipped: migration 066 (`tenant_floor_quotas`, `object_head_cache.floor`, `tenants.house_period`, `tenant_quotas.pin_hot_bytes`), per-floor PUT/copy/multipart/delete accounting, `billing/house.go` (prices verified against `prices.json`, quantity checkout, prorated resize, webhook → quotas), the billing page's steppers + receipt, the `quota_checkout` flag. Not in this PR: the Stripe prices themselves (created by hand — recipe in `internal/billing/CLAUDE.md`), the six env vars on prod, flipping the flag. Existing tenants need no mapping: nobody holds a Stripe subscription in prod, and tenants without floor rows keep the single total quota.*

- **Stripe products:** one recurring price per floor and period: Standard annual, Standard monthly, Vault annual, Vault monthly, each with `quantity` = TB. Plus pin-hot annual/monthly as an add-on line. Price ids come from env as today, amounts are asserted against `prices.json` at boot (a mismatch logs loudly and disables checkout rather than charging the wrong number).
- **One subscription, several items:** a house with both floors is one Stripe subscription with two items (Standard × 6, Vault × 1). Resizing = updating quantities with proration. Downgrading a floor to 0 removes the item; data is never touched by billing.
- **`tenant_quotas` per floor:** today one row per tenant. Add a `floor` (`standard`/`vault`) dimension (new table `tenant_floor_quotas` or a second row keyed by tier) so PUT enforcement and usage can be per floor. Migration + reconcile job + tests.
- **Checkout from the billing page:** two steppers (downstairs TB, attic TB), period toggle, live total from `prices.json`, "Checkout" → Stripe. Preselected from the intent.
- **Webhook:** `checkout.session.completed` and `customer.subscription.updated` set the floor quotas from the item quantities. Idempotent (Stripe events are already deduped in `stripe_events`).
- **Kill-switch:** feature flag `quota_checkout` (default off) so the code ships dark and is turned on for launch.
- Out of scope here: Performance tier (parked), founders slots (manual until demand is real).

### Phase 2: the house inside (3 days) — *overview and buckets* — **shipped dark**

*Shipped: `build.py` now also writes `internal/dashboard/templates/generated/house.html` (sprites, night defs, room background, house CSS — same sources hash, guarded by `TestHouseTemplate_GeneratedFromLandingSources`); `handlers/house_scene.go` ports the builder's layout and light rules to Go (pieces from the quota per floor, greedy 10/5/1 with a three-bookcase fallback for big floors, fill by used bytes as a clip, titles and links from the buckets in each piece, vibe and sign from the saved room, day/night shadows and lamp tints); overview cards say fullness per floor with "add a box" links; egress is a bar against the site's allowance (0.5× downstairs + 1× attic, or an admin limit); the dashboard layout reads the shared `sg-theme` so night follows the site's toggle; bucket list/settings/objects speak floors. Not done: a headless-Chrome harness for the house (the Go render tests cover the SVG); the dashboard's own dark palette is Phase 3.*

- **Overview = your house.** The same SVG house as the site, rendered from live data: pieces sized by the quota bought per floor, fill level by bytes used, labels from bucket names. Click a piece → its bucket. Night mode follows the theme toggle. Static SVG + a little JS, reusing the sprite defs from `landing/` (the generator gains a `dashboard` target that emits the sprite block for the dashboard templates).
- **Buckets speak floors.** Tier preference shown as "downstairs / attic / auto"; the attic badge says "minutes to open"; archive objects show restore state in floor language.
- **Fullness, not meters.** Storage used is "4.2 of 6 TB downstairs, 0.9 of 1 TB attic"; the upgrade nudge is "add a box" that opens the billing steppers with +1.
- **Egress** stays as it is today (allowance as fraction of quota, throttled, never billed) but is shown as a simple bar.

### Phase 3: the look (2 days) — **shipped**

*Shipped: `static/css/style.css` rewritten on the site's tokens (oat/charcoal palette, Montserrat + Silkscreen served from `static/fonts/`, square cards with a 2 px border, charcoal primary buttons with the ▸, ghost buttons, Silkscreen tags/table headers/captions, underline inputs, yellow focus ring, alerts with a 6 px left bar, dark tokens under both selectors); the layouts carry the pixel mark, favicon, theme-color and the shared `sg-theme` toggle (dashboard.js), the admin sidebar uses the bar colour with a yellow active link; template inline colours moved to tokens (`--dim`, `--line`, `--panel`, `--ok-ink`, `--danger-ink`, …). Lighthouse accessibility 100 on overview, buckets, billing and settings (settings inputs got explicit labels). One deviation from the guide: `--dim` is #666 on the dashboard, not #737373, because captions here sit on the oat page background and 4.5:1 needs it.*

- Port the site's design tokens (oat/charcoal palette, Montserrat + Silkscreen served from the binary, square cards, yellow/grey/orange tags, dark mode with the shared `sg-theme` toggle) into `layouts/base.html` and `layouts/admin.html`.
- Keep every existing page working: this is a stylesheet and layout swap, not a rewrite. Templates that inline their own colours (billing, analytics charts) move to tokens.
- Accessibility pass to 100 on the four main pages (overview, buckets, billing, settings) with the same Lighthouse routine used on the site.

### Phase 4: admin follow-through (1 day)

- Waitlist: export CSV with the per-floor columns; a "launch email" list filtered by intent.
- Revenue: MRR by floor from Stripe items; tenants list shows the house summary.
- Flags page: `quota_checkout` beside `signups`, with the same risk tiers as the runbook.

## Engineering approach

- **Sources and generation.** Dashboard-specific sprites and tokens come from the same generator (`internal/api/landing/build.py` gains `dashboard` output written into `internal/dashboard/templates/generated/`), stamped and guarded by a test like `landing_build_test.go`.
- **Tests first.** Each phase has DB-backed tests on the DATABASE_URL test database (never the dev DB): quota per floor, webhook → quota, proration maths against `prices.json`, and browser checks for the steppers and the live house.
- **Flags.** `quota_checkout` and `house_overview` ship dark and flip per tenant, then globally, using `internal/flags`.
- **Migrations.** Idempotent, numbered from 066, reviewed against the R9 schema-owner test.
- **No new dependencies** beyond what the site already uses.

## Risks and open decisions

- **Two-floor quotas change PUT enforcement.** The single-quota invariant (`storage_used_bytes == SUM(object_head_cache.size_bytes)`) becomes per floor; the reconcile job and the account-deletion path must follow. This is the riskiest change and gets its own PR with the reconcile run on prod after deploy.
- **Existing tenants** (bench accounts, the durability demo, the admin) need a mapping from `tenants.plan` strings to floor quotas; a one-off migration with a dry-run report first.
- **Stripe tax and currency** are unchanged (USD, no tax) unless Isaac decides otherwise before launch.
- **Decided 2026-09-27: yes.** A Vault-only house (attic, no downstairs) is sold; checkout accepts Standard = 0 with Vault ≥ 1 (one subscription item).
- **Decided 2026-09-27: both.** Annual and monthly ship from the dashboard at launch (six Stripe prices). The period is fixed for the life of a subscription; switching is a support request.

## Sequence and size

| Phase | Days | Ships behind flag | Blocks launch? |
|---|---|---|---|
| 0 Keep the promise | 2 (mostly done) | no | yes |
| 1 Quota checkout | 4 | `quota_checkout` | yes |
| 2 The house inside | 3 | `house_overview` | no |
| 3 The look | 2 | no | no |
| 4 Admin follow-through | 1 | no | no |

About twelve working days end to end. Phases 0 and 1 are the launch-critical path; 2 to 4 can follow launch without breaking anything.
