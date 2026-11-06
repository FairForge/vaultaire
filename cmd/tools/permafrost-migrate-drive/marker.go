package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// markerPath is the fleet marker under the drive's root.
var markerPath = rootFolder + "/" + drivers.FleetMarkerPath

// markerView is what the tool prints and checks of a marker.
type markerView struct {
	Index   int      `json:"index"`
	Fleet   []string `json:"fleet"`
	DriveID string   `json:"drive_id"`
	MovedTo string   `json:"moved_to"`
}

// readMarkerBytes is the marker's content on the drive, nil when absent.
func readMarkerBytes(ctx context.Context, g *graph, driveID string) ([]byte, error) {
	it, err := g.itemAt(ctx, driveID, markerPath)
	if err != nil || it == nil {
		return nil, err
	}
	rc, err := g.content(ctx, driveID, it.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read marker: %w", err)
	}
	return b, nil
}

func readMarker(ctx context.Context, g *graph, driveID string) (*markerView, error) {
	b, err := readMarkerBytes(ctx, g, driveID)
	if err != nil || b == nil {
		return nil, err
	}
	var m markerView
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode marker: %w", err)
	}
	return &m, nil
}

// writeMarker rewrites the marker on driveID from base with drive_id (and
// moved_to) set — every other field the driver wrote is kept.
func writeMarker(ctx context.Context, g *graph, driveID string, base []byte, newDriveID, movedTo string) error {
	b, err := drivers.RewriteFleetMarker(base, newDriveID, movedTo)
	if err != nil {
		return err
	}
	_, err = g.putSmall(ctx, driveID, markerPath, b, "replace")
	if err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	// Read it back: the driver will.
	got, err := readMarker(ctx, g, driveID)
	if err != nil {
		return fmt.Errorf("re-read marker: %w", err)
	}
	if got == nil || got.DriveID != newDriveID {
		return fmt.Errorf("re-read marker: drive_id is not %s", newDriveID)
	}
	return nil
}
