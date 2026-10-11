package api

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Prompt 2b.5 F2.2: the primary's open breaker and failing probe page.
// Every rule carries severity + summary + description (the ntfy bridge).
func TestBackendRuleFile_PrimaryRulesAreCritical(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/monitoring/vaultaire-backends.yml")
	require.NoError(t, err)
	var file struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &file))
	require.Len(t, file.Groups, 1)

	primaryCritical := map[string]bool{}
	for _, r := range file.Groups[0].Rules {
		assert.NotEmpty(t, r.Labels["severity"], r.Alert)
		assert.NotEmpty(t, r.Annotations["summary"], r.Alert)
		assert.NotEmpty(t, r.Annotations["description"], r.Alert)
		if strings.Contains(r.Expr, "vaultaire_primary_backend == 1") {
			assert.Contains(t, r.Expr, "on(backend)", r.Alert)
			if r.Labels["severity"] == "critical" {
				primaryCritical[r.Alert] = true
			}
		}
	}
	assert.True(t, primaryCritical["BackendCircuitOpen"], "BackendCircuitOpen is critical on the primary")
	assert.True(t, primaryCritical["PrimaryBackendProbeFailing"])
}
