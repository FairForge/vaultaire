package api

// WP-LOG-2 (D-LOG-5): the Worker's health pulled from Cloudflare's GraphQL
// dataset workersInvocationsAdaptive by the edge_worker_metrics job; the
// result in job_runs.result, the gauges read from it on scrape; a failed
// call is the run's error and the token is in no log line and no error.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	testCFAccount = "0123456789abcdef0123456789abcdef"
	testCFToken   = "cf-analytics-token-SECRET-value-0123"
)

func TestEdgeWorkerMetrics_UnregisteredWithoutBothSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	noFile := func(string) ([]byte, error) { return nil, io.EOF }
	core, logs := observer.New(zap.InfoLevel)
	log := zap.New(core)

	assert.Nil(t, setupEdgeWorkerMetrics(env(map[string]string{}), noFile, log))
	assert.Equal(t, 1, logs.FilterMessageSnippet("edge worker metrics off").Len(), "neither: one Info line")

	assert.Nil(t, setupEdgeWorkerMetrics(env(map[string]string{"CLOUDFLARE_ANALYTICS_TOKEN": testCFToken}), noFile, log))
	assert.Nil(t, setupEdgeWorkerMetrics(env(map[string]string{"CLOUDFLARE_ACCOUNT_ID": testCFAccount}), noFile, log))
	assert.Nil(t, setupEdgeWorkerMetrics(env(map[string]string{"CLOUDFLARE_ANALYTICS_TOKEN": testCFToken, "CLOUDFLARE_ACCOUNT_ID": "not-hex"}), noFile, log))
	assert.Equal(t, 3, logs.FilterLevelExact(zap.ErrorLevel).Len(), "one alone (or a malformed id): one Error line each")

	j := setupEdgeWorkerMetrics(env(map[string]string{"CLOUDFLARE_ANALYTICS_TOKEN": testCFToken, "CLOUDFLARE_ACCOUNT_ID": testCFAccount}), noFile, log)
	require.NotNil(t, j)
	assert.Equal(t, edgeWorkerMetricsJobName, j.JobName)
	sp := j.spec()
	assert.Equal(t, 5*time.Minute, sp.Every)
	assert.Equal(t, 90*time.Second, sp.BootDelay)
	assert.Equal(t, 30*time.Second, sp.MaxRunTime)
	for _, e := range logs.All() {
		assert.NotContains(t, e.Message, testCFToken)
		for _, f := range e.Context {
			assert.NotContains(t, f.String, testCFToken)
		}
	}
}

