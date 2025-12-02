package ha

import (
	"context"
	"testing"
	"time"
)

func TestNewDROrchestrator(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())

	t.Run("valid creation", func(t *testing.T) {
		dr, err := NewDROrchestrator(nil, gm, nil, nil)
		if err != nil {
			t.Fatalf("NewDROrchestrator failed: %v", err)
		}
		if dr.status != DRStatusNormal {
			t.Errorf("Expected normal status, got %s", dr.status)
		}
		if dr.activeRegion != RegionNYC {
			t.Errorf("Expected NYC as active, got %s", dr.activeRegion)
		}
	})

	t.Run("nil geoManager", func(t *testing.T) {
		_, err := NewDROrchestrator(nil, nil, nil, nil)
		if err == nil {
			t.Error("Expected error for nil geoManager")
		}
	})

	t.Run("custom config", func(t *testing.T) {
		config := &DRConfig{
			FailoverThreshold: 5,
			AutoFailover:      false,
		}
		dr, err := NewDROrchestrator(config, gm, nil, nil)
		if err != nil {
			t.Fatalf("NewDROrchestrator failed: %v", err)
		}
		if dr.config.FailoverThreshold != 5 {
			t.Errorf("Expected threshold 5, got %d", dr.config.FailoverThreshold)
		}
	})
}

func TestDefaultDRConfig(t *testing.T) {
	config := DefaultDRConfig()

	if config.FailoverThreshold != 3 {
		t.Errorf("Expected threshold 3, got %d", config.FailoverThreshold)
	}
	if !config.AutoFailover {
		t.Error("Expected auto failover enabled")
	}
	if !config.AutoRecovery {
		t.Error("Expected auto recovery enabled")
	}
}

func TestDROrchestrator_GetStatus(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	dr, _ := NewDROrchestrator(nil, gm, nil, nil)

	status := dr.GetStatus()

	if status.Status != DRStatusNormal {
		t.Errorf("Expected normal, got %s", status.Status)
	}
	if status.ActiveRegion != RegionNYC {
		t.Errorf("Expected NYC, got %s", status.ActiveRegion)
	}
	if len(status.Regions) != 2 {
		t.Errorf("Expected 2 regions, got %d", len(status.Regions))
	}
}

func TestDROrchestrator_ForceFailover(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	dr, _ := NewDROrchestrator(nil, gm, nil, nil)
	ctx := context.Background()

	t.Run("successful failover", func(t *testing.T) {
		err := dr.ForceFailover(ctx, RegionLA)
		if err != nil {
			t.Fatalf("ForceFailover failed: %v", err)
		}
		if dr.GetActiveRegion() != RegionLA {
			t.Errorf("Expected LA, got %s", dr.GetActiveRegion())
		}
	})

	t.Run("failover to failed region", func(t *testing.T) {
		gm.SetRegionHealth(RegionNYC, StateFailed)
		err := dr.ForceFailover(ctx, RegionNYC)
		if err == nil {
			t.Error("Expected error for failed target")
		}
	})
}

func TestDROrchestrator_AutomaticFailover(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	config := &DRConfig{
		FailoverThreshold: 2,
		AutoFailover:      true,
		HealthCheckPeriod: 10 * time.Millisecond,
	}

	dr, _ := NewDROrchestrator(config, gm, nil, nil)

	dr.SimulateRegionFailure(RegionNYC)

	ctx := context.Background()
	dr.checkHealth(ctx)

	if dr.GetActiveRegion() != RegionLA {
		t.Errorf("Expected failover to LA, got %s", dr.GetActiveRegion())
	}
}

func TestDROrchestrator_InitiateRecovery(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	dr, _ := NewDROrchestrator(nil, gm, nil, nil)
	ctx := context.Background()

	t.Run("recovery without failover", func(t *testing.T) {
		err := dr.InitiateRecovery(ctx, RegionNYC)
		if err == nil {
			t.Error("Expected error when not in failover")
		}
	})

	t.Run("recovery after failover", func(t *testing.T) {
		if err := dr.ForceFailover(ctx, RegionLA); err != nil {
			t.Fatalf("ForceFailover failed: %v", err)
		}

		gm.SetRegionHealth(RegionNYC, StateHealthy)
		err := dr.InitiateRecovery(ctx, RegionNYC)
		if err != nil {
			t.Fatalf("InitiateRecovery failed: %v", err)
		}

		status := dr.GetStatus()
		if status.Status != DRStatusRecovering {
			t.Errorf("Expected recovering, got %s", status.Status)
		}
	})

	t.Run("recovery to unhealthy region", func(t *testing.T) {
		gm.SetRegionHealth(RegionNYC, StateFailed)
		err := dr.InitiateRecovery(ctx, RegionNYC)
		if err == nil {
			t.Error("Expected error for unhealthy target")
		}
	})
}

func TestDROrchestrator_Events(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	dr, _ := NewDROrchestrator(nil, gm, nil, nil)
	ctx := context.Background()

	var receivedEvents []*DREvent
	dr.SetEventCallback(func(e *DREvent) {
		receivedEvents = append(receivedEvents, e)
	})

	if err := dr.ForceFailover(ctx, RegionLA); err != nil {
		t.Fatalf("ForceFailover failed: %v", err)
	}

	if len(receivedEvents) == 0 {
		t.Error("Expected events from callback")
	}

	events := dr.GetEvents(10)
	if len(events) == 0 {
		t.Error("Expected events in history")
	}

	found := false
	for _, e := range events {
		if e.Type == DREventFailoverDone {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected failover complete event")
	}
}

func TestDROrchestrator_StartStop(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)

	config := &DRConfig{
		HealthCheckPeriod: 10 * time.Millisecond,
		AutoFailover:      true,
	}

	dr, _ := NewDROrchestrator(config, gm, nil, nil)
	ctx := context.Background()

	err := dr.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	dr.Stop()
}

func TestDROrchestrator_IsHealthy(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	dr, _ := NewDROrchestrator(nil, gm, nil, nil)

	if !dr.IsHealthy() {
		t.Error("Expected healthy initially")
	}

	ctx := context.Background()
	if err := dr.ForceFailover(ctx, RegionLA); err != nil {
		t.Fatalf("ForceFailover failed: %v", err)
	}

	status := dr.GetStatus()
	if status.Status == DRStatusNormal {
		t.Error("Should not be normal after failover")
	}
}

func TestDROrchestrator_DegradedState(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	config := &DRConfig{
		FailoverThreshold: 5,
		AutoFailover:      true,
	}

	dr, _ := NewDROrchestrator(config, gm, nil, nil)
	ctx := context.Background()

	gm.SetRegionHealth(RegionNYC, StateDegraded)
	dr.checkHealth(ctx)

	status := dr.GetStatus()
	if status.Status != DRStatusAlert {
		t.Errorf("Expected alert status, got %s", status.Status)
	}

	if status.ActiveRegion != RegionNYC {
		t.Error("Should still be on NYC")
	}
}

func TestDROrchestrator_EventLimit(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	dr, _ := NewDROrchestrator(nil, gm, nil, nil)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		_ = dr.ForceFailover(ctx, RegionLA)
		gm.SetRegionHealth(RegionNYC, StateHealthy)
		_ = dr.ForceFailover(ctx, RegionNYC)
		gm.SetRegionHealth(RegionLA, StateHealthy)
	}

	events := dr.GetEvents(0)
	if len(events) > 1000 {
		t.Errorf("Events should be capped at 1000, got %d", len(events))
	}
}
