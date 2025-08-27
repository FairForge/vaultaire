# CLAUDE.md - Vaultaire Development Control
## Current Position
**Step: 66 of 510 (12.9%)**
**Branch: file-operations** (covers steps 51-55)
**Working File: internal/drivers/local.go**
**Test File: internal/drivers/local_advanced_test.go**

## Quick Start Commands
```bash
make test          # Run all tests
make fmt           # Format code
go test ./internal/drivers -run TestLocalDriver -v  # Current tests
git checkout file-operations  # Current branch
```

## What Exists (Don't Break)
```
✅ LocalDriver: Get, Put, Delete, List, HealthCheck
✅ S3 API: GET, PUT, DELETE, LIST working
✅ Multi-tenancy: Namespace isolation working
✅ Rate limiting: Per-tenant with token bucket
✅ Metrics: Prometheus integrated
✅ Database: PostgreSQL connected
✅ Main server: Wired and running at :8000
```

## Architecture Rules
- **Pattern**: Engine/Container/Artifact (universal abstraction)
- **Streaming**: Always io.Reader, never []byte
- **Context**: Every function takes context.Context first param
- **Errors**: Always `fmt.Errorf("context: %w", err)`
- **Tenancy**: Namespace with tenant ID

## TDD Workflow (USE THIS)

### 1. Explore & Plan (Don't code yet!)
```bash
# Read relevant files first
cat internal/drivers/local.go
grep -n "TODO" internal/

# Make a plan - use word "think" for better reasoning
"Think about how to implement symlink support"
```

### 2. Write Tests First
```bash
vim internal/drivers/local_advanced_test.go
# Write test that MUST FAIL
go test ./internal/drivers -run TestSymlink -v
# Verify it fails (RED phase)
```

### 3. Implement & Iterate
```bash
vim internal/drivers/local.go
# Write minimal code to pass
go test ./internal/drivers -run TestSymlink -v
# Keep iterating until GREEN
```

### 4. Commit When Done
```bash
go fmt ./...
go test ./...
git add .
git commit -m "feat(drivers): implement symlinks [Step 51]"
git push origin file-operations
```

## Step Groups (Work Together)

### Current: Steps 51-55 File Operations
```go
☑️ Step 51: SupportsSymlinks(), GetWithOptions(), GetInfo()
☑️ Step 52: SetPermissions(), GetPermissions() 
☑️ Step 53: SetOwnership(), GetOwnership()
☑️ Step 54: SetXAttr(), GetXAttr(), ListXAttrs()
☑️ Step 55: GetChecksum(), VerifyChecksum()
```

### Next Groups
☑️ Steps 56-60: Directory operations
- Steps 61-65: Atomic operations  
- Steps 66-70: File watching
- Steps 71-75: Parallel I/O (CRITICAL - 10x speed)

## Checklist for Complex Tasks

When doing migrations or fixing many issues:
1. Generate a checklist in a markdown file
2. Work through systematically
3. Check off completed items
4. Use `/clear` between major context switches

Example:
```markdown
# Lint Fixes Checklist
☐ Fix error wrapping in local.go:45
☐ Fix error wrapping in local.go:67
☐ Add missing error check in s3.go:203
```

## Git Worktrees (For Parallel Work)

```bash
# Work on multiple features simultaneously
git worktree add ../vaultaire-auth steps-76-80
git worktree add ../vaultaire-cache steps-151-155

# Clean up when done
git worktree remove ../vaultaire-auth
```

## Code Templates

### Test Pattern
```go
func TestFeature(t *testing.T) {
    t.Run("specific case", func(t *testing.T) {
        // Arrange
        driver := NewLocalDriver(t.TempDir(), zap.NewNop())
        
        // Act  
        result, err := driver.Method()
        
        // Assert
        require.NoError(t, err)
        assert.Equal(t, expected, result)
    })
}
```

### Error Wrapping
```go
if err != nil {
    return fmt.Errorf("get artifact %s/%s: %w", container, artifact, err)
}
```

## Progress Map

```
Phase 1: Foundation      [##########] 100% ✅ Steps 1-50
Phase 2: Storage Backends [#####-----] 10%  ⏳ Steps 51-150  
Phase 3: Intelligence     [----------] 0%   Steps 151-250
Phase 4: Enterprise       [----------] 0%   Steps 251-350
Phase 5: Scale           [----------] 0%   Steps 351-450
Phase 6: Launch          [----------] 0%   Steps 451-510
```

## Resume Instructions

For new chat, provide:
1. This CLAUDE.md file
2. The master plan
3. Say: "Continue from Step X implementing [feature]"

## Quality Gates (Simple)

Before commit:
- [ ] Tests written first and passing
- [ ] `go fmt ./...` applied
- [ ] Errors wrapped with context
- [ ] No `_ =` ignoring errors

Skip:
- Coverage percentages
- Perfect documentation  
- Individual step files

## Business Context
- **Product**: stored.ge at $3.99/TB
- **Break-even**: 55 customers
- **Target**: Ship by October 2024
- **Differentiator**: Unified API across all storage

## Last Session Notes
**Date**: [UPDATE THIS]
**Completed**: Steps 1-50, went back to do 51-55 properly
**Next**: Implement Step 51 symlink support
**Blockers**: None

---

## Remember
1. **Plan before coding** - Think through approach first
2. **Test drives development** - Red → Green → Refactor
3. **Ship working code** - Not documentation
4. **Use /clear frequently** - Keep context focused
5. **Course correct early** - Don't let Claude go too far astray

**This file + master plan = everything needed to continue**
```

Key improvements from the Claude Code best practices:
1. Added "explore & plan" phase before coding
2. Added checklist approach for complex tasks
3. Added git worktrees for parallel work
4. Emphasized using `/clear` to manage context
5. Added progress visualization
6. Simplified quality gates even more
7. Added "think" keyword tip for better reasoning

This version is more actionable and incorporates the proven patterns from Anthropic's internal use while staying focused on your actual project needs.