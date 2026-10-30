# Accounts — who can hold data, who pays, who acts

*One page. Written 2026-10-29 at the close of Stage 1a Prompt 51 (WP-PLAT-1), on the owner's ask: "family, as well as AI agent and AI-created apps accounts, or whatever is best". Every account-shaped WP cites it: WP-TEAM-1, WP-ACCT-1, WP-KEY-1, WP-AGENT-1, WP-APP-1, WP-FAMILY-1. It refines plan 42.2 (the tree of record) against the contract (`docs/ARCHITECTURE_CONTRACT.md`).*

## The four nouns

| Noun | What it is | Where it lives |
|---|---|---|
| **Tenant** | A **space**: the unit of data, of keys and of disclosure. It has its own buckets, its own tenant key hierarchy (contract line 4), its own dedup scope (line 5) and its own crypto-shred. A demand for one tenant never touches another's rows, keys or logs (line 13). | `tenants` |
| **Pool** | The **paying unit**: a tree of tenants under one root that pays. Quota, egress allowance and the bill are the pool's. | `tenants.parent_tenant_id` (117); `pool_root_id` (WP-ACCT-1) |
| **Person** | An **identity** that signs in. It may act on several tenants (home and work: 42.2). It is never storage. | `users`; `tenant_members` (WP-TEAM-1) |
| **Principal** | **Who acts on a request:** a member (a person with a role), a key (S3, scoped), a management key (a server, WP-KEY-1) or an **agent** (an AI acting for a person, WP-AGENT-1). Each has a scope, optional caps and an expiry, and is named in the request log and the audit trail. | `api_keys`, management keys, agent grants |

**The refinement of 42.2.** 42.2 calls the organisation (a family, a team, a business) "the paying and isolation unit". The contract, written later the same month, makes the **tenant** the unit of disclosure, and the consumer surface is Private by default. So:
- **paying** belongs to the pool;
- **isolation and disclosure** belong to the tenant.

Under this split, a **family is a pool of personal tenants**, not one tenant with members. A **team is one shared tenant with members.** Both are "organisations" in 42.1's price ladder; they differ in what a member can see.

## The edges of the tree

