package witness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
)

// Server is the witness side of c2sp.org/tlog-witness (K4c-2): the log
// POSTs <prefix>/add-checkpoint with the size it believes the witness holds,
// a consistency proof from it and the new checkpoint; the witness checks
// the log's signature against its pinned key, proves the new tree extends
// the one it cosigned last — the same <dir>/checkpoint the pull pass keeps,
// under the same lock — and answers its cosignature line. It does not
// mirror: the pull pass (cron) keeps the byte cache translog-restore reads.
//
// It answers the spec's codes: 404 an origin it does not witness, 403 no
// valid signature by the log's key (or one that fails), 400 a malformed
// body or an old size above the checkpoint's, 409 an old size that is not
// the size it holds (body: that size, text/x.tlog.size), 422 a proof that
// does not verify (or a non-empty one from 0, or another root at the same
// size). GET <prefix>/<sha256(origin) hex>/checkpoint is the monitor view:
// the note it cosigned last, with both signatures.
type Server struct {
	Dir    string
	Log    note.Verifier // the log's pinned key
	Signer note.Signer   // the witness's cosignature/v1 key
	// Logf records a refused request whose signature verified — possible
	// evidence of log misbehaviour (the spec's MAY). nil = silent.
	Logf func(format string, args ...any)
	// LockWait bounds the wait for the directory lock (default 10 s; a
	// pull pass mirroring holds it): past it the answer is 503.
	LockWait time.Duration
	// RefreshFloor (default DefaultRefreshFloor): a request for the
	// checkpoint the witness holds is answered with its existing
	// cosignature, signing and writing nothing, while that cosignature is
	// younger than this (RP46-1: the public checkpoint POSTed again must not
	// cost a signature, a history line and two fsyncs per request). Past it
	// the request is the log's refresh and is signed anew; keep it below the
	// log's refresh (6 h, internal/api/translog_witnesses.go).
	RefreshFloor time.Duration
	// Now is the clock the floor is read against (nil = time.Now).
	Now func() time.Time
}

// DefaultRefreshFloor is Server.RefreshFloor's default.
const DefaultRefreshFloor = time.Hour

const maxAddCheckpoint = 1 << 20 // a checkpoint with up to 100 signature lines, a proof of 63 hashes

// Handler serves the witness at the root of the submission prefix.
func (s *Server) Handler() http.Handler {
	monitor := "/" + originHash(s.Log.Name()) + "/checkpoint"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == translog.AddCheckpointPath:
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				plain(w, http.StatusMethodNotAllowed, "add-checkpoint is a POST\n")
				return
			}
			s.addCheckpoint(w, r)
		case r.URL.Path == monitor && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			b, err := os.ReadFile(filepath.Join(s.Dir, CosignFile)) // #nosec G304 -- the witness's own directory
			if err != nil {
				plain(w, http.StatusNotFound, "no checkpoint cosigned yet\n")
				return
			}
			plain(w, http.StatusOK, string(b))
		default:
			plain(w, http.StatusNotFound, "not found\n")
		}
	})
}

// originHash is the monitor path's origin hash: SHA-256 of the origin, lower hex.
func originHash(origin string) string {
	h := sha256.Sum256([]byte(origin))
	return hex.EncodeToString(h[:])
}

func (s *Server) addCheckpoint(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAddCheckpoint+1))
	if err != nil || len(body) > maxAddCheckpoint {
		plain(w, http.StatusBadRequest, "the body cannot be read, or is larger than 1 MiB\n")
		return
	}
	oldSize, proof, msg, err := translog.ParseAddCheckpoint(body)
	if err != nil {
		plain(w, http.StatusBadRequest, err.Error()+"\n")
		return
	}
	if origin, _, _ := bytes.Cut(msg, []byte("\n")); string(origin) != s.Log.Name() {
		plain(w, http.StatusNotFound, "this witness does not witness that origin\n")
		return
	}
	n, err := note.Open(msg, note.VerifierList(s.Log))
	if err != nil {
		var unverified *note.UnverifiedNoteError
		var invalid *note.InvalidSignatureError
		if errors.As(err, &unverified) || errors.As(err, &invalid) {
			plain(w, http.StatusForbidden, "no valid signature by the log's key\n")
			return
		}
		plain(w, http.StatusBadRequest, "the checkpoint is not a signed note\n")
		return
	}
	cp, err := translog.OpenCheckpoint(msg, s.Log)
	if err != nil {
		plain(w, http.StatusBadRequest, "the note is not a checkpoint of this log\n")
		return
	}
	if oldSize > cp.Size {
		plain(w, http.StatusBadRequest, "the old size is larger than the checkpoint's\n")
		return
	}
	if cp.Size == 0 && cp.Root != translog.EmptyRoot() {
		plain(w, http.StatusUnprocessableEntity, "a checkpoint of size 0 must have the empty tree's root\n")
		return
	}

	wait := s.LockWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	lctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	unlock, err := lockDir(lctx, s.Dir)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		plain(w, http.StatusServiceUnavailable, "the witness is busy\n")
		return
	}
	defer unlock()

	held, ok, err := readHeld(s.Dir, s.Log)
	if err != nil {
		plain(w, http.StatusInternalServerError, "the witness cannot read its state\n")
		return
	}
	heldSize := int64(0)
	if ok {
		heldSize = held.Size
	}
	if oldSize != heldSize {
		w.Header().Set("Content-Type", translog.SizeContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(w, "%d\n", heldSize)
		return
	}
	switch oldSize {
	case 0:
		if len(proof) != 0 {
			s.refuse(w, cp, oldSize, "a consistency proof from size 0 must be empty")
			return
		}
	case cp.Size:
		if len(proof) != 0 || held.Root != cp.Root {
			s.refuse(w, cp, oldSize, "another root at the size this witness holds")
			return
		}
	default:
		if err := translog.VerifyConsistency(proof, oldSize, cp.Size, held.Root, cp.Root); err != nil {
			s.refuse(w, cp, oldSize, "the consistency proof does not verify")
			return
		}
	}

	floor := s.RefreshFloor
	if floor <= 0 {
		floor = DefaultRefreshFloor
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	cos, _, err := accept(s.Dir, msg, n, s.Signer, floor, now())
	if err != nil {
		plain(w, http.StatusInternalServerError, "the witness could not record the checkpoint\n")
		return
	}
	lines := translog.CosignatureLines(sigBlock(cos), s.Signer.Name(), s.Signer.KeyHash())
	var out bytes.Buffer
	for _, l := range lines {
		out.WriteString(l)
	}
	plain(w, http.StatusOK, out.String())
}

// refuse answers 422 and logs the signed request (it may prove misbehaviour).
func (s *Server) refuse(w http.ResponseWriter, cp translog.Checkpoint, oldSize int64, why string) {
	if s.Logf != nil {
		s.Logf("witness: refused a signed checkpoint of %s size %d root %x (old size %d): %s", cp.Origin, cp.Size, cp.Root[:], oldSize, why)
	}
	plain(w, http.StatusUnprocessableEntity, why+"\n")
}

// sigBlock is a note's signature lines.
func sigBlock(msg []byte) []byte {
	if i := bytes.LastIndex(msg, []byte("\n\n")); i >= 0 {
		return msg[i+2:]
	}
	return nil
}

func plain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body) // #nosec G705 -- text/plain under nosniff: a status sentence or signed-note lines, never HTML
}
