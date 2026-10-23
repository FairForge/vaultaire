package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

// K4c-2 (Stage 1a Prompt 34b): after the translog_checkpoint job signs, it
// submits the checkpoint to every witness of TRANSLOG_WITNESSES, keeps each
// verified cosignature and serves it on the checkpoint. A witness that is
// down never fails the signing.

type cosignLab struct {
	db       *sql.DB
	store    *logstore.Store
	logV     note.Verifier
	job      *translogCheckpointer
	readURL  string
	wDir     string
	wv       note.Verifier
	wvkey    string
	wURL     string
	requests *atomic.Int64
	srv      *httptest.Server
	// skew moves the witness's clock (its cosignatures and its refresh
	// floor, RP46-1) with a test that moves the job's.
	skew *atomic.Int64
}

// newCosignLab is a private log with a signing key, the read side, and one
// witness (`translog-witness serve`'s handler) the job submits to.
func newCosignLab(t *testing.T) *cosignLab {
	t.Helper()
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	logV, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	db := logstoretest.PrivateLog(t)
	store := logstore.New(db, signer).WithHighwater(filepath.Join(t.TempDir(), "highwater"))
	read := httptest.NewServer(http.StripPrefix("/translog/", translog.NewHandler(store)))
	t.Cleanup(read.Close)

	wsk, wvk, err := translog.GenerateCosignerKey("witness.test/lab")
	require.NoError(t, err)
	var skew atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	ws, err := translog.NewCosigner(wsk, clock)
	require.NoError(t, err)
	wv, err := translog.NewCosignatureVerifier(wvk)
	require.NoError(t, err)
	wDir := t.TempDir()
	var n atomic.Int64
	h := (&witness.Server{Dir: wDir, Log: logV, Signer: ws, Now: clock}).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ws2, err := parseTranslogWitnesses("lab " + srv.URL + " " + wvk)
	require.NoError(t, err)
	logstore.SetWitnesses([]string{"lab"})
	job := newTranslogCheckpointer(store, zap.NewNop())
	job.JobName = "translog_checkpoint_cosign_test"
	job.cosign = newCosignCollector(store, ws2, zap.NewNop())
	return &cosignLab{db: db, store: store, logV: logV, job: job, readURL: read.URL + "/translog/", wDir: wDir, wv: wv, wvkey: wvk, wURL: srv.URL, requests: &n, srv: srv, skew: &skew}
}

func (l *cosignLab) append(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, _, err := l.store.Append(context.Background(), translog.KeyCeremony{Action: translog.ActionKeyGenerated,
			KeyRole: translog.KeyTranslogSigningKey, KeyFingerprint: "sha256:" + uniqueFingerprint(t), Participants: 1})
		require.NoError(t, err)
	}
}

func (l *cosignLab) run(t *testing.T) (logstore.CheckpointResult, jobReport) {
	t.Helper()
	rep, err := l.job.spec().Run(context.Background())
	require.NoError(t, err, "a witness never fails the run")
	res, ok := rep.Result.(logstore.CheckpointResult)
	require.True(t, ok)
	return res, rep
}

func (l *cosignLab) served(t *testing.T) []byte {
	t.Helper()
	resp, err := http.Get(l.readURL + "checkpoint")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func cosignCount(t *testing.T, witness, outcome string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(logstore.Collectors()...)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "vaultaire_translog_witness_cosign_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["witness"] == witness && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no series {witness=%q, outcome=%q}", witness, outcome)
	return 0
}

func TestCheckpointJob_CollectsAndServesCosignatures(t *testing.T) {
	// Arrange
	l := newCosignLab(t)
	l.append(t, 3)
	ok0 := cosignCount(t, "lab", logstore.CosignOK)

	// Act: sign 3 → the job submits.
	res, rep := l.run(t)

	// Assert: signed, the witness answered, the line kept and served.
	require.True(t, res.Signed)
	require.Equal(t, logstore.CosignOK, res.Witnesses["lab"].Outcome, res.Witnesses["lab"].Detail)
	assert.Equal(t, int64(0), res.Witnesses["lab"].Old)
	assert.Empty(t, rep.Note)
	assert.Equal(t, ok0+1, cosignCount(t, "lab", logstore.CosignOK))
	served := l.served(t)
	cp, cos, err := translog.OpenCosigned(served, l.logV, l.wv)
	require.NoError(t, err)
	assert.Equal(t, int64(3), cp.Size)
	require.Len(t, cos, 1, "the checkpoint served carries the witness's line")
	assert.Equal(t, 2, strings.Count(string(served), "\n— "), "the log's line and the witness's")
	// A reader with only the log's key still opens it.
	_, err = translog.OpenCheckpoint(served, l.logV)
	require.NoError(t, err)

	// Act: the next run with nothing new submits nothing.
	before := l.requests.Load()
	res, _ = l.run(t)
	assert.Nil(t, res.Witnesses)
	assert.Equal(t, before, l.requests.Load())

	// Act: the log grows to 5: the proof from 3.
	l.append(t, 2)
	res, _ = l.run(t)
	require.Equal(t, logstore.CosignOK, res.Witnesses["lab"].Outcome, res.Witnesses["lab"].Detail)
	assert.Equal(t, int64(3), res.Witnesses["lab"].Old)
	cp, cos, err = translog.OpenCosigned(l.served(t), l.logV, l.wv)
	require.NoError(t, err)
	assert.Equal(t, int64(5), cp.Size)
	assert.Len(t, cos, 1)
}

