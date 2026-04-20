# Bench Results Index

Last updated: 2026-04-20

## Current Baselines (use these for comparison)

| File | Tool | What | Date |
|------|------|------|------|
| `slc-v8-transport-20260420-045233.json` | bench-compare v8 | All S3 providers (28 endpoints), transport fixes applied | 2026-04-20 |
| `slc-v7-tuned-pixeldrain.json` | pixeldrain-bench v7 | Pixeldrain post-TCP-tuning baseline | 2026-04-19 |
| `slc-v8-transport-pixeldrain-20260420-043247.json` | pixeldrain-bench v8 | Pixeldrain with transport fixes (TLS cache etc.) | 2026-04-20 |
| `slc-allproviders-20260418.json` | bench-compare v2 | All S3 providers pre-transport-fixes (old baseline) | 2026-04-18 |
| `quotaless-slc-ceiling.json` | quotaless-bench-v2 | Quotaless raw HTTP ceiling test (393 MB/s dl) | 2026-04-18 |

A v9-fixes run (Quotaless UNSIGNED-PAYLOAD + Geyser FixedBucket + R2 fresh creds) is pending.

## Analysis Reports

| File | What |
|------|------|
| `COMPARISON-v2.md` | Multi-provider comparison analysis (25 endpoints) |
| `COMPARISON.md` | Earlier comparison (fewer endpoints) |

## By Provider

### Pixeldrain (cmd/pixeldrain-bench)
Optimization history v2→v7: cyclicReader, 1MB buffers, devNull, TCP tuning.

| File | Version | Key finding |
|------|---------|-------------|
| `slc-v8-transport-pixeldrain-*.json` | v8 | soak_download 941 MB/s (latest) |
| `slc-v7-tuned-pixeldrain.json` | v7 | soak_download 938 MB/s, concurrent_dl 808 MB/s |
| `slc-v7-pixeldrain.json` | v7 | Pre-TCP-tuning |
| `slc-v6-pixeldrain.json` | v6 | Added devNull + pooled buffers |
| `slc-v5-pixeldrain.json` | v5 | Added cyclicReader |
| `slc-v3-pixeldrain-20260418.json` | v3 | Baseline transport |
| `slc-v2-pixeldrain-20260417.json` | v2 | First structured bench |
| `mac-v*-pixeldrain.json` | v2-v7 | Mac residential runs (~64 MB/s ceiling) |

### Quotaless (cmd/quotaless-bench-v2)
Raw HTTP + SigV4 required. AWS SDK v2 is INCOMPATIBLE (see quotaless_README.md).

| File | What |
|------|------|
| `quotaless-slc-ceiling.json` | Download ceiling: 393 MB/s (64w×4MB) |
| `quotaless-slc-tcp-tuned.json` | Post TCP buffer tuning |
| `quotaless-mac-ceiling.json` | Mac residential ceiling test |
| `slc-quotaless-large-20260418.json` | SDK v2 test (shows failures — for reference) |

### All-provider (cmd/bench-compare)

| File | What |
|------|------|
| `slc-v8-transport-*.json` | Latest: transport fixes applied (2026-04-20) |
| `slc-allproviders-20260418.json` | Previous baseline (2026-04-18) |
| `slc-full-v2-20260415-160629.json` | Early full run |
| `slc-tod-20260416.json` | Time-of-day variance test |
| `slc-tuned-idrive-top2-20260417.json` | iDrive top-2 regions focused |

### Lighthouse / Filecoin

| File | What |
|------|------|
| `slc-vaultaire-01-lighthouse-*.json` | Filecoin/IPFS benchmark |
| `ike-2local-lighthouse-*.json` | Mac local lighthouse tests |

### Geyser (tape archive)

| File | What |
|------|------|
| `geyser-quick-20260415-143252.json` | Quick tape benchmark |
| `slc-full-with-geyser.json` | Full run including Geyser |

## Naming Convention

```
{host}-{version/label}-{tool}-{timestamp}.json

host:      slc = SLC datacenter, mac = MacBook, ike-2local = Mac hostname
version:   v2-v8 = pixeldrain optimization iterations
tool:      pixeldrain, lighthouse, quotaless (omitted for bench-compare)
timestamp: YYYYMMDD-HHMMSS
```

## OneDrive (permafrost-v3)

OneDrive results are NOT JSON files — they're console output from `cmd/permafrost-v3`.
See `internal/drivers/onedrive_README.md` for full optimization history (v1→v2→v3).
See `.private/PERMAFROST_TESTING_RESULTS.md` for detailed test results.
