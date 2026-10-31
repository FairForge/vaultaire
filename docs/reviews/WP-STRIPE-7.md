# WP-STRIPE-7 — AI agents as principals, then AI-created apps (Stage 1a Prompt 55), after Part 0 = RP88 Part 2's findings

*Plan § WP-STRIPE, the **WP-AGENT-1** and **WP-APP-1** rows (`docs/ACCOUNTS.md` § Principal, § Agents, § Apps; F-41, F-42, F-43 — all three still Open: built to their stated recommendations, dark behind flags). It asks for:*
- *an agent grant that IS a management key with a person behind it: the MCP authorization flow (OAuth 2.1 + PKCE, dynamic registration) at `mcp.stored.cloud` and `stored mcp`, a consent with TOTP, 30 days renewable, a sandbox test key, the request log and the audit naming the agent and its person;*
- *approvals for what could lose or expose data, versioned agent buckets, never Private (line 8);*
- *self-serve apps of a verified, paying account (up to 25), their end users at depth 2 in the builder's pool, the cascade, never Public until verified;*
- *claimable apps made by a partner's key, claimed with their data in one transaction, purged unclaimed.*

*Prompt: `docs/reviews/WP-STRIPE-PROMPTS.md` § Stage 1a Prompt 55 (both editions). Session 2026-10-31. **Migration 122** (`122_agents_apps.sql`: written in Part 0 with RB12-9's indexes, extended by PR A with every table of the prompt before PR A merged — D-STRIPE-43's rule). Next: **123**. The post-merge review is **RP90**.*

