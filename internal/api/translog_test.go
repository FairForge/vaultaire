package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// K4 at the server: the signing key follows the master key's rules — a
// source set but unusable stops the boot, one Info line names the source
// (and the key ID, a public value) and never the key; no source = no
// signer and vaultaire_translog_signing_key_loaded 0.
func TestTranslogSigningKeyOrFatal_RefusesTheBootAndNeverLogsTheKey(t *testing.T) {
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	secret := strings.SplitN(skey, "+", 5)[4]
	dir := t.TempDir()
	good := filepath.Join(dir, "k")
	require.NoError(t, os.WriteFile(good, []byte(skey+"\n"), 0o600))
	bad := filepath.Join(dir, "bad")
	require.NoError(t, os.WriteFile(bad, []byte(skey[:len(skey)-8]), 0o600))

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core, zap.WithFatalHook(zapcore.WriteThenPanic))
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	assert.Nil(t, translogSigningKeyOrFatal(logger, env(nil), os.ReadFile))
	assert.Equal(t, 0.0, gaugeValue(t, "vaultaire_translog_signing_key_loaded"))

	s := translogSigningKeyOrFatal(logger, env(map[string]string{"TRANSLOG_SIGNING_KEY_FILE": good, "TRANSLOG_SIGNING_KEY": skey}), os.ReadFile)
	require.NotNil(t, s)
	assert.Equal(t, 1.0, gaugeValue(t, "vaultaire_translog_signing_key_loaded"))
	require.Len(t, logs.FilterMessage("translog signing key: file").All(), 1, "one Info line naming the source")
	require.Len(t, logs.FilterMessageSnippet("ignored").All(), 1, "the env copy is named as ignored")

	assert.Panics(t, func() {
		translogSigningKeyOrFatal(logger, env(map[string]string{"TRANSLOG_SIGNING_KEY_FILE": bad}), os.ReadFile)
	}, "a malformed key is a refused boot, never 'no key'")
	require.Len(t, logs.FilterLevelExact(zapcore.FatalLevel).All(), 1)
	for _, e := range logs.All() {
		assert.NotContains(t, e.Message, secret[:16])
		for _, v := range e.ContextMap() {
			assert.NotContains(t, fmt.Sprint(v), secret[:16], "never the key")
		}
	}
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(logstore.Collectors()...)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no series %s", name)
	return 0
}

func TestTranslogCheckpointJob_ResultInJobRuns(t *testing.T) {
	jf := setupJobsFixture(t)
	c := newTranslogCheckpointer(logstore.New(jf.db, nil), zap.NewNop())
	c.JobName = "translog_checkpoint_test"
	rep, err := c.spec().Run(context.Background())
	require.NoError(t, err)
	res, ok := rep.Result.(logstore.CheckpointResult)
	require.True(t, ok)
	assert.False(t, res.Signed, "no key, nothing signed")
}
