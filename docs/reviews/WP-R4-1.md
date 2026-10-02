# WP-R4-1 — the GOVERNANCE bypass is a permission

**Worker session, 2026-10-02. Branch `wp/r4-1-governance-bypass-permission`.** Closes R4-09,
R11-21 and the "not done, deliberately" row of R12. No migration.

## The problem

`isObjectLockBypass(r)` was the header alone:

```go
func isObjectLockBypass(r *http.Request) bool {
	v := r.Header.Get("x-amz-bypass-governance-retention")
	b, _ := strconv.ParseBool(v)
	return b
}
```

Any key that could delete could bypass GOVERNANCE retention by sending
`x-amz-bypass-governance-retention: true`. AWS requires `s3:BypassGovernanceRetention`.
GOVERNANCE is "protected against most users" only if the bypass is a privilege — a backup
tool's key, the kind that gets stolen, could erase what the retention was there to keep.

The key's scope was a local of `handleS3Request`: the lock check, several calls deeper, had
no way to ask whose request it was.

## What was built

**Three conditions, one function.** `isObjectLockBypass(r)` (`internal/api/s3_lock.go`) is true
only when:

1. the request asks — `x-amz-bypass-governance-retention: true`;
2. **the ask is covered by the signature** (`bypassRequested`) — see "Presigned URLs" below;
3. **the key may** — `auth.KeyScope.CanBypassGovernanceRetention()` on the scope
   `handleS3Request` put in the request context:
   - `*` (the tenant's primary key; a key created without a permission list) → yes;
   - a scoped key → only if it lists `BypassGovernanceRetention`;
   - an STS token → only if it lists it explicitly (`*` on a token does not include it);
   - **no scope in the context → no.** A handler reached without the authenticator fails closed.

`BypassGovernanceRetention` is the one entry of `auth.ValidPermissions` that is not an S3
operation. It grants nothing by itself: the key still needs `DeleteObject` to delete.

**Every caller** goes through that function, so none could be missed: single DELETE (plain,
`?versionId`, the delete-marker branch — one check before all three), `DeleteObjects` per key,
PutObjectRetention shortening, overwrite by PUT, CopyObject and CompleteMultipartUpload.
`background_put.go` passes `false` (system writes never bypass). There is no object delete on
the management API, the user API or the dashboard — S3 is the only entry point.

**A refusal says why.** `AccessDenied … Object is protected by Object Lock.` gains
`This key does not have the BypassGovernanceRetention permission.` when the bypass was asked
for and the key may not, or `The x-amz-bypass-governance-retention header was not covered by
the request signature and was ignored.` Nothing is appended for COMPLIANCE or a legal hold.

**A header on an object that is not retained changes nothing**: a key that may not bypass
still deletes it (clients that always send the header keep working).

### STS tokens only narrow

`GenerateSTSToken` already intersected the request with the parent — but an unscoped request
*copies the parent's list*, and a parent of `*` hands the requested list through. So:

| Parent | Request | Token bypasses? |
|---|---|---|
| scoped key without the permission | `DeleteObject`, `BypassGovernanceRetention` | no — it cannot gain it |
| scoped key with it | `DeleteObject`, `BypassGovernanceRetention` | yes |
| scoped key with it | `DeleteObject` | no |
| scoped key with it | *(unscoped)* | no — the operations are copied, the bypass is not |
| the account (`*`) | *(unscoped)* → token `*` | no |
| the account (`*`) | `*` | no |
| the account (`*`) | `DeleteObject`, `BypassGovernanceRetention` | yes |

The permission stays on a token only when the request names it **and** the parent may bypass;
`KeyScope.Temporary` (set on both lookups, header auth and presign) makes a token's `*` not
count. Tokens live at most 12 hours; prod has none that predate this.

### Presigned URLs carry the key's scope — and the signer's ask

The scope comes from the same key lookup, so the rule is the same. What needed care is *who
is asking*: the holder of a presigned URL is not its signer.

- The AWS SDKs hoist `x-amz-*` headers into the **query** when they presign. Every query
  parameter is in the canonical query, so the flag there is the signer's: honoured (it was
  ignored before — a presigned bypass delete never worked). Adding it to a URL breaks the
  signature (`SignatureDoesNotMatch`).
