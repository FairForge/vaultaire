package storage

import (
    "strings"
    "testing"
    
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestDeltaEncoding(t *testing.T) {
    t.Run("creates delta between versions", func(t *testing.T) {
        // Arrange
        encoder := NewDeltaEncoder()
        
        original := []byte("The quick brown fox jumps over the lazy dog")
        modified := []byte("The quick brown fox leaps over the lazy cat")
        
        // Act
        delta := encoder.CreateDelta(original, modified)
        
        // Assert
        assert.NotNil(t, delta)
        assert.Less(t, len(delta.Data), len(modified), "Delta should be smaller than full file")
        
        // Verify we can reconstruct
        reconstructed := encoder.ApplyDelta(original, delta)
        assert.Equal(t, modified, reconstructed)
    })
    
    t.Run("handles incremental changes", func(t *testing.T) {
        // Arrange
        store := NewVersionStore()
        
        // Use larger, more similar documents that will compress well
        base := strings.Repeat("This is a test document with lots of repeated content. ", 10)
        
        v1 := []byte(base + "Version 1.")
        v2 := []byte(base + "Version 2 with small changes.")
        v3 := []byte(base + "Version 3 with a few more changes.")
        
        // Act - store versions
        id1, err := store.Store("doc.txt", v1)
        require.NoError(t, err)
        
        _, err = store.Update("doc.txt", v2)
        require.NoError(t, err)
        
        id3, err := store.Update("doc.txt", v3)
        require.NoError(t, err)
        
        // Assert - can retrieve any version
        got1, err := store.GetVersion("doc.txt", id1)
        require.NoError(t, err)
        assert.Equal(t, v1, got1)
        
        got3, err := store.GetVersion("doc.txt", id3)
        require.NoError(t, err)
        assert.Equal(t, v3, got3)
        
        // Storage should be less than storing all versions
        totalOriginal := len(v1) + len(v2) + len(v3)
        assert.Less(t, store.TotalSize(), totalOriginal, 
            "Delta storage (%d) should be less than full storage (%d)", 
            store.TotalSize(), totalOriginal)
    })
}
