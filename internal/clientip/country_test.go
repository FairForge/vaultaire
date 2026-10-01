package clientip

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCountry_TrustedOnlyBehindCloudflare(t *testing.T) {
	// A Cloudflare edge address (from cloudflareRanges) as the trusted peer.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 172.70.219.143")
	r.Header.Set("CF-IPCountry", "de")
	assert.Equal(t, "DE", Country(r))

	r.Header.Set("CF-IPCountry", "t1")
	assert.Equal(t, "T1", Country(r), "Cloudflare's Tor marker is a valid code")

	r.Header.Set("CF-IPCountry", "DEU")
	assert.Equal(t, "", Country(r), "not alpha-2")

	r.Header.Del("CF-IPCountry")
	r.Header.Add("CF-IPCountry", "DE")
	r.Header.Add("CF-IPCountry", "FR")
	assert.Equal(t, "", Country(r), "a doubled header is ambiguous")

	direct := httptest.NewRequest("GET", "/", nil)
	direct.RemoteAddr = "203.0.113.9:1234"
	direct.Header.Set("CF-IPCountry", "DE")
	assert.Equal(t, "", Country(direct), "the header is client-supplied when the peer is not Cloudflare")
}
