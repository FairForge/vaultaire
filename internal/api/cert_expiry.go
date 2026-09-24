package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// TLS certificate expiry monitoring (launch sequence 5.1/6: the origin
// Let's Encrypt cert expires 2026-10-29, two days before launch, and certbot's
// --dry-run cannot prove the real renewal will land — it asks the STAGING CA
// about a production cert and gets "Certificate not found"). So the app
// itself watches the leaf certificate HAProxy actually serves and exports
// its NotAfter; the Prometheus rules in deploy/monitoring/vaultaire-tls.yml
// page when it gets within 14 days.
//
// Targets come from TLS_CERT_PROBE_TARGETS: a comma-separated list of
// `sni@host:port` (or just `sni`, meaning `sni@sni:443`). On SLC that is
// `stored.ge@127.0.0.1:443,stored.cloud@127.0.0.1:443` — dial the local
// HAProxy with the public SNI so the probe sees the ORIGIN cert, not
// Cloudflare's edge cert. The handshake is fully verified; when verification
// fails (expired, wrong name, unknown CA) the leaf is taken from the x509
// error itself, so an expired or mis-issued cert still reports its NotAfter
// (that is the alert) without ever disabling verification.

const (
	defaultCertProbeInterval = time.Hour
	certProbeTimeout         = 10 * time.Second
)

type certProbeTarget struct {
	ServerName string // SNI + metric label
	Addr       string // host:port actually dialled
}

// parseCertProbeTargets turns the env spec into targets. Empty/blank entries
// are dropped; a bare name dials name:443.
func parseCertProbeTargets(spec string) []certProbeTarget {
	var out []certProbeTarget
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		sni, addr, found := strings.Cut(item, "@")
		sni = strings.TrimSpace(sni)
		addr = strings.TrimSpace(addr)
		if sni == "" {
			continue
		}
		if !found || addr == "" {
			addr = net.JoinHostPort(sni, "443")
		} else if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "443")
		}
		out = append(out, certProbeTarget{ServerName: sni, Addr: addr})
	}
	return out
}

// probeCertNotAfter performs a verified TLS handshake and returns the leaf's
// NotAfter. A verification failure that carries the certificate (expired,
// hostname mismatch, unknown authority) still yields NotAfter — the whole
// point of the probe is to see an expiring or already-expired cert.
func probeCertNotAfter(ctx context.Context, t certProbeTarget) (time.Time, error) {
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: certProbeTimeout},
		Config: &tls.Config{
			ServerName: t.ServerName,
			MinVersion: tls.VersionTLS12,
		},
	}
	ctx, cancel := context.WithTimeout(ctx, certProbeTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		if leaf := certFromVerifyError(err); leaf != nil {
			return leaf.NotAfter, nil
		}
		return time.Time{}, fmt.Errorf("tls dial %s (sni %s): %w", t.Addr, t.ServerName, err)
	}
	defer func() { _ = conn.Close() }()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return time.Time{}, errors.New("not a tls connection")
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, errors.New("no peer certificate presented")
	}
	return certs[0].NotAfter, nil
}

// certFromVerifyError extracts the presented leaf from the x509 verification
// errors that carry it. Returns nil for anything else (network errors,
// protocol errors) so those still surface as probe failures.
func certFromVerifyError(err error) *x509.Certificate {
	var invalid x509.CertificateInvalidError // expired, not yet valid, …
	if errors.As(err, &invalid) && invalid.Cert != nil {
		return invalid.Cert
	}
	var unknownCA x509.UnknownAuthorityError
	if errors.As(err, &unknownCA) && unknownCA.Cert != nil {
		return unknownCA.Cert
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) && hostname.Certificate != nil {
		return hostname.Certificate
	}
	return nil
}

type certExpiryState struct {
	NotAfter time.Time
	OK       bool
	Err      string
	Checked  time.Time
}

