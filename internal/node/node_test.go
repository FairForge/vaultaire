package node

// Prompt 48 Part 0 (RP74-1, RP74-3, RP74-9, RP74-10): the node says when
// the control plane will not take it, terminates TLS itself (an operator's
// pair or ACME) and refuses a public bind without it, matches the origin's
// reserved paths by their first segment, and serves /metrics on its own
// (loopback by default) address — never on the public port.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

func labEnv(extra map[string]string) func(string) string {
	env := map[string]string{
		"NODE_CONTROL_PLANE_URL": "http://127.0.0.1:1",
		"NODE_TOKEN":             strings.Repeat("ab", 32),
		"NODE_BIND":              "127.0.0.1",
	}
	for k, v := range extra {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

func TestNode_TwoNotFoundHeartbeatsLogOneError(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotFound)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer cp.Close()
	core, logs := observer.New(zapcore.InfoLevel)
	n, err := Build(labEnv(map[string]string{"NODE_CONTROL_PLANE_URL": cp.URL}), os.ReadFile, "test", zap.New(core))
	require.NoError(t, err)

	errorLines := func() int { return logs.FilterLevelExact(zapcore.ErrorLevel).Len() }
	n.beat(context.Background())
	assert.Equal(t, 0, errorLines(), "one 404 is not yet a pattern")
	n.beat(context.Background())
	require.Equal(t, 1, errorLines(), "the second consecutive 404 says what is wrong")
	assert.Contains(t, logs.FilterLevelExact(zapcore.ErrorLevel).All()[0].Message, "is NODE_CONTROL_PLANE_URL a proxied host")
	n.beat(context.Background())
	n.beat(context.Background())
	assert.Equal(t, 1, errorLines(), "ONE line until a heartbeat is taken")

	status.Store(http.StatusOK)
	n.beat(context.Background())
	status.Store(http.StatusNotFound)
	n.beat(context.Background())
	n.beat(context.Background())
	assert.Equal(t, 2, errorLines(), "taken, then two 404s again: said again")
}

func TestNodeRole_RefusesAPublicBindWithoutTLS(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "203.0.113.5", "::"} {
		_, _, err := ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": bind}), os.ReadFile)
		require.Error(t, err, "bind %q", bind)
		assert.Contains(t, err.Error(), "NODE_TLS_CERT")
	}
	for _, bind := range []string{"127.0.0.1", "::1", "localhost"} {
		_, _, err := ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": bind}), os.ReadFile)
		assert.NoError(t, err, "a loopback bind is the lab's: plain HTTP")
	}
	// Either TLS source lifts the refusal; the pair is both-or-neither.
	_, _, err := ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": "", "NODE_ACME_EMAIL": "ops@example.net", "NODE_PUBLIC_HOST": "node1.example.net"}), os.ReadFile)
	assert.NoError(t, err)
	_, _, err = ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": "", "NODE_ACME_EMAIL": "ops@example.net"}), os.ReadFile)
	assert.ErrorContains(t, err, "NODE_PUBLIC_HOST", "ACME needs the name it asks a certificate for")
	_, _, err = ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": "", "NODE_TLS_CERT": "/x.pem"}), os.ReadFile)
	assert.ErrorContains(t, err, "NODE_TLS_KEY")
	_, _, err = ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": "", "NODE_TLS_CERT": "/x.pem", "NODE_TLS_KEY": "/y.pem"}), os.ReadFile)
	assert.NoError(t, err, "the files are read at Build")

	cfg, _, err := ConfigFromEnv(labEnv(map[string]string{"NODE_BIND": "", "NODE_ACME_EMAIL": "ops@example.net", "NODE_PUBLIC_HOST": "node1.example.net", "STATE_DIRECTORY": "/var/lib/vaultaire-node"}), os.ReadFile)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/vaultaire-node", cfg.StateDir, "systemd's StateDirectory")
}

// selfSigned writes a certificate for name and its key under dir.
func selfSigned(t *testing.T, dir, name string) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	kb, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600))
	return cp, kp, cert
}

func TestNode_ServesTheOperatorsCertificate(t *testing.T) {
	certFile, keyFile, cert := selfSigned(t, t.TempDir(), "node1.example.net")
	n, err := Build(labEnv(map[string]string{"NODE_BIND": "", "NODE_TLS_CERT": certFile, "NODE_TLS_KEY": keyFile}), os.ReadFile, "test", zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, n.tlsConfig)

	srv := httptest.NewUnstartedServer(n.Router())
	srv.TLS = n.tlsConfig
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err := c.Get(srv.URL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, resp.TLS)
	assert.True(t, resp.TLS.PeerCertificates[0].Equal(cert), "the operator's certificate")

	_, err = Build(labEnv(map[string]string{"NODE_BIND": "", "NODE_TLS_CERT": certFile, "NODE_TLS_KEY": certFile}), os.ReadFile, "test", zap.NewNop())
	assert.Error(t, err, "a pair that does not load refuses the boot")
}

func TestNode_ReservedPathIsTheFirstSegment(t *testing.T) {
	for _, p := range []string{"/docs", "/docs/", "/docs/api", "/dashboard", "/dashboard/billing", "/admin", "/api/v1/x", "/legal/privacy", "/translog/checkpoint", "/robots.txt", "/DOCS/x"} {
		assert.True(t, reservedPath(p), p)
	}
	for _, p := range []string{"/docsarchive/k", "/administrative/k", "/dashboards/k", "/apis/k", "/my-bucket/docs/k", "/"} {
		assert.False(t, reservedPath(p), p)
	}
}

func TestNode_MetricsAreNotOnThePublicPort(t *testing.T) {
	n, err := Build(labEnv(nil), os.ReadFile, "test", zap.NewNop())
	require.NoError(t, err)
	w := httptest.NewRecorder()
	n.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "127.0.0.1:9100", n.cfg.MetricsAddr, "loopback by default")
	w = httptest.NewRecorder()
	n.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "go_goroutines")

	cfg, _, err := ConfigFromEnv(labEnv(map[string]string{"NODE_METRICS_ADDR": "off"}), os.ReadFile)
	require.NoError(t, err)
	assert.Empty(t, cfg.MetricsAddr)
}

// RP74-6: a node's codec writes hold their plaintext inside its byte
// budget — NODE_CODEC_BUDGET_BYTES, default GOMEMLIMIT/4, else 1 GiB.
func TestNode_CodecBudget(t *testing.T) {
	n, err := Build(labEnv(nil), os.ReadFile, "test", zap.NewNop())
	require.NoError(t, err)
	b, ok := n.host.Budget.(*dataplane.ByteBudget)
	require.True(t, ok)
	assert.Equal(t, int64(1<<30), b.Limit(), "no GOMEMLIMIT in the test: 1 GiB")
	assert.Equal(t, int64(6<<30)/4, codecBudgetDefault(6<<30), "the unit's GOMEMLIMIT=6GiB: 1.5 GiB")

	cfg, _, err := ConfigFromEnv(labEnv(map[string]string{"NODE_CODEC_BUDGET_BYTES": "512MiB"}), os.ReadFile)
	require.NoError(t, err)
	assert.Equal(t, int64(512<<20), cfg.CodecBudgetBytes)
	for _, bad := range []string{"lots", "1MiB", "-5"} {
		_, _, err = ConfigFromEnv(labEnv(map[string]string{"NODE_CODEC_BUDGET_BYTES": bad}), os.ReadFile)
		assert.Error(t, err, bad)
	}
}
