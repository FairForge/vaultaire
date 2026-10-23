package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tlog "github.com/transparency-dev/formats/log"
	"go.uber.org/zap"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

// Stage 1a Prompt 33 Part 0a (RP42-1): the producers under the freeze.

// frozenHighwater writes a high-water file far ahead of any log: every
// append behind it is refused (behind_highwater).
func frozenHighwater(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "highwater")
	body := tlog.Checkpoint{Origin: translog.Origin, Size: 1 << 40, Hash: make([]byte, translog.HashSize)}.Marshal()
	require.NoError(t, os.WriteFile(p, body, 0o600))
	return p
}

// A frozen log answers 503 log_frozen BEFORE the TOTP code is consumed
// (RP37-14's order) and appends nothing; nothing stores the 503, so the same
// call with the same code and Idempotency-Key works once the log is repaired.
func TestCeremonyRoute_LogFrozen_503_CodeNotBurned(t *testing.T) {
	s, call := ceremonyRoute(t)
	hw := frozenHighwater(t)
	s.translog = logstore.New(s.db, nil).WithHighwater(hw)
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	fp := uniqueFingerprint(t)
	key := uuid.New().String()

	w := call(ceremonyBody(fp, code), key)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error":"log_frozen"}`, w.Body.String())
	assert.Equal(t, 0, countCeremonies(t, s, fp))

	// The log repaired (here: the file gone): the very same call lands.
	require.NoError(t, os.Remove(hw))
	w = call(ceremonyBody(fp, code), key)
	assert.Equal(t, http.StatusCreated, w.Code, "the code was not burned and the 503 not replayed: %s", w.Body.String())
	assert.Empty(t, w.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, 1, countCeremonies(t, s, fp))
}

// appendRefusalCount reads vaultaire_translog_append_refusals_total{reason}.
func appendRefusalCount(t *testing.T, reason string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(logstore.Collectors()...)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "vaultaire_translog_append_refusals_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no series vaultaire_translog_append_refusals_total{reason=%q}", reason)
	return 0
}

// dumpLog copies the first m entries (with their stored hashes and
// checkpoints) of src into a fresh log: what the 03:00 dump would hold.
func dumpLog(t *testing.T, src *sql.DB, m int64) *sql.DB {
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
	copyRows(`SELECT size, root, note, signed_at FROM translog_checkpoints WHERE size <= $1`,
		`INSERT INTO translog_checkpoints (size, root, note, signed_at) VALUES ($1, $2, $3, $4)`, m, 4)
	return dst
}

// The review's failure, end to end: a slot started on the restored database
// BEFORE translog-restore. Its boot entry is refused (not written, not lost),
// the re-append from the witness's cache succeeds, the retried boot entry
// lands at N after it, and the job signs N+1 with no refusal.
func TestRestore_SlotStartedFirst_StillRestores(t *testing.T) {
	// Arrange: a served log signed at 6 and 10, witnessed after each.
	ctx := context.Background()
	skey, vkey, err := note.GenerateKey(rand.Reader, translog.Origin)
	require.NoError(t, err)
	signer, err := note.NewSigner(skey)
	require.NoError(t, err)
	verifier, err := note.NewVerifier(vkey)
	require.NoError(t, err)
	tmp := t.TempDir()
	hwFile := filepath.Join(tmp, "highwater")
	liveDB := logstoretest.PrivateLog(t)
	live := logstore.New(liveDB, signer).WithHighwater(hwFile)
	srv := httptest.NewServer(http.StripPrefix("/translog/", translog.NewHandler(live)))
	t.Cleanup(srv.Close)
	wkey, _, err := note.GenerateKey(rand.Reader, "witness.test/p33")
	require.NoError(t, err)
	ws, err := note.NewSigner(wkey)
	require.NoError(t, err)
	wdir := filepath.Join(tmp, "witness")
	w := &witness.Witness{Dir: wdir, Client: &translog.Client{BaseURL: srv.URL + "/translog/", Verifier: verifier}, Signer: ws}
	for i := 0; i < 10; i++ {
		_, _, err := live.Append(ctx, translog.KeyCeremony{Action: translog.ActionKeyGenerated, KeyRole: translog.KeyTranslogSigningKey,
			KeyFingerprint: "sha256:" + uniqueFingerprint(t), Participants: 1})
		require.NoError(t, err)
		if i == 5 || i == 9 {
			res, err := live.Checkpoint(ctx)
			require.NoError(t, err)
			require.True(t, res.Signed)
			_, err = w.Pass(ctx)
			require.NoError(t, err)
		}
	}
	restored := dumpLog(t, liveDB, 6)

	// A slot boots on the restored database first, another binary.
	exe := filepath.Join(tmp, "vaultaire-8001")
	require.NoError(t, os.WriteFile(exe, []byte("slot "+uuid.New().String()), 0o700))
	s := &Server{logger: zap.NewNop(), translog: logstore.New(restored, signer).WithHighwater(hwFile)}
	job := newTranslogCheckpointer(s.translog, zap.NewNop())
	job.JobName = "translog_checkpoint_p33_test"
	refusals := appendRefusalCount(t, "behind_highwater")

	// Act 1: the boot entry is refused, nothing written, kept pending.
	s.recordBootBuild(ctx, exe)
	job.build = s.translogBuild

	n, err := s.translog.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(6), n, "the slot's boot appended nothing")
	assert.True(t, s.translogBuild.Pending())
	assert.Equal(t, refusals+1, appendRefusalCount(t, "behind_highwater"))
	_, err = job.spec().Run(ctx) // a job run while frozen: retried, refused again, nothing signed
	require.Error(t, err)
	assert.True(t, s.translogBuild.Pending())

	// Act 2: translog-restore — the witness's cached bodies, re-appended.
	hw, ok, err := logstore.ReadHighwater(hwFile)
	require.NoError(t, err)
	require.True(t, ok)
	bodies, err := witness.OpenCache(wdir).Entries(6, hw.Size)
	require.NoError(t, err)
	require.NoError(t, s.translog.ReAppend(ctx, bodies, hw))

	// Act 3: the next job run.
	rep, err := job.spec().Run(ctx)

	// Assert: the boot entry landed at 10, after the re-appended entries,
	// and the same run signed 11 — no refusal.
	require.NoError(t, err)
	res := rep.Result.(logstore.CheckpointResult)
	assert.Empty(t, res.Refused)
	assert.True(t, res.Signed)
	assert.Equal(t, int64(11), res.Size)
	assert.False(t, s.translogBuild.Pending())
	var kind string
	require.NoError(t, restored.QueryRow(`SELECT kind FROM translog_entries WHERE idx = 10`).Scan(&kind))
	assert.Equal(t, translog.KindServerBuild, kind)
	root, err := s.translog.Root(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, hw.Root, root, "the tree the key signed at 10 is intact")
}

// An entry the log refuses (here: a git sha that is not hex) is never kept
// for a retry: the job would retry it for ever.
func TestBootBuild_InvalidEntry_NotRetried(t *testing.T) {
	prev := BuildSHA
	BuildSHA = "not-hex"
	t.Cleanup(func() { BuildSHA = prev })
	exe := filepath.Join(t.TempDir(), "vaultaire")
	require.NoError(t, os.WriteFile(exe, []byte("bin "+uuid.New().String()), 0o700))
	s := &Server{logger: zap.NewNop(), translog: logstore.New(logstoretest.PrivateLog(t), nil)}

	s.recordBootBuild(context.Background(), exe)

	assert.False(t, s.translogBuild.Pending())
	n, err := s.translog.Size(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}
