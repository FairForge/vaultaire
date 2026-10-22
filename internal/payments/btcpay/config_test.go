package btcpay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP30-6: the store key travels in every Greenfield request's header, so
// BTCPAY_URL is https — plain http only to a loopback address (the lab).
func TestLoadConfig_PlainHTTPOnlyToLoopback(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://pay.stored.cloud":   true,
		"http://127.0.0.1:23001":     true,
		"http://localhost:23001/":    true,
		"http://[::1]:23001":         true,
		"http://pay.stored.cloud":    false,
		"http://10.0.0.5:23001":      false,
		"http://127.0.0.1.nip.io:80": false,
	} {
		env := map[string]string{"BTCPAY_URL": url, "BTCPAY_STORE_ID": "S", "BTCPAY_API_KEY": "k", "BTCPAY_WEBHOOK_SECRET": "s"}
		_, missing, err := LoadConfig(func(k string) string { return env[k] }, nil)
		require.Empty(t, missing, url)
		if ok {
			assert.NoError(t, err, url)
			continue
		}
		require.ErrorIs(t, err, ErrInsecureURL, url)
		assert.Contains(t, err.Error(), "BTCPAY_URL")
	}
}