func TestCheckpointJob_WitnessDown_StillSigns(t *testing.T) {
	// Arrange: the witness is gone.
	l := newCosignLab(t)
	l.append(t, 4)
	l.srv.Close()
	down0 := cosignCount(t, "lab", logstore.CosignUnreachable)

	// Act
	res, rep := l.run(t)

	// Assert: signed; the witness is a note and a count, nothing more.
	require.True(t, res.Signed)
	assert.Equal(t, int64(4), res.CheckpointSize)
	assert.Equal(t, logstore.CosignUnreachable, res.Witnesses["lab"].Outcome)
	assert.Contains(t, rep.Note, "witness lab: unreachable")
	assert.Equal(t, down0+1, cosignCount(t, "lab", logstore.CosignUnreachable))
	cp, cos, err := translog.OpenCosigned(l.served(t), l.logV, l.wv)
	require.NoError(t, err)
	assert.Equal(t, int64(4), cp.Size)
	assert.Empty(t, cos, "no cosignature served")
	// The next run asks again (the witness has not cosigned the newest).
	res, _ = l.run(t)
	assert.Equal(t, logstore.CosignUnreachable, res.Witnesses["lab"].Outcome)
}

// A 409: the witness holds a size the log did not record (its own pull
// pass moved it, an answer was lost): the job sends the proof from the size
// it names, once.
func TestCheckpointJob_ConflictRetriedOnceFromTheNamedSize(t *testing.T) {
	// Arrange: the witness already holds 3 (submitted around the log's back).
	l := newCosignLab(t)
	l.append(t, 3)
	_, err := l.store.Checkpoint(context.Background())
	require.NoError(t, err)
	at3, ok, err := l.store.LatestCheckpoint(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	ans, err := translog.SubmitCheckpoint(context.Background(), http.DefaultClient, l.wURL, translog.AddCheckpointBody(0, nil, []byte(at3.Note)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, ans.Status)
	l.append(t, 3)
	n0 := l.requests.Load()

	// Act: the log, which recorded nothing from the witness, submits from 0.
	res, _ := l.run(t)

	// Assert: 409 "3", then the proof 3 → 6: cosigned in two requests.
	require.Equal(t, logstore.CosignOK, res.Witnesses["lab"].Outcome, res.Witnesses["lab"].Detail)
	assert.Equal(t, int64(3), res.Witnesses["lab"].Old)
	assert.Equal(t, n0+2, l.requests.Load())
	_, cos, err := translog.OpenCosigned(l.served(t), l.logV, l.wv)
	require.NoError(t, err)
	assert.Len(t, cos, 1)
}

// An answer whose line does not verify under the pinned key is never kept.
func TestCheckpointJob_BadAnswer_NothingKept(t *testing.T) {
	// Arrange: a "witness" that answers a line under the right name and key
	// ID with another key's signature.
	l := newCosignLab(t)
	l.append(t, 2)
	impostorKey, _, err := translog.GenerateCosignerKey(l.wv.Name())
	require.NoError(t, err)
	impostor, err := translog.NewCosigner(impostorKey, nil)
	require.NoError(t, err)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, cp, err := translog.ParseAddCheckpoint(body)
		require.NoError(t, err)
		n, err := note.Open(cp, note.VerifierList(l.logV))
		require.NoError(t, err)
		cos, err := note.Sign(&note.Note{Text: n.Text}, impostor)
		require.NoError(t, err)
		line := translog.CosignatureLines(cos, impostor.Name(), impostor.KeyHash())[0]
		// The pinned key's ID on the impostor's bytes.
		_, rest, _ := strings.Cut(line, " ")
		_, b64, _ := strings.Cut(rest, " ")
		raw := mustB64(t, strings.TrimSpace(b64))
		raw[0], raw[1], raw[2], raw[3] = byte(l.wv.KeyHash()>>24), byte(l.wv.KeyHash()>>16), byte(l.wv.KeyHash()>>8), byte(l.wv.KeyHash())
		_, _ = io.WriteString(w, "— "+l.wv.Name()+" "+b64Of(raw)+"\n")
	}))
	t.Cleanup(fake.Close)
	ws, err := parseTranslogWitnesses("lab " + fake.URL + " " + l.wvkey)
	require.NoError(t, err)
	l.job.cosign = newCosignCollector(l.store, ws, zap.NewNop())

	// Act
	res, _ := l.run(t)

	// Assert
	assert.Equal(t, logstore.CosignBadAnswer, res.Witnesses["lab"].Outcome)
	_, cos, err := translog.OpenCosigned(l.served(t), l.logV, l.wv)
	require.NoError(t, err)
	assert.Empty(t, cos)
}

