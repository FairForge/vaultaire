.PHONY: test
test:
	go test -v ./...

.PHONY: test-step48
test-step48:
	go test -v ./internal/api -run TestRateLimiter

.PHONY: lint
lint:
	@echo "Linting..."
	@golangci-lint run --fix || true

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
	rm -rf bin/ coverage.*

.PHONY: test-coverage
test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  test         - Run all tests"
	@echo "  test-step48  - Test rate limiter"
	@echo "  lint         - Run linter"
	@echo "  fmt          - Format code"
	@echo "  build        - Build binary"
	@echo "  clean        - Clean artifacts"
