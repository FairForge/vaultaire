// Package clientip resolves the real client address behind the proxy chain
// Vaultaire actually runs in — Cloudflare (orange-cloud hosts) → HAProxy →
// app, or client → HAProxy → app for the grey-cloud S3 endpoint — without
// trusting anything the client can write itself.
//
// Trust model (verified against the SLC haproxy.cfg, 2026-09-26):
//
//   - HAProxy runs `option forwardfor`, which APPENDS the TCP peer it accepted
//     to X-Forwarded-For. The LAST entry is therefore the address HAProxy saw;
//     every earlier entry is client-supplied. The app's own port is reachable
//     only through HAProxy (UFW allows 22/80/443), so the last entry — or
//     RemoteAddr when there is no XFF — is the trusted peer.
//   - CF-Connecting-IP is set by Cloudflare on proxied requests, but a client
//     hitting the origin directly can send it too. It is honoured only when
//     the trusted peer is inside Cloudflare's published edge ranges.
//
// Before this package, two helpers took CF-Connecting-IP and the FIRST XFF
// entry from any peer, which let one header defeat the API-key IP allowlist
// and the login rate limiter (review R1-01).
package clientip

import (
	"net"
	"net/http"
	"strings"
)

// cloudflareRanges is Cloudflare's published edge address space as of
// 2026-09-26 (https://www.cloudflare.com/ips-v4 and /ips-v6). The list has
// been stable for years; deploy/ufw-cloudflare-lockdown.sh fetches the live
// copy, and the test asserts the count so a refresh is a deliberate edit.
var cloudflareRanges = mustCIDRs(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("clientip: bad cloudflare range " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// IsCloudflare reports whether ip is inside Cloudflare's published edge ranges.
func IsCloudflare(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range cloudflareRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// FromRequest returns the client IP for r as a string suitable for allowlist
// checks, rate-limit keys and access logs. It never returns client-supplied
// text that does not parse as an IP address.
func FromRequest(r *http.Request) string {
	peer := trustedPeer(r)
	if IsCloudflare(net.ParseIP(peer)) {
		if cf := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); cf != nil {
			return cf.String()
		}
	}
	return peer
}

// trustedPeer is the address HAProxy accepted the connection from: the last
// X-Forwarded-For entry (appended by `option forwardfor`), or RemoteAddr when
// no proxy header is present.
func trustedPeer(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		entries := strings.Split(xff, ",")
		for i := len(entries) - 1; i >= 0; i-- {
			last := strings.TrimSpace(entries[i])
			if last == "" {
				continue
			}
			if ip := net.ParseIP(last); ip != nil {
				return ip.String()
			}
			break // unparsable tail: do not walk into client-supplied entries
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
