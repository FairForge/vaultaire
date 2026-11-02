# WP-VAULT-3 — the parity copy as a serving copy: the fast rebuild and the vault read order

Closeout note, 2026-11-02. The prompt is `docs/reviews/WP-VAULT-3-PROMPT.md` (Opus edition, written
by the session that finished the Vault E2E proof). The measurements are in
`bench-results/VAULT-PARITY-SERVE-2026-11.md`. **No migration**: the next number stays **123**.

| PR | What | Merge |
|---|---|---|
| #1111 (PR 0) | the E2E record corrected, `cmd/tools/parity-read-bench`, `BenchmarkParityRebuild` | `df4c999f` |
| #1112 (PR A) | the fast rebuild: one opener, parallel ranged shard reads, the reused decode buffer, the breaker settled once, metrics + rule | `12d1c2ca` |
| #1114 (PR B) | `vault_parity_serve` (the vault read order), HEAD's restore lookup bounded to 3 s | `c6ad85f5` |
| PR C | this note, the prod proof (§ 2 of the bench record), STATUS / plan / CLAUDE.md | — |

## Step 0 answers (what PR 0 found)

- **There is no failover chain.** The record's "7.5 s for the failover chain" was wrong: `engine.Get`
  asks the recorded backend alone (WP-R6-1). Geyser's 404 took under 1 s; the rest of the 7.5 s
  before the Warn line was four shard GETs opened in sequence plus the first stripe (6.5 s).
- **The cause is the lockstep pull.** On cold connections (a new pool per case, the shape of a rare
  parity read on prod), the pre-WP reader read Lyve at 3.8 MB/s with a ~7 s first stripe,
  reproducing prod's 4.7 MB/s and 6.5 s. The same four streams drained independently read at
  ~100 MB/s. With warm connections every shape read at 30–50 MB/s. That is why one process benching
  case after case showed the effect only in its first case.
- **H1's HTTP/2 half is wrong.** The Lyve client has been HTTP/1.1 since the 2026-08-05 ALPN audit
  (`s3ObjectHTTPClient` = `WithHTTP1Only`), so the four shards are four connections. The likeliest
  mechanism, not proven: a cold connection's receive window grows only as fast as the application
  drains it, and the lockstep reader drains each stream a quarter of the time.
- **H2: a minor share** (~0.85 s of sequential opens against ~0.2 s concurrent). **H3: rejected** (a
  bare SDK client is as slow as the driver).
- **The codec is not the bound.** 5.1 GB/s in memory before PR A, 11.2 GB/s after (9.8 MB allocated
  per 100 MiB, against 315 MB).

## Decisions as built (deltas named)

