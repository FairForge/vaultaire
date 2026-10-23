package witness

import (
	"errors"
	"fmt"
	"net/http"
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

// Stage 1a Prompt 35, Part 0 — the WP-P34 review's RP46-1 and RP46-2 on
// `translog-witness serve`.

// countingSigner counts the signatures its signer makes.
type countingSigner struct {
	note.Signer
	mu sync.Mutex
	n  int
}

func (c *countingSigner) Sign(msg []byte) ([]byte, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Signer.Sign(msg)
}

func (c *countingSigner) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// clockedCosigner is an ML-DSA-44 cosigner on the clock *now.
func clockedCosigner(t *testing.T, name string, now *time.Time) (note.Signer, note.Verifier) {
	t.Helper()
	sk, vk, err := translog.GenerateCosignerKey(name)
	require.NoError(t, err)
	s, err := translog.NewCosigner(sk, func() time.Time { return *now })
	require.NoError(t, err)
	v, err := translog.NewCosignatureVerifier(vk)
	require.NoError(t, err)
	return s, v
}

func historyLines(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(string(readAll(t, filepath.Join(dir, HistoryFile))), "\n")
}

// RP46-1: the public checkpoint POSTed again with old = the held size used
// to sign anew every time (ML-DSA is randomised), append a ~4.4 KB history
// line and fsync two files under the directory's lock. A same-tree request
// whose cosignature is younger than the refresh floor is answered with that
// cosignature: no signature, no write.
func TestWitnessServe_ReplayedCheckpoint_NoNewWrites(t *testing.T) {
	// Arrange: the witness cosigned 9.
	k := newLogKeys(t)
	ws, wv := cosignerKeys(t, "witness.test/replay")
	cs := &countingSigner{Signer: ws}
	dir := t.TempDir()
	w := startServe(t, dir, k, cs)
	m := memLog(t, 9, "a")
	_, msg9 := signedAt(t, m, k, 9)
	st, _, _ := w.add(t, translog.AddCheckpointBody(0, nil, msg9))
	require.Equal(t, http.StatusOK, st)
	before := stateOf(t, dir)
	signed := cs.count()

	// Act: 100 replays of the same checkpoint from the held size.
	for i := 0; i < 100; i++ {
		st, _, lines := w.add(t, translog.AddCheckpointBody(9, nil, msg9))
		require.Equal(t, http.StatusOK, st, string(lines))
		cos, err := translog.VerifyAnswer(msg9, lines, wv)
		require.NoError(t, err, "replay %d: the answer is still a valid cosignature", i)
		require.Len(t, cos, 1)
	}

	// Assert
	assert.Equal(t, 1, historyLines(t, dir), "history = trees: one tree, one line")
	assert.LessOrEqual(t, cs.count()-signed, 1, "one signature at most for 100 replays")
	assert.Equal(t, before, stateOf(t, dir), "state, cosigned note and history untouched")
}

// Past the refresh floor a same-tree request is the log's refresh: it is
// signed anew (a fresher timestamp for readers' -max-age), `cosigned` is
// rewritten, and the history — one line per tree — gains nothing.
func TestWitnessServe_ReplayPastTheRefreshFloor_ResignsWithoutAHistoryLine(t *testing.T) {
	// Arrange
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	k := newLogKeys(t)
	ws, wv := clockedCosigner(t, "witness.test/floor", &now)
	dir := t.TempDir()
	srv := &Server{Dir: dir, Log: k.verifier, Signer: ws, RefreshFloor: time.Hour, Now: func() time.Time { return now }}
	w := startServeWith(t, srv)
	m := memLog(t, 9, "a")
	_, msg9 := signedAt(t, m, k, 9)
	st, _, first := w.add(t, translog.AddCheckpointBody(0, nil, msg9))
	require.Equal(t, http.StatusOK, st)

	// Act: a replay inside the floor, then one past it.
	now = now.Add(59 * time.Minute)
	_, _, inside := w.add(t, translog.AddCheckpointBody(9, nil, msg9))
	now = now.Add(2 * time.Minute)
	st, _, past := w.add(t, translog.AddCheckpointBody(9, nil, msg9))

	// Assert
	assert.Equal(t, first, inside, "inside the floor: the same cosignature")
	require.Equal(t, http.StatusOK, st)
	cos, err := translog.VerifyAnswer(msg9, past, wv)
	require.NoError(t, err)
	require.Len(t, cos, 1)
	assert.True(t, cos[0].Time.Equal(now), "past the floor: signed at %s, want %s", cos[0].Time, now)
	_, onDisk, err := translog.OpenCosigned(readAll(t, filepath.Join(dir, CosignFile)), k.verifier, wv)
	require.NoError(t, err)
	require.Len(t, onDisk, 1)
	assert.True(t, onDisk[0].Time.Equal(now), "cosigned holds the refreshed line")
	assert.Equal(t, 1, historyLines(t, dir), "a re-sign of the same tree adds no history line")
}

// RP46-2: accept wrote `cosigned` (and the history line) BEFORE the state.
// A crash between them published — on the monitor GET — a cosignature at N
// while the state still said M < N; a different root at N, consistent with
// M, was then cosigned too: the witness equivocated. The state is written
// first now, so a crash leaves the state ahead of `cosigned` (safe: 409
// names N, and only the same root at N is cosigned).
func TestWitness_CrashAfterCosign_ForkAtSameSizeRefused(t *testing.T) {
	// Arrange: the witness holds 5 of log a; log b shares a's first 5
	// entries and differs after them — a fork the key signed at 9.
	k := newLogKeys(t)
	ws, _ := cosignerKeys(t, "witness.test/crash")
	dir := t.TempDir()
	w := startServe(t, dir, k, ws)
	a := memLog(t, 9, "a")
	_, msgA5 := signedAt(t, a, k, 5)
	st, _, _ := w.add(t, translog.AddCheckpointBody(0, nil, msgA5))
	require.Equal(t, http.StatusOK, st)
	b := &translog.Memory{}
	for i := int64(0); i < 9; i++ {
		e := a.Entry(i)
		if i >= 5 {
			e = fmt.Appendf(nil, `{"entry":%d,"tag":"fork"}`, i)
		}
		_, err := b.Append(e)
		require.NoError(t, err)
	}
	_, msgA9 := signedAt(t, a, k, 9)
	_, msgB9 := signedAt(t, b, k, 9)
	// The witness dies right after it wrote its cosigned note of a's 9.
	afterWrite = func(name string) error {
		if name == CosignFile {
			return errors.New("crash")
		}
		return nil
	}
	st, _, _ = w.add(t, translog.AddCheckpointBody(5, proofOf(t, a, 5, 9), msgA9))
	afterWrite = nil
	require.Equal(t, http.StatusInternalServerError, st, "the crash: no answer was given")

	// Act: the fork at 9, from 5 (what the log last heard) and from 9.
	stFrom5, _, body5 := w.add(t, translog.AddCheckpointBody(5, proofOf(t, b, 5, 9), msgB9))
	stFrom9, _, body9 := w.add(t, translog.AddCheckpointBody(9, nil, msgB9))

	// Assert: never cosigned. 409 names the size the state holds; the
	// other root at it is 422.
	assert.Equal(t, http.StatusConflict, stFrom5, short(body5))
	assert.Equal(t, "9\n", short(body5))
	assert.Equal(t, http.StatusUnprocessableEntity, stFrom9, short(body9))
	// The safe residue: the same root at 9 is cosigned.
	st, _, _ = w.add(t, translog.AddCheckpointBody(9, nil, msgA9))
	assert.Equal(t, http.StatusOK, st)
	held, _, err := readHeld(dir, k.verifier)
	require.NoError(t, err)
	assert.Equal(t, int64(9), held.Size)
	_, err = os.Stat(filepath.Join(dir, CosignFile))
	require.NoError(t, err)
}

func startServeWith(t *testing.T, s *Server) *witnessServer {
	t.Helper()
	w := startServe(t, s.Dir, logKeys{verifier: s.Log}, s.Signer)
	*w.srv = *s
	return w
}

// short keeps a failure message readable: an ML-DSA-44 line is ~4.4 KB.
func short(b []byte) string {
	if len(b) > 60 {
		return string(b[:60]) + "…"
	}
	return string(b)
}
