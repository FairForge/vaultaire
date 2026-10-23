package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

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

// cosignedSource serves a Memory log's checkpoint with extra signature lines.
type cosignedSource struct {
	*translog.MemorySource
	lines func(cp []byte) string
}

func (s cosignedSource) CheckpointNote(ctx context.Context) ([]byte, int64, bool, error) {
	msg, size, ok, err := s.MemorySource.CheckpointNote(ctx)
	if err != nil || !ok {
		return msg, size, ok, err
	}
	return append(msg, s.lines(msg)...), size, true, nil
}

func cosignerAt(t *testing.T, name string, at time.Time) (note.Signer, string) {
	t.Helper()
	sk, vk, err := translog.GenerateCosignerKey(name)
	require.NoError(t, err)
	s, err := translog.NewCosigner(sk, func() time.Time { return at })
	require.NoError(t, err)
	return s, vk
}

// lineOf is signer's cosignature line on the checkpoint text of msg.
func lineOf(t *testing.T, msg []byte, s note.Signer) string {
	t.Helper()
	text, _, ok := strings.Cut(string(msg), "\n\n")
	require.True(t, ok)
	cos, err := note.Sign(&note.Note{Text: text + "\n"}, s)
	require.NoError(t, err)
	return translog.CosignatureLines(cos, s.Name(), s.KeyHash())[0]
}

// K4c-2: translog-verify -witness <vkey> [-min N] accepts a checkpoint only
// with N fresh cosignatures from the given keys — a forged line, a line of
// another checkpoint and a stale timestamp do not count (the first two fail
// the run outright: the note is malformed).
func TestVerify_MinCosignatures(t *testing.T) {
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ls, err := translog.NewSigner(skey)
	require.NoError(t, err)
	now := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	wa, va := cosignerAt(t, "witness.test/a", now.Add(-10*time.Minute))
	wb, vb := cosignerAt(t, "witness.test/b", now.Add(-72*time.Hour))
	m := logOf(t, 12, -1)
	other := logOf(t, 11, -1)
	otherSrc := &translog.MemorySource{Log: other, Signer: ls}
	otherMsg, _, _, err := otherSrc.CheckpointNote(context.Background())
	require.NoError(t, err)

	serveWith := func(lines func(cp []byte) string) string {
		mux := http.NewServeMux()
		mux.Handle("/translog/", http.StripPrefix("/translog/", translog.NewHandler(cosignedSource{&translog.MemorySource{Log: m, Signer: ls}, lines})))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv.URL + "/translog"
	}
	verify := func(url string, min int, ws ...string) (string, error) {
		var out bytes.Buffer
		err := run(&out, options{url: url, vkey: vkey, state: filepath.Join(t.TempDir(), "s"), entry: -1,
			witnesses: ws, min: min, maxAge: 24 * time.Hour, now: func() time.Time { return now }})
		return out.String(), err
	}

	// Both lines present; a's is fresh, b's three days old.
	both := serveWith(func(cp []byte) string { return lineOf(t, cp, wa) + lineOf(t, cp, wb) })
	out, err := verify(both, 1, va, vb)
	require.NoError(t, err, out)
	assert.Contains(t, out, "cosignature: witness.test/a+")
	assert.Contains(t, out, "cosignatures: 1 of the 1 required")
	_, err = verify(both, 2, va, vb)
	require.ErrorIs(t, err, translog.ErrTooFewCosignatures, "b's cosignature is stale")
	out, _ = verify(both, 2, va, vb)
	assert.Contains(t, out, "stale")
	// The default -min with a -witness is 1.
	_, err = verify(both, -1, vb)
	assert.ErrorIs(t, err, translog.ErrTooFewCosignatures)

	// a's line of another checkpoint (the log at 11) on the checkpoint of 12.
	replayed := serveWith(func([]byte) string { return lineOf(t, otherMsg, wa) })
	_, err = verify(replayed, 1, va)
	assert.ErrorContains(t, err, "does not verify over this checkpoint")

	// A forged line: a's name and key ID over bytes a never signed.
	forged := serveWith(func(cp []byte) string {
		l := lineOf(t, cp, wa)
		i := strings.LastIndexByte(l, ' ')
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(l[i+1:]))
		require.NoError(t, err)
		raw[20] ^= 0xff
		return l[:i+1] + base64.StdEncoding.EncodeToString(raw) + "\n"
	})
	_, err = verify(forged, 1, va)
	assert.ErrorContains(t, err, "does not verify")

	// No line at all, -min 1: refused; without -witness the run is K4's.
	bare := serveWith(func([]byte) string { return "" })
	_, err = verify(bare, 1, va)
	assert.ErrorIs(t, err, translog.ErrTooFewCosignatures)
	_, err = verify(bare, -1)
	assert.NoError(t, err)
	// -min above the keys given is a usage error; a plain note key is no witness key.
	_, err = verify(bare, 2, va)
	assert.ErrorContains(t, err, "only 1 -witness")
	_, plain, err := note.GenerateKey(rand.Reader, "witness.test/plain")
	require.NoError(t, err)
	_, err = verify(bare, 1, plain)
	assert.ErrorContains(t, err, "not a cosignature/v1 key")
}
