package translog

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"
)

// TestTranslog_SigningKeySources: the master key's rules (docs/ENCRYPTION.md,
// internal/crypto/master_key_source.go) for the checkpoint signing key —
// the credential, then TRANSLOG_SIGNING_KEY_FILE, then TRANSLOG_SIGNING_KEY;
// a source that is set but unreadable or malformed is an error the server
// makes fatal; no source = no key, no error; errors never quote the key.
func TestTranslog_SigningKeySources(t *testing.T) {
	credKey, _, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	fileKey, _, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	envKey, _, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	credDir := "/run/credentials/vaultaire@8000.service"

	type src struct {
		env   map[string]string
		files map[string]string
		errs  map[string]error
	}
	load := func(s src) (SigningKey, error) {
		getenv := func(k string) string { return s.env[k] }
		readFile := func(p string) ([]byte, error) {
			if e, ok := s.errs[p]; ok {
				return nil, &fs.PathError{Op: "open", Path: p, Err: e}
			}
			if v, ok := s.files[p]; ok {
				return []byte(v), nil
			}
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return LoadSigningKey(getenv, readFile)
	}
	credPath := filepath.Join(credDir, CredentialSigningKey)

	// None set: no key, no error.
	sk, err := load(src{})
	require.NoError(t, err)
	assert.Nil(t, sk.Signer)
	assert.Empty(t, sk.Source)

	// All three: the credential wins, the other two are named as ignored.
	sk, err = load(src{
		env:   map[string]string{"CREDENTIALS_DIRECTORY": credDir, "TRANSLOG_SIGNING_KEY_FILE": "/etc/k", "TRANSLOG_SIGNING_KEY": envKey},
		files: map[string]string{credPath: credKey + "\n", "/etc/k": fileKey},
	})
	require.NoError(t, err)
	require.NotNil(t, sk.Signer)
	assert.Equal(t, SigningKeyFromCredential, sk.Source)
	assert.Equal(t, []string{"TRANSLOG_SIGNING_KEY_FILE", "TRANSLOG_SIGNING_KEY"}, sk.Ignored)
	assert.Equal(t, signerHash(t, credKey), sk.Signer.KeyHash())

	// A credential directory without this credential falls through to the file.
	sk, err = load(src{
		env:   map[string]string{"CREDENTIALS_DIRECTORY": credDir, "TRANSLOG_SIGNING_KEY_FILE": "/etc/k"},
		files: map[string]string{"/etc/k": fileKey},
	})
	require.NoError(t, err)
	assert.Equal(t, SigningKeyFromFile, sk.Source)
	assert.Equal(t, signerHash(t, fileKey), sk.Signer.KeyHash())

	// The variable alone.
	sk, err = load(src{env: map[string]string{"TRANSLOG_SIGNING_KEY": "  " + envKey + " "}})
	require.NoError(t, err)
	assert.Equal(t, SigningKeyFromEnv, sk.Source)

	// Set but unusable = an error, never "no key"; never a character of the key.
	fails := map[string]src{
		"credential unreadable": {env: map[string]string{"CREDENTIALS_DIRECTORY": credDir}, errs: map[string]error{credPath: fs.ErrPermission}},
		"file unreadable":       {env: map[string]string{"TRANSLOG_SIGNING_KEY_FILE": "/etc/k"}, errs: map[string]error{"/etc/k": fs.ErrPermission}},
		"file missing":          {env: map[string]string{"TRANSLOG_SIGNING_KEY_FILE": "/etc/k"}},
		"malformed env":         {env: map[string]string{"TRANSLOG_SIGNING_KEY": envKey[:len(envKey)-6] + "zzzzzz"}},
		"verifier key as env":   {env: map[string]string{"TRANSLOG_SIGNING_KEY": verifierOf(t)}},
		"other origin":          {env: map[string]string{"TRANSLOG_SIGNING_KEY": otherOriginKey(t)}},
		"empty credential":      {env: map[string]string{"CREDENTIALS_DIRECTORY": credDir}, files: map[string]string{credPath: "\n"}},
		"malformed file wins over a good env": {
			env:   map[string]string{"TRANSLOG_SIGNING_KEY_FILE": "/etc/k", "TRANSLOG_SIGNING_KEY": envKey},
			files: map[string]string{"/etc/k": "PRIVATE+KEY+garbage"},
		},
	}
	for name, s := range fails {
		_, err := load(s)
		require.Error(t, err, name)
		assert.True(t, errors.Is(err, ErrSigningKeySource), name)
		for _, k := range []string{credKey, fileKey, envKey} {
			secret := strings.SplitN(k, "+", 5)[4] // PRIVATE+KEY+<name>+<hash>+<key data, base64 — may hold "+">
			assert.NotContains(t, err.Error(), secret[:12], "%s: the error quotes a key", name)
		}
		assert.NotContains(t, err.Error(), "garbage", name)
	}
}

func signerHash(t *testing.T, skey string) uint32 {
	t.Helper()
	s, err := note.NewSigner(skey)
	require.NoError(t, err)
	return s.KeyHash()
}

func verifierOf(t *testing.T) string {
	_, v, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	return v
}

func otherOriginKey(t *testing.T) string {
	s, _, err := note.GenerateKey(rand.Reader, "example.com/log")
	require.NoError(t, err)
	return s
}
