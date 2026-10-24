# Stage 1a — Prompt 39 Session A: Part 0 (the WP-P38 review's RP54-2/3/4/6/7) and Part A (the web host answers like a website)

*2026-10-24. Session A of Prompt 39 (split: Session B runs Part B, the tenant-zero proofs, and writes the docs PR: STATUS.md, CLAUDE.md, the plan rows and `docs/reviews/WP-P39.md`, which fold this note in). Merged: #948 (Part 0) and this PR (Part A + this note). **No migration**: the next is still **108**. Residuals RP55-1…RP55-9 (Session B numbers from RP55-10).*

Lane: Engine (the erasure at the realms, the backups), Consumer (the web host a visitor types into), Trust (the dark legal pages). Contract rule 8: every sentence on the two new pages that is not a marked DRAFT is a code fact under test.

## Step 0

- #944 `cf41f162`, #946 `07d29124`, #945 `70ad5946` and #947 `ffb8c12f` (the review note) are all merged. `/version` = `70ad59463bf7` (build 16:24:39Z; #947 is docs-only, so it was not deployed). Active slot 8000 since 16:47:45 UTC: 9,334 journal lines, **0** `"level":"error"`.
- No `feature_flags` row for `recovery_vault`, `translog_public` or `legal_le_pages` (OFF), and no `legal_draft_pages` row. 0 `RECOVERY_` lines in `.env`.
- `account_deletion` `ok` at 04:34:30 UTC, result `{"settling": null}` (no account due).

## Part 0 (#948)

### RP54-6 — the Blomp backup leg's undeleted old files are counted and alerted

| Finding | Sev | Fix | RED on main | Mutation → caught by |
|---|---|---|---|---|
| `pg-backup.sh`'s Blomp leg (`offbox2_retention`) logged an old backup it could not delete and never failed or alerted, while the Sync leg fails the run. The produce page's "a deleted record stays up to 30 days in a backup" rests on it | P4 | Each leg's retention pass writes a gauge: `vaultaire_backup_offbox_retention_undeleted_files` (Sync) and `vaultaire_backup_offbox2_retention_undeleted_files` (Blomp). The value is the count of old files the pass could not delete, and 0 after a clean pass. `write_metric` takes an optional value. New rule **BackupOffboxRetentionStuck** (`vaultaire-backup.yml`, warning, `for: 10m`). Each leg gets a `leg` label (`label_replace`) so the two alerts never share a label set. The Blomp leg still never fails the run. The README's symlink loop names both files | `pg-backup_test.sh` 3d, 3g, 4f, 4h, 4i and 4g FAIL: the files are absent and there is no rule. 4h is new: two old Blomp files undeletable → the run completes and the gauge is 2. 4i: the next run deletes them and exports 0 | The gauge always written as 0 → 3g and 4h red. Blomp's `leg` label set to `sync` → promtool `FAILED` at 15m |

promtool on the box: `tests/vaultaire-backup_test.yml` **SUCCESS** (one leg stuck, both legs stuck as two alerts, a clean pass silent); `promtool check rules` 5 rules. A Blomp **listing** failure writes no gauge, so the previous value stays: RP55-1.

### RP54-4 — a realm that gives no answer is asked once per run

