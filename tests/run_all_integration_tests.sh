#!/bin/bash
echo "Running Integration Test Suite (Steps 196-200)"

echo "Step 196: Regression Tests..."
go test ./tests/regression/...

echo "Step 197: Contract Tests..."
go test ./tests/regression/... -run Contract

echo "Step 198: Security Tests..."
go test ./tests/security/...

echo "Step 199: Compliance Tests..."
go test ./tests/compliance/...

echo "Step 200: User Acceptance Tests..."
go test ./tests/acceptance/...

echo "Integration Testing Complete!"
