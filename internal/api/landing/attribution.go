package landing

import (
	"net/url"
	"strings"
)

// Attribution is where a sign-up came from (pre-launch checklist item 7):
// the referring site (host only) and the campaign labels. The landing page
// carries them into the waitlist payload and onto the /register link
// (page.js); the API and the register form store them.
type Attribution struct {
	Referrer    string // host of the referring page, e.g. "lowendtalk.com"; "" when none
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
}

const maxAttributionField = 100

// ParseAttribution normalises raw referrer/UTM input. The referrer may be a
// full URL (document.referrer, the Referer header) or a bare host; only the
// lower-cased host is kept. UTM values are trimmed, capped and stripped of
// control characters. Anything that does not parse is dropped, never rejected
// — attribution is a hint and must not fail a sign-up.
func ParseAttribution(referrer, source, medium, campaign string) Attribution {
	return Attribution{
		Referrer:    referrerHost(referrer),
		UTMSource:   cleanLabel(source),
		UTMMedium:   cleanLabel(medium),
		UTMCampaign: cleanLabel(campaign),
	}
}

// Source is the one-word answer for waitlist_signups.source: the utm_source
// if the visitor arrived with a campaign, else the referring host, else the
// default (historically always "landing").
func (a Attribution) Source(fallback string) string {
	switch {
	case a.UTMSource != "":
		return a.UTMSource
	case a.Referrer != "":
		return a.Referrer
	}
	return fallback
}

// Empty reports whether nothing was captured.
func (a Attribution) Empty() bool {
	return a.Referrer == "" && a.UTMSource == "" && a.UTMMedium == "" && a.UTMCampaign == ""
}

func referrerHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	host := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		host = u.Hostname()
	} else if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		host = raw[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || len(host) > maxAttributionField || !hostCharset(host) {
		return ""
	}
	// Our own pages are not a source.
	if host == "stored.ge" || strings.HasSuffix(host, ".stored.ge") || host == "localhost" {
		return ""
	}
	return host
}

func hostCharset(s string) bool {
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

func cleanLabel(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxAttributionField {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
