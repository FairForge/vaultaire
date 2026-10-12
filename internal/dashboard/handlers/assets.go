package handlers

import (
	"html/template"
	"net/url"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/hosts"
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
	return template.FuncMap{"asset": AssetURL, "egress": egressCopy, "host": hostCopy}
}

// hostCopy gives a template a host or an address from the one host source
// (internal/hosts, WP-DOMAIN-1) — no template types a domain:
// {{host "web"}} the canonical origin, {{host "s3"}} the S3 endpoint,
// {{host "s3host"}} its bare host name (s3cmd --host), {{host "cdn"}} the
// public-object URL prefix, {{host "cdnhost"}} its host, {{host "domain"}} the canonical domain,
// {{host "support"}} / {{host "security"}} / {{host "abuse"}} the addresses,
// {{host "brand"}} the product name, {{host "aliases"}} the legacy domains
// ("stored.ge and stored.pics"). An unknown name renders "" (the
// no-stale-host test reads every page).
func hostCopy(what string) string {
	h := hosts.Get()
	switch what {
	case "web":
		return h.Canonical
	case "s3":
		return h.S3
	case "s3host":
		if u, err := url.Parse(h.S3); err == nil {
			return u.Host
		}
	case "cdn":
		return h.CDNBase()
	case "cdnhost":
		return h.CDNHost()
	case "domain":
		return h.Domain()
	case "support":
		return h.SupportEmail()
	case "security":
		return h.SecurityEmail()
	case "abuse":
		return h.AbuseEmail()
	case "brand":
		return hosts.BrandName
	case "aliases":
		return strings.Join(hosts.LegacyDomains, " and ")
	}
	return ""
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
