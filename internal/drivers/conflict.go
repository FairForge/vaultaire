package drivers

import (
	"fmt"
	"time"
)

type Version struct {
	Path     string
	Modified time.Time
	Checksum string
}

type ConflictDetector struct{}

func NewConflictDetector() *ConflictDetector {
	return &ConflictDetector{}
}

func (c *ConflictDetector) CheckConflict(v1, v2 Version) bool {
	return v1.Checksum != v2.Checksum
}

type ConflictResolver struct{}

func NewConflictResolver() *ConflictResolver {
	return &ConflictResolver{}
}

func (c *ConflictResolver) Resolve(base, versionA, versionB []byte) ([]byte, error) {
	// Simple three-way merge with conflict markers
	if string(versionA) == string(versionB) {
		return versionA, nil
	}

	result := fmt.Sprintf(
		"<<<<<<< VERSION A\n%s\n=======\n%s\n>>>>>>> VERSION B\n",
		string(versionA),
		string(versionB),
	)

	return []byte(result), nil
}
