package api

import (
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-PAY-0 (= WP-R10-4, F-29): one product. A boot with Stripe configured
// starts no metered reporter and registers no pack plan — even with the old
// STRIPE_METER_* / STRIPE_PRICE_VAULT* variables still in an .env.
func TestServer_BootWithoutMeterEnv(t *testing.T) {
	// Arrange
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fake")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_fake")
	t.Setenv("STRIPE_METER_STORAGE", "storage_bytes")
	t.Setenv("STRIPE_METER_EGRESS", "egress_bytes")
	t.Setenv("STRIPE_PRICE_VAULT3", "price_vault3")
	core, logs := observer.New(zap.DebugLevel)

	// Act
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.New(core),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)

	// Assert
	require.NotNil(t, srv.stripe, "Stripe is configured")
	assert.Positive(t, logs.FilterMessage("stripe billing service initialized").Len())
	for _, e := range logs.All() {
		assert.NotContains(t, strings.ToLower(e.Message), "metered", "no metered reporter line: %q", e.Message)
	}
}
