# Privacy, dedup and key-management techniques we can use — survey of 2026-10-11

*Written for the Ente-model E2EE re-scope (Phase 10.0), Phase 43 data minimisation, Phase 41 private
compute and the Stage 1a crypto work (WP-R8-1). Every item says what it is, why it matters here, where it
lands in `docs/IMPLEMENTATION_PLAN.md`, and whether it is free to use. Dates and patent numbers were
checked against the sources linked at the end on 2026-10-11; anything marked **verify** is from general
knowledge and must be confirmed on Google Patents / the USPTO before a design relies on it. A patent's
term is 20 years from its earliest non-provisional filing; "expired" below means that date has passed.*

## 0. The eight corrections this survey makes to the plan

1. **41.1 must not use order-revealing encodings for dates and sizes.** Naveed–Kamara–Wright (CCS 2015)
   recovered OPE-encrypted attributes for > 80 % of patient records at 95 % of hospitals from
   ciphertexts plus public auxiliary data; Grubbs et al. (2016/2017) recovered most names and birthdates
   from ORE columns. Replace with **bucketed blind indexes** (HMAC under a client key of day / month /
   year tokens and of size buckets; range = union of buckets). Equality blind indexes stay.
2. ~~**F-12's hybrid must be a generic KEM combiner, not X-Wing as published.** The X-Wing draft (still an
   Internet-Draft at -11, September 2026) states it "should not be used outside TLS, because it relies on
   the TLS transcript to prevent malleability".~~ **Corrected 2026-10-22: this misread the draft.** X-Wing
   (draft-connolly-cfrg-xwing-kem-11) is "a general-purpose post-quantum/traditional hybrid key
   encapsulation mechanism" and aims "to be usable for most applications, including specifically HPKE"
   (§ 1.2); "X-Wing hashes the shared secrets, to be usable outside of HPKE" (§ 1.5.1). The sentence "This
   one should not be used outside of TLS, as it assumes the presence of the TLS transcript" (§ 1.5.1) is
   about the earlier X25519Kyber768Draft00. X-Wing omits the ML-KEM ciphertext from its combiner safely
   only because of ML-KEM's Fujisaki–Okamoto transform (§ 6) — never reuse its combiner with another KEM.
   The sealed box is HPKE (RFC 9180) with MLKEM768-X25519 (draft-ietf-hpke-pq), Go 1.26 `crypto/hpke`
   (`sdk/go/e2ee`).
3. **Sizes leak; pad with Padmé.** PURBs (Nikitin et al., PoPETs 2019): padding to the Padmé schedule
   leaks O(log log M) bits of the length at ≤ 12 % overhead, shrinking with size. This is the answer to
   10.7's open "chunk-size fingerprint" question for vault manifests and whole-object vault blobs
   (chunk boundaries themselves stay a leak unless the paranoid flag pads chunks too).
4. **Filename encryption: HCTR2 or AES-SIV, never CBC-CTS.** Android's fscrypt moved filenames to
   AES-256-HCTR2 (Crowley–Huckleberry–Biggers, Google 2021, GPL-2.0 kernel code) because CBC-CTS leaks
   shared 16-byte prefixes; Cryptomator binds the parent directory id as associated data with AES-SIV
   so a file cannot be moved between folders undetected; rclone crypt uses EME (Halevi–Rogaway 2003).
   10.0's manifest keeps names in encrypted metadata, so this matters only for the SDK's plain-S3
   "encrypted names" mode (north star ladder item 3).
5. **Authenticate every wrapped key and bind its context.** MEGA (Backendal–Haller–Paterson 2022):
   five attacks, four "eminently practical", from key ciphertexts without integrity (AES-ECB) and one
   master key reused for every purpose — "full compromise of all user keys encrypted with the master
   key". WP-R8-1 already specifies AEAD with AAD = tenant‖version‖purpose; 10.0 inherits the rule for
   every wrap (master, recovery, private key, space, object).
