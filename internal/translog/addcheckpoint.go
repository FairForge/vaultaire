package translog

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The c2sp.org/tlog-witness add-checkpoint call (K4c-2): the body both
// sides speak, and the log's client. The witness side is
// internal/translog/witness (Server).

// AddCheckpointPath is the call's path under a witness's submission prefix.
const AddCheckpointPath = "/add-checkpoint"

// SizeContentType is the type of a 409 answer's body: the size the witness
// holds, in decimal, and a newline.
const SizeContentType = "text/x.tlog.size"

// maxProofLines: "The client MUST NOT send more than 63 consistency proof lines."
const maxProofLines = 63

// ErrBadAddCheckpoint: a request body that is not "old <N>\n", proof lines,
// an empty line and a checkpoint.
var ErrBadAddCheckpoint = errors.New("translog: malformed add-checkpoint body")

// AddCheckpointBody is the request body: "old <size>\n", one base64 hash per
// proof line, an empty line, then the checkpoint note.
func AddCheckpointBody(oldSize int64, proof []Hash, checkpoint []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "old %d\n", oldSize)
	for _, h := range proof {
		b.WriteString(base64.StdEncoding.EncodeToString(h[:]))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.Write(checkpoint)
	return b.Bytes()
}

// ParseAddCheckpoint reads a request body strictly: the old size in decimal
// with no leading zero (unless 0), at most 63 proof lines each a canonical
// base64 hash, an empty line, and a non-empty checkpoint.
func ParseAddCheckpoint(body []byte) (oldSize int64, proof []Hash, checkpoint []byte, err error) {
	line, rest, ok := bytes.Cut(body, []byte("\n"))
	if !ok {
		return 0, nil, nil, ErrBadAddCheckpoint
	}
	num, ok := bytes.CutPrefix(line, []byte("old "))
	if !ok || !decimal(num) {
		return 0, nil, nil, fmt.Errorf("%w: the first line is not \"old <size>\"", ErrBadAddCheckpoint)
	}
	oldSize, err = strconv.ParseInt(string(num), 10, 64)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: old size: %w", ErrBadAddCheckpoint, err)
	}
	for {
		line, rest, ok = bytes.Cut(rest, []byte("\n"))
		if !ok {
			return 0, nil, nil, fmt.Errorf("%w: no empty line before the checkpoint", ErrBadAddCheckpoint)
		}
		if len(line) == 0 {
			break
		}
		if len(proof) == maxProofLines {
			return 0, nil, nil, fmt.Errorf("%w: more than %d proof lines", ErrBadAddCheckpoint, maxProofLines)
		}
		h, err := base64.StdEncoding.DecodeString(string(line))
		if err != nil || len(h) != HashSize || base64.StdEncoding.EncodeToString(h) != string(line) {
			return 0, nil, nil, fmt.Errorf("%w: a proof line is not a canonical base64 hash", ErrBadAddCheckpoint)
		}
		var hh Hash
		copy(hh[:], h)
		proof = append(proof, hh)
	}
	if len(rest) == 0 {
		return 0, nil, nil, fmt.Errorf("%w: no checkpoint", ErrBadAddCheckpoint)
	}
	return oldSize, proof, rest, nil
}

// decimal: an ASCII decimal with no extra leading zero.
func decimal(b []byte) bool {
	if len(b) == 0 || len(b) > 19 || (b[0] == '0' && len(b) > 1) {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// AddCheckpointAnswer is a witness's answer.
type AddCheckpointAnswer struct {
	Status int
	// Lines is a 200's body: the witness's cosignature lines.
	Lines []byte
	// HeldSize is a 409's body: the size of the checkpoint the witness
	// cosigned last (-1 when the body was not one).
	HeldSize int64
}

const maxAnswer = 64 << 10 // a few signature lines

// SubmitCheckpoint POSTs body to prefix + AddCheckpointPath and reads the
// answer. A transport error is an error; every HTTP status is an answer.
func SubmitCheckpoint(ctx context.Context, hc *http.Client, prefix string, body []byte) (AddCheckpointAnswer, error) {
	ans := AddCheckpointAnswer{HeldSize: -1}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(prefix, "/")+AddCheckpointPath, bytes.NewReader(body))
	if err != nil {
		return ans, fmt.Errorf("translog: add-checkpoint request: %w", err)
	}
	resp, err := hc.Do(req) // #nosec G704 -- the operator's TRANSLOG_WITNESSES (validated at boot), never a request's input
	if err != nil {
		return ans, fmt.Errorf("translog: add-checkpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return ans, fmt.Errorf("translog: add-checkpoint answer: %w", err)
	}
	if len(b) > maxAnswer {
		return ans, fmt.Errorf("translog: add-checkpoint answer larger than %d bytes", maxAnswer)
	}
	ans.Status = resp.StatusCode
	switch resp.StatusCode {
	case http.StatusOK:
		ans.Lines = b
	case http.StatusConflict:
		if s, ok := bytes.CutSuffix(b, []byte("\n")); ok && decimal(s) {
			if n, err := strconv.ParseInt(string(s), 10, 64); err == nil {
				ans.HeldSize = n
			}
		}
	}
	return ans, nil
}
