// Package clientbuilds holds the reviewed manifest of client artefact
// releases (K4c-3b, Stage 1a Prompt 36; plan 43.6: "client build hashes …
// clients refuse an unlisted build"; docs/TRANSPARENCY.md § Client builds).
//
// manifest.json is the one place a client release is declared: one row per
// release — client (a closed list), version, the commit it builds from, the
// SHA-256 of the artefact as published, the pinned toolchain and the recipe
// script. A new row is a reviewed code change. The Deploy job rebuilds every
// row's artefact from its source commit and FAILS the deploy when a hash
// differs (scripts/repro-check-clients.sh), and the server appends one
// client_build entry per row not yet in the transparency log, at boot, like
// server_build (retried by the translog_checkpoint job while the log is
// frozen). No credential reaches CI or the box for this: the manifest is in
// the binary, and the hash came from the review.
package clientbuilds

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/translog"
)

//go:embed manifest.json
var manifestJSON []byte

type manifest struct {
	Clients []translog.ClientBuild `json:"clients"`
	// Withdrawn (RP78-10): releases that must never be deployed or trusted
	// again. A row and its client_build entry are never removed or
	// rewritten (the log is append-only, and a changed row would be a new
	// entry); a withdrawal is its own list beside them.
	Withdrawn []Withdrawal `json:"withdrawn,omitempty"`
}

// Withdrawal names a listed build that is refused: the Worker's deploy
// check (edge/s3-ingest/scripts/check-withdrawn.mjs) and translog-verify
// -client refuse its bytes.
type Withdrawal struct {
	Client      string `json:"client"`
	SHA256      string `json:"sha256"`
	Reason      string `json:"reason"`
	WithdrawnAt string `json:"withdrawn_at"`
}

// Load is the manifest's rows, each validated as the entry it becomes; a
// duplicate (client, sha256) is refused (one entry per distinct pair). A
// withdrawn row is still a row (its entry is in the log).
func Load() ([]translog.ClientBuild, error) {
	return parse(manifestJSON)
}

// Withdrawn reports whether the build (client, sha256) is withdrawn.
func Withdrawn(client, sha256 string) (Withdrawal, bool) {
	_, ws, err := parseAll(manifestJSON)
	if err != nil {
		return Withdrawal{}, false
	}
	for _, w := range ws {
		if w.Client == client && w.SHA256 == sha256 {
			return w, true
		}
	}
	return Withdrawal{}, false
}

// WithdrawnSHA reports whether a build of any client with this sha256 is
// withdrawn (the artefact's hash is all a verifier may have).
func WithdrawnSHA(sha256 string) (Withdrawal, bool) {
	_, ws, err := parseAll(manifestJSON)
	if err != nil {
		return Withdrawal{}, false
	}
	for _, w := range ws {
		if w.SHA256 == sha256 {
			return w, true
		}
	}
	return Withdrawal{}, false
}

func parse(raw []byte) ([]translog.ClientBuild, error) {
	rows, _, err := parseAll(raw)
	return rows, err
}

func parseAll(raw []byte) ([]translog.ClientBuild, []Withdrawal, error) {
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("clientbuilds: manifest.json: %w", err)
	}
	if dec.More() {
		return nil, nil, errors.New("clientbuilds: manifest.json: trailing data")
	}
	seen := map[string]bool{}
	for _, b := range m.Clients {
		if _, err := translog.Encode(b, time.Now()); err != nil {
			return nil, nil, fmt.Errorf("clientbuilds: manifest.json row %s %s: %w", b.Client, b.Version, err)
		}
		if seen[b.DedupKey()] {
			return nil, nil, fmt.Errorf("clientbuilds: manifest.json: %s with sha256 %s is listed twice", b.Client, b.SHA256)
		}
		seen[b.DedupKey()] = true
	}
	for _, w := range m.Withdrawn {
		if !seen[translog.ClientBuild{Client: w.Client, SHA256: w.SHA256}.DedupKey()] {
			return nil, nil, fmt.Errorf("clientbuilds: manifest.json: withdrawn %s %s is not listed", w.Client, w.SHA256)
		}
		if w.Reason == "" || w.WithdrawnAt == "" {
			return nil, nil, fmt.Errorf("clientbuilds: manifest.json: withdrawn %s %s needs a reason and a date", w.Client, w.SHA256)
		}
	}
	return m.Clients, m.Withdrawn, nil
}
