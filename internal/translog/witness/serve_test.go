package witness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
)

// K4c-2 (Stage 1a Prompt 34b): `translog-witness serve` speaks
// c2sp.org/tlog-witness, sharing the pull pass's state under one lock.

func cosignerKeys(t *testing.T, name string) (note.Signer, note.Verifier) {
	t.Helper()
	sk, vk, err := translog.GenerateCosignerKey(name)
	require.NoError(t, err)
	s, err := translog.NewCosigner(sk, nil)
	require.NoError(t, err)
	v, err := translog.NewCosignatureVerifier(vk)
	require.NoError(t, err)
	return s, v
}

// signedAt is the log's checkpoint of m's first size entries.
func signedAt(t *testing.T, m *translog.Memory, k logKeys, size int64) (translog.Checkpoint, []byte) {
	t.Helper()
	root, err := translog.Root(size, m)
	require.NoError(t, err)
	msg, err := translog.SignCheckpoint(k.signer, size, root)
	require.NoError(t, err)
	return translog.Checkpoint{Origin: translog.Origin, Size: size, Root: root}, msg
}

func proofOf(t *testing.T, m *translog.Memory, old, size int64) []translog.Hash {
	t.Helper()
	p, err := translog.ConsistencyProof(old, size, m)
	require.NoError(t, err)
	return p
}

type witnessServer struct {
	url string
	srv *Server
}

func startServe(t *testing.T, dir string, k logKeys, ws note.Signer) *witnessServer {
	t.Helper()
	s := &Server{Dir: dir, Log: k.verifier, Signer: ws}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return &witnessServer{url: hs.URL, srv: s}
}

func (w *witnessServer) add(t *testing.T, body []byte) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Post(w.url+translog.AddCheckpointPath, "text/plain", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, b
}

func TestWitnessServe_AddCheckpoint_CosignsAndPersistsFirst(t *testing.T) {
	// Arrange
	k := newLogKeys(t)
	ws, wv := cosignerKeys(t, "witness.test/serve")
	dir := t.TempDir()
	w := startServe(t, dir, k, ws)
	m := memLog(t, 9, "a")
	_, msg5 := signedAt(t, m, k, 5)
	_, msg9 := signedAt(t, m, k, 9)

	// Act: the first checkpoint from 0, then the next with a proof 5 → 9.
	st, _, lines := w.add(t, translog.AddCheckpointBody(0, nil, msg5))
	require.Equal(t, http.StatusOK, st, string(lines))
	st9, _, lines9 := w.add(t, translog.AddCheckpointBody(5, proofOf(t, m, 5, 9), msg9))

	// Assert: each answer is the witness's line, verified over the checkpoint.
	require.Equal(t, http.StatusOK, st9, string(lines9))
	cos, err := translog.VerifyAnswer(msg9, lines9, wv)
	require.NoError(t, err)
	require.Len(t, cos, 1)
	assert.WithinDuration(t, time.Now(), cos[0].Time, time.Minute)
	held, ok, err := readHeld(dir, k.verifier)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(9), held.Size, "persisted before the answer")
	hist, err := os.ReadFile(filepath.Join(dir, HistoryFile))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(hist), "\n"))
	// The monitor view is the cosigned note.
	h := sha256.Sum256([]byte(translog.Origin))
	resp, err := http.Get(w.url + "/" + hex.EncodeToString(h[:]) + "/checkpoint")
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_, both, err := translog.OpenCosigned(b, k.verifier, wv)
	require.NoError(t, err)
	assert.Len(t, both, 1)
}

func TestWitnessServe_AddCheckpoint_Conflict_NamesItsSize(t *testing.T) {
	// Arrange: the witness holds 5.
	k := newLogKeys(t)
	ws, _ := cosignerKeys(t, "witness.test/serve")
	dir := t.TempDir()
	w := startServe(t, dir, k, ws)
	m := memLog(t, 9, "a")
	_, msg5 := signedAt(t, m, k, 5)
	st, _, _ := w.add(t, translog.AddCheckpointBody(0, nil, msg5))
	require.Equal(t, http.StatusOK, st)
	_, msg9 := signedAt(t, m, k, 9)

	for _, old := range []int64{0, 3, 7} {
		// Act: an old size that is not the one it holds.
		st, hdr, body := w.add(t, translog.AddCheckpointBody(old, proofOf(t, m, old, 9), msg9))

		// Assert: 409, the size it holds, text/x.tlog.size; nothing moved.
		assert.Equal(t, http.StatusConflict, st, "old %d", old)
		assert.Equal(t, translog.SizeContentType, hdr.Get("Content-Type"))
		assert.Equal(t, "5\n", string(body))
	}
	held, _, err := readHeld(dir, k.verifier)
	require.NoError(t, err)
	assert.Equal(t, int64(5), held.Size)
	// The proof from the size it named passes.
	st, _, _ = w.add(t, translog.AddCheckpointBody(5, proofOf(t, m, 5, 9), msg9))
	assert.Equal(t, http.StatusOK, st)
}

