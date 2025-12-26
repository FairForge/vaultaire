// internal/devops/dns_test.go
package devops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDNSManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewDNSManager(nil)
		assert.NotNil(t, manager)
		assert.Equal(t, "manual", manager.config.Provider)
		assert.Equal(t, 300, manager.config.DefaultTTL)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &DNSConfig{
			Provider:   "cloudflare",
			DefaultTTL: 600,
		}
		manager := NewDNSManager(config)
		assert.Equal(t, "cloudflare", manager.config.Provider)
		assert.Equal(t, 600, manager.config.DefaultTTL)
	})
}

func TestDNSManager_AddZone(t *testing.T) {
	manager := NewDNSManager(nil)

	t.Run("adds zone", func(t *testing.T) {
		zone, err := manager.AddZone("example.com")
		require.NoError(t, err)
		assert.Equal(t, "example.com", zone.Domain)
		assert.Empty(t, zone.Records)
	})

	t.Run("rejects empty domain", func(t *testing.T) {
		_, err := manager.AddZone("")
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_, _ = manager.AddZone("dup.com")
		_, err := manager.AddZone("dup.com")
		assert.Error(t, err)
	})
}

func TestDNSManager_GetZone(t *testing.T) {
	manager := NewDNSManager(nil)
	_, _ = manager.AddZone("get.example.com")

	t.Run("returns existing zone", func(t *testing.T) {
		zone := manager.GetZone("get.example.com")
		assert.NotNil(t, zone)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		zone := manager.GetZone("unknown.com")
		assert.Nil(t, zone)
	})
}

func TestDNSManager_ListZones(t *testing.T) {
	manager := NewDNSManager(nil)
	_, _ = manager.AddZone("list1.com")
	_, _ = manager.AddZone("list2.com")

	zones := manager.ListZones()
	assert.Len(t, zones, 2)
}

