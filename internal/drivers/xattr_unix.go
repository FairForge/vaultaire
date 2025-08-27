//go:build darwin || linux

package drivers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// SetXAttr sets an extended attribute on an artifact
func (d *LocalDriver) SetXAttr(ctx context.Context, container, artifact, name string, value []byte) error {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("artifact not found: %w", err)
		}
		return fmt.Errorf("stat failed: %w", err)
	}

	// Set extended attribute
	if err := unix.Setxattr(fullPath, name, value, 0); err != nil {
		return fmt.Errorf("setxattr failed: %w", err)
	}

	return nil
}

// GetXAttr retrieves an extended attribute from an artifact
func (d *LocalDriver) GetXAttr(ctx context.Context, container, artifact, name string) ([]byte, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact not found: %w", err)
		}
		return nil, fmt.Errorf("stat failed: %w", err)
	}

	// Get size first
	size, err := unix.Getxattr(fullPath, name, nil)
	if err != nil {
		return nil, fmt.Errorf("getxattr size failed: %w", err)
	}

	// Get actual value
	buf := make([]byte, size)
	_, err = unix.Getxattr(fullPath, name, buf)
	if err != nil {
		return nil, fmt.Errorf("getxattr failed: %w", err)
	}

	return buf, nil
}

// ListXAttrs lists all extended attributes on an artifact
func (d *LocalDriver) ListXAttrs(ctx context.Context, container, artifact string) ([]string, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact not found: %w", err)
		}
		return nil, fmt.Errorf("stat failed: %w", err)
	}

	// Get size of attribute list
	size, err := unix.Listxattr(fullPath, nil)
	if err != nil {
		return nil, fmt.Errorf("listxattr size failed: %w", err)
	}

	if size == 0 {
		return []string{}, nil
	}

	// Get actual list
	buf := make([]byte, size)
	_, err = unix.Listxattr(fullPath, buf)
	if err != nil {
		return nil, fmt.Errorf("listxattr failed: %w", err)
	}

	// Parse null-terminated strings
	var attrs []string
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] == 0 {
			if i > start {
				attrs = append(attrs, string(buf[start:i]))
			}
			start = i + 1
		}
	}

	return attrs, nil
}