- As a **header** it counts only when `X-Amz-SignedHeaders` lists it. A URL signed for `host`
  alone lets its holder send any header and the signature still verifies: before this WP a
  full-access key's plain presigned DELETE became a bypass delete in the hands of whoever
  had the URL. Reproduced red (`…the_holder_of_a_URL_cannot_add_the_bypass…`).
- The same rule on header-signed requests: the header counts only when `SignedHeaders` lists
  it (AWS refuses a request with an unsigned `x-amz-*` header; the SDKs and aws-cli sign them
  all — checked live).

### The three places a key is created

There is no key *edit* anywhere (keys are created, rotated, expired, revoked; rotation keeps
the permission list — tested).

- `POST /api/v1/manage/keys` and `POST /api/v1/user/apikeys` validate against
  `auth.ValidPermissions`: the permission is accepted and stored; OpenAPI describes it.
- The dashboard key form has an "Object Lock" group with the checkbox and one sentence:
  *"Lets this key delete, overwrite or shorten the retention of an object under GOVERNANCE
  retention by sending `x-amz-bypass-governance-retention: true` (it still needs DeleteObject
  or PutObject for the operation itself); a Full Access key already can, and no key bypasses
  COMPLIANCE retention or a legal hold."*

## COMPLIANCE is untouched

`checkObjectLock` returns for COMPLIANCE and for a legal hold before it looks at the bypass;
PutObjectRetention's COMPLIANCE branch never reads it. `TestComplianceRetention_NothingBypassesIt`
drives the most privileged caller there is (the primary key, the header, signed) through
DELETE on an unversioned and a versioned bucket, `DeleteObjects`, overwrite, shortening and a
mode change: all refused, the object, its bytes and its retention row unchanged. Checked by
making COMPLIANCE honour the bypass and watching it fail.

## Tests

`internal/api/s3_bypass_permission_test.go` — everything through `handleS3Request` with real
SigV4 (aws-sdk-go-v2 against an `httptest.Server`, `testMode` off, a real tenant, user and
`api_keys` rows), because what is under test is that the scope the authenticator found
reaches the lock check:

- `…ScopedKeyWithoutThePermissionIsRefused`, `…KeyWithThePermissionDeletes`,
  `…PrimaryKeyIsFullAccess`
- `…EveryCallerHonoursThePermission`: batch delete, delete marker, `?versionId`,
  PutObjectRetention shortening, overwrite by PUT / CopyObject / CompleteMultipartUpload —
  each refused without the permission (object, bytes and retention unchanged) and allowed with it
- `…STSTokensOnlyNarrow` (the seven rows above)
- `…PresignedURLCarriesTheKeysScope` (three cases), `…PresignedURLHolderCannotAddTheFlagToTheQuery`,
  `…UnsignedHeaderOnASignedRequestIsIgnored`, `…TheHeaderOnAnUnlockedObjectChangesNothing`
- `TestComplianceRetention_NothingBypassesIt`, `TestIsObjectLockBypass_NoScopeInTheContextIsNoBypass`
- `TestKeyAPIs_CreateAKeyWithTheBypassPermission` (user API, management API, rotation)

`internal/auth/bypass_permission_test.go` (the scope table, the permission grants no
operation, STS never inherits), `internal/dashboard/apikeys_template_test.go` (the served
form offers it, says what it does, and offers nothing the validator refuses),
`handlers/apikeys_test.go` (a key made by the form carries it).

**Red first:** the api tests were written against a compile-only stub (the context helpers)
and run: every refusal case failed with "an error is expected but got nil", the unsigned
header cases bypassed, the STS cases minted nothing or bypassed. Passing before and after by
design: the primary key, the key with the permission, COMPLIANCE (mutation-checked) and the
signed-header rule on presigned URLs (mutation-checked: drop the `X-Amz-SignedHeaders` check
and the URL-holder case fails).

Two existing tests called the handlers directly with the header and no scope
(`TestObjectLock_GovernanceBypass`, `TestObjectRetention_GovernanceShortenNeedsBypass`): they
failed closed, as designed. The lock fixture now builds the context `handleS3Request` builds
for the primary key (`asPrimaryKey`).

