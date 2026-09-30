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
# Regenerate internal/api/landing.html from its sources (internal/api/landing/).
# landing_build_test.go fails when the sources and the generated file drift.
landing:
	python3 internal/api/landing/build.py

# Drive the house builder in headless Chrome (internal/api/landing/browser).
landing-browser:
	bash internal/api/landing/browser/run.sh

# Photograph the dashboard (internal/dashboard/browser/shots.sh): every fixture
# page at 1280 px light/dark and 390 px, into /tmp/vaultaire-dash/shots.
# Needs Chrome and the local test database. dash-lighthouse also scores
# accessibility per page with Lighthouse (needs npx) and fails below 100.
dash-shots:
	bash internal/dashboard/browser/shots.sh

dash-lighthouse:
	bash internal/dashboard/browser/shots.sh lighthouse

# Re-render the social preview (internal/api/og.png) from the landing sources.
# Needs Chrome; override with CHROME=/path/to/chrome.
CHROME ?= $(shell command -v google-chrome || command -v chromium || echo "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
og:
	python3 internal/api/landing/build.py og /tmp/vaultaire-og
	"$(CHROME)" --headless=new --disable-gpu --hide-scrollbars --no-sandbox --blink-settings=preferredColorScheme=1 --window-size=1200,630 --force-device-scale-factor=1 --virtual-time-budget=4000 --screenshot=internal/api/og.png file:///tmp/vaultaire-og/og.html >/dev/null 2>&1
	@ls -la internal/api/og.png

build:
	go build -o bin/vaultaire ./cmd/vaultaire

# Clean build artifacts
clean:
	rm -rf bin/
	go clean

.PHONY: build clean landing landing-browser og dash-shots dash-lighthouse
