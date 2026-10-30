package api

import (
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// RP82-3: a pool counter that drifted is exported on scrape — one series
// for the drifting root, none for a true one — and the rule file names the
// series the collector exports.
func TestPoolDrift_TheGaugeNamesTheDriftingRoot(t *testing.T) {
	f := setupPlatformFixture(t)
	f.grant()
	_, err := f.db.Exec(`UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes + 77 WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes - 77 WHERE tenant_id = $1`, f.user.TenantID)
	})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(newPoolDriftCollector(f.db, zap.NewNop()))
	families, err := reg.Gather()
	require.NoError(t, err)
	var got float64
	found := false
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "root" && l.GetValue() == f.user.TenantID {
					got, found = m.GetGauge().GetValue(), true
				}
			}
		}
	}
	require.True(t, found, "the drifting root has a series")
	assert.Equal(t, float64(77), got)

	raw, err := os.ReadFile("../../deploy/monitoring/vaultaire-quota.yml")
	require.NoError(t, err)
	var file struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &file))
	require.Len(t, file.Groups, 1)
	require.Len(t, file.Groups[0].Rules, 1)
	assert.Equal(t, "PoolUsedDrift", file.Groups[0].Rules[0].Alert)
	assert.True(t, strings.Contains(file.Groups[0].Rules[0].Expr, "vaultaire_pool_used_drift_bytes"))
}
