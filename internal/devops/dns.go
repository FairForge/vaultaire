// internal/devops/dns.go
package devops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// RecordType represents DNS record types
type RecordType string

const (
	RecordTypeA     RecordType = "A"
	RecordTypeAAAA  RecordType = "AAAA"
	RecordTypeCNAME RecordType = "CNAME"
	RecordTypeTXT   RecordType = "TXT"
	RecordTypeMX    RecordType = "MX"
	RecordTypeNS    RecordType = "NS"
	RecordTypeCAA   RecordType = "CAA"
)

// DNSRecord represents a DNS record
type DNSRecord struct {
	Name     string     `json:"name"`
	Type     RecordType `json:"type"`
	Value    string     `json:"value"`
	TTL      int        `json:"ttl"`
	Priority int        `json:"priority,omitempty"` // For MX records
}

// DNSZone represents a DNS zone configuration
type DNSZone struct {
	Domain    string       `json:"domain"`
	Records   []*DNSRecord `json:"records"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// DNSConfig configures DNS management
type DNSConfig struct {
	Provider      string        `json:"provider"` // cloudflare, route53, manual
	DefaultTTL    int           `json:"default_ttl"`
	CheckTimeout  time.Duration `json:"check_timeout"`
	CheckInterval time.Duration `json:"check_interval"`
	Nameservers   []string      `json:"nameservers"`
}

// DefaultDNSConfig returns sensible defaults
func DefaultDNSConfig() *DNSConfig {
	return &DNSConfig{
		Provider:      "manual",
		DefaultTTL:    300,
		CheckTimeout:  10 * time.Second,
		CheckInterval: 60 * time.Second,
		Nameservers: []string{
			"8.8.8.8:53",
			"1.1.1.1:53",
		},
	}
}

// DNSManager manages DNS configuration
type DNSManager struct {
	config   *DNSConfig
	zones    map[string]*DNSZone
	resolver *net.Resolver
	mu       sync.RWMutex
}

// NewDNSManager creates a DNS manager
func NewDNSManager(config *DNSConfig) *DNSManager {
	if config == nil {
		config = DefaultDNSConfig()
	}

	// Create custom resolver if nameservers specified
	var resolver *net.Resolver
	if len(config.Nameservers) > 0 {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: config.CheckTimeout}
				return d.DialContext(ctx, "udp", config.Nameservers[0])
			},
		}
	}

	return &DNSManager{
		config:   config,
		zones:    make(map[string]*DNSZone),
		resolver: resolver,
	}
}

// AddZone adds a DNS zone
func (m *DNSManager) AddZone(domain string) (*DNSZone, error) {
	if domain == "" {
		return nil, errors.New("dns: domain is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.zones[domain]; exists {
		return nil, fmt.Errorf("dns: zone %s already exists", domain)
	}

	zone := &DNSZone{
		Domain:    domain,
		Records:   make([]*DNSRecord, 0),
		UpdatedAt: time.Now(),
	}

	m.zones[domain] = zone
	return zone, nil
}

// GetZone returns a zone by domain
func (m *DNSManager) GetZone(domain string) *DNSZone {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.zones[domain]
}

// ListZones returns all zones
func (m *DNSManager) ListZones() []*DNSZone {
	m.mu.RLock()
	defer m.mu.RUnlock()

	zones := make([]*DNSZone, 0, len(m.zones))
	for _, z := range m.zones {
		zones = append(zones, z)
	}
	return zones
}

// AddRecord adds a record to a zone
func (m *DNSManager) AddRecord(domain string, record *DNSRecord) error {
	if record == nil {
		return errors.New("dns: record is required")
	}
	if record.Name == "" {
		return errors.New("dns: record name is required")
	}
	if record.Value == "" {
		return errors.New("dns: record value is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	zone, exists := m.zones[domain]
	if !exists {
		return fmt.Errorf("dns: zone %s not found", domain)
	}

	if record.TTL == 0 {
		record.TTL = m.config.DefaultTTL
	}

	zone.Records = append(zone.Records, record)
	zone.UpdatedAt = time.Now()

	return nil
}

// GetRecords returns all records for a zone
func (m *DNSManager) GetRecords(domain string) []*DNSRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	zone, exists := m.zones[domain]
	if !exists {
		return nil
	}

	return zone.Records
}

// GetRecordsByType returns records of a specific type
func (m *DNSManager) GetRecordsByType(domain string, recordType RecordType) []*DNSRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	zone, exists := m.zones[domain]
	if !exists {
		return nil
	}

	var records []*DNSRecord
	for _, r := range zone.Records {
		if r.Type == recordType {
			records = append(records, r)
		}
	}
	return records
}

// RemoveRecord removes a record from a zone
func (m *DNSManager) RemoveRecord(domain, name string, recordType RecordType) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	zone, exists := m.zones[domain]
	if !exists {
		return fmt.Errorf("dns: zone %s not found", domain)
	}

	for i, r := range zone.Records {
		if r.Name == name && r.Type == recordType {
			zone.Records = append(zone.Records[:i], zone.Records[i+1:]...)
			zone.UpdatedAt = time.Now()
			return nil
		}
	}

	return fmt.Errorf("dns: record %s (%s) not found", name, recordType)
}

// LookupA performs an A record lookup
func (m *DNSManager) LookupA(ctx context.Context, hostname string) ([]string, error) {
	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	ips, err := resolver.LookupIP(ctx, "ip4", hostname)
	if err != nil {
		return nil, fmt.Errorf("dns: A lookup failed for %s: %w", hostname, err)
	}

	results := make([]string, len(ips))
	for i, ip := range ips {
		results[i] = ip.String()
	}
	return results, nil
}

// LookupAAAA performs an AAAA record lookup
func (m *DNSManager) LookupAAAA(ctx context.Context, hostname string) ([]string, error) {
	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	ips, err := resolver.LookupIP(ctx, "ip6", hostname)
	if err != nil {
		return nil, fmt.Errorf("dns: AAAA lookup failed for %s: %w", hostname, err)
	}

	results := make([]string, len(ips))
	for i, ip := range ips {
		results[i] = ip.String()
	}
	return results, nil
}

// LookupCNAME performs a CNAME record lookup
func (m *DNSManager) LookupCNAME(ctx context.Context, hostname string) (string, error) {
	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	cname, err := resolver.LookupCNAME(ctx, hostname)
	if err != nil {
		return "", fmt.Errorf("dns: CNAME lookup failed for %s: %w", hostname, err)
	}

	return cname, nil
}

// LookupTXT performs a TXT record lookup
func (m *DNSManager) LookupTXT(ctx context.Context, hostname string) ([]string, error) {
	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	records, err := resolver.LookupTXT(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("dns: TXT lookup failed for %s: %w", hostname, err)
	}

	return records, nil
}

// LookupMX performs an MX record lookup
func (m *DNSManager) LookupMX(ctx context.Context, hostname string) ([]*net.MX, error) {
	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	records, err := resolver.LookupMX(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("dns: MX lookup failed for %s: %w", hostname, err)
	}

	return records, nil
}

// LookupNS performs an NS record lookup
func (m *DNSManager) LookupNS(ctx context.Context, hostname string) ([]*net.NS, error) {
	resolver := m.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	records, err := resolver.LookupNS(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("dns: NS lookup failed for %s: %w", hostname, err)
	}

	return records, nil
}

// VerifyRecord verifies a DNS record matches expected value
func (m *DNSManager) VerifyRecord(ctx context.Context, record *DNSRecord) (bool, error) {
	switch record.Type {
	case RecordTypeA:
		ips, err := m.LookupA(ctx, record.Name)
		if err != nil {
			return false, err
		}
		for _, ip := range ips {
			if ip == record.Value {
				return true, nil
			}
		}
		return false, nil

	case RecordTypeTXT:
		records, err := m.LookupTXT(ctx, record.Name)
		if err != nil {
			return false, err
		}
		for _, r := range records {
			if r == record.Value {
				return true, nil
			}
		}
		return false, nil

	case RecordTypeCNAME:
		cname, err := m.LookupCNAME(ctx, record.Name)
		if err != nil {
			return false, err
		}
		// CNAME values often have trailing dots
		expected := record.Value
		if expected[len(expected)-1] != '.' {
			expected = expected + "."
		}
		return cname == expected || cname == record.Value, nil

	default:
		return false, fmt.Errorf("dns: verification not supported for %s records", record.Type)
	}
}

// SetupProductionDNS creates standard DNS records for production
func (m *DNSManager) SetupProductionDNS(domain, serverIP string) error {
	zone, err := m.AddZone(domain)
	if err != nil {
		return err
	}

	records := []*DNSRecord{
		// Root domain
		{Name: domain, Type: RecordTypeA, Value: serverIP},
		// WWW subdomain
		{Name: "www." + domain, Type: RecordTypeCNAME, Value: domain},
		// API subdomain
		{Name: "api." + domain, Type: RecordTypeA, Value: serverIP},
		// Dashboard subdomain
		{Name: "dashboard." + domain, Type: RecordTypeA, Value: serverIP},
		// CAA record for Let's Encrypt
		{Name: domain, Type: RecordTypeCAA, Value: "0 issue \"letsencrypt.org\""},
		// SPF record for email
		{Name: domain, Type: RecordTypeTXT, Value: "v=spf1 -all"},
		// DMARC record
		{Name: "_dmarc." + domain, Type: RecordTypeTXT, Value: "v=DMARC1; p=reject; adkim=s; aspf=s"},
	}

	zone.Records = append(zone.Records, records...)
	zone.UpdatedAt = time.Now()

	return nil
}

// GenerateZoneFile generates a BIND-style zone file
func (m *DNSManager) GenerateZoneFile(domain string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	zone, exists := m.zones[domain]
	if !exists {
		return "", fmt.Errorf("dns: zone %s not found", domain)
	}

	var output string
	output += fmt.Sprintf("; Zone file for %s\n", domain)
	output += fmt.Sprintf("; Generated at %s\n\n", zone.UpdatedAt.Format(time.RFC3339))
	output += fmt.Sprintf("$ORIGIN %s.\n", domain)
	output += fmt.Sprintf("$TTL %d\n\n", m.config.DefaultTTL)

	for _, r := range zone.Records {
		name := r.Name
		if name == domain {
			name = "@"
		} else if len(name) > len(domain) && name[len(name)-len(domain)-1:] == "."+domain {
			name = name[:len(name)-len(domain)-1]
		}

		switch r.Type {
		case RecordTypeMX:
			output += fmt.Sprintf("%-20s %d IN %-6s %d %s\n", name, r.TTL, r.Type, r.Priority, r.Value)
		case RecordTypeTXT:
			output += fmt.Sprintf("%-20s %d IN %-6s \"%s\"\n", name, r.TTL, r.Type, r.Value)
		default:
			output += fmt.Sprintf("%-20s %d IN %-6s %s\n", name, r.TTL, r.Type, r.Value)
		}
	}

	return output, nil
}
