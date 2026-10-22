package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog"
)

func logOf(t *testing.T, n int, rewrite int) *translog.Memory {
	t.Helper()
	m := &translog.Memory{}
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("entry-%d", i)
		if i == rewrite {
			body += "-rewritten"
		}
		_, err := m.Append([]byte(body))
		require.NoError(t, err)
	}
	return m
}

func serveLog(t *testing.T, m *translog.Memory, skey string) string {
	t.Helper()
	s, err := translog.NewSigner(skey)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/translog/", http.StripPrefix("/translog/", translog.NewHandler(&translog.MemorySource{Log: m, Signer: s})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/translog"
}

// TestTranslogVerify_RewrittenHistoryFails: the tool saves the checkpoint it
// verified; a later run against a log whose history was rewritten (a valid
// signature on the new checkpoint) fails and keeps the saved checkpoint.
func TestTranslogVerify_RewrittenHistoryFails(t *testing.T) {
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	state := filepath.Join(t.TempDir(), "checkpoint")
	var out bytes.Buffer

	honest := serveLog(t, logOf(t, 40, -1), skey)
	require.NoError(t, run(&out, options{url: honest, vkey: vkey, state: state, entry: 3}))
	assert.Contains(t, out.String(), "entry 3")
	saved, err := os.ReadFile(state)
	require.NoError(t, err)

	// The honest log grew: consistent, the state moves forward.
	grown := serveLog(t, logOf(t, 70, -1), skey)
	require.NoError(t, run(&out, options{url: grown, vkey: vkey, state: state, entry: -1}))
	grownState, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.NotEqual(t, saved, grownState)

	// Entry 5 rewritten, re-grown past the saved size: refused.
	forged := serveLog(t, logOf(t, 90, 5), skey)
	err = run(&out, options{url: forged, vkey: vkey, state: state, entry: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consistency")
	after, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, grownState, after, "a failed run never moves the saved checkpoint")

	// Another key: refused at the signature.
	_, otherV, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	assert.Error(t, run(&out, options{url: grown, vkey: otherV, state: filepath.Join(t.TempDir(), "s"), entry: -1}))
}
