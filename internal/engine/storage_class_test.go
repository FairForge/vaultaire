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

func TestResolveStorageClass_ResilientRoutesToLyve(t *testing.T) {
	drivers := map[string]Driver{"local": nil, "lyve": nil}
	backend, class := ResolveStorageClass("RESILIENT", "local", drivers)
	if backend != "lyve" || class != "RESILIENT" {
		t.Fatalf("RESILIENT with lyve registered: got (%s,%s), want (lyve,RESILIENT)", backend, class)
	}

	// Without a lyve driver registered, fall back to primary — never error.
	backend, class = ResolveStorageClass("RESILIENT", "local", map[string]Driver{"local": nil})
	if backend != "local" || class != "RESILIENT" {
		t.Fatalf("RESILIENT without lyve: got (%s,%s), want (local,RESILIENT)", backend, class)
	}
}
