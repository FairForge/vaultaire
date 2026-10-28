// cf-range refuses a host that resolves into Cloudflare's edge ranges
// (RP74-1, Stage 1a Prompt 48): a data node's control-plane URL must be a
// DNS-only origin, because the edge-call gate accepts a node's token only
// from the node's own address as HAProxy sees it — through Cloudflare's
// proxy the peer is Cloudflare. deploy/node/install.sh runs it.
//
//	cf-range <url-or-host>
//
// Exit 0: no address of the host is Cloudflare's. Exit 1: one is (named
// on stdout). Exit 2: usage, or the host does not resolve. The ranges are
// internal/clientip's table (the one the origin trusts).
package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/FairForge/vaultaire/internal/clientip"
)

func main() {
	os.Exit(run(os.Args[1:], net.LookupIP, os.Stdout))
}

func run(args []string, lookup func(string) ([]net.IP, error), out io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(out, "usage: cf-range <url-or-host>")
		return 2
	}
	host := args[0]
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Hostname() == "" {
			_, _ = fmt.Fprintf(out, "cf-range: %q is not a URL\n", host)
			return 2
		}
		host = u.Hostname()
	}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		var err error
		if ips, err = lookup(host); err != nil || len(ips) == 0 {
			_, _ = fmt.Fprintf(out, "cf-range: %s does not resolve: %v\n", host, err)
			return 2
		}
	}
	code := 0
	for _, ip := range ips {
		if clientip.IsCloudflare(ip) {
			_, _ = fmt.Fprintf(out, "%s resolves to %s, a Cloudflare address: the host is proxied\n", host, ip)
			code = 1
		}
	}
	return code
}
