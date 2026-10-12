# Trust lane, charter ("least access, any access a public event")

*Added 2026-10-12. Lives at trust.stored.cloud. Contract: `docs/ARCHITECTURE_CONTRACT.md`. Plan homes: Phase 43 (the Cape model), 43.6 the transparency log (F-16), decision F-14 (the master key), F-18 (the attested box), 41.4 (the attested data plane), 10.7 (isolation audit).*

**Who it is for.** Journalists, auditors, security researchers, the customer who reads the privacy page, and a court. Also us: it is the lane that keeps the other three honest.

**The promise, in one sentence.** We hold as little as the service needs, we can read as little as the surface allows, and nothing that widens our access happens quietly.

**What is true today, and must be said.** Standard-mode bytes are readable by us: the service decrypts them to serve them, and the S3 secret is stored for SigV4. The master key (F-14) protects those bytes from the storage vendors, not from us. "Can't read" exists only client-side: SSE-C, client-side tools, Private mode. The privacy page's per-mode table says this in plain words.

**Non-goals.** Marketing copy, "zero-knowledge" before an external audit, warrant canaries as the only signal (a published measurement that changes is a stronger signal than a canary that stops).

**Metrics.** Columns held per customer outside the inventory (zero); retention clocks met; transparency-log event types covered / defined; time from ceremony to log entry; law-enforcement requests and productions per period (the transparency report); unwrap failures (zero); escrow restore drill passed (yearly).

**The first ten.**

1. The master-key install (F-14): on-box ceremony into a `host+tpm2` systemd credential, dedicated service user, `age-plugin-sss` 2-of-3 escrow, the rewritten runbook; fix `generatedObjectWriter` plaintext into encrypted buckets before the flip.
2. The per-mode privacy table generated from `modes.json` (41.0), the "what we can produce" page and the law-enforcement policy (43.4).
3. The transparency log in its cheap form (43.6): signed statements, OpenTimestamps, a Rekor mirror; the first entries are the master-key ceremony and every client build hash.
4. Phase 43.1 inventory from the export's section list, 43.3 the retention clocks (owner confirms the table).
5. Phase 43.2 disassociation, including Ente's server pattern: e-mail encrypted at rest under a server key, keyed hash for lookup.
6. The standby box bought as EPYC with SEV-SNP exposed (F-18); used now as standby, Postgres replica, backup target and TPM-held escrow share.
7. The TPM-resident key-encryption key follow-up (no exportable master).
8. The attested data plane (41.4): measured image, no SSH, key broker on the other box, published measurements, attestation endpoint, policy changes in the log.
9. The first transparency report.
10. The external audit that unlocks the words we do not use yet.