func TestWitnessServe_BadProof_Refused(t *testing.T) {
	// Arrange: the witness holds 5 of log a.
	k := newLogKeys(t)
	ws, _ := cosignerKeys(t, "witness.test/serve")
	dir := t.TempDir()
	w := startServe(t, dir, k, ws)
	a := memLog(t, 9, "a")
	_, msg5 := signedAt(t, a, k, 5)
	st, _, _ := w.add(t, translog.AddCheckpointBody(0, nil, msg5))
	require.Equal(t, http.StatusOK, st)
	before := stateOf(t, dir)
	_, msgA9 := signedAt(t, a, k, 9)
	// Log b: the same key, entry 2 rewritten — a fork the key signed.
	b := memLog(t, 9, "a")
	bb := &translog.Memory{}
	for i := int64(0); i < 9; i++ {
		e := b.Entry(i)
		if i == 2 {
			e = []byte("rewritten")
		}
		_, err := bb.Append(e)
		require.NoError(t, err)
	}
	_, msgB9 := signedAt(t, bb, k, 9)
	_, msgB5 := signedAt(t, bb, k, 5)
	wrong := proofOf(t, a, 5, 9)
	wrong[0][0] ^= 1
	otherKey := newLogKeys(t)
	_, msgOther := signedAt(t, a, otherKey, 9)
	unknown := strings.Replace(string(msgA9), translog.Origin, "example.com/another-log", 1)

	for name, c := range map[string]struct {
		body []byte
		want int
	}{
		"a proof that does not verify":            {translog.AddCheckpointBody(5, wrong, msgA9), http.StatusUnprocessableEntity},
		"the fork's proof from the held size":     {translog.AddCheckpointBody(5, proofOf(t, bb, 5, 9), msgB9), http.StatusUnprocessableEntity},
		"another root at the held size":           {translog.AddCheckpointBody(5, nil, msgB5), http.StatusUnprocessableEntity},
		"a checkpoint by another key":             {translog.AddCheckpointBody(5, proofOf(t, a, 5, 9), msgOther), http.StatusForbidden},
		"an origin it does not witness":           {translog.AddCheckpointBody(5, proofOf(t, a, 5, 9), []byte(unknown)), http.StatusNotFound},
		"an old size above the checkpoint's":      {translog.AddCheckpointBody(10, nil, msgA9), http.StatusBadRequest},
		"a malformed body":                        {[]byte("old 5\n" + string(msgA9)), http.StatusBadRequest},
		"a signature by the log's key that fails": {translog.AddCheckpointBody(5, proofOf(t, a, 5, 9), flipSig(t, msgA9)), http.StatusForbidden},
	} {
		// Act
		st, _, body := w.add(t, c.body)

		// Assert: refused, and the witness's state, cosigned note and history untouched.
		assert.Equal(t, c.want, st, "%s: %s", name, body)
		assert.Equal(t, before, stateOf(t, dir), name)
	}
	// A non-empty proof from 0 is 422 too (a fresh witness).
	w2 := startServe(t, t.TempDir(), k, ws)
	st, _, _ = w2.add(t, translog.AddCheckpointBody(0, proofOf(t, a, 5, 9), msgA9))
	assert.Equal(t, http.StatusUnprocessableEntity, st)
	// And GET of add-checkpoint is 405.
	resp, err := http.Get(w.url + translog.AddCheckpointPath)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// stateOf is the witness's state, cosigned note and history.
func stateOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range []string{StateFile, CosignFile, HistoryFile} {
		out[f] = string(readAll(t, filepath.Join(dir, f)))
	}
	return out
}

// flipSig flips the last byte of a note's first signature, keeping its name and key ID.
func flipSig(t *testing.T, msg []byte) []byte {
	t.Helper()
	text, sigs, ok := strings.Cut(string(msg), "\n\n")
	require.True(t, ok)
	line, rest, _ := strings.Cut(sigs, "\n")
	i := strings.LastIndexByte(line, ' ')
	raw, err := base64.StdEncoding.DecodeString(line[i+1:])
	require.NoError(t, err)
	raw[len(raw)-1] ^= 1
	return []byte(text + "\n\n" + line[:i+1] + base64.StdEncoding.EncodeToString(raw) + "\n" + rest)
}

// RP42-5: passes at once — a cron and a -once=false loop — each appended
// the same history line. Under the directory's lock, one line.
func TestWitnessServe_ConcurrentPassesOneHistoryLine(t *testing.T) {
	// Arrange
	k := newLogKeys(t)
	ws, _ := witnessKeys(t)
	dir := t.TempDir()
	s := serve(t, memLog(t, 600, "a"), k)
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)

	// Act
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = newWitness(t, dir, s, k, ws).Pass(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	// Assert
	for _, err := range errs {
		require.NoError(t, err)
	}
	hist, err := os.ReadFile(filepath.Join(dir, HistoryFile))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(hist), "\n"), "one checkpoint, one history line")
}

// The pull pass and serve share one state: what serve accepted, the pass
// extends; a checkpoint serve cosigned is not cosigned again by the pass.
func TestWitnessServe_SharesStateWithThePullPass(t *testing.T) {
	// Arrange: serve accepted 300 of the served log.
	k := newLogKeys(t)
	ws, _ := cosignerKeys(t, "witness.test/both")
	dir := t.TempDir()
	m := memLog(t, 300, "a")
	s := serve(t, m, k)
	w := startServe(t, dir, k, ws)
	_, msg := signedAt(t, m, k, 300)
	st, _, _ := w.add(t, translog.AddCheckpointBody(0, nil, msg))
	require.Equal(t, http.StatusOK, st)
	hist := readAll(t, filepath.Join(dir, HistoryFile))

	// Act: a pull pass at the same checkpoint.
	res, err := newWitness(t, dir, s, k, ws).Pass(context.Background())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(300), res.PrevSize, "the pass read serve's state")
	assert.Equal(t, hist, readAll(t, filepath.Join(dir, HistoryFile)), "no second cosignature of the same tree")
	assert.Equal(t, 2, res.Bundles, "the pass mirrors what serve does not")
}
