package btcpay

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// RequestTimeout bounds every Greenfield call end to end (the prompt's
// 10 s): a BTCPay that does not answer is unready, never a hung checkout or
// webhook.
const RequestTimeout = 10 * time.Second

// newHTTPClient is the client every call goes through: the knobs of the
// drivers' TunedHTTPClient (internal/drivers/transport.go) that matter for a
// small JSON API — bounded dial, TLS handshake and response header, a small
// keep-alive pool, TLS 1.2+ — copied rather than imported, so the payments
// tree never links the storage drivers. fresh = a client whose every request
// dials its own connection: the one a GET is retried on (a pooled
// connection may be the one that stalled).
func newHTTPClient(fresh bool) *http.Client {
	dialer := &net.Dialer{Timeout: RequestTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   RequestTimeout,
		ResponseHeaderTimeout: RequestTimeout,
		ExpectContinueTimeout: time.Second,
		DisableKeepAlives:     fresh,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: tr, Timeout: RequestTimeout,
		// A redirect from the API is never followed with the key on it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
