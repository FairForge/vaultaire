package engine

import (
	"context"
	"go.uber.org/zap"
	"testing"
)

func TestCoreEngine_ImplementsInterface(t *testing.T) {
	// This test ensures CoreEngine implements Engine interface
	var _ Engine = (*CoreEngine)(nil)

	// Test construction
	logger := zap.NewNop()
	engine := NewEngine(logger)

	if engine == nil {
		t.Fatal("NewEngine returned nil")
	}

	// Test health check
	err := engine.HealthCheck(context.Background())
	if err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}
