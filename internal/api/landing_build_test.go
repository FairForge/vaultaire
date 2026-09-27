package api

import (
	"regexp"
	"testing"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/stretchr/testify/require"
)

// landing.html is GENERATED from internal/api/landing/ (make landing). The
// generator stamps a sha256 of its sources into the file's first comment;
// this test recomputes it (landing.SourcesHash, shared with the dashboard's
// generated house template) so a hand edit to landing.html, or a source edit
// without a rebuild, fails CI instead of silently drifting.
func TestLanding_GeneratedFromSources(t *testing.T) {
	stamp := regexp.MustCompile(`sources sha256:([0-9a-f]{64})`).FindStringSubmatch(landingTemplateSrc)
	require.Len(t, stamp, 2, "landing.html must carry the generator's sha256 stamp; run `make landing`")

	got, err := landing.SourcesHash("landing")
	require.NoError(t, err)
	require.Equal(t, stamp[1], got,
		"internal/api/landing/ sources changed but landing.html was not rebuilt (or was edited by hand); run `make landing`")
}
