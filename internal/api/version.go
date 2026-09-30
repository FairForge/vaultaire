package api

// Build identity, stamped at link time (WP-R14-4 / R1-10):
//
//	go build -ldflags "-X github.com/FairForge/vaultaire/internal/api.BuildSHA=$(git rev-parse --short HEAD) \
//	                   -X github.com/FairForge/vaultaire/internal/api.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// `make build` and the deploy workflow set both; a plain `go build` reports
// "dev"/"unknown" so a box can never claim a commit it was not built from.
// /version, /health and /status render them. APIVersion (server.go) is the
// wire-format constant and is unrelated.
var (
	BuildSHA  = "dev"
	BuildDate = "unknown"
)
