# Vaultaire

[![CI](https://github.com/FairForge/vaultaire/actions/workflows/ci.yml/badge.svg)](https://github.com/FairForge/vaultaire/actions/workflows/ci.yml)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8.svg)](https://go.dev/)
[![S3 Compatible](https://img.shields.io/badge/S3-Compatible-orange.svg)](https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html)
[![Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Universal storage orchestration engine. One S3-compatible API, multiple storage backends.

## What is Vaultaire?

Vaultaire provides a single S3-compatible API that routes data across multiple storage backends — local disk, iDrive e2, Seagate Lyve Cloud, Geyser tape, Cloudflare R2 and more. It handles multi-tenant isolation, streaming I/O, billing, and backend failover so you don't have to.

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   S3 API    │────▶│   Engine    │────▶│   Drivers   │
│  (bucket/   │     │ (container/ │     │ (local,     │
│   object)   │     │  artifact)  │     │  idrive, …) │
└─────────────┘     └─────────────┘     └─────────────┘
```

**S3 API** — Standard S3 protocol. Works with AWS CLI, SDKs, rclone, s3cmd, JuiceFS.

**Engine** — Orchestrates placement by storage class, circuit-breaker failover and smart demotion across backends.

**Drivers** — Pluggable storage backends. Add new ones by implementing the `engine.Driver` interface (`internal/engine/interface.go`).

## Quick Start

```bash
# Build from source
git clone https://github.com/FairForge/vaultaire.git
cd vaultaire
make build

# Run (uses local filesystem by default)
./bin/vaultaire
```

Vaultaire starts on port 8000. Use any S3 client:

```bash
aws s3 mb s3://my-bucket --endpoint-url http://localhost:8000
aws s3 cp file.txt s3://my-bucket/ --endpoint-url http://localhost:8000
```

## Features

- **S3-compatible API** — PUT, GET, DELETE, LIST, HEAD, multipart uploads, versioning, object lock
- **Multi-backend routing** — local filesystem, iDrive e2 (primary, per-region), Seagate Lyve Cloud, Geyser tape, Cloudflare R2 (public buckets)
- **Multi-tenant isolation** — namespaced storage with per-tenant quotas and billing
- **Streaming I/O** — processes 1KB to 1TB files without buffering into memory
- **Stripe billing** — subscriptions, webhook-driven plan changes
- **Circuit breakers** — per-backend breakers, ordered failover across backends
- **Object versioning** — enable/suspend per bucket, version-aware GET/DELETE
- **Object lock** — GOVERNANCE/COMPLIANCE retention modes, legal hold
- **Bucket notifications** — webhook delivery on object events
- **PostgreSQL-backed metadata cache** — HEAD/GET metadata served from `object_head_cache` without touching the backend
- **Monitoring** — Prometheus metrics, structured logging (Zap)

## Configuration

Vaultaire is configured via environment variables. Storage mode is auto-detected based on which credentials are present:

```bash
# PostgreSQL (optional for a local try-out; required for auth, quotas and metadata)
DB_HOST=localhost DB_PORT=5432 DB_NAME=vaultaire DB_USER=postgres   # example values (see docs/CONFIG.md for defaults)

# Storage backends (set one or more)
IDRIVE_ACCESS_KEY=... IDRIVE_SECRET_KEY=... IDRIVE_REGION=us-central-1
LYVE_ACCESS_KEY=... LYVE_SECRET_KEY=... LYVE_REGION=us-west-1

# Billing (optional)
STRIPE_SECRET_KEY=sk_...
```

See [docs/CONFIG.md](docs/CONFIG.md) for the full environment variable reference.

## Project Structure

```
cmd/vaultaire/       Entry point — driver init, DB connect, HTTP server
internal/
  api/               S3 protocol handlers, auth middleware, error responses
  engine/            Backend orchestration, placement, failover
  drivers/           Storage provider implementations
  auth/              User registration, JWT, S3 signature validation, MFA
  billing/           Stripe integration
  database/          PostgreSQL migrations
  dashboard/         Customer + admin web dashboard (htmx, Go templates)
  crypto/            SSE-S3 / SSE-C, chunking + dedup index
  usage/             Quota accounting
  flags/             Runtime feature flags
  audit/             Operator audit trail
  tenant/            Multi-tenant context and isolation
```

## Development

```bash
make build          # Build binary
make test-db        # Create + migrate the local vaultaire_test database (before any DB-backed test)
make test           # Quick tests with race detector
make test-unit      # Unit tests only
make test-integration # what CI runs: every package with -race against the migrated test DB
make lint           # golangci-lint (.golangci.yml)
make gosec          # the Security workflow's gosec command
make deadcode       # unreachable functions in the product binary
make fmt            # Format code
make clean          # remove every build output, including tool binaries in the repo root
make landing        # Regenerate the landing page from internal/api/landing/ (never hand-edit landing.html)
make dash-shots     # Screenshot every dashboard page (light/dark/phone)
make dash-lighthouse # Lighthouse accessibility score per dashboard page
pre-commit install && pre-commit install --hook-type pre-push  # fmt + lint on commit, short tests on push
```

TDD is the standard workflow. Tests use [testify](https://github.com/stretchr/testify) with Arrange/Act/Assert.

## Security

Report vulnerabilities to security@stored.ge — see [/.well-known/security.txt](https://stored.ge/.well-known/security.txt).

## License

[Apache 2.0](LICENSE)
