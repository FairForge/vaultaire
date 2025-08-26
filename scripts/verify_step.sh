#!/bin/bash
# Verify step completion quality

echo "🔍 Checking for bare errors..."
if grep -r "return err" internal/ | grep -v "fmt.Errorf" | grep -v test; then
    echo "❌ Found bare errors - wrap with context!"
    exit 1
fi

echo "🔍 Checking test coverage..."
go test -cover ./... | grep -E "coverage: [0-9]+\.[0-9]%" 

echo "🔍 Running linter..."
golangci-lint run

echo "🔍 Checking for backup files..."
if find . -name "*.backup" -o -name "*.bak" | grep -v .git; then
    echo "❌ Found backup files - clean them up!"
    exit 1
fi

echo "✅ All quality checks passed!"
