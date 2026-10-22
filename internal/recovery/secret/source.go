// Package secret resolves a secret by the precedence the master key and the
// translog signing key use (internal/translog/keysource.go,
// docs/ENCRYPTION.md): the systemd credential $CREDENTIALS_DIRECTORY/<name>
// (prod: LoadCredentialEncrypted=), then the file named by <ENV>_FILE, then
// <ENV>. A source that is SET but unusable is an error the caller makes a
// refused boot; no source set is "none". Errors name the source, never a
// character of the secret. It has no dependencies, so cmd/vaultaire can
// load the recovery subject key without linking the realm.
package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

const (
	FromCredential = "credential"
	FromFile       = "file"
	FromEnv        = "env"
)

// ErrSource: a source is set but cannot be used.
var ErrSource = errors.New("secret source unusable")

// Value is what Load found.
type Value struct {
	// Bytes is the secret with surrounding whitespace trimmed; nil when no
	// source is set.
	Bytes []byte
	// Source is credential, file or env ("" = none).
	Source string
	// Ignored names the lower-precedence sources also set (a Warn: a stale
	// copy of a secret).
	Ignored []string
}

// Load resolves the secret named credential (the systemd credential) /
// env (the variable, and env+"_FILE"). getenv and readFile are os.Getenv and
// os.ReadFile outside tests.
func Load(getenv func(string) string, readFile func(string) ([]byte, error), credential, env string) (Value, error) {
	type candidate struct {
		source, label, path, value string
		fromFile                   bool
	}
	var cands []candidate
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		raw, err := readFile(filepath.Join(dir, credential))
		switch {
		case err == nil:
			cands = append(cands, candidate{source: FromCredential, label: "credential " + credential, value: string(raw)})
		case errors.Is(err, fs.ErrNotExist):
			// The unit carries other credentials: next source.
		default:
			return Value{}, fmt.Errorf("%w: credential %s cannot be read: %w", ErrSource, credential, withoutPath(err))
		}
	}
	if path := getenv(env + "_FILE"); path != "" {
		cands = append(cands, candidate{source: FromFile, label: env + "_FILE", path: path, fromFile: true})
	}
	if v := getenv(env); v != "" {
		cands = append(cands, candidate{source: FromEnv, label: env, value: v})
	}
	if len(cands) == 0 {
		return Value{}, nil
	}
	chosen := cands[0]
	var out Value
	for _, c := range cands[1:] {
		out.Ignored = append(out.Ignored, c.label)
	}
	if chosen.fromFile {
		raw, err := readFile(chosen.path)
		if err != nil {
			return Value{}, fmt.Errorf("%w: %s: the file it names cannot be read: %w", ErrSource, chosen.label, withoutPath(err))
		}
		chosen.value = string(raw)
	}
	v := strings.TrimSpace(chosen.value)
	if v == "" {
		return Value{}, fmt.Errorf("%w: %s is empty", ErrSource, chosen.label)
	}
	out.Bytes, out.Source = []byte(v), chosen.source
	return out, nil
}

func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
