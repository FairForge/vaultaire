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
		// Dark (part 3A): every printed docs link stays on the canonical
		// origin's /docs until the owner's proof.
		assert.Equal(t, "https://stored.cloud/docs", h.DocsBase())
		assert.Equal(t, "https://stored.cloud/docs/rclone", h.Expand("__HOST_DOCS_BASE__/rclone"))
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
	})

	t.Run("development: the canonical origin, no docs router", func(t *testing.T) {
		h := FromEnv(env(nil), 8000, nil)
		assert.Equal(t, "http://localhost:8000", h.Docs)
		assert.False(t, h.DocsPublic(), "a lab serves the guides under /docs on its one origin")
		assert.False(t, h.IsDocsHost("localhost"))
		assert.Equal(t, "http://localhost:8000/docs", h.DocsBase())
		assert.Equal(t, "http://localhost:8000", Defaults(8000).Docs)
	})
}
