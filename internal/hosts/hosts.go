// Package hosts is the ONE source of every host name the product prints or
// matches (WP-DOMAIN-1, decision F-17): the canonical web origin, the S3
// endpoint, the public-content origin, the OAuth/e-mail base and the web
// aliases that 301 to the canonical origin. Nothing else in the repository
// types a domain into code or copy; it asks this package. It is a leaf
// package (zap only) because internal/api, internal/dashboard, internal/docs,
// internal/email and internal/sitestats all read it.
//
// Brand: Stored, a product of FairForge. stored.cloud is canonical;
// stored.ge and stored.pics are served forever as aliases — the web 301s
// (internal/api/host_canonical.go), the S3 API answers on every name with no
// redirect (a SigV4 signature covers the Host).
//
// Configured once at boot from the environment (FromEnv, then Set); read
// with Get, the way prices are read with landing.Get.
package hosts

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	"go.uber.org/zap"
)

const (
	// BrandName is the product's name as the copy and the Server header say it.
	BrandName = "Stored"
	// BrandDomain is the canonical registrable domain. It is used where a
	// real address is needed and the configured origin is a development one
	// (an e-mail address is never at localhost) and in the legal copy.
	BrandDomain = "stored.cloud"
)

// LegacyDomains are our other registrable domains, served forever as aliases
// (F-17). Always "own" for referrer statistics; cdn.<each> always routes to
// the CDN handler, whatever the environment says.
var LegacyDomains = []string{"stored.ge", "stored.pics"}

// Hosts is the configured set of origins. Origins are scheme://host[:port]
// with no trailing slash; Aliases are bare host names.
type Hosts struct {
	Canonical string   // web + dashboard origin (VAULTAIRE_PUBLIC_HOST)
	S3        string   // S3 endpoint (VAULTAIRE_ENDPOINT)
	CDN       string   // public-content origin (CDN_PUBLIC_HOST)
	BaseURL   string   // OAuth callbacks and e-mail links (VAULTAIRE_BASE_URL)
	Aliases   []string // web names that 301 to Canonical (VAULTAIRE_ALIAS_HOSTS)
}

// Defaults is the local development set: every origin http://localhost:port.
func Defaults(port int) Hosts {
	local := fmt.Sprintf("http://localhost:%d", port)
	return Hosts{Canonical: local, S3: local, CDN: local, BaseURL: local}
}

// FromEnv reads the five variables. A malformed value is logged at Warn and
// the default kept (the repository's env convention); a malformed alias is
// dropped on its own. Fallbacks, chosen so a deploy before the .env edit
// behaves exactly as before: Canonical ← VAULTAIRE_BASE_URL ← localhost;
// S3 and BaseURL ← Canonical; CDN ← https://cdn.<canonical host> when the
// canonical host is a public name, else Canonical (served under /cdn/).
func FromEnv(getenv func(string) string, port int, logger *zap.Logger) Hosts {
	if logger == nil {
		logger = zap.NewNop()
	}
	h := Defaults(port)
	origin := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return ""
		}
		o, err := parseOrigin(v)
		if err != nil {
			logger.Warn("rejected host setting, default kept", zap.String("var", name), zap.String("value", v), zap.Error(err))
			return ""
		}
		return o
	}
	public, base, endpoint, cdn := origin("VAULTAIRE_PUBLIC_HOST"), origin("VAULTAIRE_BASE_URL"), origin("VAULTAIRE_ENDPOINT"), origin("CDN_PUBLIC_HOST")

	switch {
	case public != "":
		h.Canonical = public
	case base != "":
		h.Canonical = base
	}
	h.S3, h.BaseURL = h.Canonical, h.Canonical
	if endpoint != "" {
		h.S3 = endpoint
	}
	if base != "" {
		h.BaseURL = base
	}
	h.CDN = h.Canonical
	if cdn != "" {
		h.CDN = cdn
	} else if ch := hostOf(h.Canonical); isPublicName(ch) {
		h.CDN = schemeOf(h.Canonical) + "://cdn." + ch
	}

	for _, a := range strings.Split(getenv("VAULTAIRE_ALIAS_HOSTS"), ",") {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if !isPublicName(a) {
			logger.Warn("rejected alias host, dropped", zap.String("var", "VAULTAIRE_ALIAS_HOSTS"), zap.String("value", a))
			continue
		}
		if a == hostOf(h.Canonical) || a == hostOf(h.S3) {
			logger.Warn("alias host names the canonical or the S3 host (a redirect loop), dropped",
				zap.String("var", "VAULTAIRE_ALIAS_HOSTS"), zap.String("value", a))
			continue
		}
		h.Aliases = append(h.Aliases, a)
	}
	return h
}

// parseOrigin accepts http(s)://host[:port] with at most a trailing slash.
func parseOrigin(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("want scheme://host[:port] with no path, query or credentials")
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

// normHost lower-cases a Host header value and strips its port.
func normHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}

