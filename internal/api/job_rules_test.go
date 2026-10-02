package api

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert  string            `yaml:"alert"`
			Expr   string            `yaml:"expr"`
			For    string            `yaml:"for"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func readRuleFile(t *testing.T, name string) (ruleFile, string) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/monitoring/" + name)
	require.NoError(t, err)
	var f ruleFile
	require.NoError(t, yaml.Unmarshal(raw, &f))
	return f, string(raw)
}

// The job rule file names series the server really exports and jobs the
// server really registers (WP-R13-3).
func TestJobRuleFile_MatchesTheRegisteredJobsAndTheExportedSeries(t *testing.T) {
	// Arrange: the production job table (NewServer with a database and both
	// Smart-tier backends) plus account_deletion, which joins in Start().
	jf := setupJobsFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("geyser", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.SetPrimary("idrive")
	s := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(), eng, nil, jf.db)
	s.accountDeletion = NewAccountDeletionRunner(jf.db, zap.NewNop(), eng, s.gci, nil, s.accountSvc)
	require.NotNil(t, s.accountDeletion)
	s.jobs.Register(s.accountDeletion.spec())

	file, raw := readRuleFile(t, "vaultaire-jobs.yml")
	require.Len(t, file.Groups, 1)
	rules := map[string]struct{ expr, forDur, severity string }{}
	for _, r := range file.Groups[0].Rules {
		rules[r.Alert] = struct{ expr, forDur, severity string }{r.Expr, r.For, r.Labels["severity"]}
	}

	// Assert: the four rules.
	for _, name := range []string{"JobStale", "PeriodicJobStale", "JobFailing", "AccountDeletionDeferred"} {
		require.Contains(t, rules, name)
		assert.Equal(t, "warning", rules[name].severity, name)
	}
	assert.Contains(t, rules["JobStale"].expr, "> 36 * 3600")
	assert.Contains(t, rules["JobStale"].expr, "vaultaire_job_last_success_timestamp_seconds")
	assert.Contains(t, rules["JobFailing"].expr, `vaultaire_job_runs_total{outcome="error"}`)
	assert.Contains(t, rules["AccountDeletionDeferred"].expr, `vaultaire_account_deletion_tenants_total{result="deferred"}`)
	// A job that has never succeeded reads 0: the staleness rules must give
	// the boot catch-up time to run before they fire.
	assert.Equal(t, "30m", rules["JobStale"].forDur)
	assert.Equal(t, "30m", rules["PeriodicJobStale"].forDur)

	// Every registered job is watched by exactly one staleness rule — the
	// daily ones by JobStale, the interval ones by PeriodicJobStale — and
	// the rules name no job that does not exist.
	jobsIn := func(expr string) map[string]bool {
		m := regexp.MustCompile(`job_name=~"([^"]+)"`).FindStringSubmatch(expr)
		require.NotNil(t, m, expr)
		out := map[string]bool{}
		for _, n := range strings.Split(m[1], "|") {
			out[n] = true
		}
		return out
	}
	daily, periodic := jobsIn(rules["JobStale"].expr), jobsIn(rules["PeriodicJobStale"].expr)
	wantDaily, wantPeriodic := map[string]bool{}, map[string]bool{}
	for _, name := range s.jobs.names() {
		if s.jobs.job(name).spec.daily() {
			wantDaily[name] = true
		} else {
			wantPeriodic[name] = true
		}
	}
	assert.Equal(t, wantDaily, daily, "JobStale watches every daily job")
	assert.Equal(t, wantPeriodic, periodic, "PeriodicJobStale watches every interval job")

	// Every series the file's expressions read is on /metrics, with the
	// labels the rules select on — right after a start, before any run.
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	// (Counters are package globals: other tests of this process may have
	// moved them, so presence is asserted, not a value.)
	for _, series := range []string{
		`vaultaire_job_last_success_timestamp_seconds{job_name="retention"}`,
		`vaultaire_job_last_success_timestamp_seconds{job_name="smart_demotion"}`,
		`vaultaire_job_last_success_timestamp_seconds{job_name="access_log_delivery"}`,
		`vaultaire_job_runs_total{job_name="dedup_gc",outcome="error"}`,
		`vaultaire_job_runs_total{job_name="account_deletion",outcome="ok"}`,
		`vaultaire_account_deletion_tenants_total{result="deferred"}`,
	} {
		assert.True(t, strings.Contains(body, "\n"+series+" "), "exported before any run: %s", series)
	}
	for _, series := range []string{"vaultaire_job_last_success_timestamp_seconds", "vaultaire_job_runs_total", "vaultaire_account_deletion_tenants_total"} {
		assert.True(t, strings.Contains(raw, series), "the rule file documents %s", series)
	}
	assert.False(t, strings.Contains(body, `tenant_id="`), "no tenant label on any series")
	// `job` is Prometheus's own target label: a scraped series that carries
	// one is stored as exported_job (honor_labels is off on prod), and a rule
	// selecting on it matches nothing.
	assert.False(t, regexp.MustCompile(`(?m)^vaultaire_[a-z_]+\{[^}]*\bjob="`).MatchString(body), "no vaultaire series may use the label `job`")
	for _, r := range file.Groups[0].Rules {
		assert.False(t, regexp.MustCompile(`[{,]\s*job\s*=`).MatchString(r.Expr), "%s selects on Prometheus's own job label", r.Alert)
	}
}

// RetentionJobStale moved off the gauge that read 0 after every restart.
func TestSyntheticRuleFile_NoLongerReadsTheInProcessGauge(t *testing.T) {
	file, raw := readRuleFile(t, "vaultaire-synthetic.yml")
	for _, g := range file.Groups {
		for _, r := range g.Rules {
			assert.NotContains(t, r.Expr, "vaultaire_retention_last_run_timestamp_seconds", r.Alert)
			assert.NotEqual(t, "RetentionJobStale", r.Alert, "it is JobStale{job_name=\"retention\"} in vaultaire-jobs.yml")
		}
	}
	assert.NotContains(t, strings.ReplaceAll(raw, "# RetentionJobStale lived here", ""), "vaultaire_retention_last_run_timestamp_seconds")
	require.Len(t, file.Groups, 1, "only the synthetic group is left")
}
