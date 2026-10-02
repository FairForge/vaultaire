# WP-R10-3d — an operator's erasure keeps the operator audit trail

**Worker session, 2026-10-02. PR #553, branch `wp/R10-3d-operator-audit-trail`, from `main` @ #552.**
Closes WP-R10-3 post-merge finding PM-4 (P2). No migration. Nothing to deploy by hand.

## The problem

`account.EraseRows` deleted `audit_logs` / `audit_logs_archive` rows
`WHERE user_id = $1 OR performed_by = $1`. For a customer that is the policy ("your own
entries are removed with your account"). For an **admin** it is also the record of everything
they did to other people's accounts: the dashboard's admin handlers write the admin as *both*
`user_id` and `performed_by`, with the subject tenant in `tenant_id`
(`dashboard/handlers/tenants.go`: suspend, enable, quota, bandwidth, tier), and the service-level
actions (`admin.primary_swapped`, `admin.<job>` triggers, `admin.quota_reconcile`, the waitlist
and audit exports, a global `flag.set`) carry the admin and no tenant at all. An admin account —
or whoever holds it — could schedule its own deletion and 30 days later the trail of its
suspensions, quota edits and primary swaps was gone. The same rows are *kept* (scrubbed) when
the subject tenant is erased; they were lost only when the actor was.

## Decision: (a) keep and de-identify — not (b) refuse deletion for admins

(b) would have been four lines in `Schedule`, and it does not fix the bug: "another operator
removes the account" ends in the same `EraseRows`, with the same `DELETE`. There is also one
operator today, so (b) means an operator's account can never be erased — and an operator is a
data subject too. (a) is the rule the erasure needed whoever starts it.

## What was built (`internal/account/erase.go`)

`operatorTrailScrub(table)` — one `UPDATE` per audit table, placed in `Deleted` immediately
**before** the `DELETE` of the user's own entries, so the rows it de-identifies no longer match
that `DELETE`. Of the rows that name the user as subject or actor, a row is **not** the user's
own entry when it is

1. about another tenant — `tenant_id` set and not theirs;
2. an administrative action on the service — `event_type = 'admin'`; or
3. an action with no tenant on a subject that is not the user (`tenant_id IS NULL` and
   `user_id` is not theirs: a global flag flip, anything done to another user).

Those rows stay with everything that identifies the erased person removed:

| Column | After |
|---|---|
| `user_id`, `performed_by` | `NULL` where they were the erased user; another user's id is left alone |
| `ip`, `user_agent` | `NULL` when the erased user made the request (`performed_by` = them, or no actor on record); kept when a *surviving* operator made it — the address is that operator's |
| `metadata` | every top-level string value equal (case-insensitive) to the user's id or e-mail is dropped — `admin.primary_swapped` writes `"admin": <e-mail>` — and `"actor_erased": true` is added, because a `NULL` `performed_by` alone reads as "the system did it" |
| everything else (`action`, `resource`, `tenant_id`, `timestamp`, `result`, `error_msg`) | unchanged |

Everything else that names the user — sign-ins, their own keys, their own account events, on
their own tenant or none — falls through to the `DELETE`, as before.

`Rule` gained a third key kind, `ByAccount` (`$1` user id, `$2` tenant id, `$3` e-mail);
`keyArg` became `ruleArgs` and returns the argument list. A user with no tenant row binds
`''`: every tenant-keyed row they acted on is then someone else's and is kept de-identified
(nothing personal is left in it either way).

**The FK on `performed_by`:** there is none. `audit_logs.user_id` and `performed_by` are bare
`UUID` columns (migration 008; `\d audit_logs` shows only indexes) and `audit_logs_archive` is
`LIKE … INCLUDING ALL`, which does not copy foreign keys. Nothing blocks the `DELETE FROM users`
and nothing cascades into the kept rows.

## The two things that had to keep holding

- **Schema walk** (`TestErasedTablesCoverSchema`): unchanged and green — `audit_logs` and
  `audit_logs_archive` were already in both `Deleted` and `Kept`; the new statements prepare
  against the migrated schema like every other rule.
- **The privacy policy sentence** (`templates/legal/privacy.html`, Retention): "Sign-in and
  administrative audit records — retained for the life of the service as our security record;
  your own entries are removed with your account." Read against the code: for a customer
  nothing changed — no customer request writes a row that matches any of the three clauses
  (every non-admin `audit.Record` call site sets the caller's own `UserID` or `TenantID`;
  checked by grep, listed below). For an operator, their own entries (sign-ins, keys, account
  events) are removed, and what stays is the record of what happened to *other* accounts and to
  the service, with no id, address, user agent or e-mail of the erased person in it. That is
  the "security record" half of the same sentence. **The policy text is not changed.**

## Tests

`TestEraseRows_OperatorErasureKeepsTheOperatorTrail` (`erase_db_test.go`) — the throwaway from
the post-merge review, kept: an admin past due, with

- `admin.tenant_suspended` on another tenant (admin as `user_id` and `performed_by`),
- `key.revoked` on another user of another tenant (admin as `performed_by` only),
- `admin.primary_swapped` with no tenant and `"admin": <E-MAIL IN CAPITALS>` in the metadata,
- a global `flag.set` (no tenant, no subject),
- `admin.tenant_quota_set` in `audit_logs_archive` with the admin's id in the metadata,
- a row another, surviving operator wrote *about* the admin on another tenant,
- the admin's own sign-ins (one with no tenant, one archived) and own `key.created`,
- and another operator's row about the same tenant (the neighbour).

Asserts: no row in either table names the erased user in `user_id`, `performed_by` or the
metadata; the five operator rows survive with `user_id`/`performed_by`/`ip`/`user_agent` null,
`actor_erased` set and the rest of the metadata intact; the other user's id on `key.revoked`
stays; the surviving operator's row keeps that operator's id and IP; the three own entries are
gone; the neighbour is untouched.

**Red first, seen:** written before the code and run on `main` @ #552 — all five operator
rows "must survive the operator's erasure" failed (0 rows), which is PM-4 exactly.

`TestEraseRows_RemovesEveryListedTableKeepsTheLedgers` (a customer's erasure: the user's row
gone, the tenant-keyed operator row kept and scrubbed) is unchanged and green.

`go test -race ./...` (every package, the migrated `vaultaire_test`): green. `make lint`: 0 issues.
`make gosec`: 0 issues.

## Adversarial pass — what it found

1. **The first cut nulled `ip` on every kept row.** A row where the erased user is only the
   *subject* and a surviving operator is the actor would have lost that operator's address —
   the trail of someone who is not being erased. `ip` / `user_agent` are now nulled only when
   the erased user (or nobody on record) made the request. Test row added.
2. **The e-mail in the metadata.** `performed_by = NULL` alone would have left
   `"admin": "<e-mail>"` on every primary-swap row. Found by reading every `EventType: "admin"`
   call site's `Metadata`. The scrub drops any top-level value equal to the id or the e-mail,
   whatever the key is called. Nested values are not walked: of the 36 `audit.Entry` call
   sites the only nested metadata is the erasure runner's own (`account.erased`: per-table and
   per-backend counts), and the two that carry an identity (`"admin"` on the primary swap,
   `"email"` on the sign-in rows) carry it at the top level.
