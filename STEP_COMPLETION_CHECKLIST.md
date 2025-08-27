markdown# Step Completion Checklist

## 📋 BEFORE Starting Step
- [ ] Check current position: `grep -c "✅" PROGRESS.md`
- [ ] Review plan: `grep "Step XX:" VAULTAIRE_MASTER_PLAN.md`
- [ ] Create branch: `git checkout -b step-XX-description`
- [ ] Update master plan if needed (mark previous step complete)

## 🔴 RED Phase - Write Failing Test FIRST
- [ ] Create test file: `internal/module/feature_test.go`
- [ ] Write test with clear Given/When/Then structure
- [ ] Run test and verify failure: `go test ./internal/module -run TestFeature -v`
- [ ] Confirm error message makes sense

## 🟢 GREEN Phase - Minimal Implementation
- [ ] Write ONLY enough code to pass the test
- [ ] No extra features or optimizations yet
- [ ] Run test and verify it passes: `go test ./internal/module -run TestFeature -v`
- [ ] Check coverage: `go test -cover ./internal/module`

## 🔵 REFACTOR Phase - Improve Quality
- [ ] Wrap all errors: `fmt.Errorf("context: %w", err)`
- [ ] Add structured logging: `logger.Debug/Info/Error()`
- [ ] Extract magic numbers to constants
- [ ] Add godoc comments to public functions
- [ ] Run formatter: `go fmt ./...`
- [ ] Run linter: `make lint` (must be clean)
- [ ] Verify tests still pass

## ✅ COMMIT with Proper Message
```bash
git add -A
git commit -m "feat(module): implement feature [Step XX]

- What was implemented (bullet points)
- Why it was needed
- Any important decisions

Test: Coverage at XX%
Docs: Updated relevant files
Refs: Step XX of master plan"
📝 DOCUMENT for Handoff
Create STEP_XX_COMPLETE.md:
markdown# Step XX: Feature Name ✅

## What Was Built
- Bullet points of functionality

## Test Coverage
- Test scenarios covered
- Coverage percentage
- Any skipped edge cases

## Files Modified
- List all files changed

## Key Decisions
- Why certain approaches were chosen
- Any trade-offs made

## Next Step
- What Step XX+1 should implement

## Handoff Notes
- Current branch name
- Any pending issues
- Environment setup needed
🚀 FINISH Step

 Push branch: git push origin step-XX-description
 Update PROGRESS.md: Add "✅ Step XX: Description"
 Verify CI passes (if configured)
 All tests pass: go test ./...
 No linter warnings: make lint

⚠️ QUALITY GATES (Must Pass)

 Coverage >80% for new code: go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out
 Zero unchecked errors: grep -r "_ =" internal/ | grep -v "_test.go" (should be empty)
 All errors have context: grep -r "return err" internal/ | grep -v "fmt.Errorf" (should be empty)
 No commented-out code: grep -r "^[[:space:]]*//.*func\|^[[:space:]]*//.*if\|^[[:space:]]*//.*for" internal/
 No debug prints: grep -r "fmt.Print" internal/ | grep -v "_test.go"

🎯 Definition of Done

 Tests written first and passing
 Code handles errors properly
 Documentation explains WHY
 Could hand this off to another developer
 Follows established patterns (Engine/Container/Artifact)
 Ready for production use

