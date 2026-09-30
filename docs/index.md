# Vaultaire documentation

Vaultaire is the storage engine behind stored.ge: one Go binary, one PostgreSQL, an S3-compatible API in front of several storage backends. Root `CLAUDE.md` is the operational summary; these files go deeper.

| File | What it is |
|------|------------|
| [API.md](API.md) | The customer-facing surface: S3 endpoint and auth, operations, JSON API, error codes, rate limits, SDK snippets |
| [ARCHITECTURE.md](ARCHITECTURE.md) | The three layers, placement on PUT, failover and the breaker, reads, health, chunking/dedup/encryption |
| [DRIVERS.md](DRIVERS.md) | The `engine.Driver` contract, every registered backend, conformance and transport tables, how to add one |
| [DATABASE.md](DATABASE.md) | PostgreSQL schema by domain, the migration runner, invariants |
| [CONFIG.md](CONFIG.md) | Every environment variable (the only configuration there is) and the storage-mode auto-detect |
| [DEPLOY.md](DEPLOY.md) | What production is, the push-to-main pipeline, box layout, backups, monitoring, troubleshooting |
| [SCALE_TESTING.md](SCALE_TESTING.md) | The launch load gates (`tests/load/`), the nightly k6 job and the `internal/loadtest` library |
| [PRODUCT_FEATURES.md](PRODUCT_FEATURES.md) | Roadmap and feature notes — not customer-facing |
| [DESIGN.md](DESIGN.md) | The visual and product language of the site and dashboard |
| [DASHBOARD_PLAN.md](DASHBOARD_PLAN.md) | The dashboard plan (house checkout and overview), phases and status |
| [guides/](guides/) | Customer how-tos: rclone, JuiceFS mount, JuiceFS use cases (the served copies live under `/docs` on stored.ge) |
| [reviews/](reviews/) | The code-review sessions R0–R14 (`CODE_REVIEW_PLAN.md` is the schedule); each names its work packages by id |

Quick start: `make build` → `JWT_SECRET=dev ./bin/vaultaire` (local disk backend on `:8000`, no database needed to serve S3). Before running DB-backed tests: `make test-db` once (creates and migrates `vaultaire_test`), then `make test`. `docs/archive/` is history, `docs/references/` vendor PDFs.
