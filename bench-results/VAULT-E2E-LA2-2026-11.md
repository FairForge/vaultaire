# Vault end-to-end on prod LA2: write, tape, refused GET, recall, parity-serve, delete (2026-10-30 → 2026-11-02)

The WP-VAULT-2 proof the 2026-10-30 session planted (memory `project_vault_sellable_2026_10_29.md`,
state dir on SLC `~/vaultaire-bench/vault-e2e/`). One SSE-C GLACIER object of the bench tenant
written through the customer endpoint, watched hourly by `cmd/tools/geyser-restore-probe` until
Geyser moved it to tape, then the customer restore path, the scrub's rebuild, a live parity-serve
after the Geyser blob was deleted by hand, and the DELETE. Finished by hand on 2026-11-02 because
the probe gave up on the recall after 50 min (its own `-restore-timeout`), wrote an error record and
disabled its timer; nothing else was wrong.

## The object

| | |
|---|---|
| Key | `s3://vault-e2e-20261030/e2e/sealed-96m.bin`, bench tenant `tenant-14a623b16b3f7012`, via `https://s3.stored.ge` |
| Class / encryption | `GLACIER` (floor `vault`, backend `geyser`), SSE-C (`AES256-SSE-C-v3`), key MD5 `r0+kGoh3Nj+SWh/+K6mPVg==` |
| Plaintext | 100,663,296 B, SHA-256 `ca4898ed6e8438e0bd33c4434dce2773ef653cca6723912348544616fe98dcec`, ETag `16ff3244f9cb3f71714197302f463908` |
| Blob on LA2 | `t-<tenant>/<tenant>_vault-e2e-20261030/e2e/sealed-96m.bin@e2jyez9gj0jq7hh3vd2ks42azx`, 100,664,861 B sealed (1,565 B overhead), Geyser ETag `dc89c344…`, `spectra-storage: 1.191,1.5` |
| Parity | `vault_parity` row `complete`, legs `{lyve,lyve,lyve,lyve}`, prefix `8b8eddf7eefc23c8438c70fc/16ff3244…`, protected 01:58:40 (60 s after the PUT) |

The write was allowed on prod without a master key because it was SSE-C: #1061's sealed-only guard
refuses an unkeyed GLACIER write with 503 until F-14.

## Timeline (UTC)

| When | What | Measured |
|---|---|---|
| 10-30 01:57:40 | PUT through stored.ge | head row + Geyser blob; parity protect 60 s later |
| 10-30 02:23 → 10-31 10:23 | 35 hourly HEAD+GET checks | readable from the landing zone; `x-amz-restore: ongoing-request="false", expiry-date=<the check's own time>` |
| 10-31 11:23:08 | on tape | GET → **403 `InvalidObjectState`** in 0.70 s. Landing-zone residency 33 h 25 min |
| 10-31 11:23:10 | probe's `RestoreObject` (Days 2) | **409 `RestoreAlreadyInProgress`** at 1.65 s. Stored submits no restore on a GET (`s3_restore.go` only on RestoreObject), so Geyser started the recall on the refused GET itself |
| 10-31 11:24 → 12:13 | 50 HEAD polls, 60 s apart | `ongoing-request="true"` throughout; probe wrote its error record at 12:13:52 and disabled the timer |
| 10-31 16:27:40 | recall complete | from the restored copy's fixed `expiry-date` (11-02 16:27:40 minus Days 2). **Recall 5 h 04 min** after the refused GET |
| 11-02 02:40:58 → 02:41:17 | `vault_parity` scrub (WP-VAULT-2) | shard digests OK, **rebuilt from parity alone against `object_sha256`**, `scrub_failures 0`, `rebuilt_at` set |
| 11-02 03:05 | customer GET with the key (aws-cli on SLC) | 200 in **5.84 s** wall, SHA-256 **= baseline** |
| 11-02 03:07:31 | blob deleted on Geyser by hand (prod key) | Geyser HEAD 404 |
| 11-02 03:07:34 | customer GET with the key | 200, **`x-vaultaire-served-from: parity`**, SHA-256 **= baseline**; server latency 27.04 s. The journal (slot 8001) splits it: Geyser answered 404 in under a second (the engine asks the recorded backend and no other — WP-R6-1, `readCandidates`); the four Lyve shard GETs were opened one after another and the first RS stripe (4 × 1 MiB) plus the sealed header decoded by 03:07:41.523 (`vault object served from its parity copy`) — **6.5 s for the first 4 MiB**; the remaining 92 MiB streamed by 03:08:01.051 — **19.5 s ≈ 4.7 MB/s** (corrected 2026-11-02 by WP-VAULT-3 PR 0: an earlier version of this row said 7.5 s for a failover chain; there is none) |
| 11-02 03:08:01 | HEAD after | 200 in 2.76 s, no `Restore` header: the live restore-state lookup of an archive-class object paid a Geyser round trip for a blob Geyser no longer had |
| 11-02 03:08:57 | DELETE through stored.ge, then the bucket | HEAD 404; `object_head_cache` 0, `vault_parity` 0, `buckets` 0, `vault_parity_orphans` 0; Lyve parity folder 0 objects; Geyser prefix `t-<tenant>/` 0 objects |
| 11-02 03:10 | `ssec.key` and `obj.bin` shredded on SLC | crypto-shred of the 96 MiB left on the cartridge (a delete reclaims no tape — `GEYSER-LA2-TAPE-RECLAIM-2026-10-25.md`) |

