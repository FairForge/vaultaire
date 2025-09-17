package engine

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestCoreEngine_ImplementsInterface(t *testing.T) {
	// This test ensures CoreEngine implements Engine interface
	var _ Engine = (*CoreEngine)(nil)

	// Test construction
	logger := zap.NewNop()
	engine := NewEngine(nil, logger, nil)

	if engine == nil {
		t.Fatal("NewEngine returned nil")
	}

	// Test health check
	err := engine.HealthCheck(context.Background())
	if err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}
