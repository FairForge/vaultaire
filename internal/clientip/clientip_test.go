package clientip

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func req(remoteAddr string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestFromRequest_NoProxyHeadersUsesRemoteAddr(t *testing.T) {
	assert.Equal(t, "198.51.100.4", FromRequest(req("198.51.100.4:5555", nil)))
	assert.Equal(t, "2001:db8::9", FromRequest(req("[2001:db8::9]:5555", nil)))
}

func TestFromRequest_LastForwardedEntryIsThePeerHAProxySaw(t *testing.T) {
	// HAProxy `option forwardfor` APPENDS the TCP peer; earlier entries are
	// whatever the client sent. Direct-to-origin (grey-cloud s3.stored.ge).
	assert.Equal(t, "198.51.100.4", FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For": "198.51.100.4",
	})))
	assert.Equal(t, "198.51.100.4", FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For": "9.9.9.9, 10.0.0.1 , 198.51.100.4",
	})))
}

func TestFromRequest_ForgedCloudflareHeaderFromNonCloudflarePeerIsIgnored(t *testing.T) {
	// The IP-allowlist bypass: a client on the grey-cloud S3 endpoint sends
	// CF-Connecting-IP naming an allowlisted address.
	got := FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For":  "198.51.100.4",
		"CF-Connecting-IP": "203.0.113.250",
	}))
	assert.Equal(t, "198.51.100.4", got)
}

func TestFromRequest_CloudflarePeerHonoursCFConnectingIP(t *testing.T) {
	// Orange-cloud host: Cloudflare edge (104.16.0.0/13) is HAProxy's peer,
	// Cloudflare appended the visitor to XFF and set CF-Connecting-IP.
	got := FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For":  "203.0.113.7, 104.16.1.1",
		"CF-Connecting-IP": "203.0.113.7",
	}))
	assert.Equal(t, "203.0.113.7", got)

	// A client-supplied first XFF entry still cannot win.
	got = FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For":  "9.9.9.9, 203.0.113.7, 104.16.1.1",
		"CF-Connecting-IP": "203.0.113.7",
	}))
	assert.Equal(t, "203.0.113.7", got)

	// IPv6 edge.
	got = FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For":  "2001:db8::7, 2606:4700::1",
		"CF-Connecting-IP": "2001:db8::7",
	}))
	assert.Equal(t, "2001:db8::7", got)
}

func TestFromRequest_CloudflarePeerWithoutHAProxy(t *testing.T) {
	// No XFF at all (app reached straight from an edge, e.g. a tunnel):
	// RemoteAddr itself is the peer.
	got := FromRequest(req("172.64.0.9:443", map[string]string{
		"CF-Connecting-IP": "203.0.113.7",
	}))
	assert.Equal(t, "203.0.113.7", got)
}

func TestFromRequest_GarbageIsNeverReturnedAsAnIP(t *testing.T) {
	// Unparsable CF-Connecting-IP from a real edge → fall back to the peer.
	got := FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For":  "104.16.1.1",
		"CF-Connecting-IP": "not-an-ip",
	}))
	assert.Equal(t, "104.16.1.1", got)

	// Unparsable last XFF entry → RemoteAddr.
	got = FromRequest(req("127.0.0.1:1", map[string]string{
		"X-Forwarded-For": "198.51.100.4, unknown",
	}))
	assert.Equal(t, "127.0.0.1", got)
}

func TestIsCloudflare_PublishedRangesParse(t *testing.T) {
	require.Len(t, cloudflareRanges, 22, "15 IPv4 + 7 IPv6 ranges as published 2026-09-26")
	assert.True(t, IsCloudflare(net.ParseIP("104.16.1.1")))
	assert.True(t, IsCloudflare(net.ParseIP("172.64.0.9")))
	assert.True(t, IsCloudflare(net.ParseIP("2606:4700::1")))
	assert.False(t, IsCloudflare(net.ParseIP("198.51.100.4")))
	assert.False(t, IsCloudflare(net.ParseIP("127.0.0.1")))
	assert.False(t, IsCloudflare(nil))
}

// HAProxy `option forwardfor` adds X-Forwarded-For as a NEW header occurrence
// at the end of the header list ("the server must be configured to always use
// the last occurrence of this header only" — HAProxy 2.8 manual). Go's
// Header.Get returns only the FIRST occurrence, so a client that sends its own
// X-Forwarded-For line would otherwise be read instead of the peer HAProxy
// appended.
func TestFromRequest_ClientSuppliedForwardedForLineIsIgnored(t *testing.T) {
	r := req("127.0.0.1:1", nil)
	r.Header.Add("X-Forwarded-For", "9.9.9.9")      // client-supplied line
	r.Header.Add("X-Forwarded-For", "198.51.100.4") // HAProxy-appended line
	assert.Equal(t, "198.51.100.4", FromRequest(r))

	// Same behind Cloudflare: the peer is the edge from the LAST line, and a
	// CF-Connecting-IP forged on a client line is not the one honoured.
	r = req("127.0.0.1:1", nil)
	r.Header.Add("X-Forwarded-For", "9.9.9.9")
	r.Header.Add("X-Forwarded-For", "203.0.113.7, 104.16.1.1")
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	assert.Equal(t, "203.0.113.7", FromRequest(r))

	// A client line that HAProxy followed with a bare peer line must not be
	// promoted just because the peer line has fewer entries.
	r = req("127.0.0.1:1", nil)
	r.Header.Add("X-Forwarded-For", "9.9.9.9, 8.8.8.8")
	r.Header.Add("X-Forwarded-For", "198.51.100.4")
	assert.Equal(t, "198.51.100.4", FromRequest(r))
}

// Two CF-Connecting-IP occurrences are ambiguous (one may be client-supplied);
// fall back to the trusted peer rather than guess.
func TestFromRequest_AmbiguousCloudflareHeaderFallsBackToPeer(t *testing.T) {
	r := req("127.0.0.1:1", nil)
	r.Header.Add("X-Forwarded-For", "104.16.1.1")
	r.Header.Add("CF-Connecting-IP", "9.9.9.9")
	r.Header.Add("CF-Connecting-IP", "203.0.113.7")
	assert.Equal(t, "104.16.1.1", FromRequest(r))
}
