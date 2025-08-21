.PHONY: test
test:
	go test -v -short ./...

.PHONY: test-unit
test-unit:
	go test -v -short -short ./...

.PHONY: test-step48
test-step48:
	go test -v -short ./internal/api -run TestRateLimiter

.PHONY: lint
lint:
	@echo "Linting..."
	@golangci-lint run --fix 2>/dev/null || true

.PHONY: fmt
fmt:
	@echo "Formatting..."
	@gofmt -s -w .
	@go mod tidy

.PHONY: run
run:
	go run cmd/vaultaire/main.go

.PHONY: build
build:
	go build -o bin/vaultaire cmd/vaultaire/main.go

.PHONY: clean
clean:
	rm -rf bin/ coverage.* *.test

.PHONY: test-coverage
test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  test         - Run all tests"
	@echo "  test-unit    - Run unit tests"
	@echo "  lint         - Run linter"
	@echo "  fmt          - Format code"
	@echo "  build        - Build binary"
	@echo "  clean        - Clean artifacts"