A child tenant hangs off its parent by one of three edges (`tenants.child_kind`, WP-ACCT-1; today's children are `managed`):

| Edge | The parent may | The child is | Used by |
|---|---|---|---|
| **managed** | mint and revoke its keys, read its data through them, schedule its erasure. Its erasure **cascades** (WP-PLAT-1). | user-less. An app's end user, a platform's customer, an agent's workspace. | WP-PLAT-1 (done), WP-APP-1, WP-AGENT-1 |
| **app** | everything `managed` allows, plus: the child may have `managed` children of its own (**depth 2**, the only depth beyond 1) | an app someone built (by hand or by an AI); its end users are its children | WP-APP-1 |
| **sponsored** | pay for it; see its **usage totals only** (bytes, never names); set a cap; remove it from the pool. **Never** mint its keys, read it, see its request log or erase it. | a person's own account with its own login, keys and recovery. Removed from the pool, or the sponsor erased, it **detaches** and keeps its data, never erased by the sponsor. | WP-FAMILY-1 |

**Rules for every edge:**
- **One pool, one root.** `pool_root_id` is denormalised on every tenant of a tree, so the quota check stays one indexed sum at any depth. It locks the root's row first, as PR B does today (`usage/pool.go`).
- **Caps inside the pool.** A child's own `storage_limit_bytes` is 0 = "the pool decides" (today) or a positive cap = min(cap, pool). The same holds for egress. That is 42.1's "members draw from the pool with optional caps".
- **Suspension** flows down `managed` and `app` edges only. A sponsored member is an independent person, so the sponsor's suspension for abuse does not suspend them.
- **Erasure** cascades down `managed` and `app` edges and detaches `sponsored` ones. *(Branched by WP-ACCT-1, Stage 1a Prompt 53: `account.DetachSponsored`, `TestTree_SponsoredNeverSuspendedOrErasedWithTheSponsor` on a sponsored row made by SQL. WP-FAMILY-1's `TestFamily_SponsorErasureDetachesNeverErases` proves it again through a real family invitation.)*
- **Residency** is a tenant attribute (`tenants.residency`: `eu` / `uk` / `us`, NULL = any). It is set at creation and never changed. A child inherits its parent's and may narrow it, never widen it. Bucket creation, the mirror leg and the parity leg stay inside it (D-STRIPE-30; this also closes RP76-1). It covers where bytes are stored at rest. Bytes in transit through an edge colo are not storage, and the docs say so.

## The accounts, one by one

- **A person** (today's registration): a root tenant and one user, its owner member (WP-TEAM-1).
- **A team or business:** one shared tenant with members and roles (`owner` / `admin` / `developer` / `viewer`, WP-TEAM-1). Everyone sees the shared data, by role.
- **A family** (WP-FAMILY-1): the organizer's root pays the pool (42.1's draft: up to 6 people, 3 TB, $6/mo). Each member is a **sponsored** child with their own login, keys, recovery and Private library. The organizer sees each member's usage, sets caps and removes members, and **sees nothing else**.
  - Invitations reuse WP-TEAM-1's (`tenant_invitations.kind = 'family'`).
  - Someone with an existing account accepts by attaching it, which is allowed only without an active paid entitlement of their own (F-40).
  - A **shared family space** is optional: one more tenant in the pool with every member as a WP-TEAM-1 member, for the albums everyone adds to.
  - **Adults only in v1.** A minor's account (consent, parental controls) waits for counsel (F-40).
- **A platform** (WP-PLAT-1, done): granted by an admin; its customers are `managed` children. It reaches the API with a **management key** (D-STRIPE-29, WP-KEY-1), not a person's JWT.
- **An AI-created app** (WP-APP-1): an `app` child of the account of the person who built it (or whose agent built it), with:
  - its own management key for its backend, scoped to its subtree;
  - a cap inside the builder's pool;
  - its end users as `managed` children;
  - deletable as a unit.

  Any **verified, paying** account may create apps up to a count, with no admin grant (F-43). The admin grant stays for third-party platforms and resellers (unlimited children, 18.x). An app's content is never Public until its root is verified (line 14's surface).
- **A claimable app** (WP-APP-1, the integration path for AI app builders such as Lovable, Bolt, Replit, v0 and Claude Code): a partner's key creates a provisional account with tiny caps (1 GiB, never public, 7 days), plus a claim link. A person who signs in through the link keeps it as an `app` under their own account; unclaimed, it is purged like test data. Partner-keyed only: no anonymous creation (F-42).
- **An AI agent** (WP-AGENT-1): **not an account but a principal** acting for a person or an organisation, which stays accountable (the terms say so).
  - **Connecting:** a person connects an agent through OAuth 2.1 with PKCE (the MCP authorization flow: `mcp.stored.cloud`, and `stored mcp` over stdio for local tools). The consent screen shows the scope and the caps, and the result is an **agent grant**: a management key with `actor = agent`, a name, the granting person, caps (WP-KEY-1) and a lifetime (30 days by default, renewable).
  - **Approvals:** what could lose or expose data is **never in a default agent scope**: deleting a bucket, making anything Public, bypassing Object Lock, minting keys beyond its own scope, exports, erasing the account. An agent that needs one gets 403 `approval_required` with an approval link. The person approves in the dashboard (with TOTP for the destructive ones), and the grant holds once.
  - **Safety:** buckets an agent creates are versioning-Enabled, so an agent's mistake is a version back (F-41).
  - **Test first:** every agent grant comes with a test key into the sandbox, so it can rehearse before it touches live data.
  - **Its own space:** an agent may also get an **agent workspace**, a `managed` child with its own cap, for its scratch files and outputs.
  - **Modes:** Standard and Public only. An agent never reads Private content server-side (line 8). An on-device agent reads Private content only through the SDK key core.
- **A service account** is a management key with no person behind each call: covered by WP-KEY-1, no new kind.
- **A legacy or recovery contact** belongs to the recovery vault (contract line 9, F-19), not to the tree.

## The order

| # | WP | Why then |
|---|---|---|
| 52 | **WP-TEAM-1** (+ Part 0: RP82, and RB9-2, the pool's egress allowance) | Persons on tenants, roles and invitations: everything after reuses them. |
| 53 | **WP-ACCT-1**: the tree | `pool_root_id`, `child_kind`, caps inside the pool, `tenants.residency` with its enforcement. The data model the other three need, built once. |
| 54 | **WP-KEY-1**: management keys, per-key and per-bucket budgets (F-39) | A platform, an app backend and an agent all need a credential that is not a person's session. |
| 55 | **WP-AGENT-1 + WP-APP-1** | The agent channel and the AI app builders are distribution: ChatGPT is today's one organic channel, and an AI tool that provisions storage for every app it builds is the Neon / Supabase path. |
| 56 | **WP-FAMILY-1** | Needs the consumer surface's apps and recovery to be worth selling. Its backend is small once 52–54 exist. |
| then | WP-POST-1, WP-XFER-1 (after the first node), WP-VAULT-2 | As planned. |

## Owner decisions (F-40 … F-43)

- **F-40 Family.** Price, pool size and member count (42.1's draft: $6/mo or $60/yr, 3 TB, 6 people). Whether a member who pays for their own plan may join (recommended: not until they cancel). **Minors** (recommended: adults only until counsel answers).
- **F-41 Agent safety defaults.** Which operations always need a person's approval (recommended: the list above). Versioning on by default for agent-created buckets (recommended: yes, with lifecycle expiry once Phase 12 lands). The longest agent grant (recommended: 30 days, renewable).
- **F-42 Claimable apps.** Partner-keyed only (recommended), or anonymous creation behind a rate limit. The caps and the TTL.
- **F-43 Self-serve apps.** May any verified, paying account create apps without an admin grant (recommended: yes, up to 25, each capped inside its pool)? The admin grant stays for third-party platforms.