6. **Recovery needs a second path besides the 24 words.** Ente offers only the recovery key. WhatsApp
   (2021, NCC-audited), Signal SVR3 (OSDI '24) and the open-source Juicebox realms all give PIN-based
   recovery with hardware-enforced attempt limits (OPAQUE or OPRF + secret sharing across realms). Add
   to 10.0.3 as an optional, later item; consumers lose paper.
7. **A server breach must not become an offline password attack.** Ente's `encryptedMasterKey` is only
   as strong as the password under Argon2id. 1Password's two-secret key derivation mixes a 128-bit
   Secret Key held by the user into the KDF so "data stored by the server cannot be used in cracking
   attempts". Offer it as the `account key` option for vault accounts (10.0.1), default on for Vault
   tier; the recovery key can double as it.
8. **Cross-tenant dedup has a third option besides OPRF and PIR.** Liu–Asokan–Pinkas (CCS 2015): secure
   cross-user dedup of client-encrypted data "without additional independent servers", by running a
   PAKE between uploaders of the same content. It keeps the F-3 measurement gate (is cross-tenant
   dedup worth anything?) and the 10.6 proof-of-ownership requirement; recorded as the candidate for
   the 10.3 shared-namespace mechanism if F-3 ever says yes.

## 1. Content-defined chunking and the dedup index

| Technique | Year, venue | Why it matters here | Lands in | Free? |
|---|---|---|---|---|
| Rabin fingerprinting over a sliding window | Rabin 1981 (Harvard TR); LBFS, Muthitacharoen et al., SOSP 2001 | What `restic/chunker` implements; our frozen identity (WP-R8-4) | shipped | no patent known |
| **Sticky-byte factoring** | Avamar / Undoo, **US 6,810,398**, filed 2001-02-05, issued 2004-10-26 (priority 2000-11-06) | The first commercial CDC patent — the shape of "rolling hash + threshold function"; continuation US 7,272,602 shows "Expired - Lifetime" on Google Patents | background | **expired** (filed 2001) |
| **Variable-length segment dedup, SISL** | Data Domain, **US 6,928,526** "Efficient data storage system", filed 2002-12-20, issued 2005-08-09, family 7,065,619 (Expired - Lifetime) … 9,454,318 | Stream-informed segment layout + locality-preserved caching: the techniques that make a chunk index scale beyond RAM; later family members (filed 2011–2014) still live — **verify each before copying a claim** | GCI scale, 23.0 cache | '526 **expired 2022-12**; later ones **verify** |
| **Sparse indexing** | Lillibridge et al., HP, FAST 2009 | Deduplicate a segment against only a few similar earlier segments found by sampled hashes — keeps the index in RAM at any scale; the answer when `global_content_index` outgrows Postgres' cache | GCI scale (Stage 1 WP-R6-2 neighbourhood / 23.0) | HP patents filed 2008 → **live until ~2028, verify** |
| Extreme Binning (Bhagwat 2009), ChunkStash (Debnath 2010), SiLo (Xia 2011) | MASCOTS / ATC | Same problem, different trade-offs (file-similarity bins; flash-resident index) | GCI scale | academic |
| **Bloom filter in front of the index** | Bloom 1970; Zhu–Li–Patterson FAST 2008 "Avoiding the disk bottleneck" | Skip the lookup for chunks that cannot exist; xor / ribbon filters (Graf–Lemire 2019–2020) are the modern, smaller, immutable variants — ideal for an edge-side "chunk exists?" set (38.1) | 38.1, chunk PUT pool | no patent |
| **FastCDC** | Xia et al., USENIX ATC 2016; TPDS 2020 | Gear hash + normalized chunking: 2–3× faster than Rabin at similar dedup. **Not adoptable**: any new cutter resets dedup for everything stored (WP-R8-4 one-way door). Useful only if a future "v2 chunker" is ever versioned per object | — | no patent found (**verify** CN filings) |
| Dedup-ratio estimation by sampling | Harnik, Margalit, Naor, Sotnikov, Vernik, MSST 2012; Harnik FAST 2016 | Estimate dedup + compression ratio of a data set from a sample — the cheap way to run F-3's measurement (cross-tenant fraction) without instrumenting every PUT | F-3 trigger | academic |
| **ClearBox** | Armknecht, Bohli, Karame, Youssef, CCS 2015 | Provider attests to each customer how much dedup their encrypted data got, so the customer can verify the storage they are billed for — we bill logical bytes, so this is a transparency feature, not billing | 14.4 verification reports | academic |

Historical lineage worth knowing when we write the design doc: Venti (Quinlan–Dorward, FAST 2002:
content-addressed, write-once archival blocks), Plan 9 Fossil, LBFS (2001), Farsite (Douceur et al.,
ICDCS 2002), Tarsnap (Percival 2008: client-side convergent dedup with client keys — the north star's
cited precedent), restic / borg / Kopia / Duplicacy (Duplicacy's lock-free fossil collection is
**patented, filed 2016 — avoid copying its GC**; ours is lock-based).

## 2. Convergent / message-locked encryption and its side channels

| Technique | Year, venue | Why it matters | Lands in | Free? |
|---|---|---|---|---|
| **Convergent encryption** (key = hash of content) | Douceur, Adya, Bolosky, Simon, Theimer, Farsite, ICDCS 2002; Microsoft **US 6,983,365** (application 09/565,821 filed 2000-05-05; family 7,159,110 / 7,266,689 / 7,574,598 / 7,779,253) | The origin of what `ChunkEncryptionService` does; Freenet's CHK and Mojo Nation used the idea in 2000 | shipped (10.3) | **expired 2020-05** (filed 2000); family continuations share the priority date — **verify** 7,779,253's term |
| **Convergence secret** (per-user key mixed in) | Tahoe-LAFS 2008 (Zooko: "convergent encryption reconsidered") | Exactly our per-tenant / per-space convergent key; Tahoe named the confirmation-of-a-file and learn-partial-information attacks it defeats | 10.0 F-12 | free software |
| Message-locked encryption, formal | Bellare, Keelveedhi, Ristenpart, EUROCRYPT 2013; DupLESS, USENIX Sec 2013 | The security model (PRV-CDA) our scheme should be stated in; DupLESS = the OPRF key server the north star demoted (10.8) | design doc | academic |
| **Side channels of client-side dedup** | Harnik, Pinkas, Shulman-Peleg, IEEE S&P mag. 2010; Armknecht, Boyd, Davies, Gjøsteen, Toorani, ASIA CCS 2017 (formal trade-offs); "Dark Clouds" Mulazzani et al. 2011 (the Dropbox hash attack) | Cross-user client-side dedup tells an uploader whether a file already exists; the 2010 countermeasure is a randomised threshold (dedup only after t random uploads); 2017 formalises leakage vs bandwidth. Governs 38.1 write-avoidance and any shared namespace | 38.1, 10.3 shared namespace | academic |
| **Proofs of ownership** | Halevi, Harnik, Pinkas, Shulman-Peleg, CCS 2011 | A client proves it holds the file (Merkle-tree challenges), not just its hash — the fix for the Dropbox attack; mandatory for 38.1 and any cross-tenant dedup | 10.6 | academic; Merkle tree patent US 4,309,569 (filed 1979) **expired** |
| **PAKE-based cross-user dedup** | Liu, Asokan, Pinkas, CCS 2015 (CT-RSA 2018 refined model) | Cross-user dedup of encrypted data with no key server — uploaders of equal content derive a shared key via PAKE mediated by the storage server, which learns nothing | candidate for 10.3 if F-3 says yes | academic |
| **Crypto-shredding** | Boneh–Lipton, "A Revocable Backup System", USENIX Security 1996 | The origin of "delete the key, the backup is gone" — what WP-R8-1 makes real (stored-random versioned tenant keys) | WP-R8-1, 43.3 | the 1996 idea predates any patent; the Boneh–Lipton 2000 filing **verify** |
| Compress-then-encrypt leakage | CRIME/BREACH class (2012–2013) | Compressed size leaks content entropy; a vault's paranoid flag should pad (Padmé) and may disable compression | 10.7 | — |

## 3. Keys, passwords, recovery, sharing

| Technique | Year, status | Why it matters | Lands in |
|---|---|---|---|
| **Argon2id** | PHC winner 2015; RFC 9106 (2021) | Ente's KDF with device-adaptive limits; ours | 10.0.1 |
| **SRP-6a** | Wu 1998; RFC 2945/5054; Stanford's patent **US 6,539,479** (filed 1998 → 20-year term ended 2018, **verify**); RFC 2945 mode was royalty-free from the start | Ente's login (2023); 1Password's too | 10.0.5 (F-13) |
| **OPAQUE** | **RFC 9807, July 2025** (CFRG, Bourdrez–Krawczyk–Lewi–Wood) | The modern aPAKE: no salt leak (SRP reveals the salt before authentication), formal proof; WhatsApp's backup vault uses it. Choose between SRP (Ente parity, more implementations) and OPAQUE (the standard) before 10.0.5 is prompted — recommend **OPAQUE** unless a reviewed Go+JS pair is missing | 10.0.5 |
| **Two-secret key derivation** | 1Password 2016 (BSidesLV talk; security white paper) | A 128-bit Secret Key mixed into the KDF: a stolen database cannot be cracked offline. Cost: lose both and the data is gone (1Password: no recovery) | 10.0.1 option |
| **HSM / enclave PIN recovery** | WhatsApp Backup Key Vault 2021 (HSMs, OPAQUE, NCC Group audit; Meta 2026 whitepaper "Security of End-To-End Encrypted Backups" with Cloudflare-signed fleet keys); Signal SVR3 (OSDI '24: trust split across enclaves of different clouds); **Juicebox** (open source: OPRF + secret sharing across realms, HSM realm on GCP) | Human-memorable recovery with hardware attempt limits — the missing consumer path in Ente's model | 10.0.3, later |
| **Shamir secret sharing** | Shamir 1979; **SLIP-0039** (Trezor: two-level group/member mnemonic shares); BIP-93 codex32 | Split the recovery key across trusted contacts (2-of-3) instead of wrapping it to one — 10.0.3 Legacy; and split the OPERATOR master key (32.6) | 10.0.3, 32.6 | no patent |
| **FROST** | **RFC 9591, June 2024** (two-round threshold Schnorr) | Threshold signing for operator custody (release builds, the master-key ceremony) without a single key anywhere | 32.6, release pipeline |
| **Hybrid PQ KEM** | ML-KEM FIPS 203 (2024); X-Wing draft-11 (general-purpose; correction 2 above); HPKE RFC 9180 + draft-ietf-hpke-pq (Go 1.26 `crypto/hpke`) | F-12's sharing primitive; Go `crypto/mlkem` already in the binary; browser needs a vetted JS/WASM ML-KEM | 10.0.3 |
| **Key transparency** | WhatsApp (auditable key directory, 2023–24); Signal Automatic Key Verification (2026, Cloudflare + Trail of Bits as auditors); IETF KEYTRANS | Replaces hand-compared Verification IDs for shared vaults: the server's public-key directory is a Merkle log audited by a third party | 10.0.3 later, 36.2 |
| **Deterministic AEAD for names** | AES-SIV RFC 5297 (2008), AES-GCM-SIV RFC 8452, HCTR2 (2021) | Encrypted names where plain-S3 List must still work (sort, prefix) | SDK names mode |

## 4. Private lookups, searchable metadata, compute

| Technique | Year, venue | Why it matters | Lands in |
|---|---|---|---|
| **SimplePIR / DoublePIR** | Henzinger et al., USENIX Security 2023 | Single-server PIR at ~10 GB/s/core (LWE); hint 121 MB per 1 GB database (DoublePIR 16 MB), ~242 KB per query — a "is this chunk/hash known?" service with no secret to steal, cheaper than BFV for membership | 41.3 (beside Apple's BFV library) |
| **Checklist** | Kogan–Corrigan-Gibbs, USENIX Security 2021 | Two-server offline/online PIR for blocklist lookups (Safe Browsing demo, 3.3× bandwidth, 9.8× server CPU) — the shape for the breach-list and abuse-hash lookups | 41.3 |
| **Apple Live Caller ID stack** | 2024– | OHTTP relay (RFC 9458) hides the IP, Privacy Pass authenticates anonymously, PIR hides the query — three layers we can copy one by one for the SDK's metadata calls | 41.3, 43 |
| Structured / searchable encryption | Song–Wagner–Perrig 2000; Curtmola et al. CCS 2006; Cash et al. 2013–14; leakage-abuse: Cash 2015, Naveed 2015, Grubbs 2016 | Equality blind indexes are fine; order-revealing is not (correction 1); access-pattern leakage is the residual every design must state | 41.1 |
| **Padmé / PURBs** | Nikitin et al., PoPETs 2019 | Length padding with ≤ 12 % overhead, O(log log M) leakage | 10.7, 10.0 |
| Keyed-verification anonymous credentials | Chase–Perrin–Zaverucha, "The Signal Private Group System", CCS 2020 | Membership of a shared vault proven to the server without revealing which member — the server stores encrypted entries and verifies an algebraic-MAC credential | 41.5 shared vaults, 42.2 |
| **Privacy Pass** / **RSA blind signatures** | RFC 9576–9578 (2024); RFC 9474 (Oct 2023); Chaum's blind-signature patent US 4,759,063 **expired** (1983 filing) | Pay identified (Stripe), use anonymously: the account is issued blinded tokens for quota and time; redemption is unlinkable to payment — Cape's disassociation done in cryptography; also rate limiting with no IP log (43.3) | 43.5, 43.3 |
| Oblivious HTTP | RFC 9458 (2024) | A relay (Cloudflare runs one) strips the client IP before the origin sees a metadata request | 43.3 |
| ORAM / oblivious storage | Goldreich–Ostrovsky 1996; Path ORAM 2013 | Hides access patterns at ~log N bandwidth cost; not for bulk objects, plausible for the vault's small metadata store later | 41.x research |

## 5. Durability and multi-vendor placement

| Technique | Year | Why it matters | Lands in | Free? |
|---|---|---|---|---|
| **AONT-RS** | Resch (Cleversafe) & Plank, FAST 2011; Rivest's all-or-nothing transform 1997 | All-or-nothing transform then Reed-Solomon: no single shard (vendor) reveals anything even if the data were unencrypted, and the keys are inside the package — a second confidentiality layer across iDrive/Lyve/Geyser at ~zero key management | Phase 11 (11.2/11.3) | the paper is academic; Cleversafe's dispersed-storage patents (hundreds, filings 2005–2015; e.g. 8,625,636 filed 2011, 8,813,204 filed 2013) — **earliest expire 2025–2026, later ones live into the 2030s; verify any claim we touch** |
| Reed-Solomon | 1960 | our RS 4+4 parity (WP-VAULT-1) | shipped | no patent |
| Shamir over the parity | — | k-of-n secret sharing of the per-object key alongside AONT-RS | Phase 11 | no patent |
| **Proofs of retrievability / possession** | Ateniese et al. PDP, CCS 2007; Shacham–Waters compact PoR, ASIACRYPT 2008 | A customer (or our own auditor) proves a backend still holds the bytes with a sampled challenge, without downloading — the honest "verification report" (14.4) and the Phase 35 proof story; Geyser's tape makes challenges a scheduled job | 14.4, 35, 38 audit | academic |
| LRC (Microsoft), Clay/MSR, RaptorQ | 2012–2016 | plan F-11 verdict stands: RS everywhere, LRC patented into the 2030s (avoid), Clay open, RaptorQ patents to 2028/2030 under IETF non-assert | Phase 11 | — |

## 6. Verifiability and operations

| Technique | Status | Why it matters | Lands in |
|---|---|---|---|
| **Code Verify** (Meta + Cloudflare) | shipping for WhatsApp Web (browser extension comparing served JS to a published manifest on Cloudflare) | The one defence against the "server serves malicious JS to one user" hole of browser E2EE (10.0.2) | 10.0.2 |
| **WAICT** — Web Application Integrity, Consistency, Transparency | Cloudflare proposal, Oct 2025, W3C-backed draft (integrity manifest of asset hashes + transparency log) | Site-wide version of the above; adopt the manifest shape now, the browser enforcement when it exists | 10.0.2 |
| Reproducible builds + Sigstore (Rekor transparency log) | shipping | "the hash of what runs is the hash of what is published" for the CLI/SDK | 10.0 verifiability |
| **systemd-creds with TPM2** | systemd ≥ 250; `LoadCredentialEncrypted=` | Keep `ENCRYPTION_MASTER_KEY` out of `.env` plaintext: sealed to the box's TPM2 (+ the host secret), decrypted into the unit's credential directory at start. Zero cost; check the SLC box has a TPM2 (`/dev/tpmrm0`) — the F-14 install step | F-14 runbook, `vaultaire-switch` |
| YubiHSM 2 | ~€940–1,000 (FIPS) | A hardware wrap for the master key when a second person must be unable to export it; later | 32.6 |
| Certificate-Transparency-style log of the audit trail | Trillian / Rekor | Publish Merkle roots of `audit_logs` so an operator cannot silently rewrite history — the transparency report's evidence | 43.4 |
| Differential privacy on aggregates | Dwork 2006; RAPPOR 2014 | Noise on `site_stats_daily` and the transparency report's small counts so a published table cannot single anyone out | 43.4 |

## 7. Cautionary tales to cite in the design review

- **MEGA 2022** — unauthenticated key wraps + one master key for everything → every user key recoverable. Rule: AEAD + AAD on every wrap, one purpose per key.
- **LastPass 2022** — vault blobs stolen; old accounts had low PBKDF2 iteration counts and unencrypted URLs → offline cracking. Rule: Argon2id at SENSITIVE, a second secret (correction 7), no plaintext metadata in the vault blob.
- **Dropbox 2011 ("Dark Clouds")** — client-side dedup let a hash stand in for the file. Rule: proof of ownership before any dedup hit the client did not earn.
- **Property-preserving encryption in databases (2015–2017)** — ORE/OPE columns recovered from public statistics. Rule: correction 1.
- **Proton 2021** — a no-logging policy was compelled into prospective logging of one activist's IP. Rule: Phase 43 — a column that does not exist cannot be compelled.
- **Ente's own audit (2023)** — the one high finding was weak passwords allowed in the web app. Rule: a password strength floor is part of the crypto, not the UX.

## Sources

Ente: [architecture](https://ente.com/architecture) · [SRP](https://ente.com/blog/ente-adopts-secure-remote-passwords/) · [audit](https://ente.com/blog/cryptography-audit/) · [privacy](https://ente.com/privacy). Cape: [FAQ](https://www.cape.co/faqs) · [Built to Forget](https://www.cape.co/blog/built-to-forget-cape-is-a-new-kind-of-mobile-carrier) · [privacy policy](https://www.cape.co/privacy-policy).
Chunking / dedup: [Avamar US 6,810,398](https://image-ppubs.uspto.gov/dirsearch-public/print/downloadPdf/6810398) · [Data Domain US 6,928,526 family (7,065,619)](https://patents.google.com/patent/US7065619) · [Sparse Indexing, FAST 2009](https://www.crss.ucsc.edu/pub/lillibridge-fast09.html) · [FastCDC, ATC 2016](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia) · [Dedup-ratio estimation, MSST 2012](https://doi.ieeecomputersociety.org/10.1109/MSST.2012.6232381) · [ClearBox, CCS 2015](https://doi.org/10.1145/2810103.2813630).
Convergent / MLE: [Farsite paper](https://www.microsoft.com/en-us/research/?p=152629) · [Microsoft US 6,983,365](https://image-ppubs.uspto.gov/dirsearch-public/print/downloadPdf/6983365) · [Tahoe "convergent encryption reconsidered" 2008](https://www.metzdowd.com/pipermail/cryptography/2008-March/013794.html) · [Harnik–Pinkas–Shulman-Peleg 2010 (summary)](https://cs.runi.ac.il/node/2415) · [Armknecht et al. ASIA CCS 2017](https://ntnuopen.ntnu.no/ntnu-xmlui/handle/11250/2488888) · [Proofs of Ownership, CCS 2011](https://eprint.iacr.org/2011/207) · [Liu–Asokan–Pinkas, CCS 2015](https://eprint.iacr.org/2015/455) · [Boneh–Lipton 1996](https://crypto.stanford.edu/~dabo/pubs/abstracts/backups.html).
Keys / recovery: [OPAQUE RFC 9807](https://www.rfc-editor.org/rfc/rfc9807.html) · [SRP IPR statement](https://datatracker.ietf.org/ipr/31/) · [1Password white paper](https://1password.com/files/1password-white-paper.pdf) · [WhatsApp E2EE backups 2021](https://engineering.fb.com/2021/09/10/security/whatsapp-e2ee-backups/) · [Meta 2026 backup key vault](https://engineering.fb.com/2026/05/01/security/meta-strengthening-end-to-end-encrypted-backups/) · [Signal SVR3, OSDI '24](https://www.usenix.org/conference/osdi24/presentation/connell) · [Juicebox HSM realm](https://github.com/juicebox-systems/juicebox-hsm-realm) · [Juicebox paper](https://eprint.iacr.org/2025/348) · [SLIP-0039](https://slips.readthedocs.io/en/latest/slip-0039/) · [FROST RFC 9591](https://www.rfc-editor.org/rfc/rfc9591.html) · [X-Wing draft-11](https://www.ietf.org/archive/id/draft-connolly-cfrg-xwing-kem-11.html) · [Signal key transparency 2026](https://bleepingcomputer.com/news/security/signal-adds-new-security-feature-to-thwart-man-in-the-middle-attacks) · [HCTR2 paper](https://csrc.nist.gov/csrc/media/Events/2023/third-workshop-on-block-cipher-modes-of-operation/documents/accepted-papers/Length%20Preserving%20Encryption.pdf) · [Cryptomator vault format](https://docs.cryptomator.org/security/vault/) · [MEGA attacks 2022](https://eprint.iacr.org/2022/959).
Lookups / search / compute: [SimplePIR, USENIX Sec 2023](https://eprint.iacr.org/2022/949) · [Checklist, USENIX Sec 2021](https://eprint.iacr.org/2021/345) · [Apple Live Caller ID (OHTTP + Privacy Pass + PIR)](https://developer.apple.com/icloud/identity/) · [Naveed–Kamara–Wright 2015](https://www.microsoft.com/en-us/research/publication/inference-attacks-property-preserving-encrypted-databases/) · [Grubbs et al. ORE attacks](https://eprint.iacr.org/2016/895.pdf) · [PURBs / Padmé, PoPETs 2019](https://www.petsymposium.org/popets/2019/popets-2019-0056.pdf) · [Signal Private Group System, CCS 2020](https://eprint.iacr.org/2019/1416) · [RSA blind signatures RFC 9474](https://www.rfc-editor.org/rfc/rfc9474) · [Privacy Pass issuance RFC 9578](https://datatracker.ietf.org/doc/rfc9578/).
Durability: [AONT-RS, FAST 2011](https://web.eecs.utk.edu/~jplank/plank/papers/FAST-2011.pdf) · [Cleversafe patent notices](https://www.storagenewsletter.com/?p=149009) · [PDP 2007](https://eprint.iacr.org/2007/202) · [Compact PoR 2008](https://eprint.iacr.org/2008/073).
Verifiability / ops: [Code Verify](https://www.iphoneincanada.ca/news/whatsapp-code-verify/) · [Cloudflare on JavaScript trustworthiness / WAICT (Oct 2025)](https://blog.cloudflare.com/improving-the-trustworthiness-of-javascript-on-the-web) · [systemd credentials](https://third-party-mirror.googlesource.com/systemd/+/refs/heads/master/docs/CREDENTIALS.md) · [systemd-creds(1), Ubuntu](https://manpages.ubuntu.com/manpages/resolute/man1/systemd-creds.1.html).
