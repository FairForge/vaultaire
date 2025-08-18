# Vaultaire Development Guide

## Current Status
- **Current Step**: 47 of 510 (Multi-tenancy COMPLETE)
- **Next Step**: 48 (Rate Limiting Middleware)
- **Progress**: 10% complete
- **Velocity Target**: 15 steps/day
- **GitHub**: github.com/fairforge/vaultaire

## Critical Architecture Rules (NEVER VIOLATE)
1. Use `Engine` pattern (NOT storage)
2. Use `Container/Artifact` (NOT bucket/object) internally
3. Use `Driver` interface (NOT backend)
4. Event log EVERY operation for ML
5. Stream everything (io.Reader, never []byte)
6. Context propagation on ALL functions

## Every Function MUST Have
```go
func (h *Handler) Operation(ctx context.Context, ...) (io.Reader, error) {
    // 1. Extract tenant
    tenant := ctx.Value("tenant").(string)
    
    // 2. Build namespaced key
    key := fmt.Sprintf("tenant/%s/%s", tenant, originalKey)
    
    // 3. Log event (ML training)
    h.events.Log("operation", key, size)
    
    // 4. Record metrics
    defer metrics.Record("operation", time.Now())
    
    // 5. Stream response (never []byte)
    return reader, nil
}
Common Commands
bash# Run server
go run cmd/vaultaire/main.go

# Test S3 operations
aws --endpoint-url=http://localhost:8080 s3 ls
aws --endpoint-url=http://localhost:8080 s3 mb s3://test
aws --endpoint-url=http://localhost:8080 s3 cp file.txt s3://test/

# Run tests
go test -v ./...

# Check progress
cat PROGRESS.md | grep "Step"
File Structure
vaultaire/
├── internal/
│   ├── api/         # S3 handlers
│   ├── drivers/     # Storage backends (local, lyve, quotaless)
│   ├── engine/      # Core orchestration
│   ├── events/      # ML event logging
│   └── tenant/      # Multi-tenancy
├── cmd/vaultaire/   # Main server
└── PROJECT_MASTER.md # Complete documentation
Workflow Rules

Read relevant files before coding
Make a plan before implementing
Write tests first when possible
Commit with step numbers
Update tracking files

Next Steps Queue

 Step 48: Rate Limiting (golang.org/x/time/rate)
 Step 49: Metrics collection (Prometheus)
 Step 50: Integration tests
 Step 75: Parallel I/O (3FS pattern)
 Step 85: Transaction logging

Storage Backend Status

✅ Local driver working
⏳ Quotaless integration (need $200/200TB deal)
⏳ Lyve Cloud integration
⏳ OneDrive backup tier

Business Context

Target: $3.99/TB edge tier
Break-even: 55 customers
Potential customer: $1,499/month identified
Launch target: October 2024
