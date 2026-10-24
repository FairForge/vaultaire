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
	severity := map[string]string{}
	for _, r := range file.Groups[0].Rules {
		rules[r.Alert] = r.Expr
		severity[r.Alert] = r.Labels["severity"]
	}
	require.Len(t, rules, 7)
	assert.Equal(t, map[string]string{"TranslogCheckpointStale": "warning", "TranslogAppendFailing": "warning",
		"TranslogCheckpointRefused": "page", "TranslogFrozen": "page", "TranslogWitnessStale": "warning",
		"TranslogAnchorServiceStale": "warning", "TranslogAnchorNotCompleting": "warning"}, severity,
		"a refusal to sign pages (RP37-11); so does a frozen log (RP42-1); a witness that stops cosigning warns (K4c-2); one dead calendar of two warns by itself (RP48-1), and so does a proof pending past the horizon (RP48-2)")
	exported := map[string]bool{
		"vaultaire_translog_size": true, "vaultaire_translog_checkpoint_size": true,
		"vaultaire_translog_checkpoint_timestamp_seconds": true, "vaultaire_translog_signing_key_loaded": true,
		"vaultaire_translog_append_failures_total": true, "vaultaire_translog_checkpoint_refusals_total": true,
		"vaultaire_translog_append_refusals_total": true,
		"vaultaire_translog_witnesses_configured":  true, "vaultaire_translog_cosigned_size": true,
		"vaultaire_translog_cosignature_timestamp_seconds": true, "vaultaire_translog_witness_cosign_total": true,
		"vaultaire_translog_anchors_configured": true, "vaultaire_translog_anchored_size": true,
		"vaultaire_translog_anchor_timestamp_seconds": true, "vaultaire_translog_anchor_total": true,
		"vaultaire_translog_anchor_pending":                      true,
		"vaultaire_translog_anchored_size_by_service":            true,
		"vaultaire_translog_anchor_timestamp_by_service_seconds": true,
		"vaultaire_translog_anchor_oldest_pending_seconds":       true,
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

	assert.NotContains(t, rules["TranslogAppendFailing"], "append_refusals", "a frozen log is not an append failure")
	ws := rules["TranslogWitnessStale"]
	assert.Contains(t, ws, "> 7200", "two hours")
	assert.Contains(t, ws, "max(vaultaire_translog_witnesses_configured) > 0", "no witness, no alert")
	assert.Contains(t, ws, "max(vaultaire_translog_checkpoint_size) > max(vaultaire_translog_cosigned_size)", "an idle log is not stale")
	as := rules["TranslogAnchorServiceStale"]
	assert.Contains(t, as, "> 7200", "two hours")
	assert.Contains(t, as, "max by (kind, service) (vaultaire_translog_anchor_timestamp_by_service_seconds)",
		"per service (RP48-1: one dead calendar of two must fire by itself; the series exist per configured service only, so none configured is silent)")
	assert.Contains(t, as, "max by (kind, service) (vaultaire_translog_anchored_size_by_service) < on () group_left max(vaultaire_translog_checkpoint_size)", "an idle log is not stale")
	nc := rules["TranslogAnchorNotCompleting"]
	assert.Contains(t, nc, "vaultaire_translog_anchor_oldest_pending_seconds", "RP48-2")
	assert.Contains(t, nc, "> 172800", "the 48 h re-submission horizon")
	raw, err := os.ReadFile("../../deploy/monitoring/tests/vaultaire-translog_test.yml")
	require.NoError(t, err)
	for _, s := range []string{"vaultaire-translog.yml", "TranslogCheckpointStale", "TranslogAppendFailing", "TranslogCheckpointRefused", "TranslogFrozen", "TranslogWitnessStale", "TranslogAnchorServiceStale", "TranslogAnchorNotCompleting"} {
		assert.Contains(t, string(raw), s)
	}
}