Contract lines cited (`docs/ARCHITECTURE_CONTRACT.md`):
- **4** (keys only through the one hierarchy): an agent grant, an app's backend key, a partner's key and a claimable's builder key are management keys (WP-KEY-1); every S3 key is still minted through `persistAPIKey`. No key material is added.
- **5** (convergence within a space only): untouched. A claim re-parents a tenant; its chunks, refs and keys stay that tenant's.
- **6** (the chunker is frozen): untouched.
- **8** (Private is read only on the owner's devices): an agent never reaches a Private bucket — management API and S3 alike.
- **14**'s surface: an app's bucket and a provisional tenant's are never Public until the root is verified.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1099** Part 0 | RP88-6 the egress cap refuses a response larger than what is left: `egressCapWriter` outside the handler checks the `Content-Length` at `WriteHeader`, pre-charges it in one conditional `UPDATE` per holder inside one transaction, credits back what an abort left unsent; `/cdn` the same; the edge admit's span. RP88-8 a management key narrows a cap, never widens one (409 `cap_widening_needs_person`). RP88-7 the warning claim answers the step it displaced. RP88-9 `/cdn` counts 200/206 only. RP88-11 `id = $1::uuid`. RB12-9 three partial `written_by_key` indexes (122). RB12-6 `HolderStorageDrift` | `TestKeyCap_EgressCapRefusesAResponseLargerThanWhatIsLeft`, `_EgressPreChargeCreditsBackWhatWasNotSent`, `_AKeyNeverWidensACap`, `_EgressWarningClaimAnswersThePreviousStep`; promtool cases |
| **PR A** agent grants | 122: `agent_grants`, `approvals`, `agent_oauth_clients`, `agent_oauth_codes`, `api_keys.agent_grant_id`, `claims`, `tenants.partner` / `provisional_until` / `provisional_partner`, the `partner` scope; erasure (ByTenant; ByUser revokes the person's grants) and export (`agents`, `claims`) in the same PR. `auth.MintAgentGrant`: the live key and its sandbox test key in one transaction, 30 days, the agent S3 ceiling. Renew / revoke (a person; the revoke takes every S3 key the grant minted). The audit's principal `agent:<grant>` with `performed_by` = the member; the request log `mk:<id>`. The OAuth server (`/.well-known/oauth-authorization-server`, `/oauth/register`, `/oauth/authorize`, `/oauth/token`) and the dashboard's consent (`/dashboard/agents/connect`: a session, the management-key role, a fresh TOTP code; the sign-in it interrupts returns to it). `edge/mcp/` (the Worker: Streamable HTTP, RFC 9728 metadata, seven tools, SigV4 checked against the AWS SDK's vector) and `stored mcp` (a stdio bridge to it). Flag `agent_grants` | `TestAgent_GrantExpires`, `_RequestLogNamesTheAgent`, `_ConsentMintsOneGrant` (a mutation that drops the single-use claim fails it), `_ItsKeysCarryTheGrant`, `TestAgentConnect_ConsentNeedsTOTPAndIssuesOneCode`, `TestMCP_BridgesStdioToTheServerWithTheGrantsKey`, `edge/mcp` node tests (7) |
| **PR B** approvals | An agent's DELETE of a bucket, or revoke of a key its grant did not mint, is 403 `approval_required` with `approval_id` + `approval_url`; `GET /approvals`, `POST /approvals/{id}/approve` (`code` = TOTP for a destructive act; the act's own role) and `/deny`; one action on one resource, spent once by one conditional `UPDATE`, 24 h. Agent-created buckets versioning-Enabled. Never Private: both lists leave it out, every bucket route 403 `private_bucket`, and an agent-minted S3 key (`KeyScope.AgentGrantID`, from the one lookup) is AccessDenied on a Private bucket or copy source | `TestAgent_DefaultScopeCannotDeleteABucket`, `_ApprovalGrantsOnce`, `_CreatedBucketsAreVersioned`, `_NeverReadsPrivate` (two mutations — the gate removed, the Private guard removed — fail them) |
| **PR C** apps | `POST /api/v1/manage/apps` (owner/admin) makes an `app` child of a verified, paying root with no platform grant, ≤ 25 (counted under the builder's row `FOR UPDATE`), and its backend's key (`apps` + `manage:write`); that key makes end users at depth 2 in the builder's pool; `DELETE` schedules the app and its users and revokes its keys; the door refuses Public for an app under an unverified root (`modes.ErrUnverifiedNeverPublic`). Flag `apps` | `TestApps_BuilderCreatesWithoutAGrant`, `_EndUsersAtDepthTwoDrawFromTheBuildersPool`, `_DeleteCascades`, `_NeverPublicUntilVerified` (a mutation of the door fails it) |
| **PR D** claimable apps | `PUT /api/v1/admin/tenants/{id}/partner`; `/api/v1/partner/claimables` for a partner's key only (404 to anyone else): a provisional managed child of the partner (1 GiB, never Public, 7 days), a claim link (sha256 only) and a builder key; `POST /api/v1/manage/claims/{token}` (a person, a verified root) re-parents it as an app in ONE transaction (`usage.MovePoolTx` moves its pool bytes, its keys hang on the claimer, the link spent); the `claimable_purge` job (hourly, `PeriodicJobStale`) schedules the unclaimed for the deletion runner | `TestClaimable_PartnerOnlyAndExpires`, `_ClaimMovesTheDataUnderTheClaimer` |
| **PR E** the dashboard | `/dashboard/agents` (grants, their person, renew, revoke; pending approvals with the authenticator code, deny), `/dashboard/apps` (make — the key shown once — list, delete), `/dashboard/claim/{token}`; every write through `dashboardAction` | `TestDashboard_AgentsAndAppsPages`, `TestDashboardRoutes_EveryWriteNamesItsAction` |
| **PR F** this note | STATUS, both plan rows, CLAUDE.md, `docs/API.md` § Agents + § Apps, `llms.txt`, Prompt 56 in both editions | — |

## Step 0 answers

- **Was RP88 Part 2 on main?** Yes: #1098, `78056874`, deployed.
- **`CREATE INDEX CONCURRENTLY` in 122?** No: a file of more than one statement runs as one batch, which `CONCURRENTLY` refuses (069 is a one-statement file). The three tables are small on prod; the deploy's `lock_timeout` bounds the wait.
- **The `@stored.ge` collision.** `internal/auth/helpers_test.go` deletes every `@stored.ge` user and tenant at the start of each auth test, and packages share one database in parallel: a platform fixture's owner vanished mid-test on this session's runs. The platform fixture is `@platform.example` now (RB13-7 for the rest).

## Decisions (D-STRIPE-74…)

- **D-STRIPE-74: the access token IS the grant's live key.** `/oauth/token` answers it as `access_token` and the sandbox key as `stored_test_token`; there is no refresh token — a grant is renewed by its person (30 days, never longer). A code is spent by its first exchange, right or wrong (RFC 6749 § 4.1.2).
- **D-STRIPE-75: exports, the erasure, members and management-key mints stay a person's own acts.** No approval lends them to an agent (403 `person_required`, WP-KEY-1). The approval actions of v1 are `bucket.delete` and `key.revoke` (a key the grant did not mint). Making anything Public is unreachable for any management key (the API has no public mode); turning versioning off, ACLs, Object Lock and retention are outside the agent S3 ceiling (`auth.AgentS3Permissions`).
- **D-STRIPE-76: an approval lives on the account.** A sandbox test key's asks are the account's, their resource prefixed `sandbox/`, so an approval on one never spends on the other.
- **D-STRIPE-77: an agent's audit row names the grant and the member.** `principal = agent:<grant>`, `performed_by` = the granting member (the person accountable); the request log's key column stays `mk:<id>`.
- **D-STRIPE-78: an S3 key an agent mints is its grant's.** `api_keys.agent_grant_id`: the grant's caps, its expiry clamped to the grant's, revoked with the grant, and never Private over S3 (one more column in `LookupCredential`'s SELECT, no extra query).
- **D-STRIPE-79: one tool implementation.** The tools are the Worker's; `stored mcp` bridges stdio to it. An object tool mints an S3 key on that one bucket for that one call and revokes it before it answers.
- **D-STRIPE-80: an app is made by an owner or an admin** (`ActionChildWrite`). A management key is a developer on `/api/v1/manage`, so neither a server's key nor an agent makes apps in v1 (open question 2).
- **D-STRIPE-81: an app's backend key is `apps` + `manage:write`:** its end users on the platform routes, its own buckets and keys on the management routes.
- **D-STRIPE-82: a provisional tenant is a managed child of its partner.** The partner's pool holds its bytes and the partner is accountable for them; its builder key resolves an actor (the partner's owner); the purge is "schedule the child now" for the deletion runner.
- **D-STRIPE-83: a claim needs a verified account, not a paying one** — it is the funnel. The claimed app keeps its 1 GiB child cap inside the claimer's pool; it counts in the claimer's apps, but the 25 is checked only when an app is made.

