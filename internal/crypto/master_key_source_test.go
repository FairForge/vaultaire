package crypto

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R8-1 follow-up A5 (Stage 1a Prompt 2A): the master key can come from a
// systemd credential (LoadCredentialEncrypted=, TPM-sealed on the box) or a
// file, not only the environment. Precedence credential > file > env; a
// credential or file that is set but unreadable or malformed refuses the
// boot (never "encryption silently off"); the source is named, never the
// value.
func TestMasterKey_FromCredentialDirectory_AndFile_PrecedenceAndFailLoud(t *testing.T) {
	keyCred := strings.Repeat("ab", 32)
	keyFile := strings.Repeat("cd", 32)
	keyEnv := strings.Repeat("ef", 32)
	prevCred := strings.Repeat("12", 32)

	dir := t.TempDir()
	credDir := filepath.Join(dir, "credentials")
	require.NoError(t, os.Mkdir(credDir, 0o700))
	write := func(path, body string) string {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	envOf := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("credential wins over file and env; whitespace trimmed", func(t *testing.T) {
		write(filepath.Join(credDir, CredentialMasterKey), keyCred+"\n")
		write(filepath.Join(credDir, CredentialMasterKeyPrevious), "  "+prevCred+"\r\n")
		t.Cleanup(func() {
			_ = os.Remove(filepath.Join(credDir, CredentialMasterKey))
			_ = os.Remove(filepath.Join(credDir, CredentialMasterKeyPrevious))
		})
		file := write(filepath.Join(dir, "key-file"), keyFile)
		mk, err := LoadMasterKeys(envOf(map[string]string{
			"CREDENTIALS_DIRECTORY":      credDir,
			"ENCRYPTION_MASTER_KEY_FILE": file,
			"ENCRYPTION_MASTER_KEY":      keyEnv,
		}), os.ReadFile)
		require.NoError(t, err)
		assert.Equal(t, keyCred, mk.Master)
		assert.Equal(t, MasterKeyFromCredential, mk.MasterSource)
		assert.Equal(t, prevCred, mk.Previous)
		assert.Equal(t, MasterKeyFromCredential, mk.PreviousSource)
		assert.ElementsMatch(t, []string{"ENCRYPTION_MASTER_KEY_FILE", "ENCRYPTION_MASTER_KEY"}, mk.Ignored,
			"the lower sources are named as ignored (the runbook removes them)")
	})

	t.Run("file wins over env; a credential directory without the key falls through", func(t *testing.T) {
		file := write(filepath.Join(dir, "key-file"), keyFile+"\n")
		mk, err := LoadMasterKeys(envOf(map[string]string{
			"CREDENTIALS_DIRECTORY":      credDir, // set (the unit has other credentials), no key in it
			"ENCRYPTION_MASTER_KEY_FILE": file,
			"ENCRYPTION_MASTER_KEY":      keyEnv,
		}), os.ReadFile)
		require.NoError(t, err)
		assert.Equal(t, keyFile, mk.Master)
		assert.Equal(t, MasterKeyFromFile, mk.MasterSource)
		assert.Empty(t, mk.Previous)
	})

	t.Run("env alone, as before", func(t *testing.T) {
		mk, err := LoadMasterKeys(envOf(map[string]string{
			"ENCRYPTION_MASTER_KEY":          keyEnv,
			"ENCRYPTION_MASTER_KEY_PREVIOUS": prevCred,
		}), os.ReadFile)
		require.NoError(t, err)
		assert.Equal(t, keyEnv, mk.Master)
		assert.Equal(t, MasterKeyFromEnv, mk.MasterSource)
		assert.Equal(t, prevCred, mk.Previous)
		assert.Equal(t, MasterKeyFromEnv, mk.PreviousSource)
	})

	t.Run("nothing set: encryption off, no error", func(t *testing.T) {
		mk, err := LoadMasterKeys(envOf(map[string]string{}), os.ReadFile)
		require.NoError(t, err)
		assert.Empty(t, mk.Master)
		assert.Empty(t, mk.MasterSource)
	})

	failures := map[string]struct {
		env      map[string]string
		readFile func(string) ([]byte, error)
		setup    func(t *testing.T)
	}{
		"file set but missing": {env: map[string]string{"ENCRYPTION_MASTER_KEY_FILE": filepath.Join(dir, "nope")}},
		"file malformed":       {env: map[string]string{"ENCRYPTION_MASTER_KEY_FILE": write(filepath.Join(dir, "bad"), "not-hex-"+keyEnv[8:])}},
		"file too short":       {env: map[string]string{"ENCRYPTION_MASTER_KEY_FILE": write(filepath.Join(dir, "short"), keyEnv[:62])}},
		"file empty":           {env: map[string]string{"ENCRYPTION_MASTER_KEY_FILE": write(filepath.Join(dir, "empty"), "\n")}},
		"credential unreadable": {
			env:      map[string]string{"CREDENTIALS_DIRECTORY": credDir},
			readFile: func(string) ([]byte, error) { return nil, fs.ErrPermission },
		},
		"credential malformed": {
			env: map[string]string{"CREDENTIALS_DIRECTORY": credDir, "ENCRYPTION_MASTER_KEY": keyEnv},
			setup: func(t *testing.T) {
				write(filepath.Join(credDir, CredentialMasterKey), "zz"+keyCred[2:])
				t.Cleanup(func() { _ = os.Remove(filepath.Join(credDir, CredentialMasterKey)) })
			},
		},
		"env malformed":                 {env: map[string]string{"ENCRYPTION_MASTER_KEY": "abc"}},
		"previous without a master key": {env: map[string]string{"ENCRYPTION_MASTER_KEY_PREVIOUS": prevCred}},
		"previous file unreadable":      {env: map[string]string{"ENCRYPTION_MASTER_KEY": keyEnv, "ENCRYPTION_MASTER_KEY_PREVIOUS_FILE": filepath.Join(dir, "nope")}},
	}
	for name, tc := range failures {
		t.Run("refused: "+name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			rf := tc.readFile
			if rf == nil {
				rf = os.ReadFile
			}
			_, err := LoadMasterKeys(envOf(tc.env), rf)
			require.Error(t, err, "a key source that is set but unusable refuses the boot")
			assert.True(t, errors.Is(err, ErrMasterKeySource), "%v", err)
			for _, secret := range []string{keyCred, keyFile, keyEnv, prevCred, keyCred[2:], keyEnv[8:]} {
				assert.NotContains(t, err.Error(), secret, "never the value")
			}
		})
	}
}
