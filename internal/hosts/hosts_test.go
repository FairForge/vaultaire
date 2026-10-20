package hosts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// prod is the .env of the owner click-list (WP-DOMAIN-1).
var prodEnv = map[string]string{
	"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud",
	"VAULTAIRE_ENDPOINT":    "https://s3.stored.cloud",
	"VAULTAIRE_BASE_URL":    "https://stored.cloud",
	"CDN_PUBLIC_HOST":       "https://cdn.stored.cloud",
	"VAULTAIRE_ALIAS_HOSTS": "stored.ge, www.stored.ge,www.stored.cloud",
}

func TestHosts_FromEnvDefaultsAndRejects(t *testing.T) {
	t.Run("nothing set = the local development origin everywhere", func(t *testing.T) {
		h := FromEnv(env(nil), 8000, nil)
		assert.Equal(t, "http://localhost:8000", h.Canonical)
		assert.Equal(t, "http://localhost:8000", h.S3)
		assert.Equal(t, "http://localhost:8000", h.BaseURL)
		assert.Equal(t, "http://localhost:8000", h.CDN)
		assert.Empty(t, h.Aliases)
		// The CDN shares the web origin in development: /cdn/<slug>/...
		assert.Equal(t, "http://localhost:8000/cdn/", h.CDNBase())
		assert.False(t, h.IsCDNHost("localhost"), "the dev origin is never routed wholesale to the CDN")
		// An address is never at localhost.
		assert.Equal(t, "support@stored.cloud", h.SupportEmail())
		assert.Equal(t, "security@stored.cloud", h.SecurityEmail())
	})

	t.Run("the owner's prod values", func(t *testing.T) {
		h := FromEnv(env(prodEnv), 8000, nil)
		assert.Equal(t, "https://stored.cloud", h.Canonical)
		assert.Equal(t, "https://s3.stored.cloud", h.S3)
		assert.Equal(t, "https://stored.cloud", h.BaseURL)
		assert.Equal(t, "https://cdn.stored.cloud", h.CDN)
		assert.Equal(t, []string{"stored.ge", "www.stored.ge", "www.stored.cloud"}, h.Aliases)
		assert.Equal(t, "cdn.stored.cloud", h.CDNHost())
		assert.Equal(t, "https://cdn.stored.cloud/", h.CDNBase())
		assert.Equal(t, "support@stored.cloud", h.SupportEmail())
		assert.Equal(t, "security@stored.cloud", h.SecurityEmail())
		assert.Equal(t, "stored.cloud", h.Domain())

		assert.True(t, h.IsAlias("stored.ge"))
		assert.True(t, h.IsAlias("STORED.GE:443"), "case and port do not matter")
		assert.True(t, h.IsAlias("www.stored.cloud"))
		assert.False(t, h.IsAlias("stored.cloud"), "the canonical host is never an alias")
		assert.False(t, h.IsAlias("s3.stored.ge"))

		for _, s3 := range []string{"s3.stored.cloud", "s3.stored.ge", "S3.STORED.GE:443"} {
			assert.True(t, h.IsS3Host(s3), s3)
		}
		assert.False(t, h.IsS3Host("stored.ge"))
		assert.False(t, h.IsS3Host("s3.example.com"))

		for _, cdn := range []string{"cdn.stored.cloud", "cdn.stored.ge", "cdn.stored.ge:443"} {
			assert.True(t, h.IsCDNHost(cdn), cdn)
		}
		assert.False(t, h.IsCDNHost("stored.cloud"))
	})

	t.Run("today's prod .env (only the base URL and the endpoint) keeps today's behaviour", func(t *testing.T) {
		// Merged and deployed before the owner edits .env: the canonical
		// origin follows VAULTAIRE_BASE_URL and the CDN stays cdn.<that>.
		h := FromEnv(env(map[string]string{
			"VAULTAIRE_ENDPOINT": "https://stored.ge", "VAULTAIRE_BASE_URL": "https://stored.ge",
		}), 8000, nil)
		assert.Equal(t, "https://stored.ge", h.Canonical)
		assert.Equal(t, "https://stored.ge", h.S3)
		assert.Equal(t, "https://cdn.stored.ge", h.CDN)
		assert.Equal(t, "https://cdn.stored.ge/", h.CDNBase())
		assert.True(t, h.IsCDNHost("cdn.stored.ge"))
		assert.True(t, h.IsCDNHost("cdn.stored.cloud"), "both legacy CDN names route whatever the env")
		assert.Empty(t, h.Aliases)
		assert.False(t, h.IsAlias("stored.ge"))
	})

	t.Run("a malformed value keeps the default and logs Warn", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		h := FromEnv(env(map[string]string{
			"VAULTAIRE_PUBLIC_HOST": "stored.cloud",             // no scheme
			"VAULTAIRE_ENDPOINT":    "ftp://s3.stored.cloud",    // not http(s)
			"VAULTAIRE_BASE_URL":    "https://stored.cloud/app", // a path is not an origin
			"CDN_PUBLIC_HOST":       "https://cdn.stored.cloud?x=1",
			"VAULTAIRE_ALIAS_HOSTS": "stored.ge,https://www.stored.ge,bad host,localhost:8000",
		}), 8000, zap.New(core))
		assert.Equal(t, "http://localhost:8000", h.Canonical)
		assert.Equal(t, "http://localhost:8000", h.S3)
		assert.Equal(t, "http://localhost:8000", h.BaseURL)
		assert.Equal(t, "http://localhost:8000", h.CDN)
		assert.Equal(t, []string{"stored.ge"}, h.Aliases, "each bad entry dropped, the good one kept")
		assert.GreaterOrEqual(t, logs.Len(), 7, "one Warn per rejected value")
		for _, e := range logs.All() {
			assert.Equal(t, zapcore.WarnLevel, e.Level)
		}
	})

	t.Run("a trailing slash is trimmed, scheme and host lower-cased", func(t *testing.T) {
		h := FromEnv(env(map[string]string{"VAULTAIRE_PUBLIC_HOST": "HTTPS://Stored.Cloud/"}), 8000, nil)
		assert.Equal(t, "https://stored.cloud", h.Canonical)
	})

	t.Run("an alias naming the canonical or the S3 host is dropped (a redirect loop)", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		m := map[string]string{}
		for k, v := range prodEnv {
			m[k] = v
		}
		m["VAULTAIRE_ALIAS_HOSTS"] = "stored.cloud,s3.stored.cloud,stored.ge"
		h := FromEnv(env(m), 8000, zap.New(core))
		assert.Equal(t, []string{"stored.ge"}, h.Aliases)
		assert.Equal(t, 2, logs.Len())
	})
}

