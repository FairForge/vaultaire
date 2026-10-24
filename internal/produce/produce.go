// Package produce is the "what we can produce" page and the law-enforcement
// policy's one source of facts (plan 43.4 and Post-Launch row 6b, Trust lane
// item 2, contract rules 7 and 8): internal/api/landing/produce.json, parsed
// and validated here. Every row is a code fact under test — the account
// rows are the GDPR export's sections, the retention rows the retention
// job's policies, the backup periods the backup script's, every table and
// column named exists — in internal/api/produce_test.go, where the code the
// rows describe lives. Mode names and sentences are never in the file: the
// pages render modes.json through internal/modes.
package produce

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/modes"
)

// FlagLegalLEPages serves GET /legal/law-enforcement and
// /legal/what-we-can-produce on the canonical origin and the trust host.
// Default OFF (404 on both), read globally only: the owner opens them once
// counsel has written the policy's procedural sentences (F-36).
const FlagLegalLEPages = "legal_le_pages"

// AccountRow is one section of the account export: its JSON key, a label,
// and the fields it carries.
type AccountRow struct {
	Section string   `json:"section"`
	Label   string   `json:"label"`
	Fields  []string `json:"fields"`
}

// RetentionRow is one policy of the retention job: the table, the period in
// days, delete or scrub (and the scrubbed columns), the columns the rows
// hold that the page names, and the sentence.
type RetentionRow struct {
	Table  string   `json:"table"`
	Days   int      `json:"days"`
	Action string   `json:"action"`
	Scrub  []string `json:"scrub,omitempty"`
	Holds  []string `json:"holds"`
	What   string   `json:"what"`
}

// KeptRow is a table no retention policy prunes.
type KeptRow struct {
	Table string   `json:"table"`
	Holds []string `json:"holds"`
	What  string   `json:"what"`
}

// Backups are the database backups' lives, in days.
type Backups struct {
	OnBoxDays  int `json:"on_box_days"`
	OffBoxDays int `json:"off_box_days"`
}

// Column is one column of a table and what it holds.
type Column struct {
	Column string `json:"column"`
	What   string `json:"what"`
}

// PrivateSSEC is what an object row of a Private (SSE-C) bucket holds.
type PrivateSSEC struct {
	Table string   `json:"table"`
	Holds []Column `json:"holds"`
}

// Gated is a sentence true only while a flag is ON globally.
type Gated struct {
	RequiresFlag string `json:"requires_flag"`
	Text         string `json:"text"`
}

// File is produce.json.
type File struct {
	About       string         `json:"_about,omitempty"`
	Modes       []string       `json:"modes"`
	Account     []AccountRow   `json:"account"`
	Retention   []RetentionRow `json:"retention"`
	ExportsDays int            `json:"exports_days"`
	Kept        []KeptRow      `json:"kept"`
	Backups     Backups        `json:"backups"`
	PrivateSSEC PrivateSSEC    `json:"private_ssec"`
	AdminReads  Gated          `json:"admin_reads"`
	Builds      Gated          `json:"builds"`
	Caveat      string         `json:"caveat"`
}

// Validate is what makes the file loadable: the modes are modes.json's
// ids in order, every row complete, every sentence held to the mode
// table's copy rules (no forbidden word, no unqualified "cannot read").
func Validate(f File) error {
	all := modes.All().Modes
	if len(f.Modes) != len(all) {
		return fmt.Errorf("produce: %d modes, modes.json has %d", len(f.Modes), len(all))
	}
	for i, m := range all {
		if f.Modes[i] != m.ID {
			return fmt.Errorf("produce: mode %d is %q, modes.json says %q", i, f.Modes[i], m.ID)
		}
	}
	var texts []string
	for _, a := range f.Account {
		if a.Section == "" || a.Label == "" || len(a.Fields) == 0 {
			return fmt.Errorf("produce: account row %q incomplete", a.Section)
		}
		texts = append(texts, a.Label)
	}
	for _, r := range f.Retention {
		if r.Table == "" || r.Days <= 0 || r.What == "" || len(r.Holds) == 0 {
			return fmt.Errorf("produce: retention row %q incomplete", r.Table)
		}
		if r.Action != "delete" && r.Action != "scrub" || (r.Action == "scrub") != (len(r.Scrub) > 0) {
			return fmt.Errorf("produce: retention row %q: action %q with %d scrubbed columns", r.Table, r.Action, len(r.Scrub))
		}
		texts = append(texts, r.What)
	}
	for _, k := range f.Kept {
		if k.Table == "" || k.What == "" {
			return fmt.Errorf("produce: kept row %q incomplete", k.Table)
		}
		texts = append(texts, k.What)
	}
	if f.ExportsDays <= 0 || f.Backups.OnBoxDays <= 0 || f.Backups.OffBoxDays <= 0 {
		return fmt.Errorf("produce: a period is missing")
	}
	if f.PrivateSSEC.Table == "" || len(f.PrivateSSEC.Holds) == 0 {
		return fmt.Errorf("produce: private_ssec incomplete")
	}
	for _, c := range f.PrivateSSEC.Holds {
		texts = append(texts, c.What)
	}
	for _, g := range []Gated{f.AdminReads, f.Builds} {
		if g.RequiresFlag == "" || g.Text == "" {
			return fmt.Errorf("produce: a flag-gated sentence without its flag or text")
		}
		texts = append(texts, g.Text)
	}
	if f.Caveat == "" {
		return fmt.Errorf("produce: caveat missing")
	}
	texts = append(texts, f.Caveat)
	for _, t := range texts {
		if w := modes.ForbiddenWord(t); w != "" {
			return fmt.Errorf("produce: %q says %q", t, w)
		}
		if bad := modes.UnqualifiedCannotRead(t); len(bad) > 0 {
			return fmt.Errorf("produce: unqualified cannot-read: %q", strings.Join(bad, " | "))
		}
	}
	return nil
}

// Parse reads and validates a produce file.
func Parse(b []byte) (File, error) {
	var f File
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("produce: parse: %w", err)
	}
	if err := Validate(f); err != nil {
		return File{}, err
	}
	return f, nil
}

var (
	once sync.Once
	all  File
)

// All is the embedded file, parsed and validated once (a failure panics:
// the file is in the binary and its test runs in CI).
func All() File {
	once.Do(func() {
		f, err := Parse(landing.ProduceJSON())
		if err != nil {
			panic(err)
		}
		all = f
	})
	return all
}
