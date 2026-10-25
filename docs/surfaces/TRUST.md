# Trust lane, charter ("least access, any access a public event")

*Added 2026-10-12. Lives at trust.stored.cloud. Contract: `docs/ARCHITECTURE_CONTRACT.md`. Plan homes: Phase 43 (the Cape model), 43.6 the transparency log (F-16), decision F-14 (the master key), F-18 (the attested box), 41.4 (the attested data plane), 10.7 (isolation audit).*

**Who it is for.** Journalists, auditors, security researchers, the customer who reads the privacy page, and a court. Also us: it is the lane that keeps the other three honest.

**The promise, in one sentence.** We hold as little as the service needs, we can read as little as the surface allows, and nothing that widens our access happens quietly.

**What is true today, and must be said.** Standard-mode bytes are readable by us: the service decrypts them to serve them, and the S3 secret is stored for SigV4. The master key (F-14) protects those bytes from the storage vendors, not from us. "Can't read" exists only client-side: SSE-C, client-side tools, Private mode. The privacy page's per-mode table says this in plain words.

**Non-goals.** Marketing copy, "zero-knowledge" before an external audit, warrant canaries as the only signal (a published measurement that changes is a stronger signal than a canary that stops).

**Metrics.** Columns held per customer outside the inventory (zero); retention clocks met; transparency-log event types covered / defined; time from ceremony to log entry; law-enforcement requests and productions per period (the transparency report); unwrap failures (zero); escrow restore drill passed (yearly).

**The first ten.**

1. The master-key install (F-14): on-box ceremony into a `host+tpm2` systemd credential, dedicated service user, `age-plugin-sss` 2-of-3 escrow, the rewritten runbook; fix `generatedObjectWriter` plaintext into encrypted buckets before the flip.
2. The per-mode privacy table generated from `modes.json` (41.0), the "what we can produce" page and the law-enforcement policy (43.4). **Built dark 2026-10-24 (Stage 1a Prompt 38, #938):** `/legal/what-we-can-produce` and `/legal/law-enforcement` on the canonical origin and `trust.stored.cloud`, behind `legal_le_pages` (OFF = 404); every row of the produce page is a code fact under test (`internal/api/landing/produce.json`, `internal/api/produce_test.go`); the policy's procedure is a marked DRAFT for counsel (**F-36**); `docs/reviews/43.4-PROBE.md`, `docs/reviews/WP-P38.md`.
3. The transparency log in its cheap form (43.6): signed statements, OpenTimestamps, a Rekor mirror; the first entries are the master-key ceremony and every client build hash.
4. Phase 43.1 inventory from the export's section list, 43.3 the retention clocks (owner confirms the table).
5. Phase 43.2 disassociation, including Ente's server pattern: e-mail encrypted at rest under a server key, keyed hash for lookup.
6. The standby box bought as EPYC with SEV-SNP exposed (F-18); used now as standby, Postgres replica, backup target and TPM-held escrow share.
7. The TPM-resident key-encryption key follow-up (no exportable master).
8. The attested data plane (41.4): measured image, no SSH, key broker on the other box, published measurements, attestation endpoint, policy changes in the log.
9. The first transparency report.
10. The external audit that unlocks the words we do not use yet.

**Added 2026-10-14 (F-20, F-21; plan 43.7, 43.8).**

11. The key broker now, on a rented SEV-SNP VM, unwrapping per tenant (43.7, F-20) — after the F-14 ceremony, migrated by the `PREVIOUS` re-wrap; it replaces item 7 (the KEK leaves the box rather than hiding in its TPM).
12. Before launch, two holes closed: `api_keys.secret_key` wrapped under the tenant's key; the pg dumps encrypted with `age` before they exist on disk (43.7). **2026-10-24 (Stage 1a Prompt 40, #955):** the continuous point-in-time archive of the database is encrypted on the box (`aes-256-cbc`) before it reaches R2, its passphrase only on the box and in the owner's Keychain; the off-box dumps were already encrypted (Sync's own E2EE, Blomp through rclone crypt) — the on-box dump is still plaintext. The legal pages' backup sentences (they said 7 days; the dumps live 30) now come from code facts under test, and name the archive.
13. Boot-state and build transparency (a 43.6 entry per deploy with the binary hash and an fTPM quote, a daily quote otherwise), canary credentials, the customer's own audit view, the sub-processor and data-location page, the incident clock (43.8).
14. The moderation scope written down and in the AUP (F-21): known hashes on the public surface only, a human before any action, the link before the account, an appeal, counts in the report.
