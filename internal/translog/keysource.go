package translog

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/mod/sumdb/note"
)

// Where the checkpoint signing key comes from — the master key's rules
// (internal/crypto/master_key_source.go, docs/ENCRYPTION.md): the systemd
// credential $CREDENTIALS_DIRECTORY/translog_signing_key (prod:
// LoadCredentialEncrypted=, TPM-sealed on the box), then the file named by
// TRANSLOG_SIGNING_KEY_FILE, then TRANSLOG_SIGNING_KEY. A source that is SET
// but unusable — unreadable, not a note signer key, a key named for another
// origin — is an error the server makes a refused boot. No source = no key:
// entries still append, no checkpoint is signed. Errors name the source,
// never a character of the key.

const (
	// CredentialSigningKey is the systemd credential name.
	CredentialSigningKey = "translog_signing_key"
	envSigningKey        = "TRANSLOG_SIGNING_KEY"

	SigningKeyFromCredential = "credential"
	SigningKeyFromFile       = "file"
	SigningKeyFromEnv        = "env"
)

// ErrSigningKeySource: a signing key source is set but cannot be used.
var ErrSigningKeySource = errors.New("translog signing key source unusable")

// SigningKey is what LoadSigningKey found.
type SigningKey struct {
	// Signer is nil when no source is set.
	Signer note.Signer
	// Source is credential, file or env ("" = none).
	Source string
	// Ignored names the lower-precedence sources also set (a Warn: a stale
	// copy of a key).
	Ignored []string
}

// LoadSigningKey resolves the signing key by precedence; getenv and readFile
// are os.Getenv and os.ReadFile outside tests.
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
			// The unit carries other credentials: next source.
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
		return SigningKey{}, nil
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
	// NewSigner's errors quote nothing of the key.
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
