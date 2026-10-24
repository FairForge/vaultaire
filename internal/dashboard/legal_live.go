package dashboard

import (
	"sort"

	"github.com/FairForge/vaultaire/internal/flags"
)

// legalPages are the public legal pages RegisterRoutes mounts at
// /legal/<slug>, always on.
var legalPages = map[string]string{
	"privacy":  "templates/legal/privacy.html",
	"terms":    "templates/legal/terms.html",
	"dpa":      "templates/legal/dpa.html",
	"cookies":  "templates/legal/cookies.html",
	"aup":      "templates/legal/aup.html",
	"baa":      "templates/legal/baa.html",
	"gdpr":     "templates/legal/gdpr.html",
	"data-act": "templates/legal/data-act.html",
}

// LiveLegalPaths is every /legal/ page the canonical origin serves to a
// browser right now, sorted: the always-public pages, and — only while their
// flag is on — the law-enforcement pages (legal_le_pages) and the drafts
// (legal_draft_pages). The sitemap's source (Stage 1a Prompt 40, RP56-3):
// it never names a dark page.
func LiveLegalPaths(fl *flags.Service) []string {
	var out []string
	for slug := range legalPages {
		out = append(out, "/legal/"+slug)
	}
	if legalLEOn(fl) {
		for p := range legalLEPages {
			out = append(out, p)
		}
	}
	if legalDraftOn(fl) {
		for p := range legalDraftPages {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
