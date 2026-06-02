package drivers

import (
	"context"
	"crypto/tls"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDial(_ context.Context, _, _ string) (net.Conn, error) { return nil, nil }

// The bulk-upload transport must force HTTP/1.1 (a non-nil, empty TLSNextProto
// disables the HTTP/2 upgrade), matching the CDN download transport that gave the
// fleet a +60% gain by avoiding Go's HTTP/2 flow-control window bug.
func TestOdUploadTransport_ForcesHTTP1(t *testing.T) {
	cache := tls.NewLRUClientSessionCache(8)

	up := odUploadTransport(testDial, cache)
	require.NotNil(t, up.TLSNextProto, "upload transport must set TLSNextProto to force HTTP/1.1")
	assert.Empty(t, up.TLSNextProto, "TLSNextProto must be empty (no HTTP/2 upgrade)")
	assert.False(t, up.ForceAttemptHTTP2, "upload transport must not negotiate HTTP/2")

	// Same posture as the proven CDN download transport.
	cdn := odCDNTransport(testDial, cache)
	require.NotNil(t, cdn.TLSNextProto)
	assert.Empty(t, cdn.TLSNextProto)
}