func hostOf(origin string) string {
	if u, err := url.Parse(origin); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

func schemeOf(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	return "https"
}

// isPublicName: a DNS name with a dot, [a-z0-9.-] only, not an IP literal.
func isPublicName(h string) bool {
	if h == "" || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return !strings.HasPrefix(h, ".") && !strings.HasSuffix(h, ".")
}

// IsAlias reports whether host is a configured web alias (301 candidate).
func (h Hosts) IsAlias(host string) bool {
	host = normHost(host)
	for _, a := range h.Aliases {
		if host == a {
			return true
		}
	}
	return false
}

// IsS3Host reports whether host is the S3 endpoint's host or s3.<canonical
// host> or s3.<an alias / a legacy domain>. An S3 host never redirects.
func (h Hosts) IsS3Host(host string) bool {
	host = normHost(host)
	if host == "" {
		return false
	}
	if s3 := hostOf(h.S3); s3 != "" && s3 != hostOf(h.Canonical) && host == s3 {
		return true
	}
	rest, ok := strings.CutPrefix(host, "s3.")
	if !ok {
		return false
	}
	return rest == hostOf(h.Canonical) || h.IsAlias(rest) || rest == BrandDomain || isLegacy(rest)
}

// CDNHost is the public-content origin's host name.
func (h Hosts) CDNHost() string { return hostOf(h.CDN) }

// cdnDedicated: the CDN has a host of its own (cdn.stored.cloud, later the
// non-brand content domain); otherwise it shares the web origin under /cdn/.
func (h Hosts) cdnDedicated() bool {
	c := h.CDNHost()
	return c != "" && c != hostOf(h.Canonical) && c != hostOf(h.S3)
}

// CDNBase is the prefix a public object's URL is built on:
// CDNBase() + slug + "/" + bucket + "/" + key.
func (h Hosts) CDNBase() string {
	if h.cdnDedicated() {
		return h.CDN + "/"
	}
	return h.CDN + "/cdn/"
}

// IsCDNHost reports whether a request on host is served by the CDN handler:
// the dedicated CDN host, cdn.<canonical host>, cdn.<an alias>, and
// cdn.<brand or legacy domain> (cdn.stored.ge is served forever).
func (h Hosts) IsCDNHost(host string) bool {
	host = normHost(host)
	if host == "" {
		return false
	}
	if h.cdnDedicated() && host == h.CDNHost() {
		return true
	}
	rest, ok := strings.CutPrefix(host, "cdn.")
	if !ok {
		return false
	}
	return rest == hostOf(h.Canonical) || h.IsAlias(rest) || rest == BrandDomain || isLegacy(rest)
}

func isLegacy(host string) bool {
	for _, d := range LegacyDomains {
		if host == d {
			return true
		}
	}
	return false
}

// Domain is the canonical host name, or BrandDomain while the canonical
// origin is a development one (localhost, an IP).
func (h Hosts) Domain() string {
	if c := hostOf(h.Canonical); isPublicName(c) {
		return c
	}
	return BrandDomain
}

// SupportEmail is support@<Domain>.
func (h Hosts) SupportEmail() string { return "support@" + h.Domain() }

// SecurityEmail is security@<Domain> (security.txt's Contact).
func (h Hosts) SecurityEmail() string { return "security@" + h.Domain() }

// AbuseEmail is abuse@<Domain>.
func (h Hosts) AbuseEmail() string { return "abuse@" + h.Domain() }

// URL is the canonical origin + path (path starts with "/").
func (h Hosts) URL(path string) string { return h.Canonical + path }

// IsOwn reports whether host is one of ours, for referrer statistics and
// sign-up attribution: every configured host, the brand and legacy domains
// and their subdomains, and localhost.
func (h Hosts) IsOwn(host string) bool {
	host = normHost(host)
	if host == "" {
		return false
	}
	own := append([]string{"localhost", BrandDomain}, LegacyDomains...)
	own = append(own, h.Aliases...)
	for _, o := range []string{h.Canonical, h.S3, h.CDN, h.BaseURL} {
		if hh := hostOf(o); hh != "" {
			own = append(own, hh)
		}
	}
	for _, o := range own {
		if host == o || strings.HasSuffix(host, "."+o) {
			return true
		}
	}
	return false
}

// Tokens are the placeholders hand-written copy (the docs guides, e-mail
// bodies) carries instead of a typed host, in the landing.EgressTokens style.
func (h Hosts) Tokens() map[string]string {
	return map[string]string{
		"__HOST_WEB__":      h.Canonical,
		"__HOST_S3__":       h.S3,
		"__HOST_CDN_BASE__": h.CDNBase(),
		"__HOST_DOMAIN__":   h.Domain(),
		"__SUPPORT_EMAIL__": h.SupportEmail(),
		"__ABUSE_EMAIL__":   h.AbuseEmail(),
	}
}

// Expand replaces every token of Tokens in s.
func (h Hosts) Expand(s string) string {
	pairs := make([]string, 0, 12)
	for k, v := range h.Tokens() {
		pairs = append(pairs, k, v)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

var (
	mu      sync.RWMutex
	current *Hosts
)

// Set installs the process's hosts (the server does it once at boot).
func Set(h Hosts) {
	mu.Lock()
	defer mu.Unlock()
	c := h
	current = &c
}

// Get returns the hosts installed by Set, or Defaults(8000) when nothing
// was installed (unit tests, tools).
func Get() Hosts {
	mu.RLock()
	defer mu.RUnlock()
	if current == nil {
		return Defaults(8000)
	}
	return *current
}
