package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDetectStorageMode(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set is local", nil, "local"},
		{"STORAGE_MODE wins over every pair", map[string]string{"STORAGE_MODE": "wasabi", "IDRIVE_ACCESS_KEY": "a"}, "wasabi"},
		{"iDrive stays first", map[string]string{"IDRIVE_ACCESS_KEY": "a", "WASABI_ACCESS_KEY": "w", "QUOTALESS_ACCESS_KEY": "q"}, "idrive"},
		{"Wasabi before Quotaless/S3/Geyser", map[string]string{"WASABI_ACCESS_KEY": "w", "QUOTALESS_ACCESS_KEY": "q", "S3_ACCESS_KEY": "s", "GEYSER_ACCESS_KEY": "g"}, "wasabi"},
		{"quotaless before s3", map[string]string{"QUOTALESS_ACCESS_KEY": "q", "S3_ACCESS_KEY": "s"}, "quotaless"},
		{"s3 before geyser", map[string]string{"S3_ACCESS_KEY": "s", "GEYSER_ACCESS_KEY": "g"}, "s3"},
		{"geyser alone", map[string]string{"GEYSER_ACCESS_KEY": "g"}, "geyser"},
		{"Lyve/R2/permafrost never auto-select", map[string]string{"LYVE_ACCESS_KEY": "l", "R2_ACCESS_KEY": "r", "TENANT_1_ID": "t"}, "local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectStorageMode(func(k string) string { return tc.env[k] })
			assert.Equal(t, tc.want, got)
		})
	}
}
