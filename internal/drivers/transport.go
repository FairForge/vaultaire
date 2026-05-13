package drivers

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// dnsCache is a shared DNS cache for all S3-compatible drivers.
// OneDrive has its own (odDNSCache) because it was written first.
var dnsCache sync.Map

var tunedTransportDisabled = strings.EqualFold(os.Getenv("VAULTAIRE_TUNED_TRANSPORT"), "false")

type transportConfig struct {
	insecureTLS           bool
	responseHeaderTimeout time.Duration
	http1Only             bool
}

// TransportOption configures TunedHTTPClient.
type TransportOption func(*transportConfig)

// WithInsecureTLS disables TLS certificate verification.
func WithInsecureTLS() TransportOption {
	return func(c *transportConfig) { c.insecureTLS = true }
}

// WithResponseHeaderTimeout sets how long to wait for response headers.
func WithResponseHeaderTimeout(d time.Duration) TransportOption {
	return func(c *transportConfig) { c.responseHeaderTimeout = d }
}

// WithHTTP1Only disables HTTP/2 for backends that perform better with H1.
func WithHTTP1Only() TransportOption {
	return func(c *transportConfig) { c.http1Only = true }
}

func cachedDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dialer.DialContext(ctx, network, addr)
		}
		if cached, ok := dnsCache.Load(host); ok {
			if addrs, ok := cached.([]string); ok && len(addrs) > 0 {
				return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
			}
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			return dialer.DialContext(ctx, network, addr)
		}
		dnsCache.Store(host, addrs)
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
	}
}

// TunedHTTPClient returns an HTTP client with connection pooling, DNS caching,
// TLS session resumption, and large I/O buffers tuned for high-throughput
// storage operations. Set VAULTAIRE_TUNED_TRANSPORT=false to disable.
func TunedHTTPClient(opts ...TransportOption) *http.Client {
	if tunedTransportDisabled {
		return http.DefaultClient
	}

	cfg := &transportConfig{}
	for _, o := range opts {
		o(cfg)
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.responseHeaderTimeout,
		ReadBufferSize:        4 << 20,
		WriteBufferSize:       4 << 20,
		DisableCompression:    true,
		ForceAttemptHTTP2:     !cfg.http1Only,
		DialContext:           cachedDialContext(dialer),
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
			InsecureSkipVerify: cfg.insecureTLS, // #nosec G402 -- operator opt-in via WithInsecureTLS or S3COMPAT_INSECURE_TLS
		},
	}

	if cfg.http1Only {
		transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	}

	return &http.Client{Transport: transport}
}
