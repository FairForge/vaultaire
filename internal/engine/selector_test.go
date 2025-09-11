// internal/engine/selector_test.go
package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBackendSelector_SelectBest(t *testing.T) {
	selector := NewBackendSelector()

	// Add backends with different health scores
	selector.UpdateScore("backend1", 95.0) // Best
	selector.UpdateScore("backend2", 60.0) // Degraded
	selector.UpdateScore("backend3", 30.0) // Poor

	tests := []struct {
		name      string
		operation string
		expected  string
	}{
		{
			name:      "selects highest score for read",
			operation: "GET",
			expected:  "backend1",
		},
		{
			name:      "selects highest score for write",
			operation: "PUT",
			expected:  "backend1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected := selector.SelectBackend(context.Background(), tt.operation)
			assert.Equal(t, tt.expected, selected)
		})
	}
}

func TestBackendSelector_Failover(t *testing.T) {
	selector := NewBackendSelector()

	// Backend1 fails
	selector.UpdateScore("backend1", 0.0)  // Dead
	selector.UpdateScore("backend2", 85.0) // Healthy

	selected := selector.SelectBackend(context.Background(), "GET")
	assert.Equal(t, "backend2", selected)
}
