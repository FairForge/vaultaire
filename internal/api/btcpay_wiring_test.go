package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/payments/btcpay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// WP-PAY-4 (Stage 1a Prompt 24): the BTCPay provider is registered — and
// /webhook/btcpay mounted — only with all four BTCPAY_* settings; some of
// them set is one Error line naming the missing ones, never their values.

func btcpayServer(t *testing.T, env map[string]string) (*Server, *observer.ObservedLogs) {
	t.Helper()
	for _, k := range []string{"BTCPAY_URL", "BTCPAY_STORE_ID", "BTCPAY_API_KEY", "BTCPAY_WEBHOOK_SECRET",
		"BTCPAY_API_KEY_FILE", "BTCPAY_WEBHOOK_SECRET_FILE", "CREDENTIALS_DIRECTORY"} {
		t.Setenv(k, env[k])
	}
	core, logs := observer.New(zapcore.InfoLevel)
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.New(core),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	return srv, logs
}

func postBTCPay(srv *Server) int {
	req := httptest.NewRequest(http.MethodPost, "/webhook/btcpay", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)
	return w.Code
}

func TestServer_BTCPayRouteAbsentWithoutSecrets(t *testing.T) {
	// None set: off, no route, not registered, the readiness series at 0.
	srv, logs := btcpayServer(t, nil)
	assert.Nil(t, srv.payments.Provider(payments.ProviderBTCPay))
	absent := postBTCPay(srv)
	assert.NotContains(t, []int{http.StatusBadRequest, http.StatusServiceUnavailable}, absent, "no BTCPay handler answers")
	assert.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("btcpay").Len())

	// Three of four: one Error line naming the missing one, no value, no route.
	srv, logs = btcpayServer(t, map[string]string{"BTCPAY_URL": "https://pay.test", "BTCPAY_STORE_ID": "S1",
		"BTCPAY_API_KEY": "key-value-never-logged"})
	assert.Nil(t, srv.payments.Provider(payments.ProviderBTCPay))
	assert.Equal(t, absent, postBTCPay(srv))
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("btcpay").All()
	require.Len(t, errs, 1)
	assert.Equal(t, "BTCPAY_WEBHOOK_SECRET", errs[0].ContextMap()["missing"])
	for _, e := range logs.All() {
		for _, v := range e.ContextMap() {
			s, _ := v.(string)
			assert.NotContains(t, s, "key-value-never-logged")
		}
		assert.NotContains(t, e.Message, "key-value-never-logged")
	}

	// All four (the secrets from files): registered, the route mounted
	// (the BTCPay handler answers — 503 here, the test server has no
	// database), Pending until its first check.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(dir+"/k", []byte("k\n"), 0o600))
	require.NoError(t, os.WriteFile(dir+"/s", []byte("s\n"), 0o600))
	srv, _ = btcpayServer(t, map[string]string{"BTCPAY_URL": "https://pay.test", "BTCPAY_STORE_ID": "S1",
		"BTCPAY_API_KEY_FILE": dir + "/k", "BTCPAY_WEBHOOK_SECRET_FILE": dir + "/s"})
	p := srv.payments.Provider(payments.ProviderBTCPay)
	require.NotNil(t, p)
	st, _ := p.Ready(context.Background())
	assert.Equal(t, payments.Pending, st)
	assert.Equal(t, http.StatusServiceUnavailable, postBTCPay(srv), "mounted: the BTCPay handler answers")
	assert.False(t, srv.flags.Enabled(flagPayBTCPay, ""), "pay_btcpay stays OFF")
}

// RP30-6: plain http to a host that is not loopback is one Error naming
// BTCPAY_URL, the provider not registered, no route — never a refused boot.
func TestServer_BTCPayPlainHTTPRemote_NotRegistered(t *testing.T) {
	srv, logs := btcpayServer(t, map[string]string{"BTCPAY_URL": "http://pay.stored.cloud", "BTCPAY_STORE_ID": "S1",
		"BTCPAY_API_KEY": "key-value-never-logged", "BTCPAY_WEBHOOK_SECRET": "secret-never-logged"})

	assert.Nil(t, srv.payments.Provider(payments.ProviderBTCPay))
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("btcpay").All()
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "BTCPAY_URL")
	for _, e := range logs.All() {
		assert.NotContains(t, e.Message, "never-logged")
		for _, v := range e.ContextMap() {
			s, _ := v.(string)
			assert.NotContains(t, s, "never-logged")
		}
	}
}

// A btcpay row is nothing to cancel: the runner neither defers the tenant
// (registered or not — a prepaid row) nor calls Stripe.
func TestDeletionRunner_BTCPayRow_NothingToCancel(t *testing.T) {
	for _, registered := range []bool{true, false} {
		f := setupDeletionFixture(t)
		f.seed(time.Now().Add(-time.Hour))
		f.exec(`UPDATE entitlements SET provider = 'btcpay', provider_ref = $2, renewal = 'prepaid' WHERE tenant_id = $1`,
			f.tenantID, "BtcInv_"+f.suffix)
		if registered {
			f.runner.Payments = fakeProviders{payments.ProviderStripe: f.stripe,
				payments.ProviderBTCPay: btcpay.New(btcpay.Config{URL: "http://127.0.0.1:1", StoreID: "S", APIKey: "k", WebhookSecret: "s"}, f.db, zap.NewNop())}
		}

		var te TenantErasure
		err := f.runner.cancelSubscription(context.Background(), f.tenantID, &te)

		require.NoError(t, err, "registered=%v", registered)
		assert.Zero(t, f.stripe.calls.Load(), "Stripe is not asked")
		assert.False(t, te.StripeCancelled)
	}
}
