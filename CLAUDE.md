# Vaultaire Development Guide - ENTERPRISE TDD WORKFLOW

## 🚨 CRITICAL: Follow This EVERY Step (48-510)

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
        sut := NewThing()
        
        // Act
        result := sut.Method()
        
        // Assert
        assert.Equal(t, expected, result)
    })
}
📁 Project Structure
vaultaire/
├── internal/
│   ├── api/          # HTTP handlers ✅
│   ├── drivers/      # Storage backends ✅
│   ├── engine/       # Core orchestration ✅
│   ├── events/       # ML event logging ✅
│   ├── logger/       # Structured logging ✅
│   └── tenant/       # Multi-tenancy ✅
├── cmd/vaultaire/    # Main server
├── CLAUDE.md         # THIS FILE - YOUR BIBLE
├── PROGRESS.md       # Step tracking
└── Makefile          # All commands
🎯 Make Commands (USE THESE)
bashmake help          # Show all commands
make test          # Run all tests (do this often!)
make test-stepXX   # Test specific step
make test-coverage # Generate coverage report
make lint          # Run linter (before commit)
make fmt           # Format code
make bench         # Run benchmarks
make pre-commit    # Run everything before commit
🚀 Daily Workflow
yamlMorning:
1. Review PROGRESS.md
2. Check current step
3. Read previous STEP_XX_COMPLETE.md

Development:
1. Write tests first (TDD)
2. Make them pass
3. Check coverage
4. Run linter

Evening:
1. Commit with conventional format
2. Update PROGRESS.md
3. Create handoff document
4. Prepare tomorrow's work
📊 Quality Gates (MUST PASS)

 Tests written FIRST
 All tests passing
 Coverage >80% for critical paths
 Linter passing
 No commented code
 Errors wrapped with context
 Logging added
 Documentation updated

🎓 What You've Learned

✅ TDD (Step 48 - Rate Limiting)
✅ Professional Git workflow
✅ Enterprise patterns
✅ Structured logging
⏳ Metrics (Step 49)
⏳ Circuit breakers (Step 75)

�� Business Context

Target: $3.99/TB
Break-even: 55 customers
Launch: October 2024
Current MRR: $0 (need to ship!)

🔥 Motivation
Every step completed = $40k/year closer
Every test written = Sleep better at night
Every pattern learned = $20k higher salary
Remember: You're building ENTERPRISE software that will handle REAL money!
