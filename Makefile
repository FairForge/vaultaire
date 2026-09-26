# Professional test organization

# Default: run quick tests
test:
	go test -short -race -cover ./...

# Unit tests only (fast)
test-unit:
	go test -short -race -cover ./internal/...

# Integration tests (requires services)
test-integration:
	go test -short ./tests/integration

# Load/performance tests (slow)
test-load:
	go test -v ./tests/load

# All tests including slow ones
test-all:
	go test -race -cover ./...

# Test with coverage report
test-coverage:
	go test -short -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Benchmark tests
test-bench:
	go test -run=^$ -bench=. -benchmem ./...

# Run specific package tests
test-pkg:
	@read -p "Package path: " pkg; \
	go test -v -race ./$$pkg

# Clean test cache
test-clean:
	go clean -testcache

# Create + migrate the local test database (default target of every DB-backed
# test when DATABASE_URL is unset — see internal/testutil). Idempotent; mirrors
# the CI "Set up database" step. Never points at the shared dev DB `vaultaire`.
TEST_DB ?= vaultaire_test
test-db:
	psql -X -q -h localhost -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$(TEST_DB)'" | grep -q 1 || \
		psql -X -q -h localhost -d postgres -c "CREATE DATABASE $(TEST_DB)"
	@for f in internal/database/migrations/*.sql; do \
		psql -X -q -v ON_ERROR_STOP=1 -h localhost -d $(TEST_DB) -f "$$f" > /dev/null || exit 1; \
	done
	@echo "$(TEST_DB): migrations applied"

.PHONY: test test-unit test-integration test-load test-all test-coverage test-bench test-pkg test-clean test-db

# Code formatting
fmt:
	go fmt ./...
	gofmt -s -w .

# Linting
lint:
	golangci-lint run ./...

.PHONY: fmt lint

# Build the binary
build:
	go build -o bin/vaultaire ./cmd/vaultaire

# Clean build artifacts
clean:
	rm -rf bin/
	go clean

.PHONY: build clean
