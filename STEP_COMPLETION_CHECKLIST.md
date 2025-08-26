# Step Completion Checklist

## ✅ BEFORE Starting Step
- [ ] Review PROGRESS.md to see where you are
- [ ] Create feature branch: `git checkout -b step-XX-feature`
- [ ] Create failing test file: `*_test.go`
- [ ] Run test to verify it fails (RED phase)
- [ ] Document what you're building in comments

## ✅ DURING Development  
- [ ] Write minimal code to pass test (GREEN)
- [ ] Check all errors are wrapped: `grep -r "return err" internal/ | grep -v fmt.Errorf`
- [ ] Add structured logging at key points
- [ ] Run linter: `make lint`
- [ ] Check coverage: `go test -cover ./...`
- [ ] Refactor if needed (REFACTOR)

## ✅ AFTER Completion
- [ ] All tests pass: `make test`
- [ ] No bare errors: Fixed all `return err` 
- [ ] Update PROGRESS.md
- [ ] Create STEP_XX_COMPLETE.md with learnings
- [ ] Run pre-commit: `make pre-commit`
- [ ] Commit with format: `feat(scope): description [Step XX]`
- [ ] Push and create PR
- [ ] Archive old files to docs/archive/

## ✅ Quality Checks
- [ ] Test coverage >80% for new code
- [ ] All errors have context
- [ ] Logging shows operation flow
- [ ] No ignored errors in critical paths
- [ ] Comments explain WHY not WHAT
- [ ] No backup files (*.backup, *.bak)
