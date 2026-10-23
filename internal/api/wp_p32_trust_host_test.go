package api

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// K4b PR 4 (RP35-1, P4): /translog/checkpoint and /translog/tile/* shadowed
// an ANONYMOUS GET of a public customer bucket named "translog" on the web
// and S3 hosts. With a public canonical origin the read side now answers on
// the trust host only (trust.stored.cloud, contract rule 11); everywhere
// else the two paths are the S3 API's again, signed or not. On the trust
// host a signed S3 request is 404, never the S3 API. A development origin
// keeps the K4 behaviour so labs work.

// trustServer is a server with its own signed log, an S3 stand-in that
// answers 418, and the given host set.
func trustServer(t *testing.T, hs hosts.Hosts) (s *Server, fl *flags.Service, do func(host, path string, signed bool) *httptest.ResponseRecorder) {
	t.Helper()
	jf := setupJobsFixture(t)
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	fl = flags.New(jf.db, zap.NewNop())
	registerFlags(fl)
	s = &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: jf.db, flags: fl, config: &config.Config{}, hosts: hs}
	s.translog = logstore.New(logstoretest.PrivateLog(t), signer)
	s.s3Fallback = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }
	s.registerTranslogReadRoutes()
	_, _, err = s.translog.Append(context.Background(), translog.KeyCeremony{Action: translog.ActionKeyInstalled,
		KeyRole: translog.KeyTranslogSigningKey, KeyFingerprint: "sha256:" + strings.Repeat("2", 64), Participants: 1})
	require.NoError(t, err)
	_, err = s.translog.Checkpoint(context.Background())
	require.NoError(t, err)
	do = func(host, path string, signed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		if signed {
			req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20261022/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
		}
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		return w
	}
	return s, fl, do
}

func TestTranslogRead_PublicHost_OnlyTrustHostServes(t *testing.T) {
	// Arrange: the owner's prod hosts (canonical stored.cloud).
	hs := hosts.FromEnv(func(k string) string {
		return map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud", "VAULTAIRE_ENDPOINT": "https://s3.stored.cloud",
			"VAULTAIRE_ALIAS_HOSTS": "stored.ge"}[k]
	}, 8000, nil)
	_, fl, do := trustServer(t, hs)
	require.NoError(t, fl.Set(context.Background(), flagTranslogPublic, "*", true, "test"))
	t.Cleanup(func() { _ = fl.Unset(context.Background(), flagTranslogPublic, "*") })

	// Off the trust host: the S3 API, anonymous too — a public bucket named
	// "translog" is no longer shadowed.
	for _, host := range []string{"stored.cloud", "s3.stored.cloud", "stored.ge"} {
		for _, p := range []string{"/translog/checkpoint", "/translog/tile/0/000.p/1"} {
			assert.Equal(t, http.StatusTeapot, do(host, p, false).Code, "anonymous %s%s", host, p)
			assert.Equal(t, http.StatusTeapot, do(host, p, true).Code, "signed %s%s", host, p)
		}
	}

	// On the trust host: the log; a signed S3 request is 404, never the S3 API.
	for _, host := range []string{"trust.stored.cloud", "trust.stored.ge"} {
		w := do(host, "/translog/checkpoint", false)
		require.Equal(t, http.StatusOK, w.Code, host)
		assert.True(t, strings.HasPrefix(w.Body.String(), translog.Origin+"\n"), host)
		assert.Equal(t, http.StatusOK, do(host, "/translog/tile/entries/000.p/1", false).Code, host)
		assert.Equal(t, http.StatusNotFound, do(host, "/translog/checkpoint", true).Code, "signed on %s", host)
	}

	// Flag OFF: the trust host is 404; the other hosts stay the S3 API's.
	require.NoError(t, fl.Unset(context.Background(), flagTranslogPublic, "*"))
	assert.Equal(t, http.StatusNotFound, do("trust.stored.cloud", "/translog/checkpoint", false).Code)
	assert.Equal(t, http.StatusTeapot, do("stored.cloud", "/translog/checkpoint", false).Code)
}

func TestTranslogRead_DevHost_PathsStillServe(t *testing.T) {
	// A development origin: the K4 behaviour, on its one origin.
	_, fl, do := trustServer(t, hosts.Defaults(8000))
	assert.Equal(t, http.StatusNotFound, do("localhost:8000", "/translog/checkpoint", false).Code, "flag OFF")
	assert.Equal(t, http.StatusTeapot, do("localhost:8000", "/translog/checkpoint", true).Code, "signed = the S3 API")
	require.NoError(t, fl.Set(context.Background(), flagTranslogPublic, "*", true, "test"))
	t.Cleanup(func() { _ = fl.Unset(context.Background(), flagTranslogPublic, "*") })
	assert.Equal(t, http.StatusOK, do("localhost:8000", "/translog/checkpoint", false).Code)
	assert.Equal(t, http.StatusTeapot, do("localhost:8000", "/translog/checkpoint", true).Code)
}