- **D-V3-1** as written: parfetch ranges of whole stripes, `rangeIdentity` per shard, the large-GET
  hedge and budget (`VaultParity.LargeGet` = the server's `s.largeGet`). **Delta:** the parfetch
  window is `perShard + 1` parts. The window counts the part the decoder holds, so a window of
  `perShard` parts kept ONE range in flight (`TestParityRebuild_RangesInParallelNotLockstep` caught
  it: 748 ms against a 675 ms bound). **Delta:** an `errRangeIdentity` fails the WHOLE read rather
  than dropping that shard (the decision says "the read fails"; a shard dropped would let the
  remaining three plus a tape range serve a stripe of a changed object).
- **D-V3-2** as written: 4 pooled stripes per shard from the budget, one unbudgeted stripe when the
  budget cannot spare them.
- **D-V3-3** as written: `newParityRead` → `openParityShards` → `newParityRebuilder`, for `Open` and
  the scrub's `rebuild`. **Delta:** the scrub's digest read `readShard` (WP-VAULT-2) also called
  `drv.Get`, so the structural test could not pass with it in `vault_parity_scrub.go`. It moved
  unchanged to `vault_parity_read.go`, which is now the one file that reads a parity shard.
  `newParityRebuilder` takes `[]*parityShardSource`, and the opener's `parityShardSet` is attached
  for the settle.
- **D-V3-4** as written: `prime()` decodes the first stripe inside `Open`.
  `TestVaultParity_OpenUsesTheDataRangesThatAreReadable…` now expects `Open` itself to fail on a
  first-stripe shortfall. On prod every first stripe was ≤ 1.5 s (mean 1.21 s).
- **D-V3-5** as written: one outcome per leg per rebuild. A failure is the first shard error that is
  not EOF, not `errRangeIdentity`, not a cancel; otherwise a success when the window was delivered.
  **Delta in the test:** the breaker opens at 5 failures, so the test pins "four rebuilds stay
  closed, the fifth opens" (one charge per rebuild, not per shard).
- **D-V3-6** as written: `ServeFirst` (nil flags = OFF) + `CanServe` (one row read) on both GET
  branches. A wrong SSE-C key decided on the parity is final. The parity is never tried twice, and
  `unavailable` is counted once. Beyond the prompt's list,
  `TestVaultGet_FlagOn_ParityFailsBeforeTheStatusLine_GeyserServesOnce` pins the "never twice" rule.
- **D-V3-7** as written: `headRestoreStatus` runs the lookup on its own goroutine, so the 3 s bound
  holds even for a driver that ignores its context. `vaultaire_vault_head_restore_lookups_total`.
- **D-V3-8** as written (all registered; `TestMetrics_VaultParityRebuildRegistered`). The rate gauge
  and the `stream` phase are set only for a rebuild that delivered a whole window of ≥ 16 MiB, so a
  sealed header's one-stripe window never reads as a slow rebuild. `VaultParityRebuildSlow` + three
  promtool cases.
- **D-V3-9** held: no format, stripe, name or row changed.

## The prod proof (bench record § 2)

**Met:**
- 96 MiB from parity: server latency 1.79–1.85 s (flag path 1.71 / 2.43 s), first stripe ≤ 1.5 s.
- The flag path: served from parity with the Geyser blob present; `served_first` +2.
- Clean delete: every row and every byte gone.

**Not met:** 1 GiB ≥ 100 MB/s. It read at 65–103 MB/s, over the 50 MB/s fail line.

## Residuals

- **RB-V3-1** the 1 GiB rebuild misses 100 MB/s. Lever: ranges in flight per shard (lab: 4 per
  shard read 118–162 MB/s).
- **RB-V3-2** a no-range leg's 4-stripe ring is 45–55 MB/s on 1 GiB (lab), against 142–146 for free
  streams. sync and permafrost only.
- **RB-V3-3** no customer path stores a sealed Vault object over ~100 MB with parity: SSE-C
  multipart is 0x05 (not protected, RP19-1), Cloudflare refuses a single PUT over 100 MB, and SSE-S3
  waits for F-14. The proof's 1 GiB object went in by the origin slot.
- **RB-V3-4** the whole `internal/api` package does not finish under `-race` in 25–45 min on the
  Mac when another session's suite shares it; CI was the gate for PR A and PR B.
- **RB-V3-5** (found, not ours) `internal/payments` `TestManualPayment_GrantsWithoutRow_ThenExtends`
  fails by exactly 1 h on a machine in local time after US DST ended (2026-11-01). CI runs in UTC.
- **RB-V3-6** (found, not ours) `setupChunkingFixture` inserts a fixed e-mail
  (`chunking@test.local`). A run cut by a timeout leaves the row, and every later run on that
  database fails on `tenants_email_key` until it is deleted.

## Open questions

1. **Ranges in flight for the parity read** (RB-V3-1). The parity read takes `LARGE_GET_PARALLEL_PARTS
   / m` per shard; raising the shared knob also widens the plain parallel GET. A parity-only knob
   would need a new env variable. Owner's call.
2. **Turning `vault_parity_serve` on beyond the bench tenant.** It makes a tape object answer 200 from
   the parity copy, which changes what "Vault" promises (no restore wait). It is off for every
   tenant, the copy on the site says nothing, and there is no flag row on prod after the proof.
3. **RB-V3-3** decides whether Vault can take a large sealed object at all before F-14.