| Finding | Sev | Fix | RED on main (the metric declared, nothing counted) | Mutation → caught by |
|---|---|---|---|---|
| `recoveryVault.eraseUser` asked every realm for every due tenant, even after a realm had given no answer in this run: up to 30 s per realm per tenant (200 tenants × 2 dead realms = 3.3 h of a 6 h run) | P4 | `realmOutcome` classifies each Delete. **unreachable** = an `*url.Error`, a `net.Error` or the Delete's own 30 s deadline, i.e. no answer; never a 5xx or a 401. When the parent context is already done (the tenant's deadline, a shutdown) the outcome is `""`: the failure is ours, so it is neither counted nor remembered. The runner keeps `realmsSilent` (realm id → error), reset by every `RunOnce`. A realm in it is not asked again in that run: the tenant is deferred with `<id>: not asked, no answer earlier in this run (<the first error>)`. The next run asks again | `TestAccountDeletionRunner_SilentRealmIsAskedOncePerRun`: two due tenants, realm b hangs (a 150 ms client timeout). On main `b.asked(subG)` was **"Should be zero, but was 5"** (the transport's 5 attempts) and the second tenant's error had no "no answer earlier in this run". With the fix, realm a still gets both Deletes; the second run, with b back, erases both | Never record the silent realm → that test red. Record a realm silent on **any** error → `TestAccountDeletionRunner_AnsweringRealmIsAskedForEveryTenant` red (a 503 and a refused token are answers) |

### RP54-3 — the realm that refuses is named

| Finding | Sev | Fix | RED on main | Mutation → caught by |
|---|---|---|---|---|
| A realm refusing the token (its key rotated, or its clock more than 5 s behind ours on `nbf`) deferred every erasure every run, with no metric naming it | P4 | `vaultaire_account_deletion_realms_total{realm,outcome}`: `deleted`, `invalid_auth`, `unavailable` (answered without confirming: 5xx, Unavailable, rate limited, a malformed answer), `unreachable`. It is registered with the account-deletion series, and every configured realm × outcome is created at 0 at boot (`initRealmSeries`, wired where the runner gets the vault; RP14-9). New rule **AccountDeletionRealmRefusing** (`vaultaire-jobs.yml`, warning): `sum by (realm, outcome) (increase(…{outcome!="deleted"}[2h])) > 0`, with a description per outcome | `…AnsweringRealmIsAskedForEveryTenant` (unavailable and invalid_auth subtests: "expected 2 actual 0"). `TestAccountDeletionRealmSeries_ExistBeforeTheFirstDelete` | No boot series → the series test red. `invalid_auth` left unclassified → the invalid_auth subtest red |

promtool on the box: `tests/vaultaire-jobs_test.yml` **SUCCESS** (a refusal fires with realm + outcome; `deleted` and a 0 counter stay silent; quiet again 2 h later); `promtool check rules` 10 rules. `TestJobRuleFile_MatchesTheRegisteredJobsAndTheExportedSeries` asserts the rule and that the file documents the series.

### RP54-2 — a full session table no longer answers Unavailable for a Delete it applied

| Finding | Sev | Fix | RED on main | Mutation → caught by |
|---|---|---|---|---|
| `cmd/recovery-realm` applied a request riding the handshake (`s.handle`) BEFORE `putSession`, so with the session table full it answered `Unavailable` for a Delete that had already happened | P4 | If `putSession` refuses and the request carried a SecretsRequest (Delete, Register1, Recover1), the realm answers it with `SessionLifetime: 0`: no session kept, and the client's `take()` treats the session as expired and opens a new one. A handshake that carries nothing still answers `Unavailable`. Documented in `cmd/recovery-realm/README.md` | `TestRecoveryRealm_FullSessionTableStillAnswersADelete` (`maxSessions` sessions of other subjects): on main `client.Delete` → **`recovery: not enough realms answered: realm 5454…: Unavailable`**. With the fix the Delete is OK, the record is `NotRegistered` with no material, `delete/ok` +1, the table still holds `maxSessions`, and a bare handshake is 503 `Unavailable` | Lifetime 0 for a bare handshake too → red ("a handshake with no request still needs a slot") |

The juicebox SDK's handling of a 0 lifetime was not checked (our Go client treats it as expired): RP55-2.

### RP54-7 — a checkout build with no output is a warning

| Finding | Sev | Fix | RED on main | Mutation |
|---|---|---|---|---|
| `scripts/repro-check-clients.sh`: `got=$(shasum …)` for the checkout build under `set -e`. A build that exits 0 and writes nothing ended the Deploy step the comment promises never to fail | note | `[ -s "$out" ]` and a checked `shasum`, else `::warning::repro-check-clients: this checkout's <client> built nothing`. Test seam `REPRO_CHECKOUT_BUILD` (replaces `scripts/build-client.sh` for the checkout part only) | `TestClientBuildManifest_ReproCheckWarnsWhenTheCheckoutBuildWritesNothing` (the real 0.1.0 row + a build that exits 0): with the seam and no guard the script exits with `shasum: …/checkout-sdk-js: No such file or directory` | That is the guard removed |

Part 0: `make lint` 0 issues, gosec `@latest` (the CI command) quiet on the changed packages, `pg-backup_test.sh` 100 checks under GNU coreutils, CI green.

## Part A — the web host answers like a website

**Before (prod `70ad5946`, 2026-10-24 ~20:40 UTC):** `stored.cloud` and `stored.ge` answered `/pricing`, `/signup`, `/nonsense`, `/legal/refunds` and `/legal/dmca` to a browser (`Accept: text/html,…`) with `403 application/xml` AccessDenied, `no-store, no-transform`.

### What was built

| Item | What | Where |
|---|---|---|
| 1 | **The pixel 404 page** for an anonymous browser page view on a web host. Gate: `isBrowserPageView`, factored out of `shouldRedirectToCanonical` (one predicate, not two: GET/HEAD, `Accept` names text/html, no `Authorization`, no presigned query, no `x-amz-*` header, not `/health` `/version` `/metrics` `/.well-known/` `/webhook/` `/api/` `/cdn/` `/auth/`), AND `hosts.IsWebHost`: the canonical host, an alias, `stored.cloud` / `stored.ge` / `stored.pics` and `www.` of each **whatever the env says** (prod's canonical origin is `stored.ge` today and `stored.cloud` is neither canonical nor an alias there), never an `s3.`, `cdn.` or `trust.` host. Hook: the one place an anonymous request is refused, the `ValidateRequest` failure in `handleS3Request` with no `Authorization` header. A request with no credential never reaches NoSuchBucket or NoSuchKey: the S3 API has **no anonymous read** (the lab below — public objects are `/cdn/` and the CDN hosts). Answer: `dashboard.WebNotFound`: `templates/errors/404.html` in the legal layout, `404`, `text/html; charset=utf-8`, `Cache-Control: no-store`, the dashboard's security headers, HEAD without a body | `internal/api/web_host.go`, `host_canonical.go`, `s3.go`; `internal/hosts` `IsWebHost`; `internal/dashboard/legal_draft.go` |
| 2 | `/signup` → **301 `/register`**; `/pricing` and `/pricing/` → **301 `/#pricing`**: `landing.html` has exactly one `id="pricing"`, fed by `prices.json`, so no page and no typed price. `no-store`. A web page view only: any other request at those paths is `handleS3Request` (a bucket may be named `signup` or `pricing`) | `web_host.go` `registerWebPages` (before the S3 catch-all) |
| 3 | **`/legal/refunds` and `/legal/dmca`**, dark behind the new flag **`legal_draft_pages`** (default OFF, `GlobalOnly`; OFF = the item-1 404, never S3 XML). Both pages are in the legal layout with a footer linking the canonical origin's privacy and terms. Every commitment is a marked **DRAFT** under the F-36 banner: refunds = **F-28**, the DMCA agent = new owner item **F-37**, the notice / counter-notice / repeat-infringer wording = counsel (F-36). The `<p data-fact>` paragraphs state only code facts: the egress allowance (`prices.json`'s two ratios and its two sentences), `account.GracePeriod` (30 days, cancellable meanwhile), the abuse form `/abuse` and `hosts.AbuseEmail()`, and each mode's name and what our servers hold (`modes.json`). Mounted by the **API** for a web page view only, not by the dashboard's public routes (lab finding A-1 below) | `internal/dashboard/legal_draft.go`, `templates/legal/{refunds,dmca}.html`, `internal/api/flags_wiring.go`, the admin flags page description |

### Tests (`internal/api/wp_p39_web_host_test.go`, DB-backed: without a database the S3 handler treats an anonymous request as the test tenant, so the refusal never happens; `internal/hosts` `TestIsWebHost`)

| Test | RED on main | Mutation → caught by |
|---|---|---|
| `TestWebHost_BrowserTypoIsAnHTML404`: prod-today hosts (`stored.cloud`, `stored.ge`, `www.` of both, `stored.pics`) and target hosts (`stored.cloud`); GET and HEAD | 403 XML, not 404 | **The host check dropped** → `TestWebHost_S3ClientsAndOtherHostsAreUnchanged` red (the s3 host) + `…SignupAndPricingRedirect` red. **No brand/legacy names in `IsWebHost`** → `TestIsWebHost` + this test (prod today) red |
| `TestWebHost_S3ClientsAndOtherHostsAreUnchanged`: `Accept: */*`, no Accept, an unsigned `x-amz-*` header, an `Authorization` header, a presigned query, the s3 host, a PUT → S3 XML. The cdn and trust hosts never return the page | passes on main (the guard) | **The predicate loosened to any GET** → red (curl's Accept, the x-amz header). A presigned request never reaches the hook: it fails in the presigned branch before it, so its row guards the branch, not the predicate |
| `TestWebHost_SignupAndPricingRedirect`: 301 + `Location` + `no-store`; curl at the same path and the s3 host → XML; exactly one `id="pricing"` | no 301 | The redirect for every request → red (curl `/pricing`) |
| `TestLegalDraftPages_FlagOffIsTheWeb404`: OFF → the 404 page on both host sets. Flag OFF **and** ON: curl, the s3 host and a signed request at these paths → S3 XML. The trust host's closed route list does not gain them | 403 XML | `on()` always true → red. **`webOnly` without its check** → red (the lab finding) |
| `TestLegalDraftPages_FlagOnRendersOnlyCodeFactsAndDrafts`: DRAFT banner, F-28 / F-37, `30 days`, the two ratios, `past_allowance`, `/legal/terms#billing`, `/abuse`, the abuse address, every `modes.json` name; **every `<p>` without `data-fact` contains DRAFT** | flag unregistered | — |
| `TestLegalDraftPagesFlag_DefaultOffGlobalOnly` | flag unregistered | — |
| `TestLegalDraftPages_NothingLinksThem`: `llms_txt.go`, `docs_pages.go`, `landing.html`, `landing.src.html`, `base.html` and every template except the two pages | passes (the guard) | — |

`go test -race ./internal/api/... ./internal/dashboard/... ./internal/hosts/... ./internal/flags/...` green; `make lint` 0.

### Lab (`bin/vaultaire` `bebcc9ab`, private DB `vaultaire_lab_p39a`, `STORAGE_MODE=local`, prod-today hosts `VAULTAIRE_BASE_URL` = `VAULTAIRE_ENDPOINT` = `https://stored.ge`; `curl --resolve` per name; one lab tenant with a bucket `labpub` made `public-read` and `hello.txt` in it)

| host | path | request | status | content-type · cache-control · location | body |
|---|---|---|---|---|---|
| stored.cloud | /nonsense | browser | **404** | text/html · no-store | the 404 page |
| stored.cloud | /nonsense | curl (`*/*`) | 403 | application/xml · no-store, no-transform | AccessDenied |
| stored.cloud | /pricing | browser | **301** | no-store · `/#pricing` | |
| stored.cloud | /pricing | curl | 403 | application/xml | AccessDenied |
| stored.cloud | /signup | browser | **301** | no-store · `/register` | |
| stored.cloud | /legal/refunds, /legal/dmca | browser, flag OFF | **404** | text/html · no-store | the 404 page |
| stored.cloud | /labpub/hello.txt?X-Amz-… | presigned GET (valid) | 200 | text/plain · private, no-cache | the bytes |
| stored.cloud | /labpub/hello.txt | anonymous browser, S3 path | 404 | text/html · no-store | the 404 page (main: 403 XML — the S3 API has no anonymous read) |
| stored.cloud | /cdn/lab/labpub/hello.txt | anonymous browser | 200 | text/plain · public, max-age=3600 | the bytes |
| stored.ge | (every row) | | same as stored.cloud | | |
| s3.stored.cloud | /nonsense, /pricing, /signup, /legal/refunds, /legal/dmca | browser | 403 | application/xml | AccessDenied |
| s3.stored.cloud | presigned GET · /cdn/… | | 200 · 200 | | the bytes |
| trust.stored.cloud | every path | | 404 | text/plain · no-store | (the closed trust router, unchanged) |
| cdn.stored.cloud | every path but the object's | | 404 | text/plain | (the CDN handler, unchanged) |
| cdn.stored.cloud, cdn.stored.ge | /lab/labpub/hello.txt | anonymous browser | 200 | text/plain · public, max-age=3600 | the bytes |

**A-1 (found in the lab, fixed in this PR):** the first build mounted the two legal pages in the dashboard's public routes, the way `/legal/law-enforcement` is mounted. The table showed `s3.stored.cloud/legal/refunds` answering the HTML 404, so a **signed GetObject of key `refunds` in a bucket named `legal`** would have got the page on every host. They are now mounted by the API before the catch-all (`webOnly`). With bucket `legal` + key `refunds` in the lab tenant, a signed GET (`curl --aws-sigv4`, `Accept: text/html`) on `stored.cloud`, `stored.ge` and `s3.stored.cloud` → `200 application/octet-stream`, `bucket legal key refunds`. The older dashboard-mounted legal pages (`/legal/privacy` … `/legal/law-enforcement`) still shadow those keys of a bucket named `legal`: RP55-3.

**Flag ON (a global row in the lab DB only):** both pages 200 `text/html`. The rendered text, in order. Refunds: the DRAFT banner (F-28, the Terms' Billing section linked); DRAFT on refunds; "one download allowance per calendar month (UTC): 0.5 × its Standard quota plus 1 × its Vault quota" + `prices.json`'s two sentences; DRAFT; "Deleting an account schedules its erasure 30 days later…"; DRAFT; DRAFT contact. DMCA: the banner (F-36, F-37); DRAFT agent; "Reports reach us through the abuse report form or by e-mail to abuse@stored.ge"; DRAFT notice elements; DRAFT counter-notice; the modes table (Private / Standard / Public, `modes.json`'s `server_holds`); DRAFT.

### Prod

Session A's final message gives the prod proof after this PR's deploy; Session B's `WP-P39.md` carries it.

## Residuals (RP55-1…)

- **RP55-1 (P4):** a Blomp retention **listing** that fails writes no undeleted-files gauge, so the previous value stays and nothing says the pass did not run. A per-leg retention timestamp would close it.
- **RP55-2 (note):** a handshake-riding answer with `SessionLifetime: 0` (RP54-2) is understood by our Go client as "no session". The juicebox SDKs were not checked; only our clients talk to the realm today.
- **RP55-3 (P4):** the dashboard's public `/legal/<slug>` routes (privacy, terms, dpa, cookies, aup, baa, gdpr, data-act, law-enforcement, what-we-can-produce) answer every request on every host, so a bucket named `legal` cannot serve those keys through the S3 API, even signed. Pre-existing; the two new pages avoid it (`webOnly`).
- **RP55-4 (owner, F-28):** `terms.html` § Billing already says "A full refund is available within 30 days of your first purchase, no questions asked". F-28's recommendation in the plan is 14 days for prepaid (crypto and manual) payments. The refunds page links the Terms and restates nothing, but the two must agree before `legal_draft_pages` opens.
- **RP55-5 (note):** an anonymous browser GET on the S3 path of a public-read object on the web host now gets the 404 page (it was AccessDenied XML: the S3 API has never served anonymous reads, the object is at `/cdn/…`). A link someone built by hand on the S3 path is still a dead link, now an HTML one.
- **RP55-6 (note):** the alias 301 runs first, so with `VAULTAIRE_ALIAS_HOSTS` set, `stored.ge/signup` is two hops (alias → `stored.cloud/signup` → `/register`).
