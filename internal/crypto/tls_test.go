package crypto

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultTLSConfig(t *testing.T) {
	cfg := DefaultTLSConfig()

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want %d", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.MaxVersion != tls.VersionTLS13 {
		t.Errorf("MaxVersion = %d, want %d", cfg.MaxVersion, tls.VersionTLS13)
	}
	if cfg.CommonName != "localhost" {
		t.Errorf("CommonName = %s, want localhost", cfg.CommonName)
	}
}

func TestBuildTLSConfig_AutoCert(t *testing.T) {
	cfg := DefaultTLSConfig()
	cfg.AutoCert = true

	tlsConfig, err := cfg.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if len(tlsConfig.Certificates) != 1 {
		t.Errorf("Expected 1 certificate, got %d", len(tlsConfig.Certificates))
	}

	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want %d", tlsConfig.MinVersion, tls.VersionTLS12)
	}
}

func TestBuildTLSConfig_NoCert(t *testing.T) {
	cfg := &TLSConfig{}

	_, err := cfg.BuildTLSConfig()
	if err == nil {
		t.Error("Expected error when no certificate configured")
	}
}

func TestGenerateCertFiles(t *testing.T) {
	cfg := DefaultTLSConfig()
	cfg.CommonName = "test.example.com"
	cfg.DNSNames = []string{"test.example.com", "localhost"}
	cfg.IPAddresses = []string{"127.0.0.1", "192.168.1.1"}

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")

	err := cfg.GenerateCertFiles(certPath, keyPath)
	if err != nil {
		t.Fatalf("GenerateCertFiles failed: %v", err)
	}

	// Verify files exist
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Error("Certificate file not created")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Error("Key file not created")
	}

	// Verify key file permissions (should be 0600)
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Failed to stat key file: %v", err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Errorf("Key file permissions = %o, want 0600", keyInfo.Mode().Perm())
	}

	// Load and verify the certificate
	loadCfg := &TLSConfig{
		CertFile: certPath,
		KeyFile:  keyPath,
	}

	tlsConfig, err := loadCfg.BuildTLSConfig()
	if err != nil {
		t.Fatalf("Failed to load generated cert: %v", err)
	}

	if len(tlsConfig.Certificates) != 1 {
		t.Errorf("Expected 1 certificate, got %d", len(tlsConfig.Certificates))
	}
}

func TestGetCertInfo(t *testing.T) {
	cfg := DefaultTLSConfig()
	cfg.AutoCert = true
	cfg.CommonName = "test.vaultaire.io"
	cfg.DNSNames = []string{"test.vaultaire.io", "localhost"}
	cfg.IPAddresses = []string{"127.0.0.1"}
	cfg.ValidDays = 30

	tlsConfig, err := cfg.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	info, err := GetCertInfo(tlsConfig)
	if err != nil {
		t.Fatalf("GetCertInfo failed: %v", err)
	}

	if info.IsExpired {
		t.Error("Newly generated cert should not be expired")
	}

	if info.DaysUntil < 29 || info.DaysUntil > 31 {
		t.Errorf("DaysUntil = %d, expected ~30", info.DaysUntil)
	}

	// Check DNS names
	found := false
	for _, dns := range info.DNSNames {
		if dns == "test.vaultaire.io" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DNS names %v should contain test.vaultaire.io", info.DNSNames)
	}

	t.Logf("Certificate info: Subject=%s, Expires=%s, DaysUntil=%d",
		info.Subject, info.NotAfter, info.DaysUntil)
}

func TestIsCertExpiringSoon(t *testing.T) {
	cfg := DefaultTLSConfig()
	cfg.AutoCert = true
	cfg.ValidDays = 10 // Short validity

	tlsConfig, err := cfg.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	// Should be expiring within 30 days
	expiring, err := IsCertExpiringSoon(tlsConfig, 30)
	if err != nil {
		t.Fatalf("IsCertExpiringSoon failed: %v", err)
	}
	if !expiring {
		t.Error("10-day cert should be expiring within 30 days")
	}

	// Should NOT be expiring within 5 days
	expiring, err = IsCertExpiringSoon(tlsConfig, 5)
	if err != nil {
		t.Fatalf("IsCertExpiringSoon failed: %v", err)
	}
	if expiring {
		t.Error("10-day cert should not be expiring within 5 days")
	}
}

func TestProductionTLSConfig(t *testing.T) {
	cfg := ProductionTLSConfig("/path/to/cert.pem", "/path/to/key.pem")

	if cfg.CertFile != "/path/to/cert.pem" {
		t.Errorf("CertFile = %s, want /path/to/cert.pem", cfg.CertFile)
	}
	if cfg.KeyFile != "/path/to/key.pem" {
		t.Errorf("KeyFile = %s, want /path/to/key.pem", cfg.KeyFile)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion should be TLS 1.2 for production")
	}
}

func TestTLSConfig_CipherSuites(t *testing.T) {
	cfg := DefaultTLSConfig()
	cfg.AutoCert = true

	tlsConfig, err := cfg.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	// Verify modern cipher suites are configured
	if len(tlsConfig.CipherSuites) == 0 {
		t.Error("No cipher suites configured")
	}

	// Check that we have AES-GCM suites
	hasAESGCM := false
	for _, suite := range tlsConfig.CipherSuites {
		if suite == tls.TLS_AES_256_GCM_SHA384 ||
			suite == tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384 {
			hasAESGCM = true
			break
		}
	}
	if !hasAESGCM {
		t.Error("Should have AES-GCM cipher suites")
	}

	// Verify curve preferences include X25519
	hasX25519 := false
	for _, curve := range tlsConfig.CurvePreferences {
		if curve == tls.X25519 {
			hasX25519 = true
			break
		}
	}
	if !hasX25519 {
		t.Error("Should prefer X25519 curve")
	}
}

func TestTLSConfig_PEMData(t *testing.T) {
	// First generate a cert to get PEM data
	cfg := DefaultTLSConfig()
	cfg.AutoCert = true

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")

	err := cfg.GenerateCertFiles(certPath, keyPath)
	if err != nil {
		t.Fatalf("GenerateCertFiles failed: %v", err)
	}

	// Read PEM data
	certPEM, _ := os.ReadFile(certPath)
	keyPEM, _ := os.ReadFile(keyPath)

	// Create config with PEM data
	pemCfg := &TLSConfig{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
	}

	tlsConfig, err := pemCfg.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig with PEM failed: %v", err)
	}

	if len(tlsConfig.Certificates) != 1 {
		t.Errorf("Expected 1 certificate, got %d", len(tlsConfig.Certificates))
	}
}
