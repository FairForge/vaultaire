# Vaultaire Development Guide - ENTERPRISE TDD WORKFLOW

## Step 76: AWS S3 Client Integration
Next: Set up AWS SDK and basic S3 backend structure
### BEFORE Starting Any Step:
```bash
# 1. Verify clean state
git status  # Should be clean
make test   # All passing

# 2. Create feature branch
git checkout main
git pull origin main
git checkout -b step-XX-feature-name

# 3. Write tests FIRST (TDD)
cat > internal/path/feature_test.go
# Write failing tests

# 4. Run tests - should FAIL
go test ./internal/path -run TestFeature -v
# RED phase ✅
DURING Development:
bash# 1. Write minimal code to pass ONE test
# 2. Run test - should PASS
make test-stepXX
# GREEN phase ✅

# 3. Refactor if needed
# 4. Run ALL tests
make test
# REFACTOR phase ✅

# 5. Check coverage
go test -cover ./internal/...
# Should be >80% for new code
AFTER Completing Step:
bash# 1. Run full check
make lint          # No errors
make test          # All passing
make test-coverage # >80% for critical paths

# 2. Update documentation
echo "## Step XX: Feature ✅" >> PROGRESS.md
cat > STEP_XX_COMPLETE.md  # Full details

# 3. Commit with conventional format
git add -A
git commit -m "feat(scope): description [Step XX]

- What it does
- How it works
- Tests: X/X passing
- Coverage: XX%"

# 4. Push and create PR
git push origin step-XX-feature
# Create PR on GitHub

# 5. Prepare next step
cat > NEW_CHAT_STEP_XX.md
📊 Current Status

Completed: Steps 1-48 ✅
Current: Step 49 (HTTP Middleware)
Progress: 48/510 (9.4%)
Velocity: 15 steps/day target

🏗️ Architecture Rules (NEVER VIOLATE)

Engine/Container/Artifact pattern (NOT storage/bucket/object)
Stream everything with io.Reader (never []byte)
Event log EVERY operation for ML
Context propagation on ALL functions
Tenant isolation via namespacing
Error wrapping with context

✅ Enterprise Patterns In Use

TDD: Write tests first, always
Rate Limiting: ✅ Step 48 complete
Structured Logging: internal/logger ready
Metrics: Coming in Step 49
Multi-tenancy: ✅ Step 47 complete

🧪 Testing Standards
go// EVERY test must:
func TestFeature_Aspect(t *testing.T) {
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

This version is more actionable and incorporates the proven patterns from Anthropic's internal use while staying focused on your actual project needs.## Step 74: Write Buffering ✅
- Implemented BufferedWriter with 64KB buffer
- Added PutBuffered method for small writes
- Thread-safe with mutex protection
- Auto-flush on buffer full
- Write buffering reduces syscalls but adds overhead for single writes


## Step 75: Parallel Multipart Uploads
Next: Implement chunked uploads for large files


## Step 76: AWS S3 Client Integration
Next: Set up AWS SDK and basic S3 backend structure