`go test -race ./...`, `make lint`, `make gosec`: green.

## Live proof (the branch binary, aws-cli 2, a private database)

```
## 1. primary key: bucket with Object Lock, two objects under GOVERNANCE, one under COMPLIANCE
## 2. scoped key with DeleteObject only, with the bypass header
An error occurred (AccessDenied) when calling the DeleteObject operation: Access denied.
  Object is protected by Object Lock. This key does not have the BypassGovernanceRetention permission.
## 3. the object is still there (primary key)
22
## 4. key with DeleteObject + BypassGovernanceRetention: without the header, then with it
An error occurred (AccessDenied) when calling the DeleteObject operation: Access denied.
  Object is protected by Object Lock.
exit=0                                   # aws s3api delete-object --bypass-governance-retention
An error occurred (404) when calling the HeadObject operation: Not Found
## 5. COMPLIANCE: the same key, then the primary key, with the header
An error occurred (AccessDenied) … Object is protected by Object Lock.
An error occurred (AccessDenied) … Object is protected by Object Lock.
18
## 6. primary key (full access) bypasses GOVERNANCE
exit=0
```

The two scoped keys were created with `POST /api/v1/manage/keys`
(`{"permissions":["DeleteObject"]}` and `["DeleteObject","BypassGovernanceRetention"]`); an
unknown name is still `400 invalid_permissions`.

## Adversarial pass — what it found in my own code

1. **The holder of a presigned URL could add the header** — a hole the permission alone does
   not close (the signer is a full-access key). Found while writing the presign test; closed
   by the signed-header rule. The same rule then showed that a presigned bypass had never
   worked at all (the SDK puts the flag in the query): supported, with the test that adding
   it to someone else's URL breaks the signature.
2. **STS: "only narrow" against the code path that returns first.** `intersectPermissions`
   returns the parent's list for an unscoped request and the requested list for a `*` parent
   — both before any intersection happens. Without the strip and `Temporary`, a token minted
   with no arguments from the account would have been a full bypass credential.
3. **The same logic on another entry point.** Looked for other deleters: management API, user
   API, dashboard (none delete objects); the account-erasure runner ignores locks by design
   (the tenant owns them); `background_put` never bypasses. `SIGV4_ENFORCE=false` (emergency
   mode) still resolves the key's scope, so the permission holds there; the "signed" half does
   not (nothing is verified in that mode).
4. **A handler without a scope.** Test mode sets `*` explicitly; a direct handler call gets
   nothing. Two existing tests proved the fail-closed path by failing.
5. **Concurrent writer / crash between writes / flaky backend:** the change holds no state
   and makes no second write.
6. **"Nothing bypasses COMPLIANCE"** — true at both sites that could (the lock check, the
   retention state machine); the test above is the proof, not the comment.

## What I could not prove

- **rclone / other clients** send the header signed: checked for aws-cli and aws-sdk-go-v2
  only. A client that sent it unsigned would get the "not covered by the request signature"
  refusal — fail closed, with a message that says what happened.
- **The dashboard form in a browser**: the served HTML is tested, not looked at. The API keys
  page is not among the ten pages `make dash-shots` renders, and the checkbox sits in a panel
  that is hidden until "Full Access" is unticked. `make dash-lighthouse` was run: the ten
  pages it covers are at 100.

## Prod (read-only)

2026-10-02: `object_locks` 0 rows; `api_keys` 5 rows, all full access (`["*"]`), none
revoked; `sts_tokens` 0 rows. No customer has a retention or a scoped key today: nothing
changes behaviour on deploy. Nothing was changed on the box.

## Decisions taken

- A full-access key keeps the bypass (the brief; AWS root-equivalent). The primary key is one.
- The bypass must be **signed** on every request, not only presigned ones — AWS's own rule.
- A token's `*` does not include the bypass; nothing else about `*` changes.
- The header without the permission on an unlocked object is ignored, not refused.
- The dashboard offers only this one Object Lock permission (the form never listed the
  retention operations; adding them is not this WP).

## [YOU]

Nothing to set. If a customer's backup key should be able to prune GOVERNANCE-retained
objects, it needs a new key with the permission (or the primary key).
