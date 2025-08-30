package drivers

import (
	"testing"
	"time"
)

func TestConflictDetection(t *testing.T) {
	detector := NewConflictDetector()

	// Simulate concurrent modifications
	v1 := Version{
		Path:     "/test/file.txt",
		Modified: time.Now(),
		Checksum: "abc123",
	}

	v2 := Version{
		Path:     "/test/file.txt",
		Modified: time.Now().Add(1 * time.Second),
		Checksum: "def456",
	}

	conflict := detector.CheckConflict(v1, v2)
	if !conflict {
		t.Fatal("Expected conflict not detected")
	}
}

func TestConflictResolution(t *testing.T) {
	resolver := NewConflictResolver()

	result, err := resolver.Resolve(
		[]byte("base content"),
		[]byte("version A content"),
		[]byte("version B content"),
	)

	if err != nil {
		t.Fatalf("Resolution failed: %v", err)
	}

	if len(result) == 0 {
		t.Fatal("Resolution produced empty result")
	}
}