func TestDNSManager_AddRecord(t *testing.T) {
	manager := NewDNSManager(nil)
	_, _ = manager.AddZone("records.com")

	t.Run("adds record", func(t *testing.T) {
		err := manager.AddRecord("records.com", &DNSRecord{
			Name:  "records.com",
			Type:  RecordTypeA,
			Value: "1.2.3.4",
		})
		assert.NoError(t, err)
	})

	t.Run("sets default TTL", func(t *testing.T) {
		_ = manager.AddRecord("records.com", &DNSRecord{
			Name:  "www.records.com",
			Type:  RecordTypeCNAME,
			Value: "records.com",
		})
		records := manager.GetRecords("records.com")
		assert.Equal(t, 300, records[1].TTL)
	})

	t.Run("rejects nil record", func(t *testing.T) {
		err := manager.AddRecord("records.com", nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.AddRecord("records.com", &DNSRecord{
			Type:  RecordTypeA,
			Value: "1.2.3.4",
		})
		assert.Error(t, err)
	})

	t.Run("rejects empty value", func(t *testing.T) {
		err := manager.AddRecord("records.com", &DNSRecord{
			Name: "test.records.com",
			Type: RecordTypeA,
		})
		assert.Error(t, err)
	})

	t.Run("errors for unknown zone", func(t *testing.T) {
		err := manager.AddRecord("unknown.com", &DNSRecord{
			Name:  "unknown.com",
			Type:  RecordTypeA,
			Value: "1.2.3.4",
		})
		assert.Error(t, err)
	})
}

func TestDNSManager_GetRecords(t *testing.T) {
	manager := NewDNSManager(nil)
	_, _ = manager.AddZone("getrecords.com")
	_ = manager.AddRecord("getrecords.com", &DNSRecord{Name: "getrecords.com", Type: RecordTypeA, Value: "1.2.3.4"})

	t.Run("returns records", func(t *testing.T) {
		records := manager.GetRecords("getrecords.com")
		assert.Len(t, records, 1)
	})

	t.Run("returns nil for unknown zone", func(t *testing.T) {
		records := manager.GetRecords("unknown.com")
		assert.Nil(t, records)
	})
}

func TestDNSManager_GetRecordsByType(t *testing.T) {
	manager := NewDNSManager(nil)
	_, _ = manager.AddZone("bytype.com")
	_ = manager.AddRecord("bytype.com", &DNSRecord{Name: "bytype.com", Type: RecordTypeA, Value: "1.2.3.4"})
	_ = manager.AddRecord("bytype.com", &DNSRecord{Name: "bytype.com", Type: RecordTypeTXT, Value: "test"})
	_ = manager.AddRecord("bytype.com", &DNSRecord{Name: "bytype.com", Type: RecordTypeA, Value: "5.6.7.8"})

	t.Run("filters by type", func(t *testing.T) {
		aRecords := manager.GetRecordsByType("bytype.com", RecordTypeA)
		assert.Len(t, aRecords, 2)

		txtRecords := manager.GetRecordsByType("bytype.com", RecordTypeTXT)
		assert.Len(t, txtRecords, 1)
	})
}

func TestDNSManager_RemoveRecord(t *testing.T) {
	manager := NewDNSManager(nil)
	_, _ = manager.AddZone("remove.com")
	_ = manager.AddRecord("remove.com", &DNSRecord{Name: "remove.com", Type: RecordTypeA, Value: "1.2.3.4"})

	t.Run("removes record", func(t *testing.T) {
		err := manager.RemoveRecord("remove.com", "remove.com", RecordTypeA)
		assert.NoError(t, err)
		assert.Empty(t, manager.GetRecords("remove.com"))
	})

	t.Run("errors for unknown zone", func(t *testing.T) {
		err := manager.RemoveRecord("unknown.com", "test", RecordTypeA)
		assert.Error(t, err)
	})

	t.Run("errors for unknown record", func(t *testing.T) {
		_, _ = manager.AddZone("remove2.com")
		err := manager.RemoveRecord("remove2.com", "nonexistent", RecordTypeA)
		assert.Error(t, err)
	})
}

func TestDNSManager_SetupProductionDNS(t *testing.T) {
	manager := NewDNSManager(nil)

	err := manager.SetupProductionDNS("stored.ge", "1.2.3.4")
	require.NoError(t, err)

	t.Run("creates zone", func(t *testing.T) {
		zone := manager.GetZone("stored.ge")
		assert.NotNil(t, zone)
	})

	t.Run("creates A records", func(t *testing.T) {
		aRecords := manager.GetRecordsByType("stored.ge", RecordTypeA)
		assert.GreaterOrEqual(t, len(aRecords), 3) // root, api, dashboard
	})

	t.Run("creates CNAME for www", func(t *testing.T) {
		cnameRecords := manager.GetRecordsByType("stored.ge", RecordTypeCNAME)
		assert.Len(t, cnameRecords, 1)
		assert.Equal(t, "www.stored.ge", cnameRecords[0].Name)
	})

	t.Run("creates TXT records", func(t *testing.T) {
		txtRecords := manager.GetRecordsByType("stored.ge", RecordTypeTXT)
		assert.GreaterOrEqual(t, len(txtRecords), 2) // SPF, DMARC
	})

	t.Run("creates CAA record", func(t *testing.T) {
		caaRecords := manager.GetRecordsByType("stored.ge", RecordTypeCAA)
		assert.Len(t, caaRecords, 1)
	})
}

func TestDNSManager_GenerateZoneFile(t *testing.T) {
	manager := NewDNSManager(nil)
	_ = manager.SetupProductionDNS("zonefile.com", "1.2.3.4")

	t.Run("generates zone file", func(t *testing.T) {
		output, err := manager.GenerateZoneFile("zonefile.com")
		require.NoError(t, err)

		assert.Contains(t, output, "$ORIGIN zonefile.com.")
		assert.Contains(t, output, "$TTL 300")
		assert.Contains(t, output, "IN A")
		assert.Contains(t, output, "IN CNAME")
	})

	t.Run("errors for unknown zone", func(t *testing.T) {
		_, err := manager.GenerateZoneFile("unknown.com")
		assert.Error(t, err)
	})
}

func TestDNSManager_Lookups(t *testing.T) {
	manager := NewDNSManager(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// These tests do real DNS lookups - skip in CI if needed
	t.Run("LookupA resolves google.com", func(t *testing.T) {
		ips, err := manager.LookupA(ctx, "google.com")
		require.NoError(t, err)
		assert.NotEmpty(t, ips)
	})

	t.Run("LookupTXT resolves google.com", func(t *testing.T) {
		records, err := manager.LookupTXT(ctx, "google.com")
		require.NoError(t, err)
		assert.NotEmpty(t, records)
	})

	t.Run("LookupMX resolves google.com", func(t *testing.T) {
		records, err := manager.LookupMX(ctx, "google.com")
		require.NoError(t, err)
		assert.NotEmpty(t, records)
	})

	t.Run("LookupNS resolves google.com", func(t *testing.T) {
		records, err := manager.LookupNS(ctx, "google.com")
		require.NoError(t, err)
		assert.NotEmpty(t, records)
	})
}

func TestRecordTypes(t *testing.T) {
	assert.Equal(t, RecordType("A"), RecordTypeA)
	assert.Equal(t, RecordType("AAAA"), RecordTypeAAAA)
	assert.Equal(t, RecordType("CNAME"), RecordTypeCNAME)
	assert.Equal(t, RecordType("TXT"), RecordTypeTXT)
	assert.Equal(t, RecordType("MX"), RecordTypeMX)
	assert.Equal(t, RecordType("NS"), RecordTypeNS)
	assert.Equal(t, RecordType("CAA"), RecordTypeCAA)
}

func TestDefaultDNSConfig(t *testing.T) {
	config := DefaultDNSConfig()

	assert.Equal(t, "manual", config.Provider)
	assert.Equal(t, 300, config.DefaultTTL)
	assert.Equal(t, 10*time.Second, config.CheckTimeout)
	assert.Contains(t, config.Nameservers, "8.8.8.8:53")
	assert.Contains(t, config.Nameservers, "1.1.1.1:53")
}
