//go:build !darwin && !linux

package drivers

import (
	"context"
	"fmt"
)

func (d *LocalDriver) SetXAttr(ctx context.Context, container, artifact, name string, value []byte) error {
	return fmt.Errorf("extended attributes not supported on this platform")
}

func (d *LocalDriver) GetXAttr(ctx context.Context, container, artifact, name string) ([]byte, error) {
	return nil, fmt.Errorf("extended attributes not supported on this platform")
}

func (d *LocalDriver) ListXAttrs(ctx context.Context, container, artifact string) ([]string, error) {
	return nil, fmt.Errorf("extended attributes not supported on this platform")
}
