package hosts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// K4b PR 4 (RP35-1): the transparency log's read side lives on the trust
// host (contract rule 11: trust.stored.cloud). Trust is derived like CDN:
// TRUST_PUBLIC_HOST, else https://trust.<canonical host> on a public name,
// else the canonical origin (development).
func TestHosts_Trust(t *testing.T) {
	t.Run("prod values: trust.<canonical>, and trust.<brand or legacy domain> whatever the env", func(t *testing.T) {
		h := FromEnv(env(prodEnv), 8000, nil)
		assert.Equal(t, "https://trust.stored.cloud", h.Trust)
		assert.Equal(t, "trust.stored.cloud", h.TrustHost())
		assert.True(t, h.TrustPublic())
		for _, host := range []string{"trust.stored.cloud", "trust.stored.ge", "TRUST.STORED.CLOUD:443", "trust.www.stored.ge"} {
			assert.True(t, h.IsTrustHost(host), host)
		}
		for _, host := range []string{"stored.cloud", "stored.ge", "s3.stored.cloud", "cdn.stored.cloud", "trust.example.com", ""} {
			assert.False(t, h.IsTrustHost(host), host)
		}
	})

	t.Run("today's prod .env (canonical stored.ge): trust.stored.ge and trust.stored.cloud", func(t *testing.T) {
		h := FromEnv(env(map[string]string{"VAULTAIRE_ENDPOINT": "https://stored.ge", "VAULTAIRE_BASE_URL": "https://stored.ge"}), 8000, nil)
		assert.Equal(t, "https://trust.stored.ge", h.Trust)
		assert.True(t, h.IsTrustHost("trust.stored.ge"))
		assert.True(t, h.IsTrustHost("trust.stored.cloud"))
		assert.False(t, h.IsTrustHost("stored.ge"))
	})

	t.Run("TRUST_PUBLIC_HOST overrides; a malformed one keeps the default with a Warn", func(t *testing.T) {
		e := map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "TRUST_PUBLIC_HOST": "https://log.example.org"}
		h := FromEnv(env(e), 8000, nil)
		assert.Equal(t, "https://log.example.org", h.Trust)
		assert.True(t, h.IsTrustHost("log.example.org"))
		assert.True(t, h.IsTrustHost("trust.stored.cloud"), "the brand's trust name always routes")
		assert.True(t, h.IsTrustHost("trust.stored.ge"), "a legacy domain's trust name always routes, with no alias set")

		core, logs := observer.New(zapcore.WarnLevel)
		e["TRUST_PUBLIC_HOST"] = "log.example.org/x"
		h = FromEnv(env(e), 8000, zap.New(core))
		assert.Equal(t, "https://trust.stored.cloud", h.Trust)
		assert.Equal(t, 1, logs.FilterField(zap.String("var", "TRUST_PUBLIC_HOST")).Len())
	})

	t.Run("development: the canonical origin, not a public trust host", func(t *testing.T) {
		h := FromEnv(env(nil), 8000, nil)
		assert.Equal(t, "http://localhost:8000", h.Trust)
		assert.False(t, h.TrustPublic(), "a lab serves the log on its one origin")
		assert.False(t, h.IsTrustHost("localhost"))
		assert.Equal(t, "http://localhost:8000", Defaults(8000).Trust)
	})
}
