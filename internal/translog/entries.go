package translog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The entry kinds. Entries become PUBLIC (GET /translog/…, then
// trust.stored.cloud), so each kind is a typed struct with an allow-list of
// fields, and none of them — by name or by value — may name a tenant, a
// person, an e-mail, an IP address or an object
// (TestTranslog_EntriesNeverNameATenant). Entry is sealed: a new kind is a
// new struct in this file, with its allow-list and its checks.
const (
	// KindServerBuild: a server binary started on the box (TRUST.md item
	// 13; no fTPM quote yet) — one entry per distinct (git sha, binary
	// sha256), appended at boot.
	KindServerBuild = "server_build"
	// KindKeyCeremony: a key ceremony (F-14 master-key install, re-wrap,
	// escrow share rotation; F-19 recovery realm; this log's own key).
	KindKeyCeremony = "key_ceremony"

	// entryVersion is the body's "v": a change of any kind's fields is a
	// new version.
	entryVersion = 1
	// maxStatement bounds a ceremony's statement text (bytes).
	maxStatement = 2000
)

// The ceremonies a key_ceremony entry can record.
const (
	CeremonyMasterKeyInstall    = "master_key_install"
	CeremonyMasterKeyRewrap     = "master_key_rewrap"
	CeremonyEscrowShareRotation = "escrow_share_rotation"
	CeremonyRecoveryRealm       = "recovery_realm"
	CeremonyTranslogKeyInstall  = "translog_key_install"
)

var ceremonies = map[string]bool{
	CeremonyMasterKeyInstall: true, CeremonyMasterKeyRewrap: true, CeremonyEscrowShareRotation: true,
	CeremonyRecoveryRealm: true, CeremonyTranslogKeyInstall: true,
}

// Ceremonies lists the ceremony names, for a tool's usage text.
func Ceremonies() []string {
	return []string{CeremonyMasterKeyInstall, CeremonyMasterKeyRewrap, CeremonyEscrowShareRotation, CeremonyRecoveryRealm, CeremonyTranslogKeyInstall}
}

// Entry is one log entry of a known kind.
type Entry interface {
	// Kind is the entry's kind (translog_entries.kind and the body's "kind").
	Kind() string
	// DedupKey, when not empty, makes the entry appear once: a second
	// append of the same key returns the first entry's index.
	DedupKey() string
	validate() error
}

// ServerBuild is a server binary that started.
type ServerBuild struct {
	GitSHA       string `json:"git_sha"`       // api.BuildSHA (12 hex on a deploy; "dev" unstamped)
	BinarySHA256 string `json:"binary_sha256"` // SHA-256 of os.Executable(), 64 hex
	BuildTime    string `json:"build_time"`    // api.BuildDate, RFC 3339 or "unknown"
	GoVersion    string `json:"go_version"`    // runtime.Version()
}

// Kind implements Entry.
func (ServerBuild) Kind() string { return KindServerBuild }

// DedupKey: one entry per distinct (git sha, binary).
func (b ServerBuild) DedupKey() string {
	return KindServerBuild + ":" + b.GitSHA + ":" + b.BinarySHA256
}

var (
	reGitSHA = regexp.MustCompile(`^([0-9a-f]{7,40}|dev|unknown)$`)
	reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reGoVer  = regexp.MustCompile(`^(devel )?go[0-9][0-9A-Za-z.+-]{0,40}$`)
	reFinger = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func (b ServerBuild) validate() error {
	if !reGitSHA.MatchString(b.GitSHA) {
		return errors.New("git_sha: 7–40 lowercase hex, dev or unknown")
	}
	if !reSHA256.MatchString(b.BinarySHA256) {
		return errors.New("binary_sha256: 64 lowercase hex")
	}
	if b.BuildTime != "unknown" {
		if _, err := time.Parse(time.RFC3339, b.BuildTime); err != nil {
			return errors.New("build_time: RFC 3339 or unknown")
		}
	}
	if !reGoVer.MatchString(b.GoVersion) {
		return errors.New("go_version: a Go version string")
	}
	return nil
}

// KeyCeremony is a key ceremony. It names the key by fingerprint — never the
// key, a share or a participant.
type KeyCeremony struct {
	Ceremony       string `json:"ceremony"`        // one of Ceremonies()
	KeyFingerprint string `json:"key_fingerprint"` // "sha256:" + 64 hex
	Participants   int    `json:"participants"`    // how many people took part (1–20)
	Statement      string `json:"statement"`       // what was done, plain text (≤ 2000 bytes)
}

// Kind implements Entry.
func (KeyCeremony) Kind() string { return KindKeyCeremony }

// DedupKey: every ceremony is its own entry.
func (KeyCeremony) DedupKey() string { return "" }

func (c KeyCeremony) validate() error {
	if !ceremonies[c.Ceremony] {
		return fmt.Errorf("ceremony: one of %s", strings.Join(Ceremonies(), ", "))
	}
	if !reFinger.MatchString(c.KeyFingerprint) {
		return errors.New("key_fingerprint: sha256: followed by 64 lowercase hex")
	}
	if c.Participants < 1 || c.Participants > 20 {
		return errors.New("participants: 1–20")
	}
	return checkStatement(c.Statement)
}

// What a statement must not contain: the shapes that name a person, a tenant
// or an object. A false positive (a clock time written 12:30:00) is refused
// too; say it another way.
var identifyingText = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`@`), "an e-mail address"},
	{regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`), "an IPv4 address"},
	{regexp.MustCompile(`(?i)([0-9a-f]{0,4}:){2,}[0-9a-f]{0,4}`), "an IPv6 address"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "a UUID (tenant ids are UUIDs)"},
	{regexp.MustCompile(`(?i)\btenants?[-_:/=#][0-9a-z]`), "a tenant id"},
	{regexp.MustCompile(`(^|[\s"'(])t-[0-9A-Za-z_.-]+/`), "an object path (t-<tenant>/…)"},
}

func checkStatement(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("statement: empty")
	}
	if len(s) > maxStatement {
		return fmt.Errorf("statement: longer than %d bytes", maxStatement)
	}
	if !utf8.ValidString(s) {
		return errors.New("statement: not UTF-8")
	}
	for _, r := range s {
		if r != '\n' && !unicode.IsPrint(r) {
			return errors.New("statement: a control character")
		}
	}
	return checkText("statement", s)
}