func cfServer(t *testing.T, status int, body string, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			*seen = r.Header.Get("Authorization") + "\n" + string(b)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestEdgeWorkerMetrics_AGoodAnswerIsTheGauges(t *testing.T) {
	var seen string
	srv := cfServer(t, http.StatusOK, `{"data":{"viewer":{"accounts":[{"workersInvocationsAdaptive":[
		{"dimensions":{"scriptName":"stored-s3-edge","status":"success"},"quantiles":{"cpuTimeP99":3988,"wallTimeP99":736381},"sum":{"errors":0,"requests":32,"subrequests":51}},
		{"dimensions":{"scriptName":"stored-s3-edge","status":"clientDisconnected"},"quantiles":{"cpuTimeP99":250000000,"wallTimeP99":9},"sum":{"errors":0,"requests":2,"subrequests":2}},
		{"dimensions":{"scriptName":"stored-status","status":"success"},"quantiles":{"cpuTimeP99":2067,"wallTimeP99":89039},"sum":{"errors":0,"requests":23,"subrequests":23}}]}]}},"errors":null}`, &seen)
	defer srv.Close()
	j := newEdgeWorkerMetricsJob(testCFAccount, testCFToken)
	j.URL = srv.URL
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	j.now = func() time.Time { return now }

	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Rows, 3)
	assert.Equal(t, now.Add(-15*time.Minute), res.From)
	assert.True(t, strings.HasPrefix(seen, "Bearer "+testCFToken+"\n"))
	assert.Contains(t, seen, `accountTag:\"`+testCFAccount+`\"`)
	assert.Contains(t, seen, `datetime_geq:\"2026-11-05T11:45:00Z\"`)
	assert.Contains(t, seen, "workersInvocationsAdaptive(limit:200")

	// The gauges, read from job_runs.result on scrape.
	db := setupJobsFixture(t).db
	name := "edge_worker_metrics_test_" + strings.ReplaceAll(t.Name(), "/", "_")
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO job_runs (job, last_started_at, last_finished_at, last_outcome, result) VALUES ($1, NOW(), NOW(), 'ok', $2)
		ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, name, raw)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM job_runs WHERE job = $1`, name) })
	reg := prometheus.NewRegistry()
	reg.MustRegister(newEdgeWorkerCollector(db, name, zap.NewNop()))
	fams, err := reg.Gather()
	require.NoError(t, err)
	got := map[string]float64{}
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			k := fam.GetName()
			for _, l := range m.GetLabel() {
				k += "|" + l.GetValue()
			}
			got[k] = m.GetGauge().GetValue()
		}
	}
	assert.Equal(t, 32.0, got["vaultaire_edge_worker_requests|stored-s3-edge|success"])
	assert.Equal(t, 2.0, got["vaultaire_edge_worker_requests|stored-s3-edge|clientDisconnected"])
	assert.Equal(t, 23.0, got["vaultaire_edge_worker_requests|stored-status|success"])
	assert.InDelta(t, 0.003988, got["vaultaire_edge_worker_cpu_p99_seconds|stored-s3-edge|success"], 1e-9, "microseconds → seconds")
	assert.InDelta(t, 250.0, got["vaultaire_edge_worker_cpu_p99_seconds|stored-s3-edge|clientDisconnected"], 1e-9)

	// The job's report carries the result.
	rep, err := j.spec().Run(context.Background())
	require.NoError(t, err)
	_, ok := rep.Result.(EdgeWorkerMetricsResult)
	assert.True(t, ok)
	assert.Equal(t, int64(57), rep.Rows)
}

func TestEdgeWorkerMetrics_ANon200IsTheRunsError(t *testing.T) {
	srv := cfServer(t, http.StatusForbidden, `{"errors":[{"message":"not authorized for `+testCFToken+`"}]}`+strings.Repeat("x", 400), nil)
	defer srv.Close()
	j := newEdgeWorkerMetricsJob(testCFAccount, testCFToken)
	j.URL = srv.URL
	_, err := j.RunOnce(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 403")
	assert.NotContains(t, err.Error(), testCFToken, "the token is in no error")
	assert.Less(t, len(err.Error()), 300, "the first 200 bytes of the answer")
	_, err = j.spec().Run(context.Background())
	assert.Error(t, err, "a failed call fails the run")
}

func TestEdgeWorkerMetrics_AGraphQLErrorIsTheRunsError(t *testing.T) {
	srv := cfServer(t, http.StatusOK, `{"data":null,"errors":[{"message":"unknown field cpuTimeP99","path":["viewer"]}]}`, nil)
	defer srv.Close()
	j := newEdgeWorkerMetricsJob(testCFAccount, testCFToken)
	j.URL = srv.URL
	_, err := j.RunOnce(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field cpuTimeP99")

	bad := cfServer(t, http.StatusOK, `not json`, nil)
	defer bad.Close()
	j.URL = bad.URL
	_, err = j.RunOnce(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not parse")
	assert.NotContains(t, err.Error(), testCFToken)
}

// The WP-LOG-2 rules: each in its file, a warning, on series the server
// exports, with its promtool test beside it.
func TestWPLog2RuleFiles(t *testing.T) {
	dp, dpRaw := readRuleFile(t, "vaultaire-dataplane.yml")
	sy, _ := readRuleFile(t, "vaultaire-synthetic.yml")
	got := map[string]string{}
	for _, f := range []ruleFile{dp, sy} {
		for _, g := range f.Groups {
			for _, r := range g.Rules {
				got[r.Alert] = r.Expr
				if strings.HasPrefix(r.Alert, "DataPlane") || strings.HasPrefix(r.Alert, "EdgeWorker") || r.Alert == "SyntheticEdgeLegFailing" {
					assert.Equal(t, "warning", r.Labels["severity"], r.Alert)
				}
			}
		}
	}
	for name, series := range map[string]string{
		"DataPlaneAdmitsExpiring": `vaultaire_edge_admit_reaper_total{outcome="expired"}`,
		"DataPlaneCommitsFailing": "vaultaire_data_plane_commits_total",
		"DataPlaneHostReporting":  "vaultaire_data_plane_host_reports_total",
		"DataPlaneHostException":  `vaultaire_data_plane_host_reports_total{kind="exception"}`,
		"EdgeWorkerErrors":        "vaultaire_edge_worker_requests",
		"EdgeWorkerCPUNearLimit":  `vaultaire_edge_worker_cpu_p99_seconds{script="stored-s3-edge"} > 240`,
		"DataPlaneEdgePathUnused": "vaultaire_synthetic_check_edge_enabled == 1",
		"SyntheticEdgeLegFailing": "vaultaire_synthetic_check_edge_success == 0",
	} {
		require.Contains(t, got, name)
		assert.Contains(t, got[name], series, name)
	}
	assert.Contains(t, dpRaw, "cpu_ms = 300000", "the CPU rule names the limit it is 80 % of")
	for _, f := range []string{"vaultaire-dataplane_test.yml", "vaultaire-synthetic_test.yml"} {
		raw, err := os.ReadFile("../../deploy/monitoring/tests/" + f)
		require.NoError(t, err, "a promtool unit test rides beside the file")
		for name := range map[string]bool{"DataPlaneAdmitsExpiring": true, "DataPlaneCommitsFailing": true, "SyntheticEdgeLegFailing": true} {
			if strings.Contains(f, "synthetic") == (name == "SyntheticEdgeLegFailing") {
				assert.Contains(t, string(raw), name, f)
			}
		}
	}
	toml, err := os.ReadFile("../../edge/s3-ingest/wrangler.toml")
	require.NoError(t, err)
	assert.Contains(t, string(toml), "cpu_ms = 300000", "EdgeWorkerCPUNearLimit's 240 s is 80 % of this")
}
