package ha

import (
	"context"
	"testing"
	"time"
)

func TestNewGeoManager(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		config := DefaultGeoConfig()
		gm, err := NewGeoManager(config)
		if err != nil {
			t.Fatalf("NewGeoManager failed: %v", err)
		}
		if gm == nil {
			t.Fatal("GeoManager is nil")
		}
	})

	t.Run("nil config", func(t *testing.T) {
		_, err := NewGeoManager(nil)
		if err == nil {
			t.Error("Expected error for nil config")
		}
	})

	t.Run("missing primary", func(t *testing.T) {
		config := &GeoConfig{}
		_, err := NewGeoManager(config)
		if err == nil {
			t.Error("Expected error for missing primary region")
		}
	})
}

func TestDefaultGeoConfig(t *testing.T) {
	config := DefaultGeoConfig()

	if config.PrimaryRegion != RegionNYC {
		t.Errorf("Expected primary region NYC, got %s", config.PrimaryRegion)
	}

	// Should have 2 regions (NYC + LA)
	if len(config.Regions) != 2 {
		t.Errorf("Expected 2 regions, got %d", len(config.Regions))
	}

	// Check NYC is primary
	if config.Regions[RegionNYC].Tier != TierPrimary {
		t.Error("NYC should be primary tier")
	}

	// Check LA is secondary
	if config.Regions[RegionLA].Tier != TierSecondary {
		t.Error("LA should be secondary tier")
	}

	// Check LA has cross-country latency configured
	if config.Regions[RegionLA].Latency != 60*time.Millisecond {
		t.Errorf("Expected 60ms latency for LA, got %v", config.Regions[RegionLA].Latency)
	}
}

func TestGeoManager_GetRegion(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	t.Run("existing region", func(t *testing.T) {
		info, ok := gm.GetRegion(RegionNYC)
		if !ok {
			t.Fatal("NYC region not found")
		}
		if info.DisplayName != "New York" {
			t.Errorf("Expected 'New York', got %s", info.DisplayName)
		}
	})

	t.Run("LA region", func(t *testing.T) {
		info, ok := gm.GetRegion(RegionLA)
		if !ok {
			t.Fatal("LA region not found")
		}
		if info.DisplayName != "Los Angeles" {
			t.Errorf("Expected 'Los Angeles', got %s", info.DisplayName)
		}
	})

	t.Run("non-existing region", func(t *testing.T) {
		_, ok := gm.GetRegion(Region("unknown"))
		if ok {
			t.Error("Should not find unknown region")
		}
	})
}

func TestGeoManager_GetActiveRegions(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	active := gm.GetActiveRegions()
	if len(active) != 2 {
		t.Errorf("Expected 2 active regions, got %d", len(active))
	}
}

func TestGeoManager_RegionHealth(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	// Default should be healthy
	health := gm.GetRegionHealth(RegionNYC)
	if health != StateHealthy {
		t.Errorf("Expected healthy, got %s", health)
	}

	// Set degraded
	gm.SetRegionHealth(RegionNYC, StateDegraded)
	health = gm.GetRegionHealth(RegionNYC)
	if health != StateDegraded {
		t.Errorf("Expected degraded, got %s", health)
	}
}

func TestGeoManager_SelectRegion(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	ctx := context.Background()

	// Set all regions healthy
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	t.Run("east coast client gets NYC", func(t *testing.T) {
		selected := gm.SelectRegion(ctx, RegionNYC)
		if selected != RegionNYC {
			t.Errorf("Expected NYC, got %s", selected)
		}
	})

	t.Run("west coast client gets LA", func(t *testing.T) {
		selected := gm.SelectRegion(ctx, RegionLA)
		if selected != RegionLA {
			t.Errorf("Expected LA, got %s", selected)
		}
	})

	t.Run("falls back to LA when NYC unhealthy", func(t *testing.T) {
		gm.SetRegionHealth(RegionNYC, StateFailed)
		selected := gm.SelectRegion(ctx, RegionNYC)
		if selected != RegionLA {
			t.Errorf("Expected fallback to LA, got %s", selected)
		}
	})
}

func TestGeoManager_FailoverRegion(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	t.Run("NYC failover returns LA", func(t *testing.T) {
		alternative := gm.FailoverRegion(RegionNYC)

		if gm.GetRegionHealth(RegionNYC) != StateFailed {
			t.Error("NYC should be failed")
		}
		if alternative != RegionLA {
			t.Errorf("Expected LA, got %s", alternative)
		}
	})

	t.Run("LA failover returns NYC", func(t *testing.T) {
		gm.SetRegionHealth(RegionNYC, StateHealthy) // Recover first
		alternative := gm.FailoverRegion(RegionLA)

		if gm.GetRegionHealth(RegionLA) != StateFailed {
			t.Error("LA should be failed")
		}
		if alternative != RegionNYC {
			t.Errorf("Expected NYC, got %s", alternative)
		}
	})
}

func TestGeoManager_RecoverRegion(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	gm.SetRegionHealth(RegionNYC, StateFailed)
	gm.RecoverRegion(RegionNYC)

	if gm.GetRegionHealth(RegionNYC) != StateHealthy {
		t.Error("Should be healthy after recovery")
	}

	info, _ := gm.GetRegion(RegionNYC)
	if !info.Active {
		t.Error("Region should be active after recovery")
	}
}

func TestGeoManager_Latency(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	gm.UpdateLatency(RegionLA, 75*time.Millisecond)

	latency := gm.GetLatency(RegionLA)
	if latency != 75*time.Millisecond {
		t.Errorf("Expected 75ms, got %v", latency)
	}
}

func TestGeoManager_GetReplicationTargets(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	targets := gm.GetReplicationTargets(RegionNYC)
	if len(targets) != 1 {
		t.Errorf("Expected 1 replication target, got %d", len(targets))
	}
	if targets[0] != RegionLA {
		t.Errorf("Expected LA as target, got %s", targets[0])
	}
}

func TestGeoManager_GetStatus(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateDegraded)

	status := gm.GetStatus()

	if len(status) != 2 {
		t.Errorf("Expected 2 regions in status, got %d", len(status))
	}

	if status[RegionNYC].Health != StateHealthy {
		t.Error("NYC should be healthy")
	}
	if status[RegionLA].Health != StateDegraded {
		t.Error("LA should be degraded")
	}
}
