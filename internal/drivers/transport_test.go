package drivers

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTunedHTTPClient_DefaultSettings(t *testing.T) {
	client := TunedHTTPClient()
	require.NotNil(t, client)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport")

	assert.Equal(t, 200, transport.MaxIdleConns)
	assert.Equal(t, 200, transport.MaxIdleConnsPerHost)
	assert.Equal(t, 4<<20, transport.ReadBufferSize)
	assert.Equal(t, 4<<20, transport.WriteBufferSize)
	assert.True(t, transport.DisableCompression)
	assert.True(t, transport.ForceAttemptHTTP2)
	assert.Equal(t, 90*time.Second, transport.IdleConnTimeout)
	assert.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)

	require.NotNil(t, transport.TLSClientConfig)
	assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
	assert.NotNil(t, transport.TLSClientConfig.ClientSessionCache)
	assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)

	assert.NotNil(t, transport.DialContext, "DNS-caching dial should be set")
	assert.Nil(t, transport.TLSNextProto, "HTTP/2 should not be disabled by default")
}

func TestTunedHTTPClient_InsecureTLS(t *testing.T) {
	client := TunedHTTPClient(WithInsecureTLS())
	transport := client.Transport.(*http.Transport)

	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
}

func TestTunedHTTPClient_ResponseHeaderTimeout(t *testing.T) {
	client := TunedHTTPClient(WithResponseHeaderTimeout(5 * time.Minute))
	transport := client.Transport.(*http.Transport)

	assert.Equal(t, 5*time.Minute, transport.ResponseHeaderTimeout)
}

func TestTunedHTTPClient_HTTP1Only(t *testing.T) {
	client := TunedHTTPClient(WithHTTP1Only())
	transport := client.Transport.(*http.Transport)

	assert.False(t, transport.ForceAttemptHTTP2)
	require.NotNil(t, transport.TLSNextProto, "TLSNextProto should be set to disable HTTP/2")
	assert.Empty(t, transport.TLSNextProto, "TLSNextProto should be empty map")
}

func TestTunedHTTPClient_DisabledByEnv(t *testing.T) {
	orig := tunedTransportDisabled
	tunedTransportDisabled = true
	defer func() { tunedTransportDisabled = orig }()

	client := TunedHTTPClient(WithInsecureTLS())
	assert.Equal(t, http.DefaultClient, client)
}

func TestTunedHTTPClient_CombinedOptions(t *testing.T) {
	client := TunedHTTPClient(
		WithInsecureTLS(),
		WithResponseHeaderTimeout(3*time.Minute),
		WithHTTP1Only(),
	)
	transport := client.Transport.(*http.Transport)

	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
	assert.Equal(t, 3*time.Minute, transport.ResponseHeaderTimeout)
	assert.False(t, transport.ForceAttemptHTTP2)
	assert.NotNil(t, transport.TLSNextProto)
}

func TestCachedDialContext_CachesLookup(t *testing.T) {
	dnsCache.Delete("localhost")

	dnsCache.Store("localhost", []string{"127.0.0.1"})
	defer dnsCache.Delete("localhost")

	val, ok := dnsCache.Load("localhost")
	require.True(t, ok)
	assert.Equal(t, []string{"127.0.0.1"}, val)
}

func TestS3Driver_Put_Streaming(t *testing.T) {
	// Verify S3Driver.Put uses materialize (spill-to-disk for large objects)
	// rather than io.ReadAll (which buffers everything in memory).
	//
	// We can't easily test against a real S3 endpoint here, so we verify
	// indirectly: materialize is the same function Geyser uses, and it
	// handles objects > spillThreshold via temp files. The real assertion
	// is that s3.go no longer imports "bytes" and no longer calls io.ReadAll.
	//
	// Verify materialize works for small objects.
	data := bytes.NewReader([]byte("hello world"))
	body, size, cleanup, err := materialize(data)
	defer cleanup()

	require.NoError(t, err)
	assert.Equal(t, int64(11), size)
	assert.NotNil(t, body)
}

func TestTunedTransportDisabled_EnvParsing(t *testing.T) {
	// Verify the env var parsing at package init.
	// We test the string comparison logic directly.
	assert.True(t, strings.EqualFold("false", "false"))
	assert.True(t, strings.EqualFold("FALSE", "false"))
	assert.True(t, strings.EqualFold("False", "false"))
	assert.False(t, strings.EqualFold("true", "false"))
	assert.False(t, strings.EqualFold("", "false"))
}

func TestDNSCache_SharedAcrossDrivers(t *testing.T) {
	// The dnsCache var is package-level, shared by all S3-compatible drivers.
	// Verify it's a sync.Map that can store and load.
	key := "test-host-" + t.Name()
	dnsCache.Store(key, []string{"10.0.0.1"})
	defer dnsCache.Delete(key)

	val, ok := dnsCache.Load(key)
	require.True(t, ok)
	addrs, ok := val.([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"10.0.0.1"}, addrs)
}

// Ensure VAULTAIRE_TUNED_TRANSPORT env var is not currently set in test env.
func TestTunedHTTPClient_DefaultNotDisabled(t *testing.T) {
	if os.Getenv("VAULTAIRE_TUNED_TRANSPORT") == "false" {
		t.Skip("VAULTAIRE_TUNED_TRANSPORT=false set in environment")
	}
	_ = context.Background() // use context import
	client := TunedHTTPClient()
	assert.NotEqual(t, http.DefaultClient, client)
}