func checkText(field, s string) error {
	for _, it := range identifyingText {
		if it.re.MatchString(s) {
			return fmt.Errorf("%s: looks like %s — the log is public", field, it.what)
		}
	}
	return nil
}

// Kinds lists every entry kind.
func Kinds() []string { return []string{KindServerBuild, KindKeyCeremony} }

// AllowedFields is a kind's allow-list: the JSON fields of its "entry".
func AllowedFields(kind string) []string {
	switch kind {
	case KindServerBuild:
		return []string{"git_sha", "binary_sha256", "build_time", "go_version"}
	case KindKeyCeremony:
		return []string{"ceremony", "key_fingerprint", "participants", "statement"}
	}
	return nil
}

type envelope struct {
	Kind  string `json:"kind"`
	V     int    `json:"v"`
	At    string `json:"at"`
	Entry Entry  `json:"entry"`
}

// Encode is the body of entry e recorded at at — the exact bytes hashed:
// {"kind":…,"v":1,"at":"<RFC 3339 UTC, seconds>","entry":{…}}. An entry that
// fails its checks is refused.
func Encode(e Entry, at time.Time) ([]byte, error) {
	if e == nil || reflect.ValueOf(e).Kind() != reflect.Struct {
		return nil, errors.New("translog: not an entry")
	}
	if err := e.validate(); err != nil {
		return nil, fmt.Errorf("translog: %s entry refused: %w", e.Kind(), err)
	}
	body, err := json.Marshal(envelope{Kind: e.Kind(), V: entryVersion, At: at.UTC().Truncate(time.Second).Format(time.RFC3339), Entry: e})
	if err != nil {
		return nil, fmt.Errorf("translog: encode %s: %w", e.Kind(), err)
	}
	if len(body) > MaxEntrySize {
		return nil, ErrEntryTooLarge
	}
	return body, nil
}

// Decoded is a body read back.
type Decoded struct {
	Kind  string
	At    time.Time
	Entry Entry
}

// Decode parses a body Encode wrote (a verifier's view of an entry).
func Decode(body []byte) (Decoded, error) {
	var raw struct {
		Kind  string          `json:"kind"`
		V     int             `json:"v"`
		At    string          `json:"at"`
		Entry json.RawMessage `json:"entry"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Decoded{}, fmt.Errorf("translog: decode: %w", err)
	}
	if raw.V != entryVersion {
		return Decoded{}, fmt.Errorf("translog: decode: version %d", raw.V)
	}
	at, err := time.Parse(time.RFC3339, raw.At)
	if err != nil {
		return Decoded{}, fmt.Errorf("translog: decode: at: %w", err)
	}
	d := Decoded{Kind: raw.Kind, At: at}
	switch raw.Kind {
	case KindServerBuild:
		var b ServerBuild
		err = json.Unmarshal(raw.Entry, &b)
		d.Entry = b
	case KindKeyCeremony:
		var c KeyCeremony
		err = json.Unmarshal(raw.Entry, &c)
		d.Entry = c
	default:
		return Decoded{}, fmt.Errorf("translog: decode: unknown kind %q", raw.Kind)
	}
	if err != nil {
		return Decoded{}, fmt.Errorf("translog: decode %s: %w", raw.Kind, err)
	}
	return d, nil
}
