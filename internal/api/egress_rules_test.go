package api

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The egress rule file names series this package really exports, and has
// exactly one info-level rule (WP-R10-9).
func TestEgressRuleFile_MatchesTheExportedSeries(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/monitoring/vaultaire-egress.yml")
	require.NoError(t, err)
	var file struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert  string            `yaml:"alert"`
				Expr   string            `yaml:"expr"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &file))

	require.Len(t, file.Groups, 1)
	require.Len(t, file.Groups[0].Rules, 1)
	rule := file.Groups[0].Rules[0]
	assert.Equal(t, "info", rule.Labels["severity"])
	assert.Contains(t, rule.Expr, "vaultaire_egress_throttled_tenants")
	for _, series := range []string{
		"vaultaire_egress_throttled_tenants", "vaultaire_egress_throttle_engaged_total",
		"vaultaire_egress_would_throttle_total", "vaultaire_egress_throttle_rejected_total",
		"vaultaire_egress_throttled_bytes_total",
	} {
		assert.True(t, strings.Contains(string(raw), series), "the rule file documents %s", series)
	}
}
