package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/modes"
)

// Keystone K3 (contract line 3): modes.json is the ONE source of what a mode
// is called and what it lets us read — the prices.json pattern. A mode's
// sentence typed into a template, a landing source, a served guide, the
// OpenAPI text or llms.txt fails here; so do "<name> mode", "keys we hold"
// (a modes.json sentence that is true only with the master key) and
// "zero-knowledge" (contract line 8). The names Private and Public as a
// label's whole text fail too: a bucket's mode label is modes.Name.
// "Standard" is also the downstairs TIER's name, so a bare "Standard" label
// cannot be told apart and is not checked; a mode label still comes from
// modes.Name.

// modeCopyAllowed are the typed exceptions, each with its reason: the file,
// the exact match, why. legacyVisibilityLabels are K3 PR 3's job — the
// bucket list and settings print "Private" for visibility 'private'
// (Standard: the name trap, docs/reviews/K3-PROBE.md R1/R2); behind
// modes_copy they render modes.Name, the flag-off branch keeps the old
// label until the flag is flipped globally.
var modeCopyAllowed = map[string]string{
	"landing/builder.js|private mode":                              "the browser's private browsing (a localStorage comment), not a Stored mode",
	"templates/customer/buckets.html|>Private<":                    "legacy visibility label (K3-PROBE R1) — replaced behind modes_copy",
	"templates/customer/buckets.html|>Public<":                     "legacy visibility label (K3-PROBE R1) — replaced behind modes_copy",
	"templates/customer/bucket_settings.html|>Private<":            "legacy visibility toggle (K3-PROBE R2) — replaced behind modes_copy",
	"templates/customer/bucket_settings.html|>Public<":             "legacy visibility toggle (K3-PROBE R2) — replaced behind modes_copy",
	"templates/customer/bucket_settings.html|<span>Private</span>": "legacy visibility toggle (K3-PROBE R2) — replaced behind modes_copy",
}

// modeCopySources are the files whose text a customer reads: every
// dashboard template, the landing sources, the served guides, the OpenAPI
// generator and llms.txt.
func modeCopySources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, fs.WalkDir(dashboard.Templates, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(dashboard.Templates, p)
		require.NoError(t, err)
		out[p] = string(b)
		return nil
	}))
	for _, p := range []string{
		"landing/landing.src.html", "landing/builder.html", "landing/builder.js", "landing/page.js",
		"docs_getting_started.md", "docs_rclone.md", "docs_faq.md", "llms_txt.go", "../docs/openapi.go",
	} {
		b, err := os.ReadFile(filepath.FromSlash(p))
		require.NoError(t, err)
		out[p] = string(b)
	}
	return out
}

func TestModeNamesOnlyFromModesJSON(t *testing.T) {
	f := modes.All()
	var sentences []string
	for _, m := range f.Modes {
		for _, s := range []modes.Sentence{m.ServerHolds, m.Customer, m.How} {
			for _, text := range []string{s.Text, s.WithoutMasterKey} {
				if len(text) >= 24 {
					sentences = append(sentences, text)
				}
			}
		}
	}
	require.NotEmpty(t, sentences)
	var names []string
	for _, m := range f.Modes {
		names = append(names, regexp.QuoteMeta(m.Name))
	}
	modePhrase := regexp.MustCompile(`(?i)\b(` + strings.Join(names, "|") + `) mode\b`)
	label := regexp.MustCompile(`>(Private|Public)<|<span>(Private|Public)</span>`)
	forbidden := regexp.MustCompile(`(?i)zero[- ‑]?knowledge|keys we hold`)

	allowedSeen := map[string]bool{}
	for path, body := range modeCopySources(t) {
		var hits []string
		for _, s := range sentences {
			if strings.Contains(body, s) {
				hits = append(hits, s)
			}
		}
		hits = append(hits, modePhrase.FindAllString(body, -1)...)
		hits = append(hits, label.FindAllString(body, -1)...)
		hits = append(hits, forbidden.FindAllString(body, -1)...)
		for _, h := range hits {
			if reason, ok := modeCopyAllowed[allowedKey(path, h)]; ok && reason != "" {
				allowedSeen[allowedKey(path, h)] = true
				continue
			}
			assert.Failf(t, "mode copy typed outside modes.json",
				"%s types %q — render it from internal/modes (modes.json), K3", path, h)
		}
	}
	// An allowance nothing needs any more is removed with the code that
	// needed it.
	for k := range modeCopyAllowed {
		assert.True(t, allowedSeen[k], "modeCopyAllowed[%q] matches nothing — remove it", k)
	}
}

// allowedKey is the modeCopyAllowed key of a hit: the source path as
// listed (templates/… or landing/…) and the match, the "mode" phrase
// lower-cased.
func allowedKey(path, hit string) string {
	if strings.HasSuffix(strings.ToLower(hit), " mode") {
		hit = strings.ToLower(hit)
	}
	return path + "|" + hit
}
