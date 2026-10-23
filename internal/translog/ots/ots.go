// Package ots anchors the transparency log's checkpoints in OpenTimestamps
// (K4c-3a, Stage 1a Prompt 35; plan 43.6 / F-16; docs/TRANSPARENCY.md
// § Anchors): a calendar commits to a digest at once (a pending
// attestation) and, once its aggregate is in a Bitcoin block, answers the
// path to that block header (a complete proof). Anyone runs `ots verify` on
// the .ots file; nobody has to approve it.
//
// None of the format is ours (D-P35-1): the .ots file, the operations, the
// commitment a calendar is asked for and the serialisation are
// github.com/nbd-wtf/opentimestamps's. What is ours is the HTTP (status
// codes, a size cap, no redirects, the caller's timeout), the rule that a
// pending attestation is completed only by the calendar it names when that
// calendar is the one configured, and a recover around the library (it
// panics on operations it does not implement). Nothing here verifies a
// proof against Bitcoin: that is `ots verify`'s job.
package ots

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	opentimestamps "github.com/nbd-wtf/opentimestamps"
)

// MaxProof bounds a calendar's answer and a stored proof (migration 106).
const MaxProof = 64 << 10

// The ways a calendar call fails; every one wraps one of these.
var (
	// ErrUnreachable: no answer, a timeout, a 5xx.
	ErrUnreachable = errors.New("ots: the calendar did not answer")
	// ErrRefused: a 4xx (other than a pending timestamp's 404).
	ErrRefused = errors.New("ots: the calendar refused the request")
	// ErrBadAnswer: a 200 that is not an OpenTimestamps timestamp of what
	// was asked, or larger than MaxProof.
	ErrBadAnswer = errors.New("ots: the calendar's answer is not a timestamp")
	// ErrPending: the calendar has not put it in a Bitcoin block yet.
	ErrPending = errors.New("ots: not attested in Bitcoin yet")
)

// Client talks to calendars. HC should not follow redirects (NewClient's
// does not): a calendar answers, it does not send the log elsewhere.
type Client struct {
	HC        *http.Client
	UserAgent string
}

