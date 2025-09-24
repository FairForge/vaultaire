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

.PHONY: test test-unit test-integration test-load test-all test-coverage test-bench test-pkg test-clean

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
