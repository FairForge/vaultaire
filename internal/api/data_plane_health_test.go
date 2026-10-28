package api

// WP-EDGE-3 step 13: the hosts' health — vaultaire_data_plane_host_up
// {host,kind} read from data_plane_hosts (1 when health is up and, for a
// node, its last heartbeat is within 90 s; the Worker and the origin do
// not heartbeat), the edge_admit_reaper's sweep marking nodes unseen for
// 90 s down (never a draining one), and the rule file.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func (f *edgeFixture) nodeRow(health, lastSeen string) string {
	f.t.Helper()
	host := "hb-" + uuid.New().String()[:8] + ".lab.test"
	f.exec(`INSERT INTO data_plane_hosts (host, kind, address, token_hash, weight, health, last_seen)
		VALUES ($1, 'node', '198.51.100.90', $2, 1, $3, NOW() - $4::interval)`, host, "h-"+host, health, lastSeen)
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM data_plane_hosts WHERE host = $1`, host) })
	return host
}

func hostUpValues(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "vaultaire_data_plane_host_up" {
			continue
		}
		for _, m := range mf.GetMetric() {
			out[labelOf(m, "host")+"/"+labelOf(m, "kind")] = m.GetGauge().GetValue()
		}
	}
	return out
}

func labelOf(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func TestDataPlaneHostUp_Collector(t *testing.T) {
	f := setupEdgeFixture(t)
	fresh := f.nodeRow("up", "10 seconds")
	stale := f.nodeRow("up", "2 minutes")
	down := f.nodeRow("down", "5 seconds")
	v := hostUpValues(t, newDataPlaneHostsCollector(f.db, zap.NewNop()))
	assert.Equal(t, 1.0, v[fresh+"/node"], "up and seen within 90 s")
	assert.Equal(t, 0.0, v[stale+"/node"], "unseen for 2 minutes")
	assert.Equal(t, 0.0, v[down+"/node"])
	assert.Equal(t, 1.0, v["s3.stored.cloud/edge"], "the Worker (seeded by 112) heartbeats nowhere: its health")
	assert.Equal(t, 1.0, v["slc/slc"], "the origin itself")
}

func TestEdgeReaper_MarksNodesUnseenFor90sDown(t *testing.T) {
	f := setupEdgeFixture(t)
	stale := f.nodeRow("up", "2 minutes")
	fresh := f.nodeRow("up", "10 seconds")
	draining := f.nodeRow("draining", "10 minutes")
	res, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.HostsMarkedDown, 1)
	health := func(h string) string {
		var s string
		require.NoError(t, f.db.QueryRow(`SELECT health FROM data_plane_hosts WHERE host = $1`, h).Scan(&s))
		return s
	}
	assert.Equal(t, "down", health(stale))
	assert.Equal(t, "up", health(fresh))
	assert.Equal(t, "draining", health(draining), "the operator's drain stands")
	assert.Equal(t, "up", health("s3.stored.cloud"), "the Worker is never swept (no heartbeat)")
}

func TestDataPlaneRuleFile(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/monitoring/vaultaire-dataplane.yml")
	require.NoError(t, err)
	var file struct {
		Groups []struct {
			Rules []struct {
				Alert  string            `yaml:"alert"`
				Expr   string            `yaml:"expr"`
				For    string            `yaml:"for"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &file))
	require.Len(t, file.Groups, 1)
	rules := map[string]int{}
	for i, r := range file.Groups[0].Rules {
		rules[r.Alert] = i
	}
	i, ok := rules["DataPlaneHostDown"]
	require.True(t, ok)
	r := file.Groups[0].Rules[i]
	assert.Equal(t, "warning", r.Labels["severity"])
	assert.Equal(t, "5m", r.For)
	assert.Contains(t, r.Expr, "vaultaire_data_plane_host_up")
	tf, err := os.ReadFile("../../deploy/monitoring/tests/vaultaire-dataplane_test.yml")
	require.NoError(t, err, "a promtool unit test rides beside the file")
	assert.Contains(t, string(tf), "vaultaire-dataplane.yml")
	assert.True(t, strings.Contains(string(tf), "DataPlaneHostDown"))
}
