# WP-P42 review (RP62): #969 / #970, the wrapped S3 secrets and `credential_plaintext_drop`

*2026-10-25, ~22:45 UTC. Adversarial review of Stage 1a Prompt 42 Session A: #969 `5d9c2f03` (migration 108, wrap on write, the ONE reader rule, the backfill) and #970 `bfea480b` (the drop flag, the 409 gates, the `vaultaire-switch` marker). PR A (#971, the age-sealed dump) had its own pre-PR review, RP62-1…7, which is folded into #971. This file is RP62-8…; Prompt 43 Step 0 reads it before B3.*

**Verdict: HOLDS. No P0/P1.** B3 (turn on the flag, then rotate every key) may go ahead once **RP62-8 is satisfied inside B2**. Everything below is P3/P4.

## What was checked, and how

| Claim | How it was checked | Result |
|---|---|---|
| Every writer of a secret column wraps | `grep -rnE "(INSERT INTO\|UPDATE) +(api_keys\|sts_tokens\|tenants)"` over the whole tree: Go, shell, SQL outside migrations, `cmd/`, `deploy/`, `scripts/` (not only the structure test's AST walk of Go literals) | Holds. Product writers: `persistNewAccount` (api_keys + mirror), `persistAPIKey` (new key, rotation successor), `rotateRows` (mirror), `GenerateSTSToken`, and the backfill's own statements. The one other writer, `Postgres.CreateTenant`, has no caller (RP62-9). No shell script or tool writes a secret column. |
| The ONE reader rule: a wrapped row that does not open is a 500, never the plaintext | Read `openStoredSecret` and both `LookupCredential` branches (api_keys, STS) and `PrimaryPair`. Every `LookupCredential` caller (header SigV4 `ValidateRequest`, presign verifier) and every `NewAuth` site (only `Server.s3Auth`) | Holds. A non-nil wrap is opened or refused. A `NULL` wrap reads the plaintext. An empty plaintext with no wrap is refused by BOTH consumers (`handlers.go:154`, `s3_presign.go:93`), so a dropped secret can never verify against `""`. `ErrCredentialUnavailable` maps to 500 at `s3.go:376` / `s3_presign.go:165`. |
| Nothing serves a secret from memory | Every `.Secret` / `.SecretKey` read in `internal/auth`, `internal/api`, `internal/dashboard` | Holds. `LoadFromDB` loads no secret. The maps hold copies with the secret blanked. The secret is shown once, only on the creating response (registration page, key create/rotate, STS mint). |
| The backfill compares before it nulls | `credential_backfill.go` `wrapOne` / `dropOne` | Holds. A seal is opened back and compared before it is written. A live row's plaintext is nulled only after its wrap opens back to exactly it. Both writes are guarded on what was read (`IS NOT DISTINCT FROM`). Dead rows (revoked key, key with no tenant, expired token) are nulled without a wrap; none can authenticate (`LookupCredential` refuses them before any secret is read). The mirror is nulled only once the live primary has a wrap and no plaintext. |
| The 409 gates | `guardCredentialPlaintextDrop`, composed in `guardFlagWrite` ahead of the translog guard. One `flags.Service` slot serves the admin API and the dashboard | Holds. No master key → 409 `master_key_absent`. Any live unwrapped row → 409 `credentials_not_wrapped` with the count. The guard runs on any row of the key (the flag is also `GlobalOnly`). Turning it off is always allowed. |
| The switch marker | `vaultaire-switch` `credential_guard` in `start_and_switch` (deploy, restart, rollback); the `feature_flags` columns against the migration (`flag_key`, `tenant_id = '*'`, `enabled`) | Holds. A binary without `stored/credential/v1` is refused while the flag is on **or unreadable** (fails closed). The query matches the schema. A Go test binds the marker to the constant. |
| The crypto | `credential_wrap.go` | Holds. HKDF-SHA256 of the version's chunk key under its own label `stored/credential/v1` (domain-separated from the chunk use); AES-256-GCM with a random 96-bit nonce per seal; the AAD binds tenant, key id and version, with a NUL in a name refused (no separator ambiguity); the format byte is checked. The erasure's version-row delete shreds the wraps (tested). |
| Session A's tests pass on main | `make test-db TEST_DB=vaultaire_test_p62r`, then `-run 'Credential\|KeyReencrypt_Backfill\|KeyReencrypt_Rotation\|NewAuth_Every'` over `internal/crypto`, `internal/auth`, `internal/api` | 15 top-level tests PASS (none skipped), at `bfea480b` + #971. |

## Residuals

| # | Sev | Finding | Fix / owner |
|---|---|---|---|
| RP62-8 | **P3, gates B3** | **After the drop, the master key is the only route to every S3 key.** The 409 gates prove every live row is wrapped. They do not prove the F-14 escrow can rebuild the master key. The TPM-sealed credential dies with the box (a motherboard, a reinstall, `--restore-box`). If the escrow cannot rebuild the key, every customer's S3 keys AND every SSE object are gone together, and turning the flag off brings nothing back. | In B2, before B3: rebuild the master key from 2 of the 3 escrow shares on an offline machine, and prove it is the same key without printing it: a box Info-line key ID / fingerprint, or `sha256` of the key compared by the owner in his own terminal. Record "escrow rebuild PASS" in WP-P43.md. No PASS = no B3. |
| RP62-9 | P4 | `internal/database/postgres.go` `Postgres.CreateTenant` writes `tenants.secret_key` in the clear. It has no product caller and is allow-listed as such in `credentialWriters`. A future caller would write the mirror's plaintext under the drop. | Delete it (and its test row). Cheaper than the allow-list entry. |
| RP62-10 | P4 | Both passes can starve. The wrap pass takes `ORDER BY created_at LIMIT 1000` and the drop pass takes an unordered `LIMIT 1000`, and neither skips rows that keep failing. 1,000 permanently failing rows (a tenant whose version does not open) would block every other row. Prod has a few dozen rows, so not live. | A keyset cursor in `job_runs.result`, or failing rows ordered last. |
| RP62-11 | P4 | `TenantKeys.createFirst` (via `SealCredential` → `Current`) commits version 1 on its own connection, before the registration transaction. A registration that then fails leaves a `tenant_key_versions` row for a tenant that never existed. It holds no data and is never erased. | Harmless. Note it, or let the erasure walk prune version rows with no tenant. |
| RP62-12 | P4 | A server with no master key writes the plaintext again even with the flag on (by design: "a secret is never stored nowhere"), while every wrapped row answers 500. The outage is loud, but nothing watches the plaintext coming back. | Prompt 43 Part A's gate (`secret_key IS NOT NULL` = 0 while the flag is on) is the watch. Optionally a gauge + rule later. |
| RP62-13 | P4 | An operator `INSERT INTO feature_flags …` (psql) turns the flag on without the 409 guard. | DEPLOY.md / ENCRYPTION.md: the flag is set only through the admin API or the dashboard. |

## For B3 (Prompt 43)

The order stays:
1. B2: the master key, the escrow rebuild PASS (RP62-8).
2. A deploy or restart.
3. A `key_reencrypt` run with `credentials_wrapped` = the live row count and `credential_failures` 0.
4. Turn on the flag. Expect a 409 if anything is unwrapped.
5. A run with `credentials_plaintext_dropped` = rows and `mirrors_plaintext_dropped` = tenants.
6. Rotate every live primary and scoped key.
7. The old secret answers 403.
8. Restore the next dump into a scratch DB: `secret_key IS NOT NULL` = 0 in `api_keys`, `sts_tokens` and `tenants`.

`RotateAPIKey` revokes the old row in the same transaction that inserts the wrapped successor and nulls the mirror, so after step 6 the pre-flip copies (dumps ≤ 30 d, PITR ≤ 28 d) hold only dead secrets.
