package drivers

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var tunedTransportDisabled = strings.EqualFold(os.Getenv("VAULTAIRE_TUNED_TRANSPORT"), "false")

type transportConfig struct {
	insecureTLS           bool
	responseHeaderTimeout time.Duration
	ops                   *OpTimeouts
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

// OpTimeouts are the response-header bounds of one S3-class client by
// request class (WP-R6-3): how long a request may wait for the backend's
// first byte once it is sent (Go's ResponseHeaderTimeout: the clock starts
// after the request body is written, so an upload's own time is never in
// it). A backend that answers nothing trips ITS breaker within seconds and
// never a client's read timeout — on 2026-10-17 a 60 s bound here, the PUT
// deadline of a small body and aws-cli's 60 s read timeout fired in the
// same second (RP8-9). The invariant, with its exceptions, is in
// internal/drivers/CLAUDE.md and data_path_bounds_test.go.
type OpTimeouts struct {
	// Meta: HEAD (HeadObject, HeadBucket), DELETE, a listing page, bucket
	// and multipart bookkeeping (CreateMultipartUpload, Abort, DeleteObjects).
	Meta time.Duration
	// Get: GetObject's first byte (the body then streams, ctx-bound).
	Get time.Duration
	// Put: PutObject / UploadPart's answer after the body. The per-size PUT
	// deadline (s3PutDeadline) is the PUT's real bound and fires first.
	Put time.Duration
	// Complete: CompleteMultipartUpload and a server-side copy — the vendor
	// assembles (or copies) the object before it answers, and a header
	// timeout is retried by the SDK: a re-sent Complete whose first copy had
	// committed answered NoSuchUpload (Prompt 2b.5 F3.8).
	Complete time.Duration
}

// WithOpTimeouts gives each request class its own response-header bound,
// each through its own clone of the transport (one connection pool per
// class, shared by every request of that class — never a client per
// request).
func WithOpTimeouts(t OpTimeouts) TransportOption {
	return func(c *transportConfig) { c.ops = &t }
}

// opClass is a request's class for its response-header bound.
type opClass int

const (
	classMeta opClass = iota
	classGet
	classPut
	classComplete
	opClasses
)

// requestClass reads the class off the request the SDK built (path-style,
// as every S3-class driver here addresses): GetObject carries
// `x-id=GetObject`, a listing `list-type=2`; a POST with `uploadId` is the
// Complete, one with `uploads` the Create; a PUT with x-amz-copy-source is
// a server-side copy; a POST `?restore` (Geyser) is classed with the
// Complete.
func requestClass(req *http.Request) opClass {
	q := req.URL.Query()
	hasKey := strings.Contains(strings.Trim(req.URL.Path, "/"), "/")
	switch req.Method {
	case http.MethodGet:
		if q.Get("x-id") == "GetObject" || (hasKey && len(q) == 0) {
			return classGet
		}
	case http.MethodPut:
		if req.Header.Get("X-Amz-Copy-Source") != "" {
			return classComplete
		}
		if hasKey {
			return classPut
		}
	case http.MethodPost:
		// RestoreObject (Geyser's recall) keeps the long bound with the
		// Complete: the tape gateway's answer to it is not measured.
		if q.Has("uploadId") || q.Has("restore") {
			return classComplete
		}
	}
	return classMeta
}

// opSplitTransport sends each request through the transport of its class.
type opSplitTransport struct {
	byClass [opClasses]*http.Transport
}

func (t *opSplitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.byClass[requestClass(req)].RoundTrip(req)
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

// cachedDialContext dials through the drivers' shared DNS cache
// (dns_cache.go): an answer kept DRIVER_DNS_TTL, the dials rotated over it.
func cachedDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return sharedDNS.dialer(dialer.DialContext, dialer.Timeout)
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
			InsecureSkipVerify: cfg.insecureTLS, // #nosec G402 -- operator opt-in via WithInsecureTLS
		},
	}

	if cfg.http1Only {
		transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	}

	if cfg.ops != nil {
		split := &opSplitTransport{}
		for class, d := range [opClasses]time.Duration{classMeta: cfg.ops.Meta, classGet: cfg.ops.Get, classPut: cfg.ops.Put, classComplete: cfg.ops.Complete} {
			tr := transport.Clone()
			tr.ResponseHeaderTimeout = d
			split.byClass[class] = tr
		}
		return &http.Client{Transport: split}
	}
	return &http.Client{Transport: transport}
}
