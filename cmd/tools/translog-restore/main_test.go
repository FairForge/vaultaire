package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

// K4b PR 3 (RP38-6): the round trip the restore runbook relies on — a log
// served and witnessed, a database restored from a dump behind the
// high-water, the lost entries put back from the WITNESS'S cache, and the
// checkpoint job passing on its own.

// dumpOf copies the first m entries (and their stored hashes and
// checkpoints) of src into a fresh log: what the 03:00 dump would hold.
func dumpOf(t *testing.T, src *sql.DB, m int64) *sql.DB {
	t.Helper()
	dst := logstoretest.PrivateLog(t)
	copyRows := func(query, insert string, arg int64, cols int) {
		rows, err := src.Query(query, arg)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			vals := make([]any, cols)
			ptrs := make([]any, cols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, rows.Scan(ptrs...))
			_, err := dst.Exec(insert, vals...)
			require.NoError(t, err)
		}
		require.NoError(t, rows.Err())
	}
	copyRows(`SELECT idx, kind, body, dedup_key FROM translog_entries WHERE idx < $1 ORDER BY idx`,
		`INSERT INTO translog_entries (idx, kind, body, dedup_key) VALUES ($1, $2, $3, $4)`, m, 4)
	copyRows(`SELECT idx, hash FROM translog_hashes WHERE idx < $1`,
		`INSERT INTO translog_hashes (idx, hash) VALUES ($1, $2)`, translog.StoredHashCount(m), 2)
	copyRows(`SELECT size, root, note FROM translog_checkpoints WHERE size <= $1`,
		`INSERT INTO translog_checkpoints (size, root, note) VALUES ($1, $2, $3)`, m, 3)
	return dst
}

func TestTranslogRestore_FromTheWitnessCache_TheJobPassesAgain(t *testing.T) {
	// Arrange: a live log of 9 server builds and ceremonies, signed at 5 and
	// at 9, served and witnessed after each signature.
	ctx := context.Background()
	signer, verifier := logKey(t)
	tmp := t.TempDir()
	hwFile := filepath.Join(tmp, "highwater")
	liveDB := logstoretest.PrivateLog(t)
	live := logstore.New(liveDB, signer).WithHighwater(hwFile)
	srv := httptest.NewServer(http.StripPrefix("/translog/", translog.NewHandler(live)))
	t.Cleanup(srv.Close)
	wskey, _, err := note.GenerateKey(rand.Reader, "witness.test/restore")
	require.NoError(t, err)
	ws, err := note.NewSigner(wskey)
	require.NoError(t, err)
	wdir := filepath.Join(tmp, "witness")
	w := &witness.Witness{Dir: wdir, Client: &translog.Client{BaseURL: srv.URL + "/translog/", Verifier: verifier}, Signer: ws}
	for i := 0; i < 9; i++ {
		_, _, err := live.Append(ctx, translog.KeyCeremony{Action: translog.ActionKeyGenerated, KeyRole: translog.KeyTranslogSigningKey,
			KeyFingerprint: "sha256:" + strings.Repeat("0123456789abcdef", 4)[:63] + string("0123456789"[i]), Participants: 1})
		require.NoError(t, err)
		if i == 4 || i == 8 {
			res, err := live.Checkpoint(ctx)
			require.NoError(t, err)
			require.True(t, res.Signed)
			_, err = w.Pass(ctx)
			require.NoError(t, err)
		}
	}
	// The disk dies; the database comes back from the dump taken at 5.
	restored := dumpOf(t, liveDB, 5)
	job := logstore.New(restored, signer).WithHighwater(hwFile)
	res, err := job.Checkpoint(ctx)
	require.Error(t, err, "before: behind the high-water")
	require.Equal(t, logstore.RefusedBehindHighwater, res.Refused)
	var out bytes.Buffer

	// Act
	err = run(ctx, &out, options{from: wdir, highwater: hwFile}, restored)

	// Assert: the tree at 9 is the high-water's and the job signs it again.
	require.NoError(t, err)
	assert.Contains(t, out.String(), "re-appended entries 5…8")
	hw, _, err := logstore.ReadHighwater(hwFile)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "the high-water's")
	root, err := job.Root(ctx, 9)
	require.NoError(t, err)
	assert.Equal(t, hw.Root, root)
	res, err = job.Checkpoint(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Refused)

	// Run again: the database is at the high-water — refused, nothing written.
	err = run(ctx, &out, options{from: wdir, highwater: hwFile}, restored)
	assert.ErrorContains(t, err, "nothing to re-append")
}

func TestTranslogRestore_MissingBundleIsNamed(t *testing.T) {
	// Arrange: a high-water at 9 but a witness that never saw past 5.
	ctx := context.Background()
	signer, verifier := logKey(t)
	tmp := t.TempDir()
	hwFile := filepath.Join(tmp, "highwater")
	liveDB := logstoretest.PrivateLog(t)
	live := logstore.New(liveDB, signer).WithHighwater(hwFile)
	srv := httptest.NewServer(http.StripPrefix("/translog/", translog.NewHandler(live)))
	t.Cleanup(srv.Close)
	wskey, _, err := note.GenerateKey(rand.Reader, "witness.test/restore")
	require.NoError(t, err)
	ws, err := note.NewSigner(wskey)
	require.NoError(t, err)
	wdir := filepath.Join(tmp, "witness")
	w := &witness.Witness{Dir: wdir, Client: &translog.Client{BaseURL: srv.URL + "/translog/", Verifier: verifier}, Signer: ws}
	for i := 0; i < 9; i++ {
		_, _, err := live.Append(ctx, translog.ServerBuild{GitSHA: "dev", BinarySHA256: strings.Repeat("ab", 31) + string("0123456789"[i]) + "0",
			BuildTime: "unknown", GoVersion: "go1.26.10"})
		require.NoError(t, err)
		if i == 4 || i == 8 {
			_, err := live.Checkpoint(ctx)
			require.NoError(t, err)
			if i == 4 {
				_, err = w.Pass(ctx)
				require.NoError(t, err)
			}
		}
	}
	restored := dumpOf(t, liveDB, 3)

	// Act
	err = run(ctx, &bytes.Buffer{}, options{from: wdir, highwater: hwFile}, restored)

	// Assert: refused, naming what is missing; nothing written.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry bundle 0 holds 5 entries, 9 are needed")
	n, err := logstore.New(restored, nil).Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
}

func logKey(t *testing.T) (note.Signer, note.Verifier) {
	t.Helper()
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := translog.NewSigner(skey)
	require.NoError(t, err)
	v, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	return s, v
}

// The restore tool is an operator tool: the product binary never links it.
func TestProductBinaryDoesNotLinkTheRestoreTool(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/FairForge/vaultaire/cmd/vaultaire").Output()
	require.NoError(t, err)
	for _, line := range strings.Split(string(out), "\n") {
		assert.NotContains(t, line, "/cmd/tools/", "cmd/vaultaire links an operator tool")
		assert.NotEqual(t, "github.com/FairForge/vaultaire/internal/translog/witness", line)
	}
}
