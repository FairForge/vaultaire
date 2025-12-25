// internal/devops/ssl_test.go
package devops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSSLManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewSSLManager(nil)
		assert.NotNil(t, manager)
		assert.Equal(t, "/etc/vaultaire/tls", manager.config.CertDir)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &SSLConfig{
			CertDir:   "/custom/path",
			AutoRenew: false,
		}
		manager := NewSSLManager(config)
		assert.Equal(t, "/custom/path", manager.config.CertDir)
	})
}

func TestSSLManager_GenerateSelfSigned(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{CertDir: tempDir}
	manager := NewSSLManager(config)

	t.Run("generates certificate", func(t *testing.T) {
		info, err := manager.GenerateSelfSigned("test.local", 24*time.Hour)
		require.NoError(t, err)

		assert.Equal(t, "test.local", info.Domain)
		assert.Equal(t, CertTypeSelfSigned, info.Type)
		assert.False(t, info.IsExpired())
		assert.FileExists(t, info.CertPath)
		assert.FileExists(t, info.KeyPath)
	})

	t.Run("rejects empty domain", func(t *testing.T) {
		_, err := manager.GenerateSelfSigned("", 24*time.Hour)
		assert.Error(t, err)
	})
}

func TestSSLManager_LoadCertificate(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{CertDir: tempDir}
	manager := NewSSLManager(config)

	// Generate a certificate first
	generated, err := manager.GenerateSelfSigned("load.test", 24*time.Hour)
	require.NoError(t, err)

	// Create a new manager and load the certificate
	manager2 := NewSSLManager(config)

	t.Run("loads existing certificate", func(t *testing.T) {
		loaded, err := manager2.LoadCertificate(generated.CertPath, generated.KeyPath)
		require.NoError(t, err)

		assert.Equal(t, "load.test", loaded.Domain)
		assert.Equal(t, CertTypeSelfSigned, loaded.Type)
	})

	t.Run("errors on missing cert", func(t *testing.T) {
		_, err := manager2.LoadCertificate("/nonexistent/cert.pem", generated.KeyPath)
		assert.Error(t, err)
	})

	t.Run("errors on missing key", func(t *testing.T) {
		_, err := manager2.LoadCertificate(generated.CertPath, "/nonexistent/key.pem")
		assert.Error(t, err)
	})
}

func TestSSLManager_GetCertificate(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{CertDir: tempDir}
	manager := NewSSLManager(config)

	_, err := manager.GenerateSelfSigned("get.test", 24*time.Hour)
	require.NoError(t, err)

	t.Run("returns existing certificate", func(t *testing.T) {
		cert := manager.GetCertificate("get.test")
		assert.NotNil(t, cert)
		assert.Equal(t, "get.test", cert.Domain)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		cert := manager.GetCertificate("unknown.test")
		assert.Nil(t, cert)
	})
}

func TestSSLManager_ListCertificates(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{CertDir: tempDir}
	manager := NewSSLManager(config)

	_, _ = manager.GenerateSelfSigned("list1.test", 24*time.Hour)
	_, _ = manager.GenerateSelfSigned("list2.test", 24*time.Hour)

	certs := manager.ListCertificates()
	assert.Len(t, certs, 2)
}

func TestSSLManager_CheckRenewalNeeded(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{
		CertDir:     tempDir,
		RenewBefore: 30 * 24 * time.Hour,
	}
	manager := NewSSLManager(config)

	// Certificate expiring soon (in 7 days)
	_, _ = manager.GenerateSelfSigned("expiring.test", 7*24*time.Hour)

	// Certificate not expiring soon (in 60 days)
	_, _ = manager.GenerateSelfSigned("valid.test", 60*24*time.Hour)

	needsRenewal := manager.CheckRenewalNeeded()
	assert.Len(t, needsRenewal, 1)
	assert.Equal(t, "expiring.test", needsRenewal[0].Domain)
}

func TestSSLManager_DeleteCertificate(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{CertDir: tempDir}
	manager := NewSSLManager(config)

	info, _ := manager.GenerateSelfSigned("delete.test", 24*time.Hour)
	certPath := info.CertPath
	keyPath := info.KeyPath

	t.Run("deletes certificate", func(t *testing.T) {
		err := manager.DeleteCertificate("delete.test")
		assert.NoError(t, err)

		assert.Nil(t, manager.GetCertificate("delete.test"))
		assert.NoFileExists(t, certPath)
		assert.NoFileExists(t, keyPath)
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := manager.DeleteCertificate("unknown.test")
		assert.Error(t, err)
	})
}

func TestCertificateInfo_Expiry(t *testing.T) {
	t.Run("IsExpired returns false for valid cert", func(t *testing.T) {
		info := &CertificateInfo{
			NotAfter: time.Now().Add(24 * time.Hour),
		}
		assert.False(t, info.IsExpired())
	})

	t.Run("IsExpired returns true for expired cert", func(t *testing.T) {
		info := &CertificateInfo{
			NotAfter: time.Now().Add(-24 * time.Hour),
		}
		assert.True(t, info.IsExpired())
	})

	t.Run("IsExpiringSoon works correctly", func(t *testing.T) {
		info := &CertificateInfo{
			NotAfter: time.Now().Add(7 * 24 * time.Hour),
		}
		assert.True(t, info.IsExpiringSoon(30*24*time.Hour))
		assert.False(t, info.IsExpiringSoon(3*24*time.Hour))
	})

	t.Run("DaysUntilExpiry calculates correctly", func(t *testing.T) {
		info := &CertificateInfo{
			NotAfter: time.Now().Add(10 * 24 * time.Hour),
		}
		days := info.DaysUntilExpiry()
		assert.InDelta(t, 10, days, 1)
	})
}

func TestValidateCertKeyPair(t *testing.T) {
	tempDir := t.TempDir()
	config := &SSLConfig{CertDir: tempDir}
	manager := NewSSLManager(config)

	info, err := manager.GenerateSelfSigned("validate.test", 24*time.Hour)
	require.NoError(t, err)

	t.Run("validates matching pair", func(t *testing.T) {
		err := ValidateCertKeyPair(info.CertPath, info.KeyPath)
		assert.NoError(t, err)
	})

	t.Run("rejects mismatched pair", func(t *testing.T) {
		// Generate another certificate
		info2, _ := manager.GenerateSelfSigned("other.test", 24*time.Hour)

		// Try to validate with mismatched key
		err := ValidateCertKeyPair(info.CertPath, info2.KeyPath)
		assert.Error(t, err)
	})

	t.Run("rejects missing cert", func(t *testing.T) {
		err := ValidateCertKeyPair("/nonexistent/cert.pem", info.KeyPath)
		assert.Error(t, err)
	})

	t.Run("rejects missing key", func(t *testing.T) {
		err := ValidateCertKeyPair(info.CertPath, "/nonexistent/key.pem")
		assert.Error(t, err)
	})

	t.Run("rejects invalid PEM", func(t *testing.T) {
		invalidPath := filepath.Join(tempDir, "invalid.pem")
		err := os.WriteFile(invalidPath, []byte("not a pem"), 0600)
		require.NoError(t, err)

		err = ValidateCertKeyPair(invalidPath, info.KeyPath)
		assert.Error(t, err)
	})
}

func TestDefaultSSLConfig(t *testing.T) {
	config := DefaultSSLConfig()

	assert.Equal(t, "/etc/vaultaire/tls", config.CertDir)
	assert.True(t, config.AutoRenew)
	assert.Equal(t, 30*24*time.Hour, config.RenewBefore)
	assert.Contains(t, config.ACMEDirectory, "letsencrypt")
}
