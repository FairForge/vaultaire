# WP-VAULT-1 — the Vault layer (queue item 11), after a warm-up from the #584 review

**Worker session, 2026-10-05. Branch per part, one PR each, from `main` @ #585.** Plan queue item 11;
the measurements are `bench-results/VAULTAIRE-PATH-2026-10-04.md` §15–§16. Tests on the private test
database `vaultaire_test_vault`, everything under `-race`; `make lint` 0, `make gosec` 0 before each PR.

| Part | What | PR |
|---|---|---|
| 0 | warm-up: the primary cannot be expired; an allowlist of `0.0.0.0/0` / `::/0` is refused | _below_ |
| 1 | Geyser `Get` as parallel ranges | _pending_ |
| 2 | the parity second copy for `vault`-floor objects (flag `vault_parity`, migration 077) | _pending_ |
| 3 | `cmd/tools/geyser-restore-probe` | _pending_ |
| 4 | per-PUT deadline + one retry in the fixed-bucket driver | _pending_ |

## Part 0 — warm-up (post-merge review of #584)

### The bugs, seen first

**(a) The primary pair could be expired.** `SetAPIKeyExpiration` checked `RevokedAt` and not
`IsPrimary`, so `POST /api/v1/user/apikeys/{id}/expire` on the primary answered 200 and wrote
`expires_at` on the one key the account cannot replace from the outside: the export download link,
the presigned-URL route and the credentials page all mint from it (WP-R5-14), and rotation copied
`ExpiresAt` onto the successor — so once expired, every rotation handed out another expired primary.
`TestUserKeyAPI_DaysMustBeAtLeastOne` had been *asserting* the 200 on the primary. Prod, read-only
before the fix: 0 primary rows carry an expiry, 0 are expired.

**(b) `0.0.0.0/0` / `::/0` were accepted.** `ValidateIPAllowlist` took any CIDR `net.ParseCIDR`
accepts; an allowlist of `0.0.0.0/0` is stored, shown as a restriction, and restricts nothing. The STS
route had the same hole when the parent key has no allowlist (`intersectIPRestrict` returns the
requested list as is; with a parent allowlist a `/0` already failed the overlap check). Prod: 0 live
keys with such an entry.

### What was built

- `auth.ErrPrimaryKeyExpire` (the `ErrPrimaryKeyRevoke` shape): `SetAPIKeyExpiration` refuses the
  primary; the user API answers **409 `primary_key`** ("cannot be given an expiry; rotate it instead").
  Rotating first changes nothing — the successor is the primary and refuses too.
- `RotateAPIKey` refuses an **expired** key, primary or scoped, with `auth.ErrKeyExpired`: the
  successor inherits the expiry and would be born dead. User API **409 `key_expired`**; dashboard
  flash "create a new key instead". A refused rotation revokes nothing and mints nothing.
- `auth.ErrUnrestrictedIPAllowlist`: any CIDR with prefix length 0 (`0.0.0.0/0`, `::/0`, `1.2.3.4/0`),
  alone or hidden among valid entries. Management API **400 `unrestricted_ip_allowlist`**
  (`param: ip_allowlist`, "leave ip_allowlist empty to allow every address"); dashboard form sentence
  with the entry; STS `ip_restrict` → 400 `scope_error` ("restricts nothing; omit ip_restrict to
  inherit the parent key's allowlist"). OpenAPI text updated for the four routes.

### Same logic on other entry points (audit)

Every production caller of `GenerateAPIKey` (dashboard form, user API, management API) goes through
`KeyCreateOptions.Validate` → `ValidateIPAllowlist`; `RotateAPIKey` has two callers (dashboard, user
API) and `SetAPIKeyExpiration` one (user API — neither the management API nor the dashboard has an
expiry route; a future one inherits the service guard). The only other allowlist writer is the STS
mint (`intersectIPRestrict`), covered. `0.0.0.0/1` + `128.0.0.0/1` together also admit everything and
are not refused — a deliberate two-entry construction, not a mistake (kept out, noted).

### Adversarial pass

| # | case | result |
|---|---|---|
| 1 | expire the primary, then rotate, then expire the successor | 409 `primary_key` both times (`TestUserKeyAPI_PrimaryCannotBeExpired`) |
| 2 | a primary row that already carries a past expiry (stamped straight into the cache) | rotation refused with `ErrKeyExpired`, the row stays live, no successor (`TestRotateAPIKey_ExpiredKeyIsRefused`) |
| 3 | `::/0` hidden in `["203.0.113.7","10.0.0.0/8","::/0","2001:db8::/32"]` | refused on the service, 400 on the management API, sentence on the dashboard, no key created |
| 4 | `1.2.3.4/0`, ` 0.0.0.0/0 ` (padded) | refused — the check is on the parsed prefix length |
| 5 | `0.0.0.0/1`, `128.0.0.0/1`, `::/1` | accepted (real networks) |
| 6 | STS `ip_restrict: ["203.0.113.7","0.0.0.0/0"]` from a parent with no allowlist | 400 `scope_error`, no `sts_tokens` row |

### Tests

`internal/auth/primary_key_expiry_test.go`, `key_hygiene_test.go` (+2), `internal/api/
user_api_keys_hygiene_test.go` (+3; the pre-existing days test now expires a scoped key),
`credential_lifecycle_api_test.go` (+1, DB), `internal/dashboard/handlers/apikeys_test.go` (+2).

### Decisions

- **D-VAULT-0a** An expired key is not rotated (refused), rather than rotated into a successor without
  the expiry. A scoped key that expired is replaced by a new key; the primary can no longer reach that
  state. If a primary row with a past expiry ever exists (none on prod), the operator clears
  `expires_at` on the row — recorded here so nobody adds a "rotate clears it" path by accident.
- **D-VAULT-0b** `0.0.0.0/0` / `::/0` are a 400, not a warning. [YOU] Isaac: reverse to
  "allowed with a warning" if a customer asks; the check is one `ones == 0` branch in
  `ValidateIPAllowlist` and one in `intersectIPRestrict`.

### Not done

- `docs/API.md` line 43 ("a revoked key answers `AccessDenied`" → `InvalidAccessKeyId` since
  WP-R5-14): still one word for the next docs pass, as #584 noted.
