package api

// docs/DURABILITY.md is the page a base layer sells (Stage 1a Prompt 58 PR
// C): every number on it is the code's. Change a constant, a rule or the page
// and this fails until they agree (the produce_test.go rule).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

func TestDurabilityPage_ClaimsAreTheCodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "DURABILITY.md"))
	require.NoError(t, err)
	doc := string(raw)
	days := func(d time.Duration) int { return int(d / (24 * time.Hour)) }
	for _, claim := range []string{
		fmt.Sprintf("RS %d+%d", vaultParityK, vaultParityM),
		fmt.Sprintf("%d MiB per shard per stripe", vaultParityStripe>>20),
		fmt.Sprintf("any %d of the %d", vaultParityK, vaultParityK+vaultParityM),
		fmt.Sprintf("every %d days", days(defaultTapeAuditEvery)),
		fmt.Sprintf("`VAULT_PARITY_SCRUB_CYCLE` (default %d days)", days(defaultScrubCycle)),
		"`" + blobScrubJobName + "`", "`" + flagBlobScrub + "`", "`SCRUB_CYCLE`", "`scrub_findings`",
		"vendor_allowance", "`" + flagEdgeMirror + "`",
	} {
		assert.Contains(t, doc, claim)
	}
	// The alerts it names are the rule file's.
	rules, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "monitoring", "vaultaire-scrub.yml"))
	require.NoError(t, err)
	for _, alert := range []string{"ScrubFailing", "ScrubCycleStale"} {
		assert.Contains(t, doc, "`"+alert+"`")
		assert.Contains(t, string(rules), "alert: "+alert)
	}
	// The bucket page's sentence, as the page quotes it, is durability.json's.
	var c struct {
		Verified string `json:"verified"`
	}
	require.NoError(t, json.Unmarshal(landing.DurabilityJSON(), &c))
	quoted := strings.TrimSuffix(c.Verified, " on {date}.")
	assert.Contains(t, doc, "*\""+quoted+" on &lt;date&gt;\"*")
}
