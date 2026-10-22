package api

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The translog rule file reads only series the server exports, and a
// promtool unit test rides beside it (run on the box).
func TestTranslogRules_ParseAndNameExportedSeries(t *testing.T) {
	file, _ := readRuleFile(t, "vaultaire-translog.yml")
	require.Len(t, file.Groups, 1)
	rules := map[string]string{}
	for _, r := range file.Groups[0].Rules {
		rules[r.Alert] = r.Expr
		assert.Equal(t, "warning", r.Labels["severity"], r.Alert)
	}
	require.Len(t, rules, 2)
	exported := map[string]bool{
		"vaultaire_translog_size": true, "vaultaire_translog_checkpoint_size": true,
		"vaultaire_translog_checkpoint_timestamp_seconds": true, "vaultaire_translog_signing_key_loaded": true,
		"vaultaire_translog_append_failures_total": true,
	}
	names := regexp.MustCompile(`vaultaire_[a-z_]+`)
	for alert, expr := range rules {
		for _, m := range names.FindAllString(expr, -1) {
			assert.True(t, exported[m], "%s reads %s, which nothing exports", alert, m)
		}
	}
	stale := rules["TranslogCheckpointStale"]
	assert.Contains(t, stale, "vaultaire_translog_signing_key_loaded) == 1", "no key, no alert")
	assert.Contains(t, stale, "> 3600")
	assert.Contains(t, stale, "max(vaultaire_translog_size) > max(vaultaire_translog_checkpoint_size)", "an idle log is not stale")

	raw, err := os.ReadFile("../../deploy/monitoring/tests/vaultaire-translog_test.yml")
	require.NoError(t, err)
	for _, s := range []string{"vaultaire-translog.yml", "TranslogCheckpointStale", "TranslogAppendFailing"} {
		assert.Contains(t, string(raw), s)
	}
}
