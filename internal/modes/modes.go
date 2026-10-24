// Package modes is the bucket mode (keystone K3, plan Phase 41.0,
// docs/ARCHITECTURE_CONTRACT.md lines 1, 3 and 8): the three modes and their
// sentences from internal/api/landing/modes.json — the one table every
// surface renders from — and the one door through which a bucket's mode
// changes (SetBucket).
//
// A mode is a property of the surface, never a user's choice: Private (keys
// only on the customer's side), Standard (keys we hold), Public (plaintext).
// Vault is a tier, never a mode; "zero-knowledge" is said nowhere.
package modes

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

// FlagModesCopy turns on the mode copy generated from modes.json (K3 PR 3,
// default OFF, per tenant first): the dashboard's bucket labels and — read
// globally — the privacy page's per-mode table. The owner flips it globally
// after reading the privacy table (a legal page).
const FlagModesCopy = "modes_copy"

// Mode ids — the values of buckets.mode (migration 102). Their names and
// sentences are modes.json's.
const (
	Private  = "private"
	Standard = "standard"
	Public   = "public"
)

// Sentence is one sentence of the table. RequiresMasterKey marks a sentence
// true only while ENCRYPTION_MASTER_KEY is loaded ("keys we hold");
// WithoutMasterKey is what is true until then.
type Sentence struct {
	Text              string `json:"text"`
	RequiresMasterKey bool   `json:"requires_master_key,omitempty"`
	WithoutMasterKey  string `json:"without_master_key,omitempty"`
}

// Render is the sentence that is true now: the text, or its pre-key
// sentence when it needs a master key and none is loaded.
func (s Sentence) Render(masterKeyLoaded bool) string {
	if s.RequiresMasterKey && !masterKeyLoaded {
		return s.WithoutMasterKey
	}
	return s.Text
}

// Mode is one row of the table: its id, the customer-facing name, what the
// server holds (what we can read), the customer's sentence and how a bucket
// gets it.
type Mode struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	ServerHolds Sentence `json:"server_holds"`
	Customer    Sentence `json:"customer"`
	How         Sentence `json:"how"`
}

// Surface is a surface's default mode and the others it offers.
type Surface struct {
	Default string   `json:"default"`
	Also    []string `json:"also"`
	Note    string   `json:"note"`
}

// File is modes.json.
type File struct {
	About    string             `json:"_about,omitempty"`
	Modes    []Mode             `json:"modes"`
	Surfaces map[string]Surface `json:"surfaces"`
	// DeletedBytes is the privacy page's retention sentence for the bytes of
	// a deleted or overwritten file (RP37-8): the retired_blobs grace on
	// disk providers, dedup GC's for chunks, never freed on tape.
	DeletedBytes Sentence `json:"deleted_bytes"`
}

// forbiddenWords no sentence may say (contract line 8).
var forbiddenWords = []string{"zero-knowledge", "zero knowledge", "zero‑knowledge", "zeroknowledge"}

// ForbiddenWord returns the first forbidden word text contains, else "" —
// for other generated copy held to this table's rules (internal/produce).
func ForbiddenWord(text string) string {
	low := strings.ToLower(text)
	for _, w := range forbiddenWords {
		if strings.Contains(low, w) {
			return w
		}
	}
	return ""
}