3. **A non-object `metadata`** (an array, a scalar) would make `jsonb_each` raise and roll the
   whole erase back. Guarded with `jsonb_typeof(metadata) = 'object'`; `NULL` and non-objects
   become `{"actor_erased": true}`.
4. **Attacker: a customer who wants rows to survive, or an operator who wants them gone.**
   The classification is by the shape of the row, not by `users.role` at erase time — an admin
   demoted to `user` before scheduling the deletion leaves the same trail. A customer cannot
   write a row of the three shapes: the call sites without `EventType: "admin"` are
   `auth.*` (own `UserID`), `key.*` (own `UserID` + own tenant), `account.*`, `sts.*`,
   `webhook.*` (own tenant), `mfa.*` (own), and `flag.*`, whose only callers are the admin API
   and the admin dashboard.
5. **The other entry point.** Nothing else deletes audit rows: `grep 'FROM audit_logs'` finds
   the writer, the reader and `erase.go`. The retention job keeps `audit_logs` forever.
6. **"Kept" against the statement that runs first.** The scrub must precede the `DELETE`:
   with the two `audit_logs` rules swapped the test fails on all three rows of that table
   (run and seen — the rows are gone before the `UPDATE` looks). The comment on the rule says
   so, and both are adjacent in `Deleted`.
7. **Audit export filter values.** `admin.audit_exported` stores the filter the admin typed
   (`actor`, `user`, `ip`). A filter value equal to their own id or e-mail is dropped by the
   scrub; an IP address typed as a filter is kept — it is a search term, not the request's
   address, and the row does not say whose it is.

## Prod (read-only, 2026-10-02)

1 admin, 4 users, all `active`; **0** accounts pending deletion. `audit_logs`: 11 rows,
`audit_logs_archive`: 0. Of the 9 rows that name the admin, the rule keeps **1** (an
`event_type = 'admin'` row) and removes **8** (`auth` sign-ins). Nothing was changed.

## Not done

- The `/admin/audit` page shows an erased actor as an empty actor cell with
  `actor_erased` in the details; no dedicated label.
- Whether the *last* admin may delete their own account at all is not decided here — nothing
  stops it (D-16: the grace period blocks nothing). With one operator that is a 30-day
  cancellable mistake, not a data loss; say if it should be refused.

## [YOU]

Nothing.

## Post-merge review (plan driver, 2026-10-02)

Read: `operatorTrailScrub` and its place in `Deleted` (before the `DELETE` it protects rows
from). **No finding.** The classification by the shape of the row rather than by `users.role`
at erase time is the right call: a demoted admin leaves the same trail.

Noted: the scrub runs inside the erase transaction, so a statement error there rolls the erase
back and the tenant is deferred — visible (`AccountDeletionDeferred`), not silent. The
`jsonb_typeof` guard covers the one input that could raise.