// certExpiryMonitor owns the probe loop + last-known state per target.
type certExpiryMonitor struct {
	targets  []certProbeTarget
	interval time.Duration
	probe    func(context.Context, certProbeTarget) (time.Time, error)
	logger   *zap.Logger

	mu     sync.RWMutex
	states map[string]certExpiryState
}

func newCertExpiryMonitor(targets []certProbeTarget, logger *zap.Logger) *certExpiryMonitor {
	if len(targets) == 0 {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &certExpiryMonitor{
		targets:  targets,
		interval: defaultCertProbeInterval,
		probe:    probeCertNotAfter,
		logger:   logger,
		states:   make(map[string]certExpiryState, len(targets)),
	}
}

// newCertExpiryMonitorFromEnv returns nil when TLS_CERT_PROBE_TARGETS is unset.
func newCertExpiryMonitorFromEnv(getenv func(string) string, logger *zap.Logger) *certExpiryMonitor {
	return newCertExpiryMonitor(parseCertProbeTargets(getenv("TLS_CERT_PROBE_TARGETS")), logger)
}

// probeAll runs every target once and records the outcome.
func (m *certExpiryMonitor) probeAll(ctx context.Context) {
	for _, t := range m.targets {
		notAfter, err := m.probe(ctx, t)
		st := certExpiryState{NotAfter: notAfter, OK: err == nil, Checked: time.Now()}
		if err != nil {
			st.Err = err.Error()
			m.logger.Warn("tls cert probe failed", zap.String("sni", t.ServerName), zap.String("addr", t.Addr), zap.Error(err))
		} else if until := time.Until(notAfter); until < 14*24*time.Hour {
			m.logger.Warn("tls cert expires soon", zap.String("sni", t.ServerName), zap.Time("not_after", notAfter), zap.Duration("remaining", until))
		}
		m.mu.Lock()
		// A failed probe keeps the last good NotAfter so the expiry series does
		// not vanish (and un-fire the alert) just because one dial failed.
		if err != nil {
			if prev, ok := m.states[t.ServerName]; ok && !prev.NotAfter.IsZero() {
				st.NotAfter = prev.NotAfter
			}
		}
		m.states[t.ServerName] = st
		m.mu.Unlock()
	}
}

// run probes immediately, then on the interval, until ctx is cancelled.
func (m *certExpiryMonitor) run(ctx context.Context) {
	m.probeAll(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeAll(ctx)
		}
	}
}

func (m *certExpiryMonitor) snapshot() map[string]certExpiryState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]certExpiryState, len(m.states))
	for k, v := range m.states {
		out[k] = v
	}
	return out
}

// certExpiryCollector exports the monitor state on every scrape.
type certExpiryCollector struct {
	m       *certExpiryMonitor
	expiry  *prometheus.Desc
	probeOK *prometheus.Desc
}

func newCertExpiryCollector(m *certExpiryMonitor) *certExpiryCollector {
	return &certExpiryCollector{
		m: m,
		expiry: prometheus.NewDesc("vaultaire_tls_cert_expiry_timestamp_seconds",
			"NotAfter (unix seconds) of the leaf certificate served for the SNI; last good value is kept across probe failures.",
			[]string{"sni"}, nil),
		probeOK: prometheus.NewDesc("vaultaire_tls_cert_probe_ok",
			"1 if the last TLS handshake for the SNI succeeded, else 0.",
			[]string{"sni"}, nil),
	}
}

func (c *certExpiryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.expiry
	ch <- c.probeOK
}

func (c *certExpiryCollector) Collect(ch chan<- prometheus.Metric) {
	states := c.m.snapshot()
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := states[n]
		ok := 0.0
		if st.OK {
			ok = 1
		}
		ch <- prometheus.MustNewConstMetric(c.probeOK, prometheus.GaugeValue, ok, n)
		if !st.NotAfter.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.expiry, prometheus.GaugeValue, float64(st.NotAfter.Unix()), n)
		}
	}
}
