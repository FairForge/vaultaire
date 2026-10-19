package api

// RP14-12 (WP-P13 post-merge review): vaultaire_routing_truth_last_run_missing_ratio
// exported NO series on prod although the collector reads job_runs.result
// and the run had completed — the collector emitted a backend's ratio only
// when it had a verdict (present + missing > 0), and the one backend with
// rows (idrive, 501) had 10 errors and no verdict. A rule on a series that
// does not exist alerts on nothing. Now every backend of the last run has
// its ratio, its rows, its verdicts and its errors; and the NULL-backend
// count of the two head tables is a series at 0 when there is none.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gauge finds one series of a gathered family by its labels.
func gauge(mfs []*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			for k, v := range labels {
				if got[k] != v {
					continue next
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func TestRoutingCollector_ExportsEveryBackendOfTheLastRunAndTheUnknownRows(t *testing.T) {
	// Arrange: the run another process wrote — one backend with a verdict,
	// one the job could not ask (prod 2026-10-18: idrive, 501 rows, 10
	// errors, no verdict), one with no rows — and a head row on a name no
	// driver has.
	f := setupRoutingTruthFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	res := RoutingTruthResult{StartedAt: now.Add(-time.Second), FinishedAt: now, Backends: map[string]*BackendTruth{
		f.hot:    {Rows: 50, Sampled: 50, Present: 49, Missing: 1, MissingRatio: 0.02},
		"idrive": {Rows: 501, Sampled: 10, Errors: 10, Skipped: "10 consecutive errors: the rest of the sample was not asked"},
		"r2":     {},
	}, Unknown: map[string]int64{}, Summary: "test"}
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	recordJobFinish(f.db, f.checker.JobName, now, "ok", res.Summary, 0, raw)
	f.row("b", "k", "onedrive", false)

	// Act: a fresh collector (a process that has just started) is scraped.
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(newRoutingCollector(f.checker))
	mfs, err := reg.Gather()
	require.NoError(t, err)

	// Assert: one ratio per backend of the result, 0 where there is no
	// verdict, with the rows, verdicts and errors that say so.
	for backend, want := range map[string]struct{ ratio, rows, verdicts, errors float64 }{
		f.hot: {0.02, 50, 50, 0}, "idrive": {0, 501, 0, 10}, "r2": {0, 0, 0, 0},
	} {
		lbl := map[string]string{"backend": backend}
		v, ok := gauge(mfs, "vaultaire_routing_truth_last_run_missing_ratio", lbl)
		assert.True(t, ok, "missing_ratio series for %s", backend)
		assert.InDelta(t, want.ratio, v, 1e-9, backend)
		v, ok = gauge(mfs, "vaultaire_routing_truth_last_run_rows", lbl)
		assert.True(t, ok, "rows series for %s", backend)
		assert.Equal(t, want.rows, v, backend)
		v, ok = gauge(mfs, "vaultaire_routing_truth_last_run_verdicts", lbl)
		assert.True(t, ok, "verdicts series for %s", backend)
		assert.Equal(t, want.verdicts, v, backend)
		v, ok = gauge(mfs, "vaultaire_routing_truth_last_run_errors", lbl)
		assert.True(t, ok, "errors series for %s", backend)
		assert.Equal(t, want.errors, v, backend)
	}
	ts, ok := gauge(mfs, "vaultaire_routing_truth_last_run_timestamp_seconds", nil)
	assert.True(t, ok)
	assert.Equal(t, float64(now.Unix()), ts)

	// The unknown rows from the tables, and the NULL count of both head
	// tables as a series even when it is 0.
	v, ok := gauge(mfs, "vaultaire_routing_unknown_backend_rows", map[string]string{"table": "object_head_cache", "backend": "onedrive"})
	assert.True(t, ok, "the row on an unregistered name")
	assert.Equal(t, 1.0, v)
	for _, table := range []string{"object_head_cache", "object_versions"} {
		v, ok := gauge(mfs, "vaultaire_routing_unknown_backend_rows", map[string]string{"table": table, "backend": ""})
		assert.True(t, ok, "%s: the NULL count is a series at 0", table)
		assert.Zero(t, v)
	}
}

func TestRoutingCollector_NoRunYetExportsNoLastRunSeries(t *testing.T) {
	// Arrange: no job_runs row under this name.
	f := setupRoutingTruthFixture(t)

	// Act
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(newRoutingCollector(f.checker))
	mfs, err := reg.Gather()
	require.NoError(t, err)

	// Assert: nothing is invented for a run that never happened; the table
	// gauges are there.
	_, ok := gauge(mfs, "vaultaire_routing_truth_last_run_timestamp_seconds", nil)
	assert.False(t, ok)
	_, ok = gauge(mfs, "vaultaire_routing_unknown_backend_rows", map[string]string{"table": "object_head_cache", "backend": ""})
	assert.True(t, ok)
}
