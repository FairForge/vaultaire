# Vaultaire

[![CI](https://github.com/FairForge/vaultaire/actions/workflows/ci.yml/badge.svg)](https://github.com/FairForge/vaultaire/actions/workflows/ci.yml)
[![Go 1.24+](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](https://go.dev/)
[![S3 Compatible](https://img.shields.io/badge/S3-Compatible-orange.svg)](https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html)
[![Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Universal storage orchestration engine. One S3-compatible API, multiple storage backends.

## What is Vaultaire?

Vaultaire provides a single S3-compatible API that routes data across multiple storage backends — local disk, AWS S3, Seagate Lyve Cloud, and more. It handles multi-tenant isolation, streaming I/O, billing, and backend failover so you don't have to.

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   S3 API    │────▶│   Engine    │────▶│   Drivers   │
│  (bucket/   │     │ (container/ │     │  (local, s3 │
│   object)   │     │  artifact)  │     │  lyve, ...) │
└─────────────┘     └─────────────┘     └─────────────┘
```

**S3 API** — Standard S3 protocol. Works with AWS CLI, SDKs, rclone, s3cmd, JuiceFS.

**Engine** — Orchestrates routing, tiering, caching, and replication across backends.

**Drivers** — Pluggable storage backends. Add new ones by implementing the `storage.Backend` interface.

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
- **Multi-backend routing** — local filesystem, AWS S3, Seagate Lyve Cloud, Quotaless, OneDrive
- **Multi-tenant isolation** — namespaced storage with per-tenant quotas and billing
- **Streaming I/O** — processes 1KB to 1TB files without buffering into memory
- **Stripe billing** — metered subscriptions, usage-based invoicing, webhook-driven plan changes
- **Circuit breakers** — exponential backoff, health-check failover across backends
- **Object versioning** — enable/suspend per bucket, version-aware GET/DELETE
- **Object lock** — GOVERNANCE/COMPLIANCE retention modes, legal hold
- **Bucket notifications** — webhook delivery on object events
- **Redis caching** — LRU metadata cache reducing database load
- **Monitoring** — Prometheus metrics, structured logging (Zap)

## Configuration

Vaultaire is configured via environment variables. Storage mode is auto-detected based on which credentials are present:

```bash
# PostgreSQL (optional — degrades gracefully without it)
DB_HOST=localhost DB_PORT=5432 DB_NAME=vaultaire DB_USER=viera

# Storage backends (set one or more)
S3_ACCESS_KEY=... S3_SECRET_KEY=...
LYVE_ACCESS_KEY=... LYVE_SECRET_KEY=... LYVE_REGION=us-east-1

# Billing (optional)
STRIPE_SECRET_KEY=sk_...
```

See [CLAUDE.md](CLAUDE.md) for the full environment variable reference.

## Project Structure

```
cmd/vaultaire/       Entry point — driver init, DB connect, HTTP server
internal/
  api/               S3 protocol handlers, auth middleware, error responses
  engine/            Backend orchestration, tiering, caching
  drivers/           Storage provider implementations
  auth/              User registration, JWT, S3 signature validation, MFA
  billing/           Stripe integration, metered subscriptions
  database/          PostgreSQL migrations
  tenant/            Multi-tenant context and isolation
```

## Development

```bash
make build          # Build binary
make test           # Quick tests with race detector
make test-unit      # Unit tests only
make lint           # golangci-lint
make fmt            # Format code
```

TDD is the standard workflow. Tests use [testify](https://github.com/stretchr/testify) with Arrange/Act/Assert.

## License

[Apache 2.0](LICENSE)
