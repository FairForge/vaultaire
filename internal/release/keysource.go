package release

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/mod/sumdb/note"
)

// Where the release signing key comes from — the master key's and the
// transparency log's rules (internal/translog/keysource.go): the systemd
// credential $CREDENTIALS_DIRECTORY/release_signing_key, then the file named
// by RELEASE_SIGNING_KEY_FILE, then RELEASE_SIGNING_KEY. A source that is
// SET but unusable — unreadable, not a note signer key, a key named for
// another purpose — is an error (the tool refuses); no source = no key: the
// sign verb refuses with a clear message and nothing else changes (dark
// until the owner's key ceremony, docs/RELEASES.md). Errors name the
// source, never a character of the key. The key NEVER lands in GitHub
// Actions or the repository: signing is an owner step.

const (
	// CredentialSigningKey is the systemd credential name.
	CredentialSigningKey = "release_signing_key"
	envSigningKey        = "RELEASE_SIGNING_KEY"

	SigningKeyFromCredential = "credential"
	SigningKeyFromFile       = "file"
	SigningKeyFromEnv        = "env"
)

// ErrSigningKeySource: a signing key source is set but cannot be used.
var ErrSigningKeySource = errors.New("release signing key source unusable")

// ErrNoSigningKey: no source is set.
var ErrNoSigningKey = errors.New("no release signing key: set RELEASE_SIGNING_KEY_FILE (or the credential release_signing_key) on the machine that holds the key — docs/RELEASES.md")

// SigningKey is what LoadSigningKey found.
type SigningKey struct {
	Signer  note.Signer
	Source  string
	Ignored []string // lower-precedence sources also set
}

// LoadSigningKey resolves the signing key by precedence; getenv and readFile
// are os.Getenv and os.ReadFile outside tests. No source set = ErrNoSigningKey.
func LoadSigningKey(getenv func(string) string, readFile func(string) ([]byte, error)) (SigningKey, error) {
	type candidate struct {
		source, label, path, value string
		fromFile                   bool
	}
	var cands []candidate
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		raw, err := readFile(filepath.Join(dir, CredentialSigningKey))
		switch {
		case err == nil:
			cands = append(cands, candidate{source: SigningKeyFromCredential, label: "credential " + CredentialSigningKey, value: string(raw)})
		case errors.Is(err, fs.ErrNotExist):
		default:
			return SigningKey{}, fmt.Errorf("%w: credential %s cannot be read: %w", ErrSigningKeySource, CredentialSigningKey, errWithoutPath(err))
		}
	}
	if path := getenv(envSigningKey + "_FILE"); path != "" {
		cands = append(cands, candidate{source: SigningKeyFromFile, label: envSigningKey + "_FILE", path: path, fromFile: true})
	}
	if v := getenv(envSigningKey); v != "" {
		cands = append(cands, candidate{source: SigningKeyFromEnv, label: envSigningKey, value: v})
	}
	if len(cands) == 0 {
		return SigningKey{}, ErrNoSigningKey
	}
	chosen := cands[0]
	var sk SigningKey
	for _, c := range cands[1:] {
		sk.Ignored = append(sk.Ignored, c.label)
	}
	if chosen.fromFile {
		raw, err := readFile(chosen.path)
		if err != nil {
			return SigningKey{}, fmt.Errorf("%w: %s: the file it names cannot be read: %w", ErrSigningKeySource, chosen.label, errWithoutPath(err))
		}
		chosen.value = string(raw)
	}
	s, err := NewSigner(strings.TrimSpace(chosen.value))
	if err != nil {
		return SigningKey{}, fmt.Errorf("%w: %s (from the %s): %w", ErrSigningKeySource, envSigningKey, chosen.source, err)
	}
	sk.Signer, sk.Source = s, chosen.source
	return sk, nil
}

func errWithoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
