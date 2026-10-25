// Package release signs and verifies release artefacts (WP-SUPPLY-1, Stage 1a
// Prompt 41 Part A.2; docs/RELEASES.md).
//
// The signature is a DETACHED signed note (golang.org/x/mod/sumdb/note — the
// format the transparency log's checkpoints use, so one key kind, one
// verifier library and the key ID in every signature): the text names the
// artefact, its SHA-256 and its size, and the note is signed with an Ed25519
// key named stored.cloud/release/v1. Why this and not minisign/signify: the
// note format is already the project's (translog-keygen, translog-verify,
// the published checkpoint verifier keys), its verifier is a few hundred
// lines of the Go module checksum database's own code, the key hash in the
// signature line names the key that signed, and a verifier that knows the
// log's keys knows these. A reader with nothing but release-sign verify
// -vkey <public key> <artefact> <sig> checks it offline.
//
//	stored.cloud/release/v1
//	name vaultaire-linux
//	sha256 <64 hex>
//	size <bytes>
//
//	— stored.cloud/release/v1 <base64 key hash + Ed25519 signature>
//
// The name in the statement is what the artefact was called when it was
// signed; verification checks the BYTES (sha256 and size) and reports the
// name — a download renamed by a browser still verifies.
package release

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/mod/sumdb/note"
)

// Name is the signing key's name and the statement's first line: the
// format's version. A key named otherwise is refused.
const Name = "stored.cloud/release/v1"

// Statement is what a signature says about an artefact.
type Statement struct {
	Name   string // the artefact's base name when it was signed
	SHA256 string // hex
	Size   int64
}

// ErrFormat: the signature file or its statement is not of this format.
var ErrFormat = errors.New("not a stored.cloud/release/v1 signature")

// ErrMismatch: the signature verifies but does not describe these bytes.
var ErrMismatch = errors.New("the artefact does not match its signature")

// GenerateKey makes a signing key and its verifier key, both named Name.
func GenerateKey(rand io.Reader) (skey, vkey string, err error) {
	return note.GenerateKey(rand, Name)
}

// NewSigner parses a signer key; a key named for anything but Name is
// refused (the error quotes nothing of the key).
func NewSigner(skey string) (note.Signer, error) {
	s, err := note.NewSigner(strings.TrimSpace(skey))
	if err != nil {
		return nil, errors.New("not a note signer key")
	}
	if s.Name() != Name {
		return nil, fmt.Errorf("the signer key is named %q, not %s", s.Name(), Name)
	}
	return s, nil
}

// NewVerifier parses a verifier key named Name.
func NewVerifier(vkey string) (note.Verifier, error) {
	v, err := note.NewVerifier(strings.TrimSpace(vkey))
	if err != nil {
		return nil, fmt.Errorf("not a note verifier key: %w", err)
	}
	if v.Name() != Name {
		return nil, fmt.Errorf("the verifier key is named %q, not %s", v.Name(), Name)
	}
	return v, nil
}

// Digest is an artefact's SHA-256 (hex) and size.
func Digest(r io.Reader) (sum string, size int64, err error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Text is the statement as signed.
func (s Statement) Text() string {
	return Name + "\nname " + s.Name + "\nsha256 " + s.SHA256 + "\nsize " + strconv.FormatInt(s.Size, 10) + "\n"
}

// ParseStatement reads a statement text strictly: exactly these four lines,
// in this order, a 64-hex sha256, a non-negative size, a name with no
// path separator or space.
func ParseStatement(text string) (Statement, error) {
	var st Statement
	sc := bufio.NewScanner(strings.NewReader(text))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 4 || lines[0] != Name {
		return st, fmt.Errorf("%w: %d lines, first %q", ErrFormat, len(lines), first(lines))
	}
	for i, key := range []string{"name", "sha256", "size"} {
		k, v, ok := strings.Cut(lines[i+1], " ")
		if !ok || k != key || v == "" {
			return st, fmt.Errorf("%w: line %d is not %q", ErrFormat, i+2, key)
		}
		switch key {
		case "name":
			if strings.ContainsAny(v, "/\\ \t") || v == "." || v == ".." {
				return st, fmt.Errorf("%w: name %q", ErrFormat, v)
			}
			st.Name = v
		case "sha256":
			if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
				return st, fmt.Errorf("%w: sha256 is not 64 lower-case hex", ErrFormat)
			}
			st.SHA256 = v
		case "size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != v {
				return st, fmt.Errorf("%w: size %q", ErrFormat, v)
			}
			st.Size = n
		}
	}
	return st, nil
}

func first(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// Sign reads the artefact at path and returns its detached signature.
func Sign(signer note.Signer, path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator's own artefact
	if err != nil {
		return nil, fmt.Errorf("read the artefact: %w", err)
	}
	defer func() { _ = f.Close() }()
	sum, size, err := Digest(f)
	if err != nil {
		return nil, fmt.Errorf("hash the artefact: %w", err)
	}
	st := Statement{Name: filepath.Base(path), SHA256: sum, Size: size}
	return note.Sign(&note.Note{Text: st.Text()}, signer)
}

// Verify checks that sig is a signature by v over a statement that describes
// the bytes of the artefact at path, and returns the statement. A signature
// by another key, a damaged or truncated signature, a statement of another
// format, or bytes that differ in one bit are errors.
func Verify(v note.Verifier, path string, sig []byte) (Statement, error) {
	st, err := Open(v, sig)
	if err != nil {
		return st, err
	}
	f, err := os.Open(path) // #nosec G304 -- the reader's own download
	if err != nil {
		return st, fmt.Errorf("read the artefact: %w", err)
	}
	defer func() { _ = f.Close() }()
	sum, size, err := Digest(f)
	if err != nil {
		return st, fmt.Errorf("hash the artefact: %w", err)
	}
	if sum != st.SHA256 || size != st.Size {
		return st, fmt.Errorf("%w: signed sha256 %s size %d, the file sha256 %s size %d", ErrMismatch, st.SHA256, st.Size, sum, size)
	}
	return st, nil
}

// Open verifies the signature alone and returns its statement (the digest
// to compare against bytes the caller already has).
func Open(v note.Verifier, sig []byte) (Statement, error) {
	if !bytes.HasPrefix(sig, []byte(Name+"\n")) {
		return Statement{}, ErrFormat
	}
	n, err := note.Open(sig, note.VerifierList(v))
	if err != nil {
		return Statement{}, fmt.Errorf("signature: %w", err)
	}
	if len(n.Sigs) == 0 {
		return Statement{}, errors.New("signature: no signature by the pinned key")
	}
	return ParseStatement(n.Text)
}
