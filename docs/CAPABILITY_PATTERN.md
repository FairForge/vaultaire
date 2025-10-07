# Vaultaire Capability Pattern Guide

## Philosophy
**Everything that CAN be toggleable SHOULD be toggleable.**

Vaultaire's architecture allows features to work natively with backends that support them, while gracefully degrading to Vaultaire-level enforcement when they don't.

## The Pattern

### 1. Define Capabilities in Backend Interface
```go
// internal/engine/backend.go
type Capability string

const (
    // Storage capabilities
    CapabilityVersioning      Capability = "versioning"
    CapabilityObjectLock      Capability = "object_lock"      // Lyve, S3
    CapabilityLifecycle       Capability = "lifecycle"        // S3
    CapabilityLegalHold       Capability = "legal_hold"       // S3, Lyve

    // Advanced features (add as needed)
    CapabilityDeduplication   Capability = "deduplication"
    CapabilityErasureCoding   Capability = "erasure_coding"
    CapabilityP2PNetwork      Capability = "p2p_network"
    CapabilityEncryption      Capability = "native_encryption"
)

type Backend interface {
    // ...
    Capabilities() []Capability
    HasCapability(cap Capability) bool
}
2. Backend Implementations Declare What They Support
go// Lyve Cloud Backend
func (b *LyveBackend) Capabilities() []Capability {
    return []Capability{
        CapabilityStreaming,
        CapabilityVersioning,
        CapabilityObjectLock,      // ✅ Lyve supports this!
        CapabilityLegalHold,       // ✅ Lyve supports this!
        CapabilityMultipart,
    }
}

// Local Backend
func (b *LocalBackend) Capabilities() []Capability {
    return []Capability{
        CapabilityStreaming,
        // No object lock - will use Vaultaire enforcement
    }
}
3. Features Check Capabilities Before Using
go// Pattern: Try native, fallback to Vaultaire
func (s *RetentionService) ApplyRetention(ctx context.Context, backend Backend, policy *RetentionPolicy) error {
    if policy.UseBackendObjectLock && backend.HasCapability(CapabilityObjectLock) {
        // Use native backend feature
        s.logger.Info("using native object lock",
            zap.String("backend", backend.Name()))
        return backend.SetObjectLock(ctx, policy.Container, policy.RetentionPeriod)
    }

    // Fallback to Vaultaire-level enforcement
    s.logger.Info("using vaultaire enforcement",
        zap.String("backend", backend.Name()),
        zap.String("reason", "backend lacks object_lock capability"))
    return s.enforceRetentionVaultaire(ctx, policy)
}
4. Track Backend Capabilities in Database
go// Auto-discover and track what each backend supports
func (s *BackendService) RegisterBackend(ctx context.Context, backend Backend) error {
    caps := backend.Capabilities()

    query := `
        INSERT INTO backend_capabilities (backend_id, supports_object_lock, supports_versioning, ...)
        VALUES ($1, $2, $3, ...)
        ON CONFLICT (backend_id) DO UPDATE SET ...
    `

    _, err := s.db.ExecContext(ctx, query,
        backend.ID(),
        hasCapability(caps, CapabilityObjectLock),
        hasCapability(caps, CapabilityVersioning),
        // ...
    )
    return err
}
Examples
Retention Policies (Step 342) ✅ IMPLEMENTED
gopolicy := &RetentionPolicy{
    BackendID:            "lyve-us-east-1",
    UseBackendObjectLock: true,  // Toggle: use native if available
}
Deduplication (Steps 511-520) - FUTURE
goconfig := &DedupConfig{
    Enabled:              true,
    UseBackendDedup:      true,  // Toggle: use backend's native dedup
    FallbackToVaultaire:  true,  // Toggle: use Vaultaire dedup if backend doesn't support
}
Erasure Coding (Steps 581-590) - FUTURE
gopolicy := &ErasureCodingPolicy{
    Enabled:              true,
    UseBackendEC:         true,  // Toggle: use backend's native EC (e.g., S3 reduced redundancy)
    DataShards:           10,
    ParityShards:         4,
}
P2P Networking (Steps 811-820) - FUTURE
gobackend := &P2PBackend{
    P2PEnabled:           true,  // Toggle: enable P2P
    FallbackToCentral:    true,  // Toggle: fallback if P2P unavailable
}
Benefits

Flexibility: Use the best tool for the job
Performance: Native features are often faster
Cost: Some native features reduce egress (e.g., Lyve Object Lock)
Graceful Degradation: Works everywhere, optimized where possible
Future-Proof: Easy to add new capabilities as backends evolve

Rules
Always Toggle When:

✅ Feature has backend-native alternative (object lock, versioning)
✅ Feature is performance optimization (dedup, compression)
✅ Feature is optional enhancement (P2P, erasure coding)
✅ Feature has cost implications (replication, tiering)

Never Toggle When:

❌ Feature is architectural foundation (multi-tenancy, RBAC)
❌ Feature is security-critical and must be enforced (encryption at rest)
❌ Feature is compliance-required (audit logging)

Implementation Checklist
For each new feature:

 Define capability constant
 Add to backend interface documentation
 Update backend implementations to declare support
 Add capability check before using native feature
 Implement Vaultaire fallback
 Add toggle to configuration/policy
 Document behavior in both modes
 Test with and without capability
