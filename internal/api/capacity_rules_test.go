package api

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-CAP-1 C: the capacity rule file parses and every series its
// expressions read is one the capacity collector or the server exports —
// the backend_rules_test.go / job_rules_test.go pattern. The series that
// need a run or a plan row are proven live in
// TestBackendCapacity_GaugesReadFromJobRuns; here the names are pinned so a
// renamed gauge cannot leave a rule watching nothing.
func TestCapacityRules_ParseAndNameExportedSeries(t *testing.T) {
	file, raw := readRuleFile(t, "vaultaire-capacity.yml")
	require.Len(t, file.Groups, 1)
	require.Equal(t, "vaultaire-capacity", file.Groups[0].Name)

	rules := map[string]struct{ expr, forDur, severity string }{}
	for _, r := range file.Groups[0].Rules {
		rules[r.Alert] = struct{ expr, forDur, severity string }{r.Expr, r.For, r.Labels["severity"]}
	}

	// The thirteen alerts and their severities: capacity at 90 (of the plan
	// or of the vendor's own quota), the 7-day renewal (the 2026-08-27
	// shape) and a missing card page; the rest warn.
	for name, sev := range map[string]string{
		"BackendCapacity80": "warning", "BackendCapacity90": "critical",
		"BackendVendorQuota80": "warning", "BackendVendorQuota90": "critical",
		"BackendCapacityForecast30d": "warning", "BackendEgressFree80": "warning",
		"BackendPlanRenewal30d": "warning", "BackendPlanRenewal14d": "warning", "BackendPlanRenewal7d": "critical",
		"BackendPlanNoPayment": "critical", "BackendPlanUnknown": "warning",
		"BackendVendorDrift": "warning", "BackendCapacityStale": "warning",
	} {
		require.Contains(t, rules, name)
		assert.Equal(t, sev, rules[name].severity, name)
	}
	assert.Len(t, rules, 13, "every rule in the file is one the test names")

	// Every metric an expression reads is one of the exported names — the
	// capacity collector's (metrics_capacity.go), the backend probe's, or
	// the scheduler's.
	exported := map[string]bool{
		"vaultaire_backend_stored_bytes":                    true,
		"vaultaire_backend_vendor_used_bytes":               true,
		"vaultaire_backend_vendor_limit_bytes":              true,
		"vaultaire_backend_vendor_egress_month_bytes":       true,
		"vaultaire_backend_plan_bytes":                      true,
		"vaultaire_backend_plan_term_end_timestamp_seconds": true,
		"vaultaire_backend_plan_payment_on_file":            true,
		"vaultaire_backend_egress_free_bytes":               true,
		"vaultaire_backend_health":                          true, // backendCollector (prom_metrics.go)
		"vaultaire_job_last_success_timestamp_seconds":      true, // the scheduler's collector
	}
	names := regexp.MustCompile(`vaultaire_[a-z_]+`)
	for alert, r := range rules {
		for _, m := range names.FindAllString(r.expr, -1) {
			assert.True(t, exported[m], "%s reads %s, which nothing exports", alert, m)
		}
	}

	// The specific shapes the plan names.
	assert.Contains(t, rules["BackendCapacity80"].expr, "0.8 * vaultaire_backend_plan_bytes")
	assert.Contains(t, rules["BackendCapacity80"].expr, "vaultaire_backend_vendor_used_bytes",
		"the larger of stored and vendor_used decides")
	for _, a := range []string{"BackendCapacity80", "BackendCapacity90"} {
		assert.Contains(t, rules[a].expr, "unless on (backend) vaultaire_backend_vendor_limit_bytes",
			"a backend with its own hard quota is watched by BackendVendorQuota*, never paged twice")
	}
	assert.Contains(t, rules["BackendVendorQuota80"].expr, "0.8 * vaultaire_backend_vendor_limit_bytes")
	assert.Contains(t, rules["BackendVendorQuota90"].expr, "0.9 * vaultaire_backend_vendor_limit_bytes")
	assert.NotContains(t, rules["BackendVendorQuota90"].expr, "plan_bytes", "no plan row needed for a hard quota")
	assert.Contains(t, rules["BackendCapacityForecast30d"].expr, "predict_linear")
	assert.Contains(t, rules["BackendCapacityForecast30d"].expr, "[14d:1h]")
	assert.Contains(t, rules["BackendEgressFree80"].expr, "0.8 * vaultaire_backend_egress_free_bytes")
	assert.Contains(t, rules["BackendPlanUnknown"].expr, `backend=~"idrive|lyve|geyser|wasabi|r2"`,
		"the paid backends; local and sync are not bought by the TB")
	assert.Contains(t, rules["BackendPlanUnknown"].expr, "unless on (backend) vaultaire_backend_plan_bytes")
	assert.Contains(t, rules["BackendPlanNoPayment"].expr, "vaultaire_backend_plan_payment_on_file == 0")
	assert.Contains(t, rules["BackendVendorDrift"].expr, "1.2 *")
	assert.Equal(t, "3d", rules["BackendVendorDrift"].forDur)
	assert.Contains(t, rules["BackendCapacityStale"].expr, `job_name="backend_capacity"`)
	assert.Contains(t, rules["BackendCapacityStale"].expr, "> 26 * 3600")
	// A promtool unit test rides beside the file.
	_ = raw
	_, err := readRuleTestFile(t)
	require.NoError(t, err)
}

// readRuleTestFile asserts the promtool unit test file exists and names the
// rule file (it runs on the box; promtool is not on every dev machine).
func readRuleTestFile(t *testing.T) (string, error) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/monitoring/tests/vaultaire-capacity_test.yml")
	if err != nil {
		return "", err
	}
	assert.Contains(t, string(raw), "vaultaire-capacity.yml")
	for _, alert := range []string{"BackendCapacity80", "BackendPlanRenewal7d", "BackendPlanUnknown", "BackendVendorQuota80"} {
		assert.Contains(t, string(raw), alert, "the promtool test covers %s", alert)
	}
	return string(raw), nil
}
