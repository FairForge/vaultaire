package cache

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGeoCacheManager_AddEdgeNode(t *testing.T) {
	manager := NewGeoCacheManager()

	node := &EdgeNode{
		ID: "edge-us-west",
		Location: GeoLocation{
			Latitude:  37.7749,
			Longitude: -122.4194,
			Region:    "us-west",
			City:      "San Francisco",
		},
		Capacity: 100 * 1024 * 1024,
		Active:   true,
	}

	manager.AddEdgeNode(node)

	assert.Equal(t, 1, len(manager.edges))
	assert.Equal(t, node, manager.edges["edge-us-west"])
}

func TestGeoCacheManager_FindNearestEdge(t *testing.T) {
	manager := NewGeoCacheManager()

	// Add multiple edge nodes
	sfNode := &EdgeNode{
		ID:       "edge-sf",
		Location: GeoLocation{Latitude: 37.7749, Longitude: -122.4194},
		Active:   true,
		Load:     0.3,
	}

	nyNode := &EdgeNode{
		ID:       "edge-ny",
		Location: GeoLocation{Latitude: 40.7128, Longitude: -74.0060},
		Active:   true,
		Load:     0.5,
	}

	manager.AddEdgeNode(sfNode)
	manager.AddEdgeNode(nyNode)

	// User near SF
	userLoc := GeoLocation{Latitude: 37.5, Longitude: -122.0}
	nearest := manager.FindNearestEdge(userLoc)

	require.NotNil(t, nearest)
	assert.Equal(t, "edge-sf", nearest.ID)
}

func TestGeoCacheManager_SkipsOverloadedNodes(t *testing.T) {
	manager := NewGeoCacheManager()

	// Overloaded node
	overloaded := &EdgeNode{
		ID:       "edge-overloaded",
		Location: GeoLocation{Latitude: 37.0, Longitude: -122.0},
		Active:   true,
		Load:     0.9, // Over 80% threshold
	}

	// Available node (farther but available)
	available := &EdgeNode{
		ID:       "edge-available",
		Location: GeoLocation{Latitude: 40.0, Longitude: -120.0},
		Active:   true,
		Load:     0.2,
	}

	manager.AddEdgeNode(overloaded)
	manager.AddEdgeNode(available)

	userLoc := GeoLocation{Latitude: 37.0, Longitude: -122.0}
	nearest := manager.FindNearestEdge(userLoc)

	// Should select available node even though it's farther
	assert.Equal(t, "edge-available", nearest.ID)
}