## Residuals (RB13-n)

- **RB13-1:** at the edge only the admit's snapshot is checked: N parallel edge reads each under the remaining may pass a cap by their sum (the count stays at the commit, D-STRIPE-72). The origin pre-charges.
- **RB13-2:** the MCP object tools' one-call keys make a grant's per-key storage cap a per-call bound; the tenant's and the pool's caps hold.
- **RB13-3:** the optional agent workspace (a managed child with its own cap) is not built.
- **RB13-4:** `/oauth/register` has no rate limit and registered clients are never collected.
- **RB13-5:** a partner that is also a platform sees its provisional children in its platform list.
- **RB13-6:** `usage.MovePoolTx` seeds a counting floor row the claimer's root has and the tenant lacks from its head rows only; retained versions and uploads on it are the reconcile's (the drift gauges show them).
- **RB13-7:** `internal/auth/helpers_test.go`'s `@stored.ge` purge still reaches every other fixture on that domain.
- **RB13-8:** no nav link to Agents or Apps: the consent, the `approval_url` and the URLs reach them.
- **RB13-9:** the `POST /keys` answer of an agent's key shows the expiry asked for, not the clamped one (the row has the clamp).

## Open questions

1. **F-41, F-42, F-43 are still Open** in the plan: built to their recommendations, dark behind `agent_grants` and `apps`. The owner decides before either flag is turned on for anyone but tenant zero.
2. **May an agent make an app?** `docs/ACCOUNTS.md` says an app may be built "by the builder's agent"; v1 refuses it (D-STRIPE-80).
3. `standard_sealed_default` per tenant: still the owner's (WP-STRIPE-5's).

## The prod proof

**Owed.** It needs the owner: a TOTP code (the consent, an approval, a management-key mint — and WP-KEY-1's owed proof, RB12-7, in the same sitting), and the `mcp.stored.cloud` route deployed (`cd edge/mcp && npx -y wrangler@4.151.3 deploy` on the production account; the DNS record is the zone's). The steps:
1. Flags on for tenant zero only: `agent_grants`, `apps` (admin API). Deploy `edge/mcp`.
2. Claude (claude.ai → Settings → Connectors → `https://mcp.stored.cloud/mcp`): register, consent on the dashboard with the TOTP code, `create_bucket`, `put_object`, then `delete_bucket` → `approval_required`; the owner approves on `/dashboard/agents` with a code; the call again succeeds.
3. `POST /api/v1/manage/apps {"label": "proof"}` as the owner; with the app's key `POST /api/v1/platform/tenants {"label": "user 1"}`, its key, one PutObject; the pool counter of tenant zero moves.
4. An operator grants a throwaway root `partner`; its partner key makes a claimable; the owner opens the claim link and keeps it.
5. WP-KEY-1's proof (`docs/reviews/WP-STRIPE-6.md` § The prod proof).

## Post-merge review

**RP90** reads PRs 0 and A–F against D-STRIPE-74…83, RB13-1…9 and the two plan rows.
