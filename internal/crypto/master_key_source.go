package crypto

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Where the master key comes from (WP-R8-1 follow-up A5). The install
// design of 2026-10-12 (docs/ENCRYPTION.md "Setting the master key"): the
// key is generated ON the box and sealed with `systemd-creds encrypt
// --with-key=tpm2+host`; the unit hands it over with
// LoadCredentialEncrypted=encryption_master_key:…, which systemd decrypts
// into $CREDENTIALS_DIRECTORY for the service alone. It is never typed into
// a laptop, a password manager or a chat, and it is not in .env.
//
// Precedence, per key: the credential, then <NAME>_FILE, then <NAME> in the
// environment. A source that is SET but unusable — a file that cannot be
// read, a value that is not 64 hex characters — is an error the caller
// turns into a refused boot: a typo must never mean "encryption off" while
// sse_enabled buckets exist. Errors name the source, never the value.

const (
	// CredentialMasterKey is the systemd credential name of
	// ENCRYPTION_MASTER_KEY ($CREDENTIALS_DIRECTORY/encryption_master_key).
	CredentialMasterKey = "encryption_master_key"
	// CredentialMasterKeyPrevious is ENCRYPTION_MASTER_KEY_PREVIOUS's.
	CredentialMasterKeyPrevious = "encryption_master_key_previous"

	MasterKeyFromCredential = "credential"
	MasterKeyFromFile       = "file"
	MasterKeyFromEnv        = "env"
)

// ErrMasterKeySource: a master key source is set but cannot be used.
var ErrMasterKeySource = errors.New("master key source unusable")

// MasterKeys is what LoadMasterKeys found: the two keys (hex, trimmed; ""
// when not set) and where each came from.
type MasterKeys struct {
	Master, Previous             string
	MasterSource, PreviousSource string
	// Ignored names the lower-precedence variables that were also set and
	// lost to a higher source (worth a Warn: a stale copy of a key).
	Ignored []string
}

// LoadMasterKeys reads ENCRYPTION_MASTER_KEY and ENCRYPTION_MASTER_KEY_PREVIOUS
// from the credential directory, a file or the environment. getenv and
// readFile are os.Getenv and os.ReadFile outside tests.
func LoadMasterKeys(getenv func(string) string, readFile func(string) ([]byte, error)) (MasterKeys, error) {
	var mk MasterKeys
	var err error
	mk.Master, mk.MasterSource, err = loadOneKey(getenv, readFile, "ENCRYPTION_MASTER_KEY", CredentialMasterKey, &mk.Ignored)
	if err != nil {
		return MasterKeys{}, err
	}
	mk.Previous, mk.PreviousSource, err = loadOneKey(getenv, readFile, "ENCRYPTION_MASTER_KEY_PREVIOUS", CredentialMasterKeyPrevious, &mk.Ignored)
	if err != nil {
		return MasterKeys{}, err
	}
	if mk.Previous != "" && mk.Master == "" {
		return MasterKeys{}, fmt.Errorf("%w: ENCRYPTION_MASTER_KEY_PREVIOUS is set (%s) but ENCRYPTION_MASTER_KEY is not", ErrMasterKeySource, mk.PreviousSource)
	}
	return mk, nil
}

// loadOneKey resolves one key by precedence. "" with no error = not set.
func loadOneKey(getenv func(string) string, readFile func(string) ([]byte, error), name, credential string, ignored *[]string) (string, string, error) {
	type candidate struct {
		source, label, path, value string
		fromFile                   bool
	}
	var cands []candidate
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		path := filepath.Join(dir, credential)
		raw, err := readFile(path)
		switch {
		case err == nil:
			cands = append(cands, candidate{source: MasterKeyFromCredential, label: "credential " + credential, value: string(raw)})
		case errors.Is(err, fs.ErrNotExist):
			// The unit carries other credentials, or not this one: next source.
		default:
			return "", "", fmt.Errorf("%w: %s: credential %s cannot be read: %w", ErrMasterKeySource, name, credential, errWithoutPath(err))
		}
	}
	if path := getenv(name + "_FILE"); path != "" {
		cands = append(cands, candidate{source: MasterKeyFromFile, label: name + "_FILE", path: path, fromFile: true})
	}
	if v := getenv(name); v != "" {
		cands = append(cands, candidate{source: MasterKeyFromEnv, label: name, value: v})
	}
	if len(cands) == 0 {
		return "", "", nil
	}
	chosen := cands[0]
	for _, c := range cands[1:] {
		*ignored = append(*ignored, c.label)
	}
	if chosen.fromFile {
		raw, err := readFile(chosen.path)
		if err != nil {
			return "", "", fmt.Errorf("%w: %s: the file it names cannot be read: %w", ErrMasterKeySource, chosen.label, errWithoutPath(err))
		}
		chosen.value = string(raw)
	}
	key := strings.TrimSpace(chosen.value)
	if err := validMasterKeyHex(key); err != nil {
		return "", "", fmt.Errorf("%w: %s (from the %s): %w", ErrMasterKeySource, name, chosen.source, err)
	}
	return key, chosen.source, nil
}

// validMasterKeyHex is parseMasterKey's check with errors that quote nothing
// of the value (encoding/hex's error names the offending character).
func validMasterKeyHex(h string) error {
	if h == "" {
		return errors.New("empty")
	}
	if len(h) != 64 {
		return fmt.Errorf("must be 64 hex characters (32 bytes), got %d characters", len(h))
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return errors.New("must be 64 hex characters (32 bytes): it contains a non-hex character")
		}
	}
	if _, err := parseMasterKey(h); err != nil {
		if strings.Contains(err.Error(), "all zeros") {
			return errors.New("is all zeros")
		}
		return errors.New("is not a valid master key")
	}
	return nil
}

// errWithoutPath keeps the reason of a file error (permission, not found)
// without repeating the path the caller already named.
func errWithoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
