# Engine lane, charter

*Added 2026-10-12. The lane that owns `cmd/vaultaire` and everything the three surfaces stand on. Contract: `docs/ARCHITECTURE_CONTRACT.md`. Backlog: the FULL-PLAN TRACK in `docs/IMPLEMENTATION_PLAN.md`, Stage 1 first.*

**Who it is for.** The other three lanes. The engine has no customers of its own; it has promises the surfaces repeat.

**The promise, in one sentence.** Bytes that are never lost, served at the speed the price implies, with the mode the surface declared and nothing else read.

**Default mode.** None. The engine implements all three and asserts which one a request is in.

**Non-goals.** Copy, pricing pages, app UX, anything a surface should own. The engine does not decide defaults; the surfaces do (contract line 1).

**Metrics.** Synthetic check success and latency; conformance pass rate; routing-truth missing ratio; job staleness; parity coverage of Vault-tier objects; chunked-GET truncations; p99 of PUT and GET through the public path.

**The first ten.**

1. The Stage 1 one-way doors (WP-R2-1 write-new-key-then-swap, then WP-R6-1 location-authoritative reads).
2. Keystone: streaming SSE (WP-R8-5) → WP-R3-3 SSE multipart, WP-R3-1 multipart through the chunk pipeline.
3. Keystone: the bucket mode column and `modes.json` with its test (41.0).
4. Keystone: the SDK key core as a Go module compiled to WASM (10.0.1 core), with golden vectors shared with the chunker's.
5. Keystone: the transparency-log library (append, checkpoint, witness, verify).
6. Keystone: the recovery-vault interface (realm API; the first realm on the attested box).
7. The master-key install (F-14): credential read from `$CREDENTIALS_DIRECTORY`, the dedicated service user, the on-box ceremony runbook; then the TPM-resident KEK follow-up.
8. WP-SSEC-1: streaming SSE-C, multipart, copy, the "SSE-C required" bucket flag.
9. WP-DOMAIN-1: stored.cloud canonical everywhere the code names a host.
10. The attested data plane (41.4) once the EPYC box exists.

**What this lane never does.** Add a feature without its mode declaration (line 2). Introduce a second key derivation (line 4). Touch the chunker identity (line 6).

**Added 2026-10-24 (Stage 1a Prompt 40).** The metadata database's durability has a stated RPO and RTO: point-in-time recovery (pgBackRest into R2, encrypted; RPO ≤ ~61 s, RTO 12.8 s measured by the drill), beside the nightly dump (`docs/DEPLOY.md` § Point-in-time recovery). A request carrying S3 credentials is the S3 API's on every host before any route (CLAUDE.md Architecture Decision 8).

**Added 2026-10-25 (Stage 1a Prompt 41).** The recovery drill runs itself (monthly, the 2nd at 02:10 UTC) and leaves a metric a rule watches — a backup nobody has restored is a hope; the installer re-runs as a no-op and has a path for a box lost too; the two sign-in doors share one per-account lockout (`docs/reviews/WP-P41.md`).
