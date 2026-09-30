package landing

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseAttribution(t *testing.T) {
	a := ParseAttribution("https://lowendtalk.com/discussion/12345?x=1", " let ", "forum", "launch\x00day")
	assert.Equal(t, "lowendtalk.com", a.Referrer, "host only — no path, no query")
	assert.Equal(t, "let", a.UTMSource)
	assert.Equal(t, "forum", a.UTMMedium)
	assert.Equal(t, "launchday", a.UTMCampaign, "control characters dropped")
	assert.Equal(t, "let", a.Source("landing"), "utm_source wins")

	b := ParseAttribution("www.Reddit.com/r/DataHoarder/", "", "", "")
	assert.Equal(t, "www.reddit.com", b.Referrer)
	assert.Equal(t, "www.reddit.com", b.Source("landing"), "referrer host when no campaign")

	c := ParseAttribution("https://stored.ge/#packs", "", "", "")
	assert.True(t, c.Empty(), "our own pages are not a source")
	assert.Equal(t, "landing", c.Source("landing"))

	d := ParseAttribution("javascript:alert(1)", "", "", "")
	assert.Equal(t, "", d.Referrer)
	e := ParseAttribution("", strings.Repeat("x", 500), "", "")
	assert.Len(t, e.UTMSource, maxAttributionField, "capped")
	f := ParseAttribution("not a host!", "", "", "")
	assert.Equal(t, "", f.Referrer)
}
