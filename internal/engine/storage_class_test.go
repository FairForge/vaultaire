package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBackendRegion(t *testing.T) {
	tests := []struct {
		backend string
		want    string
	}{
		{"idrive", "us"},
		{"idrive-us-west-1", "us"},
		{"idrive-eu-west-1", "eu"},
		{"idrive-eu-central-2", "eu"},
		{"geyser", "us"},
		{"lyve", "us"},
		{"local", "us"},
		{"s3", "us"},
		{"onedrive", "us"},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			assert.Equal(t, tt.want, BackendRegion(tt.backend))
		})
	}
}