// Validate is what makes the table loadable: exactly Private, Standard and
// Public in that order, every name and sentence non-empty, a pre-key
// sentence for every sentence that needs the key, no "Vault" mode, no
// forbidden word, every surface default a known mode.
func Validate(f File) error {
	want := []string{Private, Standard, Public}
	if len(f.Modes) != len(want) {
		return fmt.Errorf("modes.json: %d modes, want exactly %d (private, standard, public)", len(f.Modes), len(want))
	}
	for i, m := range f.Modes {
		if m.ID != want[i] {
			return fmt.Errorf("modes.json: mode %d is %q, want %q", i, m.ID, want[i])
		}
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("modes.json: mode %s has no name", m.ID)
		}
		if strings.Contains(strings.ToLower(m.Name), "vault") {
			return fmt.Errorf("modes.json: mode %s is named %q — Vault is a tier, never a mode", m.ID, m.Name)
		}
		for field, s := range map[string]Sentence{"server_holds": m.ServerHolds, "customer": m.Customer, "how": m.How} {
			if strings.TrimSpace(s.Text) == "" {
				return fmt.Errorf("modes.json: %s.%s is empty", m.ID, field)
			}
			if s.RequiresMasterKey && strings.TrimSpace(s.WithoutMasterKey) == "" {
				return fmt.Errorf("modes.json: %s.%s needs the master key but has no without_master_key sentence", m.ID, field)
			}
			for _, text := range []string{m.Name, s.Text, s.WithoutMasterKey} {
				for _, w := range forbiddenWords {
					if strings.Contains(strings.ToLower(text), w) {
						return fmt.Errorf("modes.json: %s.%s says %q", m.ID, field, w)
					}
				}
				if bad := UnqualifiedCannotRead(text); len(bad) > 0 {
					return fmt.Errorf("modes.json: %s.%s says we cannot read with neither \"at rest\" nor a client-side clause: %q", m.ID, field, bad[0])
				}
			}
		}
	}
	for _, w := range forbiddenWords {
		if strings.Contains(strings.ToLower(f.DeletedBytes.Text), w) {
			return fmt.Errorf("modes.json: deleted_bytes says %q", w)
		}
	}
	if bad := UnqualifiedCannotRead(f.DeletedBytes.Text); len(bad) > 0 {
		return fmt.Errorf("modes.json: deleted_bytes says we cannot read with neither \"at rest\" nor a client-side clause: %q", bad[0])
	}
	for name, s := range f.Surfaces {
		for _, id := range append([]string{s.Default}, s.Also...) {
			if id != Private && id != Standard && id != Public {
				return fmt.Errorf("modes.json: surface %s names unknown mode %q", name, id)
			}
		}
	}
	return nil
}

var (
	once   sync.Once
	parsed File
	byID   map[string]Mode
)

// All returns the parsed, validated table. The file is checked into the
// binary, so a parse or validation failure is a build defect and panics at
// first use (the prices.json rule).
func All() File {
	once.Do(func() {
		var f File
		if err := json.Unmarshal(landing.ModesJSON(), &f); err != nil {
			panic("landing/modes.json: " + err.Error())
		}
		if err := Validate(f); err != nil {
			panic(err.Error())
		}
		parsed, byID = f, map[string]Mode{}
		for _, m := range f.Modes {
			byID[m.ID] = m
		}
	})
	return parsed
}

// Get is one mode by id.
func Get(id string) (Mode, bool) {
	All()
	m, ok := byID[id]
	return m, ok
}

// DeveloperModes are the modes the developer surface offers — its default,
// then the others (modes.json: standard, private): what a bucket can be
// created in through the API (RP33-1 c). Public is the dashboard's (F-32).
func DeveloperModes() []string {
	sf := All().Surfaces["developer"]
	return append([]string{sf.Default}, sf.Also...)
}

// Name is the customer-facing name of a mode id ("" when unknown).
func Name(id string) string {
	m, _ := Get(id)
	return m.Name
}

// masterKeyLoaded: the process has ENCRYPTION_MASTER_KEY (set once at boot,
// cmd/vaultaire). Sentences marked requires_master_key render only while
// it is true.
var masterKeyLoaded atomic.Bool

// SetMasterKeyLoaded records whether the master key is loaded (boot).
func SetMasterKeyLoaded(v bool) { masterKeyLoaded.Store(v) }

// MasterKeyLoaded reports it.
func MasterKeyLoaded() bool { return masterKeyLoaded.Load() }

// cannotReadPhrases are the claims RP37-4 qualifies, and the clauses that
// make one true: SSE-C (the key reaches our server with every request, and
// we decrypt to serve it) is "can't read at rest", never "can't read";
// only a key kept on the customer's device is (contract line 8).
var (
	cannotReadPhrases = []string{"cannot read", "can't read", "can’t read", "cannot decrypt", "can't decrypt", "can’t decrypt"}
	cannotReadQualify = []string{"at rest", "on your device", "client-side", "before they are uploaded"}
)

// UnqualifiedCannotRead returns the sentences of text that say we cannot
// read (or decrypt) with neither "at rest" nor a client-side clause.
func UnqualifiedCannotRead(text string) []string {
	var bad []string
	for _, sentence := range strings.Split(text, ". ") {
		low := strings.ToLower(sentence)
		claims := false
		for _, p := range cannotReadPhrases {
			claims = claims || strings.Contains(low, p)
		}
		if !claims {
			continue
		}
		qualified := false
		for _, q := range cannotReadQualify {
			qualified = qualified || strings.Contains(low, q)
		}
		if !qualified {
			bad = append(bad, sentence)
		}
	}
	return bad
}
