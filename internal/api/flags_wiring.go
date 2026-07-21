package api

import (
	"os"
	"strconv"
)

// Day-one feature flags (1.13 live-iteration kit). Adding a flag is a key
// constant + a Register call in NewServer + a call site — no schema change.
const (
	// flagSignups gates public account creation. Global-only (tenant IDs are
	// meaningless for signup), checked via the auth chokepoint
	// CreateUserWithTenant. Its in-code default reads SIGNUPS_ENABLED so a
	// deploy never reopens signups; a DB row overrides env with no deploy.
	flagSignups = "signups"

	// flagChunking is the kill-switch + per-tenant override for the
	// content-defined chunking/dedup PUT path, gated at the handleChunkedPut
	// entry check. Off ⇒ plain whole-object PUTs; reads are unaffected
	// (manifests are self-describing, chunked GETs keep working).
	flagChunking = "chunking"
)

// signupsDefaultFromEnv is the `signups` flag's in-code default: the
// SIGNUPS_ENABLED env var (unset or unparsable = enabled, matching the
// pre-1.13 behavior). The env value only seeds the DEFAULT — a feature_flags
// row overrides it in either direction at runtime.
func signupsDefaultFromEnv() bool {
	if v := os.Getenv("SIGNUPS_ENABLED"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			return enabled
		}
	}
	return true
}
