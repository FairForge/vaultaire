# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Vaultaire is a universal storage orchestration engine providing a unified S3-compatible API across multiple storage backends (local, S3, Lyve Cloud, Quotaless, OneDrive, Geyser). It is the core of FairForge's commercial product stored.ge ($3.99/TB S3-compatible storage).

**Language**: Go 1.24.6 | **Database**: PostgreSQL 15+ | **Router**: chi/v5 | **Logging**: Uber Zap

## Build, Test, and Lint Commands

```bash
# Build
make build                # -> bin/vaultaire

# Test
make test                 # Quick: -short -race -cover ./...
make test-unit            # Unit only: -short -race -cover ./internal/...
make test-all             # Full suite with -race
make test-coverage        # Generate coverage.out and coverage.html

# Run a single test
go test ./internal/auth/... -run TestCreateUserWithTenant -v

# Run a specific package
go test -v -race ./internal/api/...

# Lint
make lint                 # golangci-lint run ./...

# Format
make fmt                  # go fmt + gofmt -s -w
```

## Architecture

### Three-Layer Design

```
API Layer (internal/api)      S3 protocol translation, auth middleware, HTTP handlers
Engine Layer (internal/engine) Backend orchestration, tiering, replication, caching, ML routing
Driver Layer (internal/drivers) Storage provider implementations (local, s3, lyve, quotaless, onedrive)
```

### Entry Point

`cmd/vaultaire/main.go` — initializes drivers from environment variables, connects to PostgreSQL (optional, degrades gracefully), starts the HTTP server. Storage mode auto-detected: Quotaless > Lyve > S3 > local.

### Dual Terminology

External (S3-compatible): Bucket, Object, Key
Internal: Container, Artifact, Path

The `storage.Backend` interface is the sacred contract all drivers implement.

### Key Database Tables

Registration persists to **four tables in order**: `users` -> `tenants` -> `api_keys` -> `tenant_quotas`. Missing any causes failures. S3 auth queries the `tenants` table (not `api_keys` or `users`) — `access_key` and `secret_key` live there.

Migrations are in `internal/database/migrations/`.

## Architecture Decisions (Non-Negotiable)

1. **TCP dial for health checks** — HTTP health checks against S3 backends are unreliable (EOF, 403 vary by vendor). TCP dial is backend-agnostic.
2. **HEAD serves from `object_head_cache`** — never fetches from backend. Size/ETag/content-type stored on PUT, queried on HEAD (~1ms).
3. **ETags computed via MD5 stream on upload** — never return MD5 of empty string.
4. **Always stream, never buffer** — use `io.Reader`, never `[]byte` in memory for large data.
5. **Always propagate context** — every service method takes `ctx context.Context` as first parameter.
6. **Always wrap errors with context** — `return fmt.Errorf("create bucket %s: %w", name, err)`

## Development Methodology

- **TDD is mandatory**: Red -> Green -> Refactor. Write failing test first.
- Tests follow Arrange/Act/Assert pattern with testify (`require` for fatal, `assert` for non-fatal).
- Pre-commit hooks run: `go fmt`, `go test ./... -short`, `golangci-lint run`.

## Git Workflow

```bash
git checkout -b step-XXX-feature-name   # Branch from main
# ... TDD cycle ...
git commit -m "feat(scope): description [Step XXX]"
gh pr create --base main
gh pr merge --squash --delete-branch --admin
```

Commit format: `type(scope): description [Step NNN]` where type is feat/fix/refactor/test/docs.

Never include `Co-Authored-By` lines mentioning Claude or AI in commit messages.

Branch protection: direct pushes to main are blocked; CI must pass before merge.

## CI/CD

GitHub Actions CI (`.github/workflows/ci.yml`) runs on every push/PR:
- PostgreSQL 15 service container
- `go build ./...`
- `go test ./...` with DATABASE_URL and JWT_SECRET env vars
- golangci-lint

GitHub Actions Deploy (`.github/workflows/deploy.yml`):
- `main` branch → builds, runs migrations, deploys to prod (SLC)
- `develop` branch → builds, runs migrations, deploys to dev server
- Migrations are idempotent (CREATE IF NOT EXISTS) — safe to re-run

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | 8000 | Server port |
| `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` | localhost, 5432, vaultaire, viera, "" | PostgreSQL connection |
| `DATA_PATH` | /tmp/vaultaire-data | Local storage directory |
| `STORAGE_MODE` | auto-detect | Force specific backend |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | — | AWS S3 credentials |
| `LYVE_ACCESS_KEY`, `LYVE_SECRET_KEY`, `LYVE_REGION` | — | Seagate Lyve Cloud |
| `QUOTALESS_ACCESS_KEY`, `QUOTALESS_SECRET_KEY`, `QUOTALESS_ENDPOINT` | — | Quotaless storage |

## Production

- Server: `slc-vaultaire-01` (Ubuntu 24.04, Salt Lake City), SSH alias `vaultaire-slc`
- Binary at `/opt/vaultaire/bin/vaultaire`, config at `/opt/vaultaire/configs/.env`
- HAProxy fronts the service; Cloudflare proxies stored.ge
- UFW firewall: ports 22, 80, 443 only
- Daily PostgreSQL backups at 3am UTC (7-day retention) in `/opt/vaultaire/backups/`
- Deploy: push to `main` triggers `.github/workflows/deploy.yml` (build → migrate → swap → health check)
- Health: `curl https://stored.ge/health`
- Cross-compile: `GOOS=linux GOARCH=amd64 go build -o vaultaire-bin ./cmd/vaultaire`