func TestHosts_IsOwn(t *testing.T) {
	h := FromEnv(env(prodEnv), 8000, nil)
	for _, own := range []string{"stored.cloud", "www.stored.cloud", "cdn.stored.cloud", "stored.ge", "s3.stored.ge", "stored.pics", "localhost"} {
		assert.True(t, h.IsOwn(own), own)
	}
	for _, other := range []string{"news.ycombinator.com", "notstored.ge", "stored.ge.evil.com"} {
		assert.False(t, h.IsOwn(other), other)
	}
	// A dedicated content domain configured later is ours too.
	h.CDN = "https://usercontent.example"
	assert.True(t, h.IsOwn("usercontent.example"))
}

func TestHosts_SetGet(t *testing.T) {
	prev := Get()
	t.Cleanup(func() { Set(prev) })
	h := FromEnv(env(prodEnv), 8000, nil)
	Set(h)
	require.Equal(t, h, Get())
	assert.Equal(t, "https://stored.cloud/legal/aup", Get().URL("/legal/aup"))
}

// WP-PAY-3 (D-P20-A1): the issuer's legal data has one source.
func TestLegal_OneSource(t *testing.T) {
	l := Defaults(8000).Legal()
	if l.Name != "FairForge LLC" {
		t.Fatalf("name %q", l.Name)
	}
	if l.Email != "support@stored.cloud" {
		t.Fatalf("a development origin still names the brand domain: %q", l.Email)
	}
	if len(l.AddressLines) == 0 || l.Jurisdiction == "" {
		t.Fatal("the receipt needs lines under the name")
	}
}
