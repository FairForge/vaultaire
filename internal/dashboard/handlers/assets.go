package handlers

import (
	"html/template"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

// Static assets are served from the binary under /static/ and cached at
// Cloudflare's edge for hours. A deploy that changes style.css or a script
// must not pair new HTML with the cached old file, so every asset URL the
// templates emit carries a version query that changes with the files:
// {{asset "/static/css/style.css"}} → /static/css/style.css?v=<hash>.
//
// AssetVersion is set by the dashboard package at init from a hash of the
// embedded static/ tree ("dev" until then, e.g. in handler unit tests).
var AssetVersion = "dev"

// AssetURL returns path with the version query.
func AssetURL(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "v=" + AssetVersion
}

// TemplateFuncs is the FuncMap every layout is parsed with.
func TemplateFuncs() template.FuncMap {
	return template.FuncMap{"asset": AssetURL, "egress": egressCopy}
}

// egressCopy gives a template the egress rule as the price file words it
// (prices.json `egress`; WP-R10-9): "std" and "vault" are the allowance
// ratios, "past" what happens past the allowance, "one" that the allowance
// is one number per account.
func egressCopy(what string) string {
	e := landing.Get().Egress
	switch what {
	case "std":
		return strconv.FormatFloat(e.StandardFreeRatio, 'g', -1, 64)
	case "vault":
		return strconv.FormatFloat(e.VaultRestoreFreeRatio, 'g', -1, 64)
	case "past":
		return e.PastAllowance
	case "one":
		return e.OneAllowance
	}
	return ""
}
