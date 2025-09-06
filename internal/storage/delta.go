package storage

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// DeltaEncoder handles delta encoding between versions
type DeltaEncoder struct{}

// NewDeltaEncoder creates a new delta encoder
func NewDeltaEncoder() *DeltaEncoder {
	return &DeltaEncoder{}
}

// Delta represents a change between versions
type Delta struct {
	Data   []byte
	Offset int
	Type   string // "xor-compressed", "full"
}

// CreateDelta creates a delta between original and modified
func (de *DeltaEncoder) CreateDelta(original, modified []byte) *Delta {
	// XOR to find differences
	maxLen := len(original)
	if len(modified) > maxLen {
		maxLen = len(modified)
	}

	xorData := make([]byte, maxLen)
	for i := 0; i < len(original) && i < len(modified); i++ {
		xorData[i] = original[i] ^ modified[i]
	}

	// Add remaining bytes from modified if it's longer
	if len(modified) > len(original) {
		copy(xorData[len(original):], modified[len(original):])
	}

	// Compress the XOR result (zeros compress well)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(xorData[:len(modified)])
	_ = gz.Close()

	compressed := buf.Bytes()

	// Only use delta if it's actually smaller
	if len(compressed) < len(modified) {
		return &Delta{
			Data: compressed,
			Type: "xor-compressed",
		}
	}

	// Otherwise store full
	return &Delta{
		Data: modified,
		Type: "full",
	}
}

// ApplyDelta applies a delta to reconstruct the modified version
func (de *DeltaEncoder) ApplyDelta(original []byte, delta *Delta) []byte {
	if delta.Type == "full" {
		return delta.Data
	}

	// Decompress
	reader, err := gzip.NewReader(bytes.NewReader(delta.Data))
	if err != nil {
		return nil
	}
	defer func() { _ = reader.Close() }()

	xorData, err := io.ReadAll(reader)
	if err != nil {
		return nil
	}

	// Apply XOR
	result := make([]byte, len(xorData))
	for i := 0; i < len(xorData); i++ {
		if i < len(original) {
			result[i] = original[i] ^ xorData[i]
		} else {
			result[i] = xorData[i]
		}
	}

	return result
}

// VersionStore manages versioned storage with deltas
type VersionStore struct {
	versions map[string][]*Version
	data     map[string][]byte // version ID -> data
}

// Version represents a stored version
type Version struct {
	ID       string
	Filename string
	Size     int
	IsDelta  bool
	BaseID   string // For deltas, the base version
}

// NewVersionStore creates a new version store
func NewVersionStore() *VersionStore {
	return &VersionStore{
		versions: make(map[string][]*Version),
		data:     make(map[string][]byte),
	}
}

// Store saves the initial version
func (vs *VersionStore) Store(filename string, content []byte) (string, error) {
	versionID := fmt.Sprintf("v1-%s", filename)

	version := &Version{
		ID:       versionID,
		Filename: filename,
		Size:     len(content),
		IsDelta:  false,
	}

	vs.versions[filename] = []*Version{version}
	vs.data[versionID] = content

	return versionID, nil
}

// Update stores a new version as a delta
func (vs *VersionStore) Update(filename string, content []byte) (string, error) {
	versions, exists := vs.versions[filename]
	if !exists || len(versions) == 0 {
		return vs.Store(filename, content)
	}

	// Get the latest version
	lastVersion := versions[len(versions)-1]
	lastContent, err := vs.GetVersion(filename, lastVersion.ID)
	if err != nil {
		return "", err
	}

	// Create delta
	encoder := NewDeltaEncoder()
	delta := encoder.CreateDelta(lastContent, content)

	versionID := fmt.Sprintf("v%d-%s", len(versions)+1, filename)

	version := &Version{
		ID:       versionID,
		Filename: filename,
		Size:     len(delta.Data),
		IsDelta:  delta.Type != "full",
		BaseID:   lastVersion.ID,
	}

	vs.versions[filename] = append(vs.versions[filename], version)
	vs.data[versionID] = delta.Data

	return versionID, nil
}

// GetVersion retrieves a specific version
func (vs *VersionStore) GetVersion(filename string, versionID string) ([]byte, error) {
	// Find the version
	versions, exists := vs.versions[filename]
	if !exists {
		return nil, fmt.Errorf("file not found: %s", filename)
	}

	var targetVersion *Version
	for _, v := range versions {
		if v.ID == versionID {
			targetVersion = v
			break
		}
	}

	if targetVersion == nil {
		return nil, fmt.Errorf("version not found: %s", versionID)
	}

	// If it's not a delta, return directly
	if !targetVersion.IsDelta {
		return vs.data[versionID], nil
	}

	// Reconstruct from deltas
	baseContent, err := vs.GetVersion(filename, targetVersion.BaseID)
	if err != nil {
		return nil, err
	}

	encoder := NewDeltaEncoder()
	deltaType := "xor-compressed"
	if !targetVersion.IsDelta {
		deltaType = "full"
	}
	delta := &Delta{Data: vs.data[versionID], Type: deltaType}
	return encoder.ApplyDelta(baseContent, delta), nil
}

// TotalSize returns total storage used
func (vs *VersionStore) TotalSize() int {
	total := 0
	for _, data := range vs.data {
		total += len(data)
	}
	return total
}
