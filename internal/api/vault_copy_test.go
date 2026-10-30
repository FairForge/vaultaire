package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// What the public pages say about a Vault restore's time comes from ONE place
// — prices.json `vault.restore_time` / `restore_time_short` — and the claims
// no code keeps are gone (the Vault-tier sweep of 2026-10-30):
//   - a "30-day minimum per object": nothing enforces or bills one (D-21:
//     drop it);
//   - "a restore takes minutes to start" with nothing past it: measured
//     200–459 s, one LA2 restore past 92 min, the vendor's own envelope 12 h;
//   - "seconds to a couple of minutes for first byte" and "roughly two weeks"
//     on the staging disk: a read of an object on tape answers 503 / 403 until
//     the restore lands, and LA2 moves to tape in one to four days.
func TestVaultCopy_OneRestoreSentenceAndNoUnkeptMinimum(t *testing.T) {
	v := landing.Get().Vault
	require.NotEmpty(t, v.RestoreTime)
	require.NotEmpty(t, v.RestoreTimeShort)
	assert.Contains(t, v.RestoreTime, "12 hours", "the sentence names the outer bound, not only the usual case")
	docs := renderDocsPages(zap.NewNop(), hosts.Get())
	pages := map[string]string{
		"faq":             string(docs["faq"]),
		"getting-started": string(docs["getting-started"]),
		"rclone":          string(docs["rclone"]),
		"llms.txt":        llmsTxtBody(hosts.Get()),
		"landing":         landingTemplateSrc,
		"pricing":         pricingTemplateSrc,
	}
	require.NotEmpty(t, pages["getting-started"])

	assert.Contains(t, pages["faq"], v.RestoreTime)
	assert.Equal(t, 2, strings.Count(pages["landing"], v.RestoreTime), "the Vault card and the Standard-or-Vault answer")
	assert.Contains(t, pages["pricing"], v.RestoreTimeShort)
	// The S3 answer to a read of a Smart-demoted object on tape says it too.
	assert.Contains(t, autoRestoreMessage(), v.RestoreTime)
	assert.NotContains(t, autoRestoreMessage(), "within a few hours")

	for name, body := range pages {
		for _, retired := range []string{
			"30-day minimum", "30 days)", "per-object minimum",
			"minutes to start", "restores start in minutes",
			"for first byte", "roughly two weeks", "a few hours once",
			"__VAULT_",
		} {
			assert.NotContains(t, body, retired, "%s still carries %q", name, retired)
		}
	}
}

// The sub-processor lists name every vendor that holds customer data or the
// database's backups, and say what each holds: Lyve holds the Vault tier's
// parity copy on prod (VAULT_PARITY_LEGS=lyve,… since 2026-10-25), Sync.com the
// off-site database backups (and is the parity's standby leg), Blomp the
// second encrypted backup copy (docs/DEPLOY.md § Backups). The three pages say
// the same.
func TestLegalSubProcessors_NameEveryVendorHoldingData(t *testing.T) {
	dir := filepath.Join("..", "dashboard", "templates", "legal")
	for _, page := range []string{"dpa.html", "gdpr.html", "privacy.html"} {
		b, err := os.ReadFile(filepath.Join(dir, page))
		require.NoError(t, err)
		body := string(b)
		for _, vendor := range []string{"iDrive", "Lyve", "Geyser", "Cloudflare", "Sync.com", "Blomp", "OneDrive", "Stripe"} {
			assert.Contains(t, body, vendor, "%s names no %s", page, vendor)
		}
		lyve := body[strings.Index(body, "Lyve"):]
		lyve = lyve[:strings.Index(lyve, "\n")]
		assert.Contains(t, lyve, "Vault", "%s: Lyve's row says it holds the Vault parity copy", page)
	}
}