## What this proves

1. **The Vault path end to end through the customer endpoint, with SSE-C:** write → landing zone →
   tape → refused GET → recall → bit-identical read. The restore window and the `x-amz-restore`
   header behave as a Glacier client expects.
2. **The parity copy is a complete copy, twice:** the scrub rebuilt the object from the four parity
   shards alone and matched its recorded digest; a live GET after the Geyser blob was gone was
   served from parity with the same bytes.
3. **DELETE is complete:** head row, parity row, shards and the Geyser object all gone; nothing
   for the orphan reconcile to find.

## Findings

- **Recall took 5 h, not minutes.** The 2026-10-25 LA2 multipart restore took 459 s on a
  freshly written object. This object had been on tape for hours before the recall and took
  5 h 04 min. Any customer copy must say hours. The probe's 50 min timeout was sized to the
  earlier number; next time `-restore-timeout 12h`.
- **Geyser starts the recall on a refused GET.** Convenient for a human; irrelevant to restic and
  Kopia, which see the 403 and fail. The hydrate-first design stands.
- **Parity-serve is slow: 19.5 s for 96 MiB ≈ 5 MB/s** against the 240 MB/s rebuild-from-Lyve
  bench of 2026-10-07. Worth a look before the parity copy is sold as the serving copy
  (the shards may be fetched in sequence, or the SSE-C stream open is the bound — WP-VAULT-3).
- **There is no failover chain before the parity fallback** (corrected by WP-VAULT-3 PR 0; this
  bullet first said "the failover chain costs 7.5 s"). `engine.Get` asks the recorded backend alone
  (`internal/engine/engine.go` `readCandidates`, WP-R6-1). Of the 7.5 s before the Warn line, Geyser's
  404 took under a second; the other ~6.5 s were the four shard GETs opened in sequence and the first
  stripe decoded. The measurement and the fix are `bench-results/VAULT-PARITY-SERVE-2026-11.md`.
- **HEAD of an archive-class object makes a live backend call** (2.76 s here) by design
  (Architecture Decision 2's one exception). Fine for a human, slow for a tool that HEADs often.
- **Landing-zone residency 33 h** before the object counted as on tape; the `x-amz-restore`
  header during that time reports an expiry equal to the request time.

## Still owed for Vault to sell (unchanged)

F-14 master key + escrow (unkeyed GLACIER writes are 503 until then), the Lyve promo end date in
writing, the post-Lyve parity-leg decision, and for backup-tool repositories the hot-metadata /
hydrate-first design (session 2026-11-01/02 notes). The next `vault_tape_audit` (~2026-11-05)
will find no bench object; that is expected.

## Files on SLC

`~/vaultaire-bench/vault-e2e/`: `README`, `run.sh`, `probe.log` (every check), `report.jsonl` (the
probe's error record), `report-resume.jsonl` (empty), `state/8c7e5926dfd538ef.json`. Key and
plaintext shredded; the timer is disabled and can be removed.
