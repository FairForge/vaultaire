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
}

// Load is the manifest's rows, each validated as the entry it becomes; a
// duplicate (client, sha256) is refused (one entry per distinct pair).
func Load() ([]translog.ClientBuild, error) {
	return parse(manifestJSON)
}

func parse(raw []byte) ([]translog.ClientBuild, error) {
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("clientbuilds: manifest.json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("clientbuilds: manifest.json: trailing data")
	}
	seen := map[string]bool{}
	for _, b := range m.Clients {
		if _, err := translog.Encode(b, time.Now()); err != nil {
			return nil, fmt.Errorf("clientbuilds: manifest.json row %s %s: %w", b.Client, b.Version, err)
		}
		if seen[b.DedupKey()] {
			return nil, fmt.Errorf("clientbuilds: manifest.json: %s with sha256 %s is listed twice", b.Client, b.SHA256)
		}
		seen[b.DedupKey()] = true
	}
	return m.Clients, nil
}
