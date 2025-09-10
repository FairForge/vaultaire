# Vaultaire - Universal Storage Orchestration Engine

[![Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go 1.21+](https://img.shields.io/badge/go-1.21+-00ADD8.svg)](https://go.dev/)
[![S3 Compatible](https://img.shields.io/badge/S3-Compatible-orange.svg)](docs/s3-api.md)

> Turn any storage backend into intelligent, unified infrastructure

## What is Vaultaire?

Vaultaire is a storage orchestration engine that provides a single S3-compatible API across multiple storage backends. Think of it as a universal translator for storage - use one API, store anywhere.

## Three Ways to Use Vaultaire

### 🚀 stored.ge - Managed Storage for Developers
- $3.99/TB/month
- 100GB Seagate Lyve Cloud included
- Perfect for indie developers
- [Sign up at stored.ge](https://stored.ge)

### 🏢 stored.cloud - Enterprise Storage Platform
- Starting at $19.99/TB/month
- 100% enterprise infrastructure
- SLA guarantees & compliance
- [Learn more at stored.cloud](https://stored.cloud)

### ��️ Vaultaire Core - Self-Hosted Solution
- Open source (Apache 2.0)
- Bring your own backends
- Deploy on your infrastructure
- [Get started below](#quick-start)

## Quick Start (Self-Hosted)

```bash
# Run with Docker
docker run -d \
  -p 9000:9000 \
  -v /etc/vaultaire:/config \
  vaultaire/core:latest

# Or build from source
git clone https://github.com/fairforge/vaultaire
cd vaultaire
make build
./bin/vaultaire serve
Features

✅ Universal S3 API - Works with any S3 client
✅ Multi-Backend Support - Mix Wasabi, R2, Hetzner, etc.
✅ Intelligent Tiering - Automatic hot/cold data management
✅ Erasure Coding - Built-in redundancy
✅ Event Streaming - Full audit trail
✅ ML-Ready - Predictive caching (Enterprise)

Architecture
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   S3 API    │────▶│   Engine    │────▶│   Drivers   │
│  (bucket/   │     │ (container/ │     │  (Multiple  │
│   object)   │     │  artifact)  │     │  Backends)  │
└─────────────┘     └─────────────┘     └─────────────┘
Read full architecture docs →
Supported Backends

✅ Local filesystem
✅ S3 / S3-compatible
✅ Seagate Lyve Cloud
✅ Wasabi
✅ Cloudflare R2
✅ Backblaze B2
✅ MinIO
🔄 Hetzner Storage Box (coming soon)
🔄 Google Cloud Storage (coming soon)

Documentation

Architecture Overview
API Reference
Configuration Guide
Driver Development
Deployment Guide
Contributing Guidelines
Code of Conduct

Contributing
See CONTRIBUTING.md for details on our code of conduct and the process for submitting pull requests.
License
Apache 2.0 - see LICENSE
This project uses Apache 2.0 to ensure:

✅ Enterprise-friendly usage
✅ Patent protection for all users
✅ Clear contribution guidelines
✅ Compatible with commercial use

Why We Built This
Storage is fragmented. Every provider has different APIs, pricing, and capabilities. Vaultaire unifies them all behind a single, intelligent interface.
We believe in making enterprise-grade storage accessible to everyone through intelligent orchestration.
Status

🚧 Current Phase: MVP Development
📊 Progress: Step 47 of 500
🎯 Next Milestone: S3 DELETE/LIST operations
�� Launch Target: Q1 2025

Built by @fairforge | Blog | Twitter
