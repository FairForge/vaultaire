package api

import (
	"strings"

	"github.com/FairForge/vaultaire/internal/payments/btcpay"
	"go.uber.org/zap"
)

// setupBTCPay registers the BTCPay provider (WP-PAY-4, docs/PAYMENTS.md
// § 3) when all four of BTCPAY_URL, BTCPAY_STORE_ID, BTCPAY_API_KEY and
// BTCPAY_WEBHOOK_SECRET are set (the two secrets by credential → _FILE →
// env, as IDRIVE_RESELLER_API_KEY). None set = BTCPay off, one Info line;
// some set = one Error line naming the missing ones (never a value), not
// registered; a secret source that is set but unusable = a refused boot,
// never "BTCPay off". The webhook route is mounted by registerBTCPayRoute
// whatever the pay_btcpay flag (late notifications must land); the flag
// gates new checkouts only (payments.Registry.For).
func (s *Server) setupBTCPay(getenv func(string) string, readFile func(string) ([]byte, error)) {
	cfg, missing, err := btcpay.LoadConfig(getenv, readFile)
	if err != nil {
		s.logger.Fatal("BTCPay configuration: refusing to start", zap.Error(err))
	}
	switch {
	case len(missing) == 4:
		s.logger.Info("btcpay provider off (BTCPAY_URL, BTCPAY_STORE_ID, BTCPAY_API_KEY, BTCPAY_WEBHOOK_SECRET not set)")
		return
	case len(missing) > 0:
		s.logger.Error("btcpay provider NOT registered: some BTCPAY_* settings are set but not all — /webhook/btcpay is not mounted",
			zap.String("missing", strings.Join(missing, ",")))
		return
	}
	s.btcpay = btcpay.New(cfg, s.db, s.logger)
	if s.payments != nil {
		s.payments.Register(s.btcpay)
	}
	s.logger.Info("btcpay provider registered (checkout gated by pay_btcpay)", zap.String("store", cfg.StoreID))
}

// registerBTCPayRoute mounts POST /webhook/btcpay while the provider is
// registered — beside /webhook/stripe; no auth middleware, the signature
// is the auth.
func (s *Server) registerBTCPayRoute() {
	if s.btcpay == nil {
		return
	}
	s.logger.Info("Registering BTCPay webhook route")
	s.router.Post("/webhook/btcpay", s.btcpay.ServeHTTP)
}