// A run that refuses to sign never puts the log in front of a witness.
func TestCheckpointJob_RefusedRun_SubmitsNothing(t *testing.T) {
	// Arrange: a checkpoint the witness has NOT cosigned (signed with the
	// collect step off), so it would be due.
	l := newCosignLab(t)
	l.append(t, 2)
	collector := l.job.cosign
	l.job.cosign = nil
	l.run(t)
	l.job.cosign = collector
	n0 := l.requests.Load()
	// The high-water says 9: the database is behind (a restore).
	hw := filepath.Join(t.TempDir(), "hw")
	require.NoError(t, writeTestHighwater(hw, 9))
	l.job.store = l.store.WithHighwater(hw)

	// Act
	_, err := l.job.spec().Run(context.Background())

	// Assert: refused, and the witness was never asked.
	require.Error(t, err)
	assert.Equal(t, n0, l.requests.Load())
}

func TestParseTranslogWitnesses(t *testing.T) {
	_, v1, err := translog.GenerateCosignerKey("witness.example/a")
	require.NoError(t, err)
	_, v2, err := translog.GenerateCosignerKey("witness.example/b")
	require.NoError(t, err)
	_, plain, err := note.GenerateKey(rand.Reader, "witness.example/plain")
	require.NoError(t, err)

	ws, err := parseTranslogWitnesses("a https://w.example/a " + v1 + ";\n b http://127.0.0.1:7380/ " + v2 + "\n")
	require.NoError(t, err)
	require.Len(t, ws, 2)
	assert.Equal(t, "http://127.0.0.1:7380", ws[1].URL, "the prefix without its trailing slash")
	for name, bad := range map[string]string{
		"two fields":         "a https://w.example/a",
		"an upper-case name": "A https://w.example/a " + v1,
		"http to the world":  "a http://w.example/a " + v1,
		"a plain note key":   "a https://w.example/a " + plain,
		"a name twice":       "a https://w.example/a " + v1 + "; a https://w.example/b " + v2,
		"a key twice":        "a https://w.example/a " + v1 + "; b https://w.example/b " + v1,
		"a query":            "a https://w.example/a?x=1 " + v1,
	} {
		_, err := parseTranslogWitnesses(bad)
		assert.Error(t, err, name)
	}
	none, err := parseTranslogWitnesses("  ")
	require.NoError(t, err)
	assert.Empty(t, none)
}

// Unset = no collector, the gauge 0; a list that does not validate = an
// Error line and none.
func TestTranslogWitnessesFromEnv(t *testing.T) {
	assert.Nil(t, translogWitnessesFromEnv(zap.NewNop(), func(string) string { return "" }))
	assert.Equal(t, 0.0, gaugeValue(t, "vaultaire_translog_witnesses_configured"))
	assert.Nil(t, newCosignCollector(nil, nil, zap.NewNop()))
	assert.Nil(t, translogWitnessesFromEnv(zap.NewNop(), func(string) string { return "broken" }))
	var c *cosignCollector
	assert.Nil(t, c.collect(context.Background()), "no witnesses: nothing collected, nothing fails")
	_ = time.Second
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return b
}

func b64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// writeTestHighwater writes a high-water file of this log at size (a root of zeros).
func writeTestHighwater(path string, size int64) error {
	body := fmt.Sprintf("%s\n%d\n%s\n", translog.Origin, size, b64Of(make([]byte, 32)))
	return os.WriteFile(path, []byte(body), 0o600)
}

// An idle log: the newest checkpoint's cosignature is refreshed after
// witnessRefresh (old size = the size), so a reader's max age keeps passing;
// the read side serves the newest line of the witness, one line.
func TestCheckpointJob_RefreshesAnIdleCosignature(t *testing.T) {
	// Arrange: 3 signed and cosigned.
	l := newCosignLab(t)
	l.append(t, 3)
	res, _ := l.run(t)
	require.Equal(t, logstore.CosignOK, res.Witnesses["lab"].Outcome)
	first := l.served(t)

	// Act: seven hours later, nothing new (the witness's clock too: past
	// its refresh floor it signs anew).
	l.job.cosign.now = func() time.Time { return time.Now().Add(7 * time.Hour) }
	l.skew.Store(int64(7 * time.Hour))
	res, _ = l.run(t)

	// Assert: asked again from its own size, a second row, one line served.
	require.Equal(t, logstore.CosignOK, res.Witnesses["lab"].Outcome, res.Witnesses["lab"].Detail)
	assert.Equal(t, int64(3), res.Witnesses["lab"].Old)
	var rows int
	require.NoError(t, l.db.QueryRow(`SELECT COUNT(*) FROM translog_cosignatures WHERE checkpoint_size = 3`).Scan(&rows))
	assert.Equal(t, 2, rows)
	served := l.served(t)
	_, cos, err := translog.OpenCosigned(served, l.logV, l.wv)
	require.NoError(t, err)
	require.Len(t, cos, 1)
	assert.Equal(t, 1, strings.Count(string(served), "\n— "+l.wv.Name()+" "), "the newest line per witness, one line on the note")
	assert.NotEqual(t, string(first), string(served))
	// Within the window, not again.
	l.job.cosign.now = time.Now
	res, _ = l.run(t)
	assert.Nil(t, res.Witnesses)
}
