// internal/devops/ssl.go
package devops

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CertificateType represents the type of certificate
type CertificateType string

const (
	CertTypeSelfSigned  CertificateType = "self-signed"
	CertTypeLetsEncrypt CertificateType = "letsencrypt"
	CertTypeManual      CertificateType = "manual"
)

// CertificateInfo holds certificate metadata
type CertificateInfo struct {
	Domain       string          `json:"domain"`
	Type         CertificateType `json:"type"`
	NotBefore    time.Time       `json:"not_before"`
	NotAfter     time.Time       `json:"not_after"`
	Issuer       string          `json:"issuer"`
	Subject      string          `json:"subject"`
	SerialNumber string          `json:"serial_number"`
	CertPath     string          `json:"cert_path"`
	KeyPath      string          `json:"key_path"`
}

// IsExpired returns true if the certificate is expired
func (c *CertificateInfo) IsExpired() bool {
	return time.Now().After(c.NotAfter)
}

// IsExpiringSoon returns true if the certificate expires within the given duration
func (c *CertificateInfo) IsExpiringSoon(within time.Duration) bool {
	return time.Now().Add(within).After(c.NotAfter)
}

// DaysUntilExpiry returns the number of days until the certificate expires
func (c *CertificateInfo) DaysUntilExpiry() int {
	duration := time.Until(c.NotAfter)
	return int(duration.Hours() / 24)
}

// SSLConfig configures SSL certificate management
type SSLConfig struct {
	CertDir        string        `json:"cert_dir"`
	AutoRenew      bool          `json:"auto_renew"`
	RenewBefore    time.Duration `json:"renew_before"`
	ACMEEmail      string        `json:"acme_email"`
	ACMEDirectory  string        `json:"acme_directory"`
	PreferredChain string        `json:"preferred_chain"`
}

// DefaultSSLConfig returns sensible defaults
func DefaultSSLConfig() *SSLConfig {
	return &SSLConfig{
		CertDir:       "/etc/vaultaire/tls",
		AutoRenew:     true,
		RenewBefore:   30 * 24 * time.Hour, // 30 days
		ACMEDirectory: "https://acme-v02.api.letsencrypt.org/directory",
	}
}

// SSLManager manages SSL certificates
type SSLManager struct {
	config *SSLConfig
	certs  map[string]*CertificateInfo
	mu     sync.RWMutex
}

// NewSSLManager creates an SSL manager
func NewSSLManager(config *SSLConfig) *SSLManager {
	if config == nil {
		config = DefaultSSLConfig()
	}
	return &SSLManager{
		config: config,
		certs:  make(map[string]*CertificateInfo),
	}
}

// GenerateSelfSigned generates a self-signed certificate for development
func (m *SSLManager) GenerateSelfSigned(domain string, validFor time.Duration) (*CertificateInfo, error) {
	if domain == "" {
		return nil, errors.New("ssl: domain is required")
	}

	// Generate private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ssl: failed to generate private key: %w", err)
	}

	// Generate serial number
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("ssl: failed to generate serial number: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(validFor)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Vaultaire Development"},
			CommonName:   domain,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain, "localhost"},
	}

	// Create certificate
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("ssl: failed to create certificate: %w", err)
	}

	// Ensure cert directory exists
	if err := os.MkdirAll(m.config.CertDir, 0700); err != nil {
		return nil, fmt.Errorf("ssl: failed to create cert directory: %w", err)
	}

	// Write certificate
	certPath := filepath.Join(m.config.CertDir, domain+".crt")
	if err := writeCertFile(certPath, derBytes); err != nil {
		return nil, err
	}

	// Write private key
	keyPath := filepath.Join(m.config.CertDir, domain+".key")
	if err := writeKeyFile(keyPath, privateKey); err != nil {
		return nil, err
	}

	info := &CertificateInfo{
		Domain:       domain,
		Type:         CertTypeSelfSigned,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		Issuer:       "Vaultaire Development",
		Subject:      domain,
		SerialNumber: serialNumber.String(),
		CertPath:     certPath,
		KeyPath:      keyPath,
	}

	m.mu.Lock()
	m.certs[domain] = info
	m.mu.Unlock()

	return info, nil
}

// writeCertFile writes a certificate to disk
func writeCertFile(path string, derBytes []byte) error {
	certFile, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("ssl: failed to create cert file: %w", err)
	}

	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		_ = certFile.Close()
		return fmt.Errorf("ssl: failed to write certificate: %w", err)
	}

	if err := certFile.Close(); err != nil {
		return fmt.Errorf("ssl: failed to close cert file: %w", err)
	}

	return nil
}

