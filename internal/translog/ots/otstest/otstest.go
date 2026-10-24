// Package otstest is an OpenTimestamps calendar for tests: POST /digest
// answers a pending attestation naming the calendar, GET /timestamp/<hex>
// answers 404 until Confirm, then a path to a Bitcoin block header
// attestation. Its answers are built with github.com/nbd-wtf/opentimestamps's
// types and serialiser, never by hand. Imported by tests only.
package otstest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	opentimestamps "github.com/nbd-wtf/opentimestamps"
)

// Calendar is a running fake calendar.
type Calendar struct {
	URL string

	srv       *httptest.Server
	mu        sync.Mutex
	confirmed bool
	height    uint64
	// Submits and Gets count the requests it answered.
	Submits, Gets atomic.Int64
	// Down makes every request a 503.
	Down atomic.Bool
	// PendingURL, when set, is the URL its pending attestations name
	// instead of its own.
	PendingURL string
	// Reverse puts the "reverse" operation (0xf2) on the pending path — one
	// the library does not implement (computing the commitment panics).
	Reverse bool
	// known maps each answered commitment (hex) to its submission order, and
	// lostBefore makes the calendar answer 404 for ever for commitments
	// submitted before it — a calendar that lost them (WP-P36, RP48-2).
	known      map[string]int
	submitSeq  int
	lostBefore int
}

// Lose makes every commitment answered so far 404 for ever, Confirm or not:
// the calendar lost them (RP48-2).
func (c *Calendar) Lose() {
	c.mu.Lock()
	c.lostBefore = c.submitSeq
	c.mu.Unlock()
}

// New starts one; it stops when t ends.
func New(t testing.TB) *Calendar {
	t.Helper()
	c := &Calendar{height: 900000}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	c.URL = c.srv.URL
	return c
}

// Confirm makes every later GET /timestamp answer a Bitcoin attestation.
func (c *Calendar) Confirm() {
	c.mu.Lock()
	c.confirmed = true
	c.mu.Unlock()
}

// ConfirmAt is Confirm with the Bitcoin attestation's block height chosen.
func (c *Calendar) ConfirmAt(height uint64) {
	c.mu.Lock()
	c.confirmed = true
	c.height = height
	c.mu.Unlock()
}

func (c *Calendar) serve(w http.ResponseWriter, r *http.Request) {
	if c.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/digest":
		c.Submits.Add(1)
		d, err := io.ReadAll(io.LimitReader(r.Body, 65))
		if err != nil || len(d) == 0 || len(d) > 64 {
			http.Error(w, "digest too long", http.StatusBadRequest)
			return
		}
		url := c.URL
		if c.PendingURL != "" {
			url = c.PendingURL
		}
		nonce := make([]byte, 16)
		_, _ = rand.Read(nonce)
		seq := opentimestamps.Sequence{op(0xf0, nonce), op(0x08, nil), {Attestation: &opentimestamps.Attestation{CalendarServerURL: url}}}
		if c.Reverse {
			seq = append(opentimestamps.Sequence{{Operation: &opentimestamps.Operation{Name: "reverse", Tag: 0xf2}}}, seq...)
		} else {
			// The commitment the client will ask for: the sequence applied to
			// the digest (append nonce, then sha256).
			com := sha256.Sum256(append(append([]byte{}, d...), nonce...))
			c.mu.Lock()
			if c.known == nil {
				c.known = map[string]int{}
			}
			c.known[hex.EncodeToString(com[:])] = c.submitSeq
			c.submitSeq++
			c.mu.Unlock()
		}
		write(w, d, seq)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/timestamp/"):
		c.Gets.Add(1)
		commitment, err := hex.DecodeString(strings.TrimPrefix(r.URL.Path, "/timestamp/"))
		if err != nil {
			http.Error(w, "commitment must be hex-encoded bytes", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		ok, h := c.confirmed, c.height
		idx, knows := c.known[strings.TrimPrefix(r.URL.Path, "/timestamp/")]
		lost := knows && idx < c.lostBefore
		c.mu.Unlock()
		if lost || !ok {
			http.Error(w, "Pending confirmation in Bitcoin blockchain", http.StatusNotFound)
			return
		}
		x := make([]byte, 32)
		_, _ = rand.Read(x)
		seq := opentimestamps.Sequence{op(0xf1, x), op(0x08, nil), op(0x08, nil), {Attestation: &opentimestamps.Attestation{BitcoinBlockHeight: h}}}
		write(w, commitment, seq)
	default:
		http.NotFound(w, r)
	}
}

// op is one operation, by its OpenTimestamps tag (append 0xf0, prepend
// 0xf1, sha256 0x08): the library's Operation type, serialised by the library.
func op(tag byte, arg []byte) opentimestamps.Instruction {
	ops := map[byte]*opentimestamps.Operation{
		0xf0: {Name: "append", Tag: 0xf0, Binary: true, Apply: func(c, a []byte) []byte { return append(append([]byte{}, c...), a...) }},
		0xf1: {Name: "prepend", Tag: 0xf1, Binary: true, Apply: func(c, a []byte) []byte { return append(append([]byte{}, a...), c...) }},
		0x08: {Name: "sha256", Tag: 0x08, Apply: func(c, _ []byte) []byte { h := sha256.Sum256(c); return h[:] }},
	}
	return opentimestamps.Instruction{Operation: ops[tag], Argument: arg}
}

// write answers a calendar's timestamp of msg: the sequence's serialisation
// without the file header (what a calendar sends).
func write(w http.ResponseWriter, msg []byte, seq opentimestamps.Sequence) {
	b := (&opentimestamps.File{Digest: msg, Sequences: []opentimestamps.Sequence{seq}}).SerializeInstructionSequences()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}

// PendingProof is a whole .ots file of digest whose one path ends in a
// pending attestation naming url — what a pool's answer looks like once
// stored (WP-P36, RP48-1: Submit refuses such an answer, so a test that
// needs one builds it here, with the library's serialiser).
func PendingProof(digest [32]byte, url string) []byte {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	seq := opentimestamps.Sequence{op(0xf0, nonce), op(0x08, nil), {Attestation: &opentimestamps.Attestation{CalendarServerURL: url}}}
	return (&opentimestamps.File{Digest: digest[:], Sequences: []opentimestamps.Sequence{seq}}).SerializeToFile()
}
