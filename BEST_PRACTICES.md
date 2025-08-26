# Vaultaire Development Best Practices

## 🧪 Test-Driven Development (TDD)
```go
// ALWAYS write tests first
func TestFeature(t *testing.T) {
    // 1. Arrange - setup test conditions
    // 2. Act - execute the feature
    // 3. Assert - verify expectations
}
🔍 Error Handling
go// ALWAYS wrap errors with context
if err != nil {
    return fmt.Errorf("operation failed for tenant %s: %w", tenantID, err)
}

// NEVER ignore errors
_ = someFunction() // ❌ WRONG
if err := someFunction(); err != nil {
    // handle or return
}
📊 Logging Best Practices
go// Use structured logging with context
logger.Error("operation failed",
    zap.String("tenant", tenantID),
    zap.String("operation", "put"),
    zap.String("container", container),
    zap.Error(err),
)

// Log levels:
// Debug: Detailed diagnostic info
// Info: Important business events
// Warn: Recoverable issues
// Error: Operation failures
🏗️ Code Organization

/internal/api - HTTP handlers and routing
/internal/engine - Core business logic
/internal/drivers - Storage backends
/internal/database - Data persistence
/internal/auth - Authentication/authorization
Tests live next to code (*_test.go)

✅ Step Completion Checklist
Before Starting a Step:

 Create feature branch: git checkout -b step-XX-feature
 Write failing tests first (RED phase)
 Document what you're building in comments

During Development:

 Make tests pass with minimal code (GREEN phase)
 Check error handling - all errors wrapped?
 Add structured logging at key points
 Run linter: make lint
 Check test coverage: go test -cover ./...

After Completing:

 All tests passing: make test
 Document in PROGRESS.md
 Create STEP_XX_COMPLETE.md with learnings
 Commit with conventional format
 Push and create PR
 Clean up any temporary files

🎯 Quality Gates

Test coverage >80% for new code
All errors handled and wrapped
Structured logging in place
No linter warnings
Documentation updated
Git history clean

💡 Key Patterns
Multi-tenancy
go// Always extract tenant from context
tenant, err := tenant.FromContext(ctx)
if err != nil {
    return fmt.Errorf("no tenant in context: %w", err)
}
Streaming I/O
go// Never load entire files in memory
// Use io.Reader/Writer interfaces
func Process(r io.Reader) error {
    // Stream processing
}
Context Propagation
go// Always pass context through
func Operation(ctx context.Context, ...) error {
    // Use ctx for cancellation, timeouts, values
}

## When Ignoring Errors is Acceptable
```go
// OK in error response paths (already handling an error)
if err != nil {
    _, _ = w.Write([]byte("error"))  // ✅ OK
    return
}

// OK in tests for setup that's not under test
_ = tracker.RecordUpload(...)  // ✅ OK in tests

// NOT OK in business logic
result, _ := db.Query(...)  // ❌ WRONG - handle the error!
