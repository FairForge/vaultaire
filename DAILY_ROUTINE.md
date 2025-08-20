# My Daily Vaultaire Routine

## 🌅 Morning (5 min)
```bash
cd ~/fairforge/vaultaire
git pull origin main
cat PROGRESS.md | tail -20  # Where am I?
make test                    # Everything working?
💻 Before Coding (10 min)
bash# 1. Create branch
git checkout -b step-XX-feature

# 2. Write tests FIRST
code internal/api/feature_test.go

# 3. Run tests - should FAIL
go test ./internal/api -run TestFeature -v
��️ During Coding (2-3 hours)
bash# TDD Cycle - repeat until done:
while tests_failing; do
    # Write minimal code
    code internal/api/feature.go
    
    # Test it
    make test-stepXX
    
    # Check coverage
    go test -cover ./internal/api
done
✅ After Coding (15 min)
bash# 1. Full quality check
make lint
make test
make test-coverage

# 2. Document
echo "Step XX complete" >> PROGRESS.md
git add -A
git commit -m "feat: step XX complete"
git push

# 3. Prepare tomorrow
cat > NEW_CHAT_STEP_XX.md
🎯 Weekly Goals

Monday: Plan 5 steps
Tuesday-Thursday: Implement 15 steps
Friday: Test, document, refactor
Weekend: Study similar systems

📊 Track Progress

Steps/day: ___
Tests written: ___
Coverage: ___%
Bugs found by tests: ___
Time saved: ___ hours
