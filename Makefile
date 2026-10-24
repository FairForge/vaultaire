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

# Linting — configuration in .golangci.yml (Review R15). The version is
# CI's (ci.yml golangci-lint-action `version:`), run through `go run` so the
# pre-commit hook never depends on whatever golangci-lint is on PATH (a
# Homebrew build for an older Go refused the module — RP20-3); the first
# run builds it into the Go cache, later runs start at once. The check
# below fails when the two pins drift.
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: lint-version-check
	$(GOLANGCI_LINT) run ./...

# The js/wasm side of sdk/wasm (RP51-2): `make lint` skips its build tag, and
# `go run` under GOOS=js builds the linter itself for js (sdk/CLAUDE.md). So the
# pinned linter is built for the HOST into a temporary GOBIN, then run with
# GOOS=js GOARCH=wasm. CI's lint step runs it; it must stay at 0 issues.
lint-wasm: lint-version-check
	@bin=$$(mktemp -d "$${TMPDIR:-/tmp}/lint-wasm.XXXXXX") && trap 'rm -rf "$$bin"' EXIT && \
		GOBIN="$$bin" GOOS= GOARCH= go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) && \
		GOOS=js GOARCH=wasm "$$bin/golangci-lint" run ./sdk/wasm/

lint-version-check:
	@grep -q 'version: $(GOLANGCI_LINT_VERSION)$$' .github/workflows/ci.yml || \
		{ echo "Makefile GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) differs from .github/workflows/ci.yml"; exit 1; }

# The Security workflow's gosec command, verbatim (see security.yml for the
# exclusion rationale). Needs: go install github.com/securego/gosec/v2/cmd/gosec@latest
gosec:
	gosec -severity medium -exclude-dir=tests -exclude-dir=cmd/tools -exclude=G101,G115,G301,G304 ./...

# Unreachable functions in the product binary (R0's tool of record).
# x/tools v0.50 needs Go 1.26 — the toolchain go.mod pins (GOTOOLCHAIN=auto fetches it).
deadcode:
	go run golang.org/x/tools/cmd/deadcode@v0.50.0 ./cmd/vaultaire

.PHONY: fmt lint lint-wasm lint-version-check gosec deadcode

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

# Build identity stamped into /version, /health and /status (WP-R14-4): the
# commit and its COMMIT time (UTC), never the wall clock. `make build` and
# deploy.yml run the same reproducible recipe, scripts/build-release.sh
# (RP37-9); `make repro-check` builds twice from fresh copies and compares.
BUILD_SHA  ?= $(shell git rev-parse HEAD 2>/dev/null | cut -c1-12)
BUILD_DATE ?= $(shell TZ=UTC git show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD 2>/dev/null)

build:
	BUILD_SHA=$(BUILD_SHA) BUILD_DATE=$(BUILD_DATE) scripts/build-release.sh bin/vaultaire

repro-check:
	GOOS=linux GOARCH=amd64 scripts/repro-check.sh

version:
	@echo "$(BUILD_SHA) $(BUILD_DATE)"

# Remove every build output, including the tool binaries that used to pile up
# in the repo root (WP-R0-5: ~35 gitignored files). Keeps bench-results/*.md.
clean:
	rm -rf bin/ dist/ sdk/wasm/dist/ coverage.out coverage.html cov.out
	rm -f vaultaire vaultaire-bin vaultaire-linux vaultaire-darwin vaultaire-windows.exe
	rm -f backend-matrix backend-matrix-linux bench bench-linux bench-compare bench-compare-linux
	rm -f dedup-migrate dedup-migrate-linux erasure-bench erasure-bench-linux
	rm -f geyser-admin-test geyser-cloudsync-probe geyser-console-probe geyser-smoke geyser-smoke-bin geyser-test
	rm -f lighthouse-bench loadtest loadtest-linux onedrive-bench onedrive-bench-linux
	rm -f permafrost-benchmark permafrost-benchmark-linux permafrost-fleet permafrost-fleet-linux
	rm -f permafrost-parallel permafrost-parallel-linux permafrost-stress permafrost-stress-linux
	rm -f permafrost-v2 permafrost-v2-linux permafrost-v3 permafrost-v3-linux
	rm -f pixeldrain-bench pixeldrain-bench-linux pipeline-bench quotaless-bench-v2 quotaless-bench-v2-linux
	rm -f uloz-bench uloz-bench-linux validate validate-linux webdav-bench webdav-bench-linux
	rm -f *.bin *.test test*.txt test_output.log downloaded.txt cache_benchmark_results.txt compress.txt
	rm -rf bench-results/quotaless-*/
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
	go clean

.PHONY: build repro-check version clean landing landing-browser og dash-shots dash-lighthouse sdk-wasm

# The Private-mode key core for the browser (keystone K2): sdk/go/e2ee compiled
# to WebAssembly, with the SAME toolchain's wasm_exec.js beside it (they must
# match). sdk/js/e2ee/index.mjs loads the pair. TestE2EE_WASMMatchesNative
# builds it too and runs it in node against native Go.
sdk-wasm:
	mkdir -p sdk/wasm/dist
	GOOS=js GOARCH=wasm go build -trimpath -ldflags=-s -o sdk/wasm/dist/e2ee.wasm ./sdk/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" sdk/wasm/dist/wasm_exec.js
