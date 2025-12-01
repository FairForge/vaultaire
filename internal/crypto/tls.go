package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// TLSConfig holds TLS configuration options
type TLSConfig struct {
	// Certificate paths (for loading from files)
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`

	// Direct PEM data (alternative to files)
	CertPEM []byte `json:"-"`
	KeyPEM  []byte `json:"-"`

	// TLS version constraints
	MinVersion uint16 `json:"min_version,omitempty"` // Default: TLS 1.2
	MaxVersion uint16 `json:"max_version,omitempty"` // Default: TLS 1.3

	// Client authentication
	ClientAuth tls.ClientAuthType `json:"client_auth,omitempty"`
	ClientCAs  []string           `json:"client_cas,omitempty"` // Paths to client CA certs

	// Auto-generate self-signed cert for development
	AutoCert bool `json:"auto_cert,omitempty"`

	// Certificate details for auto-generation
	CommonName   string   `json:"common_name,omitempty"`
	Organization string   `json:"organization,omitempty"`
	DNSNames     []string `json:"dns_names,omitempty"`
	IPAddresses  []string `json:"ip_addresses,omitempty"`
	ValidDays    int      `json:"valid_days,omitempty"` // Default: 365
}

// DefaultTLSConfig returns secure defaults
func DefaultTLSConfig() *TLSConfig {
	return &TLSConfig{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		ClientAuth:   tls.NoClientCert,
		CommonName:   "localhost",
		Organization: "Vaultaire Development",
		DNSNames:     []string{"localhost"},
		IPAddresses:  []string{"127.0.0.1", "::1"},
		ValidDays:    365,
	}
}

// ProductionTLSConfig returns strict production settings
func ProductionTLSConfig(certFile, keyFile string) *TLSConfig {
	return &TLSConfig{
		CertFile:   certFile,
		KeyFile:    keyFile,
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		ClientAuth: tls.NoClientCert,
	}
}

// BuildTLSConfig creates a tls.Config from TLSConfig
func (c *TLSConfig) BuildTLSConfig() (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: c.MinVersion,
		MaxVersion: c.MaxVersion,
		ClientAuth: c.ClientAuth,

		// Prefer server cipher suites (more secure)
		PreferServerCipherSuites: true,

		// Modern cipher suites only
		CipherSuites: []uint16{
			// TLS 1.3 cipher suites (always enabled when TLS 1.3 is used)
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_CHACHA20_POLY1305_SHA256,

			// TLS 1.2 cipher suites (for compatibility)
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},

		// Prefer modern curves
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
			tls.CurveP384,
		},
	}

	// Load or generate certificate
	var cert tls.Certificate
	var err error

	if c.CertPEM != nil && c.KeyPEM != nil {
		cert, err = tls.X509KeyPair(c.CertPEM, c.KeyPEM)
	} else if c.CertFile != "" && c.KeyFile != "" {
		cert, err = tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	} else if c.AutoCert {
		cert, err = c.generateSelfSignedCert()
	} else {
		return nil, fmt.Errorf("no certificate configured: set CertFile/KeyFile, CertPEM/KeyPEM, or AutoCert")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	tlsConfig.Certificates = []tls.Certificate{cert}

	// Load client CAs if mTLS is enabled
	if c.ClientAuth >= tls.VerifyClientCertIfGiven && len(c.ClientCAs) > 0 {
		caCertPool := x509.NewCertPool()
		for _, caPath := range c.ClientCAs {
			caCert, err := os.ReadFile(caPath)
			if err != nil {
				return nil, fmt.Errorf("failed to read client CA %s: %w", caPath, err)
			}
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("failed to parse client CA %s", caPath)
			}
		}
		tlsConfig.ClientCAs = caCertPool
	}

	return tlsConfig, nil
}

// generateSelfSignedCert creates a self-signed certificate for development
func (c *TLSConfig) generateSelfSignedCert() (tls.Certificate, error) {
	// Generate private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to generate serial number: %w", err)
	}

	validDays := c.ValidDays
	if validDays <= 0 {
		validDays = 365
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   c.CommonName,
			Organization: []string{c.Organization},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(0, 0, validDays),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              c.DNSNames,
	}

	// Parse IP addresses
	for _, ipStr := range c.IPAddresses {
		if ip := net.ParseIP(ipStr); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}

	// Create certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// GenerateCertFiles generates self-signed cert and key files
func (c *TLSConfig) GenerateCertFiles(certPath, keyPath string) error {
	cert, err := c.generateSelfSignedCert()
	if err != nil {
		return err
	}

	// Write certificate
	certOut, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	defer func() { _ = certOut.Close() }()

	for _, certBytes := range cert.Certificate {
		if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certBytes}); err != nil {
			return fmt.Errorf("failed to write cert: %w", err)
		}
	}

	// Write private key
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer func() { _ = keyOut.Close() }()

	privKey, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("unexpected private key type")
	}

	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("failed to write key: %w", err)
	}

	return nil
}

// TLSInfo returns information about the certificate
type TLSInfo struct {
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	NotBefore    time.Time `json:"not_before"`
	NotAfter     time.Time `json:"not_after"`
	DNSNames     []string  `json:"dns_names"`
	IPAddresses  []string  `json:"ip_addresses"`
	SerialNumber string    `json:"serial_number"`
	IsExpired    bool      `json:"is_expired"`
	DaysUntil    int       `json:"days_until_expiry"`
}

// GetCertInfo returns information about the loaded certificate
func GetCertInfo(tlsConfig *tls.Config) (*TLSInfo, error) {
	if len(tlsConfig.Certificates) == 0 {
		return nil, fmt.Errorf("no certificates configured")
	}

	cert := tlsConfig.Certificates[0]
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("empty certificate")
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	var ipStrings []string
	for _, ip := range x509Cert.IPAddresses {
		ipStrings = append(ipStrings, ip.String())
	}

	now := time.Now()
	daysUntil := int(x509Cert.NotAfter.Sub(now).Hours() / 24)

	return &TLSInfo{
		Subject:      x509Cert.Subject.String(),
		Issuer:       x509Cert.Issuer.String(),
		NotBefore:    x509Cert.NotBefore,
		NotAfter:     x509Cert.NotAfter,
		DNSNames:     x509Cert.DNSNames,
		IPAddresses:  ipStrings,
		SerialNumber: x509Cert.SerialNumber.String(),
		IsExpired:    now.After(x509Cert.NotAfter),
		DaysUntil:    daysUntil,
	}, nil
}

// IsCertExpiringSoon checks if certificate expires within given days
func IsCertExpiringSoon(tlsConfig *tls.Config, days int) (bool, error) {
	info, err := GetCertInfo(tlsConfig)
	if err != nil {
		return false, err
	}
	return info.DaysUntil <= days, nil
}
