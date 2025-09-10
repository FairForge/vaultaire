# Vaultaire Architecture

## Overview

Vaultaire uses a layered architecture that separates concerns and enables flexibility. This document describes the core components, design decisions, and deployment models.

## Core Components

### 1. API Layer (S3 Compatible)
- Handles S3 protocol
- Translates to internal operations
- Manages authentication
- Returns S3-compliant responses

### 2. Engine Layer
- Orchestrates operations across backends
- Manages tiering policies
- Handles replication strategies
- Emits events for monitoring

### 3. Driver Layer
- Interfaces with storage backends
- Abstracts provider differences
- Handles retries and error recovery
- Provides consistent interface

## Hub and Spoke Architecture

### The Hub (Stateful Brain)
**Server**: Intel Special 6 Core - NYC Metro
**Specs**: 12 Cores @ 3.5GHz, 256GB RAM, 8TB SSD
**Role**:
- Master PostgreSQL database (source of truth)
- High-speed RAM cache (hot data)
- Orchestration logic & decision making
- User authentication & billing
- Metrics aggregation
- SINGLE INSTANCE (no HA needed initially)

### The Spokes (Stateless Workers)
**Servers**: 6x Distributed VPS
**Locations**: Kansas City, Montreal, Amsterdam, Mumbai, etc.
**Role**:
- Handle S3 API requests
- Execute storage operations
- Run background jobs
- Scale horizontally
- Can lose any spoke without data loss
- MULTIPLE INSTANCES (auto-scaling ready)

### Data Flow
User Request → Nearest Spoke (stateless)
↓
Hub validates & routes (stateful)
↓
Spoke executes on backend
↓
Result to user

### Why This Architecture?
- **Cost Efficient**: One expensive stateful server, many cheap stateless
- **Resilient**: Spokes can fail/restart without data loss
- **Scalable**: Add more spokes as needed
- **Simple**: No complex distributed state management initially

## Dual Terminology Design

We use dual terminology to future-proof the system:

| External (S3) | Internal (Engine) | Purpose |
|---------------|-------------------|---------|
| Bucket | Container | Namespace for objects |
| Object | Artifact | Stored item |
| Key | Path | Location identifier |

This separation allows us to:
- Support non-S3 protocols in the future
- Maintain clean internal abstractions
- Evolve independently from S3 spec

## Storage Tiering

```yaml
policies:
  - hot: 0-7 days (SSD/Premium)
  - warm: 7-30 days (Standard)
  - cold: 30-90 days (Archive)
  - frozen: 90+ days (Deep Archive)
Automatic Tiering Logic

New uploads go to hot tier
After 7 days, migrate to warm
After 30 days, migrate to cold
After 90 days, migrate to frozen
Access patterns can promote back to hot

Event System
Every operation emits events for:

Audit Logging: Complete operation history
ML Training Data: Learn access patterns
Usage Analytics: Understand user behavior
Billing: Track resource consumption
Monitoring: System health and performance

Event Structure
gotype Event struct {
    ID        string
    Type      string
    Container string
    Artifact  string
    Operation string
    TenantID  string
    Timestamp time.Time
    Data      map[string]interface{}
}
Backend Drivers
Current Drivers

LocalDriver: Filesystem storage for development
S3Driver: AWS S3 and compatible
LyveDriver: Seagate Lyve Cloud
QuotalessDriver: Budget cloud storage
OneDriveDriver: Microsoft OneDrive

Driver Interface
gotype Driver interface {
    Name() string
    Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
    Put(ctx context.Context, container, artifact string, data io.Reader) error
    Delete(ctx context.Context, container, artifact string) error
    List(ctx context.Context, container string) ([]string, error)
    HealthCheck(ctx context.Context) error
}
Deployment Models
Managed (stored.ge/cloud)

We handle infrastructure
Automatic updates
Managed backends
SLA guarantees

Self-Hosted (Vaultaire Core)

You provide infrastructure
You manage backends
Full control
Community or enterprise support

Security Model
Authentication

API keys for S3 compatibility
JWT tokens for web dashboard
Service accounts for internal communication

Authorization

Per-container ACLs
Tenant isolation
Rate limiting per user

Encryption

TLS for all communications
Encryption at rest (optional)
Client-side encryption support

Performance Considerations
Caching Strategy

RAM cache on Hub for metadata
Local SSD cache for hot data
CDN for geographic distribution

Optimization Techniques

Parallel uploads for large files
Multipart upload support
Connection pooling
Batch operations

Future Enhancements
Planned Features

 ML-powered predictive caching
 Blockchain verification for compliance
 IPFS integration for decentralized storage
 GraphQL API alongside S3
 Real-time sync capabilities

Research Areas

Quantum-resistant encryption
Zero-knowledge proofs for privacy
Edge computing integration
Autonomous storage optimization
