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

// PUBLIC is our internal class for public-read buckets: it routes to the
// Cloudflare R2 backend (public-bucket/CDN-origin role only — never a tier)
// when one is registered, and otherwise falls back to the primary exactly like
// every other unavailable mapping. Clients never see "PUBLIC": r2 reports
// STANDARD on HEAD/LIST like the other hot backends.
func TestResolveStorageClass_PublicRoutesToR2(t *testing.T) {
	backend, class := ResolveStorageClass("PUBLIC", "idrive", map[string]Driver{"idrive": nil, "r2": nil})
	if backend != "r2" || class != "PUBLIC" {
		t.Fatalf("PUBLIC with r2 registered: got (%s,%s), want (r2,PUBLIC)", backend, class)
	}

	backend, class = ResolveStorageClass("PUBLIC", "idrive", map[string]Driver{"idrive": nil})
	if backend != "idrive" || class != "PUBLIC" {
		t.Fatalf("PUBLIC without r2: got (%s,%s), want (idrive,PUBLIC)", backend, class)
	}

	assert.Equal(t, "STANDARD", BackendToStorageClass("r2"))
}