// NewClient is a Client whose HTTP client follows no redirect; the caller
// bounds each call with its context.
func NewClient(userAgent string) *Client {
	return &Client{UserAgent: userAgent, HC: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

// Submit sends digest to calendar (POST <calendar>/digest) and returns the
// .ots file of its answer: the file header naming digest, then the
// calendar's timestamp exactly as it sent it — parsed by the library first,
// so an answer that is not a timestamp is ErrBadAnswer.
func (c *Client) Submit(ctx context.Context, calendar string, digest [32]byte) ([]byte, error) {
	body, status, err := c.call(ctx, http.MethodPost, strings.TrimSuffix(calendar, "/")+"/digest", digest[:])
	if err != nil {
		return nil, err
	}
	if err := classify(status); err != nil {
		return nil, err
	}
	file, err := fileOf(digest[:], body)
	if err != nil {
		return nil, err
	}
	if len(file) > MaxProof {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadAnswer, len(file))
	}
	return file, nil
}

// Upgrade asks calendar to complete every pending attestation of proof that
// names it (GET <calendar>/timestamp/<commitment hex>, the commitment the
// library computes at that attestation) and returns the proof with each
// answered one replaced by the calendar's path to a Bitcoin block header.
// complete = the result has a Bitcoin attestation. ErrPending when the
// calendar has nothing yet (404) — the proof is then returned unchanged. A
// pending attestation naming another URL is never asked (a calendar must
// not send the log to an address it chose).
func (c *Client) Upgrade(ctx context.Context, calendar string, proof []byte) (out []byte, complete bool, err error) {
	f, err := parse(proof)
	if err != nil {
		return nil, false, err
	}
	want := strings.TrimSuffix(calendar, "/")
	var kept []opentimestamps.Sequence
	var upgraded int
	for _, seq := range f.Sequences {
		att := seq.GetAttestation()
		if att.CalendarServerURL == "" || strings.TrimSuffix(att.CalendarServerURL, "/") != want {
			kept = append(kept, seq)
			continue
		}
		commitment, err := compute(seq, f.Digest)
		if err != nil {
			return nil, false, err
		}
		body, status, err := c.call(ctx, http.MethodGet, want+"/timestamp/"+hex.EncodeToString(commitment), nil)
		if err != nil {
			return nil, false, err
		}
		if status == http.StatusNotFound {
			kept = append(kept, seq)
			continue
		}
		if err := classify(status); err != nil {
			return nil, false, err
		}
		tail, err := fileOf(commitment, body)
		if err != nil {
			return nil, false, err
		}
		tf, err := parse(tail)
		if err != nil {
			return nil, false, err
		}
		for _, t := range tf.Sequences {
			// The library's own splice (UpgradeSequence): the path to the
			// pending attestation, then the calendar's path from there.
			kept = append(kept, append(slices.Clone(seq[:len(seq)-1]), t...))
		}
		upgraded++
	}
	if upgraded == 0 {
		return proof, false, ErrPending
	}
	out, err = serialize(&opentimestamps.File{Digest: f.Digest, Sequences: kept})
	if err != nil {
		return nil, false, err
	}
	if len(out) > MaxProof {
		return nil, false, fmt.Errorf("%w: %d bytes", ErrBadAnswer, len(out))
	}
	done, err := Complete(out)
	return out, done, err
}

// Complete reports whether proof has a Bitcoin block header attestation.
func Complete(proof []byte) (bool, error) {
	f, err := parse(proof)
	if err != nil {
		return false, err
	}
	return len(f.GetBitcoinAttestedSequences()) > 0, nil
}

// Combine merges proofs of the same digest into one .ots file: every
// distinct path of every proof (the library serialises the tree).
func Combine(proofs ...[]byte) ([]byte, error) {
	if len(proofs) == 0 {
		return nil, errors.New("ots: nothing to combine")
	}
	var all *opentimestamps.File
	for _, p := range proofs {
		f, err := parse(p)
		if err != nil {
			return nil, err
		}
		if all == nil {
			all = &opentimestamps.File{Digest: f.Digest}
		} else if !bytes.Equal(all.Digest, f.Digest) {
			return nil, errors.New("ots: the proofs are of different digests")
		}
		for _, s := range f.Sequences {
			if !slices.ContainsFunc(all.Sequences, func(o opentimestamps.Sequence) bool {
				return slices.CompareFunc(o, s, opentimestamps.CompareInstructions) == 0
			}) {
				all.Sequences = append(all.Sequences, s)
			}
		}
	}
	return serialize(all)
}

// Attestation is one path's end, as `ots info` would say it.
type Attestation struct {
	// Calendar is a pending attestation's calendar ("" for a Bitcoin one).
	Calendar string
	// BitcoinHeight is a Bitcoin block header attestation's height (0 for a
	// pending one).
	BitcoinHeight uint64
}

// Describe is a proof's digest and the end of each of its paths.
func Describe(proof []byte) (digest []byte, atts []Attestation, err error) {
	f, err := parse(proof)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range f.Sequences {
		a := s.GetAttestation()
		atts = append(atts, Attestation{Calendar: a.CalendarServerURL, BitcoinHeight: a.BitcoinBlockHeight})
	}
	return f.Digest, atts, nil
}

// call does one request: the body (at most MaxProof bytes) and the status.
func (c *Client) call(ctx context.Context, method, url string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	req.Header.Set("Accept", "application/vnd.opentimestamps.v1")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HC
	if hc == nil {
		hc = NewClient("").HC
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxProof+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	if len(b) > MaxProof {
		return nil, 0, fmt.Errorf("%w: more than %d bytes", ErrBadAnswer, MaxProof)
	}
	return b, resp.StatusCode, nil
}

func classify(status int) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status >= 500:
		return fmt.Errorf("%w: HTTP %d", ErrUnreachable, status)
	default:
		return fmt.Errorf("%w: HTTP %d", ErrRefused, status)
	}
}

// fileOf is the .ots file of a calendar's timestamp of digest: the header
// the library writes for a file with no paths yet, then the calendar's bytes.
func fileOf(digest, timestamp []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, fmt.Errorf("%w: a commitment of %d bytes, not a SHA-256", ErrBadAnswer, len(digest))
	}
	head, err := serialize(&opentimestamps.File{Digest: digest})
	if err != nil {
		return nil, err
	}
	file := append(head, timestamp...)
	f, err := parse(file)
	if err != nil {
		return nil, err
	}
	if len(f.Sequences) == 0 {
		return nil, fmt.Errorf("%w: no attestation", ErrBadAnswer)
	}
	for _, seq := range f.Sequences {
		if a := seq.GetAttestation(); a.CalendarServerURL == "" && a.BitcoinBlockHeight == 0 {
			return nil, fmt.Errorf("%w: a path that ends in no attestation", ErrBadAnswer)
		}
	}
	return file, nil
}

// parse is the library's ReadFromFile, a panic or an error an ErrBadAnswer.
func parse(proof []byte) (f *opentimestamps.File, err error) {
	defer func() {
		if p := recover(); p != nil {
			f, err = nil, fmt.Errorf("%w: %v", ErrBadAnswer, p)
		}
	}()
	f, err = opentimestamps.ReadFromFile(proof)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadAnswer, err)
	}
	return f, nil
}

// compute is the message at a path's end (the library's Compute).
func compute(seq opentimestamps.Sequence, digest []byte) (out []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("%w: %v", ErrBadAnswer, p)
		}
	}()
	out, _ = seq.Compute(digest)
	return out, nil
}

// serialize is the library's SerializeToFile.
func serialize(f *opentimestamps.File) (out []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("%w: %v", ErrBadAnswer, p)
		}
	}()
	return f.SerializeToFile(), nil
}
