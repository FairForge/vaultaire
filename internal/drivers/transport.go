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
	completeHeaderTimeout time.Duration
	http1Only             bool
	freshConnections      bool
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

// WithCompleteMultipartHeaderTimeout gives CompleteMultipartUpload (POST
// ?uploadId=) its own, longer response-header timeout. The vendor assembles
// the object before it answers, and a header timeout is an error the SDK's
// retryer re-sends: a Complete that legitimately took longer than the
// general timeout was sent twice, the second answered NoSuchUpload (→ 500)
// after the first had committed (Prompt 2b.5 F3.8).
func WithCompleteMultipartHeaderTimeout(d time.Duration) TransportOption {
	return func(c *transportConfig) { c.completeHeaderTimeout = d }
}

// completeMultipartHeaderTimeout is the Complete's own bound: minutes, not
// the 60 s of every other call.
var completeMultipartHeaderTimeout = 10 * time.Minute

// completeSplitTransport sends CompleteMultipartUpload through complete (the
// same transport with the longer header timeout), everything else through
// the embedded one.
type completeSplitTransport struct {
	*http.Transport
	complete *http.Transport
}

func (t *completeSplitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.URL.Query().Has("uploadId") {
		return t.complete.RoundTrip(req)
	}
	return t.Transport.RoundTrip(req)
}

// WithFreshConnections makes every request dial its own connection and
// close it afterwards (no keep-alive pool). It is the client a request is
// retried on after a stall: a pooled connection may be the one that stalled.
func WithFreshConnections() TransportOption {
	return func(c *transportConfig) { c.freshConnections = true }
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
	cfg := &transportConfig{}
	for _, o := range opts {
		o(cfg)
	}
	if tunedTransportDisabled && !cfg.freshConnections {
		return http.DefaultClient
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
		DisableKeepAlives:     cfg.freshConnections,
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

	if cfg.completeHeaderTimeout > 0 && cfg.responseHeaderTimeout > 0 {
		complete := transport.Clone()
		complete.ResponseHeaderTimeout = cfg.completeHeaderTimeout
		return &http.Client{Transport: &completeSplitTransport{Transport: transport, complete: complete}}
	}
	return &http.Client{Transport: transport}
}
