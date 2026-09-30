# Professional test organization

# Default: run quick tests
test:
	go test -short -race -cover ./...

# Unit tests only (fast)
test-unit:
	go test -short -race -cover ./internal/...

# DB-backed suites against the migrated local test database (what CI runs):
# every package, race detector, no -short. Needs `make test-db` first.
TEST_DSN ?= postgres://$(USER)@localhost:5432/$(TEST_DB)?sslmode=disable
test-integration: test-db
	DATABASE_URL="$(TEST_DSN)" JWT_SECRET=local-test-secret go test -race ./...

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

# Linting — configuration in .golangci.yml (Review R15); CI runs the same.
lint:
	golangci-lint run ./...

# The Security workflow's gosec command, verbatim (see security.yml for the
# exclusion rationale). Needs: go install github.com/securego/gosec/v2/cmd/gosec@latest
gosec:
	gosec -severity medium -exclude-dir=tests -exclude-dir=cmd/tools -exclude=G101,G115,G301,G304 ./...

# Unreachable functions in the product binary (R0's tool of record).
# x/tools v0.50 needs the Go 1.26 toolchain; GOTOOLCHAIN=auto downloads it.
deadcode:
	go run golang.org/x/tools/cmd/deadcode@v0.50.0 ./cmd/vaultaire

.PHONY: fmt lint gosec deadcode

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

# Build identity stamped into /version, /health and /status (WP-R14-4).
# deploy.yml stamps the same two variables from GITHUB_SHA.
BUILD_SHA  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X github.com/FairForge/vaultaire/internal/api.BuildSHA=$(BUILD_SHA) \
              -X github.com/FairForge/vaultaire/internal/api.BuildDate=$(BUILD_DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/vaultaire ./cmd/vaultaire

version:
	@echo "$(BUILD_SHA) $(BUILD_DATE)"

# Remove every build output, including the tool binaries that used to pile up
# in the repo root (WP-R0-5: ~35 gitignored files). Keeps bench-results/*.md.
clean:
	rm -rf bin/ dist/ coverage.out coverage.html cov.out
	rm -f vaultaire vaultaire-bin vaultaire-linux vaultaire-darwin vaultaire-windows.exe
	rm -f backend-matrix backend-matrix-linux bench bench-linux bench-compare bench-compare-linux
	rm -f dedup-migrate dedup-migrate-linux erasure-bench erasure-bench-linux
	rm -f geyser-admin-test geyser-cloudsync-probe geyser-console-probe geyser-smoke geyser-smoke-bin geyser-test
	rm -f lighthouse-bench loadtest loadtest-linux onedrive-bench onedrive-bench-linux
	rm -f permafrost-benchmark permafrost-benchmark-linux permafrost-fleet permafrost-fleet-linux
	rm -f permafrost-parallel permafrost-parallel-linux permafrost-stress permafrost-stress-linux
	rm -f permafrost-v2 permafrost-v2-linux permafrost-v3 permafrost-v3-linux
	rm -f pixeldrain-bench pixeldrain-bench-linux pipeline-bench quotaless-bench-v2 quotaless-bench-v2-linux
	rm -f uloz-bench uloz-bench-linux validate validate-linux
	rm -f *.bin *.test test*.txt test_output.log downloaded.txt cache_benchmark_results.txt compress.txt
	rm -rf bench-results/quotaless-*/
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
	go clean

.PHONY: build version clean landing landing-browser og dash-shots dash-lighthouse
