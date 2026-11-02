package hosts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// WP-DOMAIN-1 part 3 (contract rule 11, docs/surfaces/DEVELOPER.md "The
// first ten" 1–3): the last hosts of the F-17 map. Each is derived like
// Trust: its *_PUBLIC_HOST, else https://<name>.<canonical host> on a public
// name, else the canonical origin (development); a malformed value is a Warn
// and the default kept; none is a web host; the S3, CDN and trust hosts do
// not move.
func TestHosts_DocsStatusApp_Defaults(t *testing.T) {
	t.Run("prod values: docs.<canonical>, docs.<brand or legacy domain> whatever the env", func(t *testing.T) {
		h := FromEnv(env(prodEnv), 8000, nil)
		assert.Equal(t, "https://docs.stored.cloud", h.Docs)
		assert.Equal(t, "docs.stored.cloud", h.DocsHost())
		assert.True(t, h.DocsPublic())
		for _, host := range []string{"docs.stored.cloud", "docs.stored.ge", "DOCS.STORED.CLOUD:443", "docs.www.stored.ge", "docs.stored.pics"} {
			assert.True(t, h.IsDocsHost(host), host)
			assert.False(t, h.IsWebHost(host), "a docs host is never a web host: %s", host)
			assert.False(t, h.IsS3Host(host), host)
			assert.False(t, h.IsCDNHost(host), host)
			assert.False(t, h.IsTrustHost(host), host)
			assert.True(t, h.IsOwn(host), host)
		}
		for _, host := range []string{"stored.cloud", "stored.ge", "s3.stored.cloud", "cdn.stored.cloud", "trust.stored.cloud", "docs.example.com", ""} {
			assert.False(t, h.IsDocsHost(host), host)
		}
		// Since the owner's proof (2026-11-02) every printed docs link names
		// the docs host; the canonical /docs answers for ever.
		assert.Equal(t, "https://docs.stored.cloud", h.DocsBase())
		assert.True(t, h.DocsFlipped())
		assert.Equal(t, "https://docs.stored.cloud/rclone", h.Expand("__HOST_DOCS_BASE__/rclone"))
	})

	t.Run("the hosts that existed before are unchanged", func(t *testing.T) {
		h := FromEnv(env(prodEnv), 8000, nil)
		for _, c := range []struct {
			host                string
			s3, cdn, trust, web bool
		}{
			{"stored.cloud", false, false, false, true},
			{"stored.ge", false, false, false, true},
			{"www.stored.cloud", false, false, false, true},
			{"s3.stored.cloud", true, false, false, false},
			{"s3.stored.ge", true, false, false, false},
			{"cdn.stored.cloud", false, true, false, false},
			{"trust.stored.cloud", false, false, true, false},
		} {
			assert.Equal(t, c.s3, h.IsS3Host(c.host), "s3 %s", c.host)
			assert.Equal(t, c.cdn, h.IsCDNHost(c.host), "cdn %s", c.host)
			assert.Equal(t, c.trust, h.IsTrustHost(c.host), "trust %s", c.host)
			assert.Equal(t, c.web, h.IsWebHost(c.host), "web %s", c.host)
			assert.False(t, h.IsDocsHost(c.host), "docs %s", c.host)
		}
	})

	t.Run("DOCS_PUBLIC_HOST overrides; a malformed one keeps the default with a Warn", func(t *testing.T) {
		e := map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "DOCS_PUBLIC_HOST": "https://developers.example.org"}
		h := FromEnv(env(e), 8000, nil)
		assert.Equal(t, "https://developers.example.org", h.Docs)
		assert.True(t, h.IsDocsHost("developers.example.org"))
		assert.True(t, h.IsDocsHost("docs.stored.cloud"), "the brand's docs name always routes")
		assert.True(t, h.IsDocsHost("docs.stored.ge"), "a legacy domain's docs name always routes, with no alias set")
		assert.False(t, h.IsWebHost("developers.example.org"))

		core, logs := observer.New(zapcore.WarnLevel)
		e["DOCS_PUBLIC_HOST"] = "developers.example.org/x"
		h = FromEnv(env(e), 8000, zap.New(core))
		assert.Equal(t, "https://docs.stored.cloud", h.Docs)
		assert.Equal(t, 1, logs.FilterField(zap.String("var", "DOCS_PUBLIC_HOST")).Len())
	})

	t.Run("a DOCS_PUBLIC_HOST naming the canonical host routes nothing wholesale", func(t *testing.T) {
		h := FromEnv(env(map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "DOCS_PUBLIC_HOST": "https://stored.cloud"}), 8000, nil)
		assert.False(t, h.IsDocsHost("stored.cloud"))
		assert.True(t, h.IsWebHost("stored.cloud"))
		assert.Equal(t, "https://stored.cloud/docs", h.DocsBase(), "and its links stay on the canonical /docs")
		assert.False(t, h.DocsFlipped())
	})

	t.Run("development: the canonical origin, no docs router", func(t *testing.T) {
		h := FromEnv(env(nil), 8000, nil)
		assert.Equal(t, "http://localhost:8000", h.Docs)
		assert.False(t, h.DocsPublic(), "a lab serves the guides under /docs on its one origin")
		assert.False(t, h.IsDocsHost("localhost"))
		assert.Equal(t, "http://localhost:8000/docs", h.DocsBase())
		assert.False(t, h.DocsFlipped())
		assert.Equal(t, "http://localhost:8000", Defaults(8000).Docs)
	})

	t.Run("status: status.<canonical>, its own origin; status.<brand or legacy domain> whatever the env", func(t *testing.T) {
		h := FromEnv(env(prodEnv), 8000, nil)
		assert.Equal(t, "https://status.stored.cloud", h.Status)
		assert.Equal(t, "status.stored.cloud", h.StatusHost())
		assert.True(t, h.StatusPublic())
		for _, host := range []string{"status.stored.cloud", "status.stored.ge", "STATUS.STORED.CLOUD:443", "status.www.stored.ge", "status.stored.pics"} {
			assert.True(t, h.IsStatusHost(host), host)
			assert.False(t, h.IsWebHost(host), "a status host is never a web host: %s", host)
			assert.False(t, h.IsS3Host(host), host)
			assert.False(t, h.IsCDNHost(host), host)
			assert.False(t, h.IsTrustHost(host), host)
			assert.False(t, h.IsDocsHost(host), host)
			assert.True(t, h.IsOwn(host), host)
		}
		for _, host := range []string{"stored.cloud", "stored.ge", "s3.stored.cloud", "docs.stored.cloud", "trust.stored.cloud", "status.example.com", ""} {
			assert.False(t, h.IsStatusHost(host), host)
		}
		for _, host := range []string{"stored.cloud", "stored.ge", "docs.stored.cloud"} {
			assert.Equal(t, host != "docs.stored.cloud", h.IsWebHost(host), host)
		}

		e := map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "STATUS_PUBLIC_HOST": "https://status.example.org"}
		h = FromEnv(env(e), 8000, nil)
		assert.Equal(t, "https://status.example.org", h.Status)
		assert.True(t, h.IsStatusHost("status.example.org"))
		assert.True(t, h.IsStatusHost("status.stored.ge"), "a legacy domain's status name always routes")

		core, logs := observer.New(zapcore.WarnLevel)
		e["STATUS_PUBLIC_HOST"] = "status.example.org/x"
		h = FromEnv(env(e), 8000, zap.New(core))
		assert.Equal(t, "https://status.stored.cloud", h.Status)
		assert.Equal(t, 1, logs.FilterField(zap.String("var", "STATUS_PUBLIC_HOST")).Len())

		h = FromEnv(env(map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "STATUS_PUBLIC_HOST": "https://stored.cloud"}), 8000, nil)
		assert.False(t, h.IsStatusHost("stored.cloud"), "a STATUS_PUBLIC_HOST naming the canonical host routes nothing wholesale")
	})

	t.Run("status, development: the canonical origin's /status, no status router", func(t *testing.T) {
		h := FromEnv(env(nil), 8000, nil)
		assert.Equal(t, "http://localhost:8000", h.Status)
		assert.False(t, h.StatusPublic())
		assert.False(t, h.IsStatusHost("localhost"))
		assert.Equal(t, "http://localhost:8000", Defaults(8000).Status)
	})

	t.Run("app: app.<canonical>, its own origin; app.<brand or legacy domain> whatever the env", func(t *testing.T) {
		h := FromEnv(env(prodEnv), 8000, nil)
		assert.Equal(t, "https://app.stored.cloud", h.App)
		assert.Equal(t, "app.stored.cloud", h.AppHost())
		assert.True(t, h.AppPublic())
		for _, host := range []string{"app.stored.cloud", "app.stored.ge", "APP.STORED.CLOUD:443", "app.www.stored.ge", "app.stored.pics"} {
			assert.True(t, h.IsAppHost(host), host)
			assert.False(t, h.IsWebHost(host), "an app host is never a web host: %s", host)
			assert.False(t, h.IsS3Host(host), host)
			assert.False(t, h.IsCDNHost(host), host)
			assert.False(t, h.IsTrustHost(host), host)
			assert.False(t, h.IsDocsHost(host), host)
			assert.False(t, h.IsStatusHost(host), host)
			assert.True(t, h.IsOwn(host), host)
		}
		for _, host := range []string{"stored.cloud", "stored.ge", "s3.stored.cloud", "docs.stored.cloud", "status.stored.cloud", "dashboard.stored.cloud", "app.example.com", ""} {
			assert.False(t, h.IsAppHost(host), host)
		}
		// Dark (part 3C-1): every printed dashboard link stays on the
		// canonical origin until the owner's proof.
		assert.Equal(t, "https://stored.cloud", h.AppBase())

		e := map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "APP_PUBLIC_HOST": "https://console.example.org"}
		h = FromEnv(env(e), 8000, nil)
		assert.Equal(t, "https://console.example.org", h.App)
		assert.True(t, h.IsAppHost("console.example.org"))
		assert.True(t, h.IsAppHost("app.stored.ge"), "a legacy domain's app name always routes")

		core, logs := observer.New(zapcore.WarnLevel)
		e["APP_PUBLIC_HOST"] = "console.example.org/x"
		h = FromEnv(env(e), 8000, zap.New(core))
		assert.Equal(t, "https://app.stored.cloud", h.App)
		assert.Equal(t, 1, logs.FilterField(zap.String("var", "APP_PUBLIC_HOST")).Len())

		h = FromEnv(env(map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "APP_PUBLIC_HOST": "https://stored.cloud"}), 8000, nil)
		assert.False(t, h.IsAppHost("stored.cloud"), "an APP_PUBLIC_HOST naming the canonical host routes nothing wholesale")
		assert.True(t, h.IsWebHost("stored.cloud"))
	})

	t.Run("app, development: the canonical origin, no app router", func(t *testing.T) {
		h := FromEnv(env(nil), 8000, nil)
		assert.Equal(t, "http://localhost:8000", h.App)
		assert.False(t, h.AppPublic())
		assert.False(t, h.IsAppHost("localhost"))
		assert.Equal(t, "http://localhost:8000", h.AppBase())
		assert.Equal(t, "http://localhost:8000", Defaults(8000).App)
	})
}
