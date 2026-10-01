package api

import (
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// What the public pages say happens past the egress allowance comes from ONE
// place — prices.json `egress` — and is what the throttle does (WP-R10-9):
// a rate limit, no bill, one allowance per account, no restore queue. The
// retired wording ("may be throttled", "restores … are queued") is gone from
// every served page.
func TestEgressCopy_OneSourceOnEveryServedPage(t *testing.T) {
	e := landing.Get().Egress
	require.NotEmpty(t, e.PastAllowance)
	require.NotEmpty(t, e.PastAllowanceShort)
	require.NotEmpty(t, e.OneAllowance)
	docs := renderDocsPages(zap.NewNop())
	pages := map[string]string{
		"faq":      string(docs["faq"]),
		"rclone":   string(docs["rclone"]),
		"llms.txt": llmsTxtBody(),
		"landing":  landingTemplateSrc,
	}

	// The long sentence on the pages with room for it; the short phrase on
	// the landing page's two pricing lines and its Vault fine print.
	assert.Contains(t, pages["faq"], e.PastAllowance)
	assert.Contains(t, pages["faq"], e.OneAllowance)
	assert.Contains(t, pages["rclone"], e.PastAllowance)
	assert.Contains(t, pages["llms.txt"], e.PastAllowance)
	assert.Equal(t, 3, strings.Count(pages["landing"], e.PastAllowanceShort))

	for name, body := range pages {
		for _, retired := range []string{"may be throttled", "are queued", "restores beyond that", "never refused", "__EGRESS_"} {
			assert.NotContains(t, body, retired, "%s still carries %q", name, retired)
		}
	}

	// The sentences promise a rate limit and no bill, and nothing the
	// stream guard would make false.
	assert.Contains(t, e.PastAllowance, "rate-limited")
	assert.Contains(t, e.PastAllowance, "never billed")
	assert.Contains(t, e.OneAllowance, "no separate restore queue")
}