// writeKeyFile writes a private key to disk
func writeKeyFile(path string, privateKey *ecdsa.PrivateKey) error {
	keyFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("ssl: failed to create key file: %w", err)
	}

	privBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		_ = keyFile.Close()
		return fmt.Errorf("ssl: failed to marshal private key: %w", err)
	}

	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}); err != nil {
		_ = keyFile.Close()
		return fmt.Errorf("ssl: failed to write private key: %w", err)
	}

	if err := keyFile.Close(); err != nil {
		return fmt.Errorf("ssl: failed to close key file: %w", err)
	}

	return nil
}

// LoadCertificate loads an existing certificate from disk
func (m *SSLManager) LoadCertificate(certPath, keyPath string) (*CertificateInfo, error) {
	// Read certificate
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("ssl: failed to read certificate: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("ssl: failed to parse certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ssl: failed to parse certificate: %w", err)
	}

	// Verify key exists
	if _, err := os.Stat(keyPath); err != nil {
		return nil, fmt.Errorf("ssl: key file not found: %w", err)
	}

	// Determine certificate type
	certType := CertTypeManual
	if cert.Issuer.CommonName == cert.Subject.CommonName {
		certType = CertTypeSelfSigned
	} else if containsString(cert.Issuer.Organization, "Let's Encrypt") {
		certType = CertTypeLetsEncrypt
	}

	domain := cert.Subject.CommonName
	if len(cert.DNSNames) > 0 {
		domain = cert.DNSNames[0]
	}

	info := &CertificateInfo{
		Domain:       domain,
		Type:         certType,
		NotBefore:    cert.NotBefore,
		NotAfter:     cert.NotAfter,
		Issuer:       cert.Issuer.CommonName,
		Subject:      cert.Subject.CommonName,
		SerialNumber: cert.SerialNumber.String(),
		CertPath:     certPath,
		KeyPath:      keyPath,
	}

	m.mu.Lock()
	m.certs[domain] = info
	m.mu.Unlock()

	return info, nil
}

// GetCertificate returns certificate info for a domain
func (m *SSLManager) GetCertificate(domain string) *CertificateInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.certs[domain]
}

// ListCertificates returns all managed certificates
func (m *SSLManager) ListCertificates() []*CertificateInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	certs := make([]*CertificateInfo, 0, len(m.certs))
	for _, cert := range m.certs {
		certs = append(certs, cert)
	}
	return certs
}

// CheckRenewalNeeded returns certificates that need renewal
func (m *SSLManager) CheckRenewalNeeded() []*CertificateInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var needsRenewal []*CertificateInfo
	for _, cert := range m.certs {
		if cert.IsExpiringSoon(m.config.RenewBefore) {
			needsRenewal = append(needsRenewal, cert)
		}
	}
	return needsRenewal
}

// DeleteCertificate removes a certificate
func (m *SSLManager) DeleteCertificate(domain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cert, exists := m.certs[domain]
	if !exists {
		return fmt.Errorf("ssl: certificate for %s not found", domain)
	}

	// Remove files
	if cert.CertPath != "" {
		_ = os.Remove(cert.CertPath)
	}
	if cert.KeyPath != "" {
		_ = os.Remove(cert.KeyPath)
	}

	delete(m.certs, domain)
	return nil
}

// ValidateCertKeyPair validates that a certificate and key match
func ValidateCertKeyPair(certPath, keyPath string) error {
	// Read certificate
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("ssl: failed to read certificate: %w", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return errors.New("ssl: failed to parse certificate PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("ssl: failed to parse certificate: %w", err)
	}

	// Read key
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("ssl: failed to read key: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return errors.New("ssl: failed to parse key PEM")
	}

	// Try parsing as different key types
	var keyPublic interface{}

	if key, err := x509.ParseECPrivateKey(keyBlock.Bytes); err == nil {
		keyPublic = &key.PublicKey
	} else if key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err == nil {
		keyPublic = &key.PublicKey
	} else if key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err == nil {
		switch k := key.(type) {
		case *ecdsa.PrivateKey:
			keyPublic = &k.PublicKey
		default:
			return errors.New("ssl: unsupported key type")
		}
	} else {
		return errors.New("ssl: failed to parse private key")
	}

	// Compare public keys
	certPublic, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ssl: certificate has non-ECDSA public key")
	}

	keyECDSA, ok := keyPublic.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ssl: key has non-ECDSA public key")
	}

	if !certPublic.Equal(keyECDSA) {
		return errors.New("ssl: certificate and key do not match")
	}

	return nil
}

// helper function
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
