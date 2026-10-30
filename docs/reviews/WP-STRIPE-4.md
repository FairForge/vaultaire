# WP-STRIPE-4 — teams: members, roles and invitations on a tenant (Stage 1a Prompt 52), after Part 0 = RP82's fixes

*Plan § WP-STRIPE, the **WP-TEAM-1** row. It asks for:*
- *members on a tenant (`tenant_members`: tenant, user, role `owner` / `admin` / `developer` / `viewer`, invited by, accepted at);*
- *invitations by e-mail (the existing sender, a signed link, 7 days), the dashboard's Members page and the management API's `members` routes;*
- *the session carrying the active tenant and the role, every write authorised by role, the audit row naming the member;*
- *a user in several tenants who switches in the dashboard, with the one-user-one-tenant registration still working.*

*Part 0 is the RP82 review's findings (`docs/reviews/WP-STRIPE-3-REVIEW.md`, #1062) and D-STRIPE-31.*

*Prompt: `docs/reviews/WP-STRIPE-PROMPTS.md` § Stage 1a Prompt 52 (the session ran the Opus edition). Session 2026-10-29. **Migration 118** (`118_members.sql`, PR A; WP-VAULT-2's `118_vault_parity_scrub.sql` landed the same day under the same number — both apply in filename order, like the two 004 files). Next: 120 (119 is the engine lane's Prompt 52E, #1064). The post-merge review is RP84.*

Contract lines cited (`docs/ARCHITECTURE_CONTRACT.md`):
- **4** (keys only through the one hierarchy): untouched. A member acts on a tenant's keys; the keys stay the tenant's, written through `persistAPIKey` as before. `api_keys.user_id` now names the member who made a key, never its owner.
- **5** (convergence within a space only): untouched. A team is ONE tenant (`docs/ACCOUNTS.md`), so one space; a person in two tenants acts on two spaces, never one merged.
- **6** (the chunker is frozen): untouched.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1067** Part 0 | **D-STRIPE-31 / RB9-2 / RP82-2:** `usage.PoolRoot` (the one resolver, `COALESCE(parent_tenant_id, id)`); `EgressAllowance` answers the root's allowance (`Root` names it), `MonthEgressBytes` the pool's month; the egress meter keys its counter, decision, flag read and token bucket by the root while each response's unrecorded bytes stay its own tenant's (`egressTenant.member`); the alert ladder's candidates are pool roots.<br>**RP82-1:** a child gets counting floor rows (limit 0) at creation, by `SetHouseTx` and, when missing, by `ReserveTx` before the write; `ReserveTx` and a guarded `ApplyLedgerTx` check the owner's floor limit against the pool's sum; lock order owner total → child total → owner floor → child floor.<br>**RP82-5:** `GenerateChildKey`'s unused read dropped. | `TestPool_AChildsDownloadsSpendThePoolsAllowance`, `TestEgress_ZeroAllowanceIsNotUnlimitedForAChild`, `TestPool_ChildFloorsDrawFromTheOwnersFloors` (usage + S3), `TestPool_SetHouseGivesChildrenCountingFloors`, `TestLedger_LockOrder_PoolOwnerFirst_Structure` |
| **#1068** PR A, members and roles | Migration 118: `tenant_members` (one owner per tenant; the backfill: every user the owner of the tenant it registered, by e-mail) and `tenant_invitations` (`kind member|family`).<br>Registration writes the owner member (five rows, one transaction).<br>`internal/auth/members.go`: `Role`, `Can(role, action)` (the one table), `MemberRole`.<br>`requireJWT` reads the role per request; `requireMember` (403 `not_a_member`) and `requireRole(action)` (403 `role_forbidden`) on `/api/v1/manage` and `/api/v1/platform`.<br>The dashboard: `ResolveMember` + `dashboardAction` on every `/dashboard` write.<br>Erasure by tenant and by user; a `members` export section. | `TestRegister_CreatesTheOwnerMember`, `TestMembers_ViewerCannotCreateAKey`, `TestMembers_OnlyTheOwnerDeletesTheAccount`, `TestMembers_APlatformsMemberActsOnItsChildren`, `TestMembers_ANonMemberIsRefused`, `TestManagementRoutes_EveryWriteNamesItsAction`, `TestDashboardRoutes_EveryWriteNamesItsAction`, `TestCan_TheActionTable`, `TestMemberRole_RowThenRegistrationElseNotMember`, the schema walks |
| **#1069** PR B, invitations | `internal/auth/invitations.go` (create, accept, register through the link, list, revoke, set role, remove); `/api/v1/manage/members[/{user}]`, `/invitations/{id}`, `/invitations/{token}/accept`; the e-mail; the Members page; the public `/auth/invitations/{token}` page; OpenAPI entries. | `TestMembers_InvitationExpiresIn7Days`, `TestMembers_InvitationIsTheAddressees`, `TestMembers_TheOwnerIsPermanent`, `TestRegisterInvitedUser_JoinsWithoutATenantOfItsOwn` |
| **#1071** PR C, several tenants | `POST /auth/login` answers `tenants: [{id, role, name}]` and takes `tenant_id`; `auth.Memberships` / `ActiveTenant`; dashboard sessions start on the active tenant (password, MFA, OAuth); the switcher (`POST /dashboard/switch-tenant`, the Members page).<br>Keys tenant-keyed: `ListAPIKeys`, `RevokeAPIKey`, `RotateAPIKey`, `SetAPIKeyExpiration`, `GetOwnedAPIKey` and the free-tier cap by tenant; `GenerateAPIKeyForTenant` / `GenerateTestKeyForTenant`; `notChildKeySQL` removed.<br>The role check on `/api/v1/user`, `/api/v1/webhooks`, `/api/v1/events`, `/api/v1/sts` too.<br>`audit.WithTenant`: every row of a tenant surface names the member (the actor) and the active tenant. | `TestMembers_AUserInTwoTenantsSeesTheRightOne`, `TestMembers_AuditRowNamesTheMember` |

## Decisions (D-STRIPE-32…)

- **D-STRIPE-32: the role is read from `tenant_members` on every request; the JWT's `role` claim is never trusted.** The plan says "the JWT carries the active tenant and the role", and it does, for the client to read. The server's answer comes from the row, so a removed member loses the tenant at once and a changed role applies at once. A token minted before 118 carries no role and works exactly as the row says.
- **D-STRIPE-33: no member row → the tenant's registering user (by e-mail) is its owner; anyone else is no member.** The previous build keeps registering accounts during the deploy overlap without a member row, and the migration's backfill runs again on every deploy (it is idempotent), so the next deploy writes their rows. The fallback is the same rule as the backfill.
- **D-STRIPE-34: the action table.** `viewer`: read. `developer`: + buckets, keys, webhooks, the sandbox reset, a platform child's keys and STS. `admin`: + members (never the owner) and a platform's children (create, erase). `owner`: + delete the account, export it, billing. The export is the owner's: it is every member's personal data and the account's whole content. `webhook.write` is an action the prompt did not name (D-STRIPE-35).
- **D-STRIPE-35 (scope): the role check covers every tenant surface, not only `/api/v1/manage` and `/api/v1/platform`.** `DELETE /api/v1/user` is the same account-deletion handler as `DELETE /api/v1/manage/account`. Without the check, a viewer could have scheduled the erasure of the active tenant through it. `/api/v1/user`'s key writes, `/api/v1/webhooks` and `/api/v1/sts` are the same kind of write. All of them now take `requireMember` and the action. The router walk covers the five groups.
- **D-STRIPE-36: an invitation token is 32 random bytes, and only its SHA-256 is stored.** It is not the `VERIFY_SECRET` HMAC shape the chain edition mentions. Single use needs a row anyway, and a stored hash of a random token cannot be forged from a leaked key.
- **D-STRIPE-37: registering through the link is open while signups are closed.** It adds a person to an account that exists, with no storage of their own: a sign-in with **no tenant** (the five-table rule's tenant step is the inviting tenant's member row). The link verifies the address. An address that already has an account never types its password on the link page. It signs in (with its MFA) and accepts at `/dashboard/invitations/{token}`.
- **D-STRIPE-38: the public page is `/auth/invitations/{token}`, not `/invitations/{token}`.** A new first segment would be a new reserved bucket name (#964's one-way door, `TestNodeReservedSegments_AreTheRoutersReservedNames`). `/auth/` is reserved already and disallowed in `robots.txt`.
- **D-STRIPE-39: the audit row's tenant is stamped by `requireMember`, never by `requireJWT`.** The operator routes also pass through `requireJWT`. Their rows are about other tenants, and stamping the operator's own tenant on them would erase them with it (the R10-3d rule).
- **D-STRIPE-40: keys are the tenant's.** List, revoke, rotate, expire and the STS parent are looked up by tenant, and the free-tier cap counts the tenant's keys whoever made them. The key row's `user_id` is the member who made it. `notChildKeySQL` is gone: a child's keys are the child's tenant's and appear in no member's list. The boot load now holds them in memory too. No lookup trusts that map: `LookupCredential` reads the row.
- **D-STRIPE-41: a pool's egress decision reads the root's `egress_throttle` row.** The pool has one position and one token bucket. A per-child exemption would let one child spend everyone's allowance.
- **D-STRIPE-42: the switcher is on the Members page and rotates the session.** It creates a new session on the target tenant and deletes the old token. There is no global nav switcher (`sessionData` has no database, and the base layout renders on every page); the nav links to Members.

## Residuals (RB10-n)

- **RB10-1:** a member with no tenant of its own cannot delete its sign-in. Account deletion is the owner's action on the active tenant, and leaving means an admin removes you. Self-leave and the erasure of a tenant-less sign-in go to WP-ACCT-1.
- **RB10-2:** `requireJWT` reads `tenant_members` on every request (one indexed lookup, two on the e-mail fallback). No cache, by design (D-STRIPE-32), until a measurement says otherwise.
- **RB10-3:** a failed invitation e-mail leaves the row. The page says so; the admin revokes and invites again. There is no resend route.
- **RB10-4:** the dashboard's other pages still show the active tenant's data to every member (as they did for its one user): billing is read-only for a non-owner (its writes are the owner's), and the settings page's personal sections are the signed-in person's.
- **RB10-5:** the egress meter's root cache never expires. A tenant's parent never changes; an erased tenant's entry is harmless and is dropped when the map fills.
- **RB10-6:** the JSON sign-in's `tenants` list names each tenant's `name` (the registration's company). A child or a sandbox is never listed (neither has a member).

## The prod proof

**Owed — needs the owner (TOTP and the mailbox).** Code and CI only in this session. The steps, after PRs A–C deploy:
1. Sign in to the dashboard as tenant zero's owner (TOTP). Members → invite a second address of the owner's as `viewer`.
2. Open the e-mailed link (`/auth/invitations/<token>`). For a new address, choose a password; for an existing account, sign in and accept from the dashboard.
3. As the invitee: the buckets list shows tenant zero's buckets; creating a key or a bucket is refused (403). The JSON sign-in answers `tenants` with tenant zero as `viewer`.
4. `/admin/audit`: `member.invited` performed by the owner, `member.joined` by the invitee, both on tenant zero.

## Open questions

- None blocking. RB10-1 (self-leave, a tenant-less sign-in's erasure) is for WP-ACCT-1 to decide.

## Post-merge review

**RP84, 2026-10-30 (`docs/reviews/WP-STRIPE-4-REVIEW.md`): HOLDS at P2**, every finding latent on prod. Prompt 53's Part 0 is filled with them, in this order:
- RP84-1 (P2): `api_keys` is erased by its maker only. A team's erasure leaves its members' keys live; a member's own erasure takes the team's keys.
- RP84-2 (P3): a JWT on no tenant is an owner on the API.
- RP84-3 (P3): the invitation subject's header injection, and no invitation cap.
- RP84-4 (P3): a removed member's keys are not named at removal.
- RP84-5/6 (P4).
- **The prod proof waits for that Part 0.**
