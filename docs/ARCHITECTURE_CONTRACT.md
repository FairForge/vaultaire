# Architecture Contract

*One page. Every worker prompt cites it. A change that cannot name the line it serves is building sideways. Added 2026-10-12 (owner decisions of that day: one brand, three ambitions, least access). The seven non-negotiables in `CLAUDE.md` still hold; these are the lines that keep three surfaces on one engine.*

## The end state

**Stored** is one brand (a FairForge product) on one domain, **stored.cloud**, with three ambitions on one engine:

1. **Least access.** We hold as little customer data as the service needs, we can read as little of it as the surface allows, and any act that could widen our access is a public event.
2. **The Stripe of storage.** An API that never surprises, documentation and SDKs a developer trusts, a platform others resell.
3. **The Apple of storage.** For the person who never reads documentation: it works, it is beautiful, it is never lost, and recovery works.

The engine (`cmd/vaultaire`) is the same under all three. The surfaces differ in defaults, voice and documentation, never in primitives.

## The lines

1. **Mode is a property of the surface, never a user choice.** Three modes: **Private** (keys exist only on the customer's devices; Ente's hierarchy, decision F-12), **Standard** (keys we hold; SSE-S3 + convergent chunks under a stored, versioned tenant key, WP-R8-1), **Public** (plaintext by definition). The developer surface defaults to Standard. The consumer surface defaults to Private once the passcode recovery vault exists (F-19), and to a recoverable default with "Advanced protection" opt-in before that. "Vault" names the tape tier, never a mode.
2. **Every feature declares its mode** before it is built: which modes it works in, what it reads, what it leaks (sizes, counts, timestamps, access, within-space chunk equality). A feature that reads content runs in Standard and Public only, or on the device, or in the attested data plane.
3. **One mode table, machine-read.** `modes.json` (Phase 41.0) is the single source for the privacy page's per-mode table, the OpenAPI text, the dashboard labels and a test that fails when the code disagrees. Nothing hand-writes what a mode lets us read.
4. **Keys exist only through the one hierarchy.** Standard: `tenant_key_versions` wrapped under the master key, which lives in a TPM-sealed systemd credential, never a file (F-14). Private: the F-12 hierarchy in the SDK key core, one Go module compiled to WASM, used by every client. No surface invents a key, a wrap or a derivation.
5. **Cross-tenant convergence and least access are opposites.** Convergent chunks dedup within a space only. Cross-tenant dedup exists only in the opt-in public-artefact namespace (10.3, 10.6) and behind F-3's measurement. There is never a global convergent key.
6. **The chunker identity is frozen** (WP-R8-4: restic Rabin, polynomial, 20 average bits). Clients cut the same boundaries. Changing it is a knowing dedup reset.
7. **Any act that could widen our access writes to the transparency log** (F-16): a client build, a server image measurement, a key-release policy, an escrow or recovery ceremony, a law-enforcement production, an admin read of customer metadata. The log has signed checkpoints and an external witness. Customers read outcomes; auditors read the log.
8. **"Can't read" is only ever client-side.** SSE-C, client-side tools, Private mode. The master key protects Standard data from the storage vendors and never from us. The copy for Standard is "encrypted at rest with keys we hold". The words "zero-knowledge" wait for an external audit.
9. **Recovery is part of encryption.** No default flips to Private on a surface without a recovery path a person can use: the passcode vault with hardware attempt limits, trusted contacts, passkeys. Losing data to protect it is a failure of the product, not a feature.
10. **The attested data plane is the one shared foundation** (41.4, F-9, F-18): self-hosted confidential VMs on EPYC hardware with SEV-SNP, a key broker on another box, published measurements, an attestation endpoint. It hosts developer-grade processing, the consumer recovery realm and cloud AI, and makes any server-side access visible. Until it exists, nothing claims it.
11. **One domain.** stored.cloud is canonical (F-17): `app.`, `s3.`, `docs.`, `trust.`, `status.` and `/developers`, `/photos`, `/platform`. stored.ge is served for as long as any customer has it configured. Public customer content lives on a separate non-brand content domain, never on the brand origin.
12. **Metadata has clocks.** Phase 43: every column we hold on a customer is in the generated inventory, has a stated retention, and the identity record and the data plane are joined by an opaque id, not an e-mail.
13. **The tenant is the unit of disclosure** (43.7, F-20). Keys are unwrapped per tenant by a broker that never rests with the data; answering a demand for one account never touches another account's keys, rows or logs; a request for the whole is contested and, if it survives, is itself a public event.
14. **Content is never classified in private; known hashes are matched in public** (F-21). No classifier runs on private content in any mode; the public and shared surface is matched against known-CSAM hash lists with a threshold and a human; the first action is the link, never the account.

## How the plan is worked

- **Four lanes:** Engine (this repository's core), Developer surface (API, DX, docs, SDKs, platform), Consumer surface (the apps, the SDK client, recovery), Trust (Phase 43, the transparency log, the master-key install, the attested box). Charters in `docs/surfaces/`.
- **Keystones first, built once in the Engine lane:** streaming SSE (WP-R8-5), the SDK key core as a Go module + WASM, the bucket mode column, the transparency-log library, the recovery-vault interface. A surface consumes a keystone; it never re-implements one.
- **WIP limit:** one deep code lane and one surface lane in code at a time. Writing lanes run in parallel.
- **Weekly owner decision batch** from the D and F tables. Work proceeds under a stated assumption and says so in its PR.
- **Convergence checklist** in `docs/STATUS.md`: features without a declared mode, plaintext paths reachable in Private, log event types covered, conformance pass rate, documented API operations.
- **All four lanes build now, dark until proven (owner decision 2026-10-12, evening: "I don't want to defer anything, I am already using it"; the 2026-10-01 full-plan decision applied to the lanes).** Every lane codes from today; new behaviour ships behind a feature flag OFF, on a new host or route, or as an additive migration whose number is assigned in `docs/STATUS.md` at prompt time; the launch path (the S3 API in Standard mode, billing, the deploy) is protected by CI, the conformance suite, the synthetic check and the Stage-6 freeze on launch-path changes only, never by a lane waiting; a flag flips after a proof on tenant zero; merges go through the one queue, and review is the pacing bottleneck, so PRs stay small and the keystones go first.
