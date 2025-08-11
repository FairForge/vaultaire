# Vaultaire Engine Makefile
BINARY := vaultaire
VERSION := 0.1.0
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# Go build flags
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GitCommit=$(GIT_COMMIT)"
GOFLAGS := -v

.PHONY: all
all: clean test build

.PHONY: build
build:
	@echo "🔨 Building Vaultaire Engine v$(VERSION)..."
	@go build $(GOFLAGS) -race $(LDFLAGS) -o bin/$(BINARY) ./cmd/vaultaire
	@echo "✅ Build complete: bin/$(BINARY)"

.PHONY: test
test:
	@echo "🧪 Running tests..."
	@go test -race -cover ./...
	@echo "✅ Tests complete"

.PHONY: run
run: build
	@echo "🚀 Starting Vaultaire Engine..."
	@./bin/$(BINARY)

.PHONY: clean
clean:
	@echo "🧹 Cleaning..."
	@rm -rf bin/ coverage.out *.prof
	@echo "✅ Clean complete"

.PHONY: dev
dev:
	@echo "👨‍💻 Starting development mode..."
	@go run -race ./cmd/vaultaire

.PHONY: help
help:
	@echo "Vaultaire Engine Makefile"
	@echo ""
	@echo "Usage:"
	@echo "  make build    - Build the binary"
	@echo "  make test     - Run tests"
	@echo "  make run      - Build and run"
	@echo "  make clean    - Clean build artifacts"
	@echo "  make dev      - Run in development mode"
	@echo "  make help     - Show this help"
