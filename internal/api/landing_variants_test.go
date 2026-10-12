package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// TestLanding_WriteVariants is a hook for the browser checks, not a test of
// its own: with LANDING_OUT set it writes the two rendered page variants
// (signups closed / open) into that directory for
// internal/api/landing/browser/run.sh to load in headless Chrome. Without
// the env var it is skipped.
func TestLanding_WriteVariants(t *testing.T) {
	dir := os.Getenv("LANDING_OUT")
	if dir == "" {
		t.Skip("LANDING_OUT not set")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "closed.html"), landingPages(hosts.Get())[0], 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "open.html"), landingPages(hosts.Get())[1], 0o644))
}
