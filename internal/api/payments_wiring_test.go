package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// WP-PAY-2: the server's money wiring — no Stripe customer at
// registration, the webhook route independent of the provider flag, the
// readiness series from boot, the lapse job reading the registry.

// countingStripeAPI answers every Stripe call and counts customer creations.
func countingStripeAPI(t *testing.T) *atomic.Int32 {
	t.Helper()
	var customers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/customers" {
			customers.Add(1)
			_, _ = w.Write([]byte(`{"id":"cus_reg","object":"customer"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"fake"}}`))
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0), LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })
	return &customers
}

func stripeServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fake")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_test")
	return NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
}

// 43.2: no third party knows a tenant until it buys — neither registration
// path creates a Stripe customer (both did before WP-PAY-2).
func TestRegister_CreatesNoStripeCustomer(t *testing.T) {
	// Arrange
	customers := countingStripeAPI(t)
	srv := stripeServer(t)
	h := srv.httpServer.Handler
	suffix := fmt.Sprint(time.Now().UnixNano())

	// Act: the API and the dashboard form.
	body, _ := json.Marshal(map[string]string{"email": "api-" + suffix + "@stored.test", "password": "securepass123"})
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	form := url.Values{"email": {"form-" + suffix + "@stored.test"}, "password": {"securepass123"}}
	req = httptest.NewRequest("POST", "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Less(t, w.Code, 400, w.Body.String())

	// Assert
	assert.Zero(t, customers.Load(), "no Stripe customer at registration")
}

// A flag never unmounts a webhook route: with the secrets set and
// pay_stripe off, a signed delivery is still handled (late notifications
// must land — docs/PAYMENTS.md § 2.3).
func TestWebhookRoute_MountedWhileFlagOff(t *testing.T) {
	srv := stripeServer(t)
	srv.flags.Register(flagPayStripe, false)
	require.False(t, srv.flags.Enabled(flagPayStripe, ""))

	payload := []byte(`{"id":"evt_noop_p19","object":"event","api_version":"` + stripe.APIVersion + `","type":"ping","data":{"object":{}}}`)
	ts := time.Now().Unix()
	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewReader(payload))
	req.Header.Set("Stripe-Signature", stripeSignature(payload, "whsec_test", ts))
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestPaymentProviders_RegisteredWithFlagsAndGauge(t *testing.T) {
	// Without a key: Stripe is registered, never ready, flag default off.
	t.Setenv("STRIPE_SECRET_KEY", "")
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	require.NotNil(t, srv.payments.Provider(payments.ProviderStripe))
	ok, why := srv.payments.Provider(payments.ProviderStripe).Ready(context.Background())
	assert.False(t, ok)
	assert.Contains(t, why, "STRIPE_SECRET_KEY")
	assert.False(t, srv.flags.Enabled(flagPayStripe, ""), "pay_stripe defaults to a key being set")
	assert.False(t, srv.flags.Enabled(flagPayBTCPay, ""))
	assert.True(t, srv.flags.Registered(flagPayBTCPay), "pay_btcpay is registered so /admin/flags shows it")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	assert.Contains(t, w.Body.String(), `vaultaire_payment_provider_ready{provider="stripe"} 0`, "the series exists from boot")

	// With a key: pay_stripe defaults on.
	srv = stripeServer(t)
	assert.True(t, srv.flags.Enabled(flagPayStripe, ""))
}

// stripeSignature is Stripe's v1 scheme: HMAC-SHA256 over "<t>.<body>".
func stripeSignature(body []byte, secret string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", ts, body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}
