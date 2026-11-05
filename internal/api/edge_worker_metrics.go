package api

// The edge_worker_metrics job (WP-LOG-2, D-LOG-5): the Worker's health is
// PULLED from Cloudflare's free GraphQL dataset workersInvocationsAdaptive —
// requests, errors, subrequests and the CPU/wall p99 per script and status
// over the last 15 minutes — every 5 minutes. A data-plane host keeps no log
// (D-LOG-1: invocation logs off), so this is how the origin sees a Worker
// that throws, runs near its CPU limit or stops being invoked. The result
// lives in job_runs.result and the gauges are read from it on scrape (the
// R13 lesson: never an in-process value); a failed call is the run's error.
//
// Registered only with BOTH CLOUDFLARE_ANALYTICS_TOKEN (credential
// cloudflare_analytics_token → _FILE → the variable; an account-scoped,
// read-only Analytics token) and CLOUDFLARE_ACCOUNT_ID (32 hex): neither =
// one Info line; one = one Error line, not registered; a token source set
// but unusable = a refused boot (btcpay_wiring.go's rule). The token is
// never logged and never in a run's error.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/recovery/secret"
)

const (
	edgeWorkerMetricsJobName = "edge_worker_metrics"
	// cloudflareGraphQLURL is the analytics endpoint (verified 2026-11-05).
	cloudflareGraphQLURL     = "https://api.cloudflare.com/client/v4/graphql"
	edgeWorkerMetricsWindow  = 15 * time.Minute
	edgeWorkerMetricsTimeout = 20 * time.Second
	// edgeWorkerMetricsErrorBody is how much of a refused answer the run's
	// error quotes.
	edgeWorkerMetricsErrorBody = 200
)

var cloudflareAccountIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// EdgeWorkerScriptStatus is one (script, status) row of the window.
type EdgeWorkerScriptStatus struct {
	Script      string `json:"script"`
	Status      string `json:"status"`
	Requests    int64  `json:"requests"`
	Errors      int64  `json:"errors"`
	Subrequests int64  `json:"subrequests"`
	// CPUTimeP99Micros and WallTimeP99Micros are Cloudflare's quantiles, in
	// microseconds as the dataset gives them.
	CPUTimeP99Micros  float64 `json:"cpu_time_p99_us"`
	WallTimeP99Micros float64 `json:"wall_time_p99_us"`
}

// EdgeWorkerMetricsResult is a run's job_runs.result.
type EdgeWorkerMetricsResult struct {
	From time.Time                `json:"from"`
	To   time.Time                `json:"to"`
	Rows []EdgeWorkerScriptStatus `json:"rows"`
}

// EdgeWorkerMetricsJob is the job.
type EdgeWorkerMetricsJob struct {
	JobName   string
	AccountID string
	token     string
	// URL is the GraphQL endpoint (a field so the tests use httptest).
	URL    string
	Client *http.Client
	now    func() time.Time
}

// setupEdgeWorkerMetrics reads the two settings and returns the job, or nil
// (one Info line for neither, one Error line for one alone). A token source
// that is set but unusable is fatal (logger.Fatal), as for BTCPay's secrets.
func setupEdgeWorkerMetrics(getenv func(string) string, readFile func(string) ([]byte, error), logger *zap.Logger) *EdgeWorkerMetricsJob {
	tok, err := secret.Load(getenv, readFile, "cloudflare_analytics_token", "CLOUDFLARE_ANALYTICS_TOKEN")
	if err != nil {
		logger.Fatal("Cloudflare analytics token: refusing to start", zap.Error(err))
		return nil
	}
	account := strings.TrimSpace(getenv("CLOUDFLARE_ACCOUNT_ID"))
	haveToken := len(tok.Bytes) > 0
	switch {
	case !haveToken && account == "":
		logger.Info("edge worker metrics off (CLOUDFLARE_ANALYTICS_TOKEN, CLOUDFLARE_ACCOUNT_ID not set)")
		return nil
	case !haveToken:
		logger.Error("edge worker metrics NOT registered: CLOUDFLARE_ACCOUNT_ID is set but CLOUDFLARE_ANALYTICS_TOKEN is not",
			zap.String("missing", "CLOUDFLARE_ANALYTICS_TOKEN"))
		return nil
	case account == "":
		logger.Error("edge worker metrics NOT registered: CLOUDFLARE_ANALYTICS_TOKEN is set but CLOUDFLARE_ACCOUNT_ID is not",
			zap.String("missing", "CLOUDFLARE_ACCOUNT_ID"))
		return nil
	case !cloudflareAccountIDShape.MatchString(account):
		logger.Error("edge worker metrics NOT registered: CLOUDFLARE_ACCOUNT_ID is not 32 lower-case hex characters",
			zap.String("setting", "CLOUDFLARE_ACCOUNT_ID"))
		return nil
	}
	if len(tok.Ignored) > 0 {
		logger.Warn("Cloudflare analytics token: lower-precedence sources also set are ignored", zap.Strings("ignored", tok.Ignored))
	}
	logger.Info("edge worker metrics on: Cloudflare's workersInvocationsAdaptive every 5 minutes", zap.String("token_source", tok.Source))
	return newEdgeWorkerMetricsJob(account, string(tok.Bytes))
}

func newEdgeWorkerMetricsJob(account, token string) *EdgeWorkerMetricsJob {
	return &EdgeWorkerMetricsJob{JobName: edgeWorkerMetricsJobName, AccountID: account, token: token, URL: cloudflareGraphQLURL,
		Client: &http.Client{Timeout: edgeWorkerMetricsTimeout}, now: time.Now}
}

func (j *EdgeWorkerMetricsJob) spec() jobSpec {
	return jobSpec{Name: j.JobName, Every: 5 * time.Minute, BootDelay: 90 * time.Second, MaxRunTime: 30 * time.Second,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := j.RunOnce(ctx)
			if err != nil {
				return jobReport{}, err
			}
			var n int64
			for _, r := range res.Rows {
				n += r.Requests
			}
			return jobReport{Rows: n, Result: res}, nil
		}}
}

// edgeWorkerQuery is the one query (verified live 2026-11-05).
func edgeWorkerQuery(account string, from, to time.Time) string {
	return fmt.Sprintf(`{ viewer { accounts(filter:{accountTag:%q}) { workersInvocationsAdaptive(limit:200, filter:{datetime_geq:%q, datetime_leq:%q}) { sum { requests errors subrequests } quantiles { cpuTimeP99 wallTimeP99 } dimensions { scriptName status } } } } }`,
		account, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
}

// RunOnce asks Cloudflare once. A non-200, a GraphQL errors array or an
// answer that does not parse is the run's error (the status and the first
// 200 bytes of the answer — never the token).
func (j *EdgeWorkerMetricsJob) RunOnce(ctx context.Context) (EdgeWorkerMetricsResult, error) {
	to := j.now().UTC().Truncate(time.Second)
	from := to.Add(-edgeWorkerMetricsWindow)
	res := EdgeWorkerMetricsResult{From: from, To: to, Rows: []EdgeWorkerScriptStatus{}}
	body, err := json.Marshal(map[string]string{"query": edgeWorkerQuery(j.AccountID, from, to)})
	if err != nil {
		return res, err
	}
	cctx, cancel := context.WithTimeout(ctx, edgeWorkerMetricsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, j.URL, bytes.NewReader(body))
	if err != nil {
		return res, fmt.Errorf("cloudflare graphql: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+j.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.Client.Do(req)
	if err != nil {
		return res, fmt.Errorf("cloudflare graphql: %w", j.scrub(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return res, fmt.Errorf("cloudflare graphql: read the answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("cloudflare graphql: status %d: %s", resp.StatusCode, j.quote(raw))
	}
	var answer struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Rows []struct {
						Sum struct {
							Requests    int64 `json:"requests"`
							Errors      int64 `json:"errors"`
							Subrequests int64 `json:"subrequests"`
						} `json:"sum"`
						Quantiles struct {
							CPUTimeP99  float64 `json:"cpuTimeP99"`
							WallTimeP99 float64 `json:"wallTimeP99"`
						} `json:"quantiles"`
						Dimensions struct {
							ScriptName string `json:"scriptName"`
							Status     string `json:"status"`
						} `json:"dimensions"`
					} `json:"workersInvocationsAdaptive"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return res, fmt.Errorf("cloudflare graphql: the answer does not parse: %s", j.quote(raw))
	}
	if len(answer.Errors) > 0 {
		return res, fmt.Errorf("cloudflare graphql: errors: %s", j.quote(raw))
	}
	if len(answer.Data.Viewer.Accounts) == 0 {
		return res, fmt.Errorf("cloudflare graphql: no account in the answer (is the token scoped to %s?)", j.AccountID)
	}
	byKey := map[string]*EdgeWorkerScriptStatus{}
	for _, a := range answer.Data.Viewer.Accounts {
		for _, r := range a.Rows {
			k := r.Dimensions.ScriptName + "\x00" + r.Dimensions.Status
			row := byKey[k]
			if row == nil {
				row = &EdgeWorkerScriptStatus{Script: r.Dimensions.ScriptName, Status: r.Dimensions.Status}
				byKey[k] = row
			}
			row.Requests += r.Sum.Requests
			row.Errors += r.Sum.Errors
			row.Subrequests += r.Sum.Subrequests
			row.CPUTimeP99Micros = max(row.CPUTimeP99Micros, r.Quantiles.CPUTimeP99)
			row.WallTimeP99Micros = max(row.WallTimeP99Micros, r.Quantiles.WallTimeP99)
		}
	}
	for _, r := range byKey {
		res.Rows = append(res.Rows, *r)
	}
	sort.Slice(res.Rows, func(a, b int) bool {
		if res.Rows[a].Script != res.Rows[b].Script {
			return res.Rows[a].Script < res.Rows[b].Script
		}
		return res.Rows[a].Status < res.Rows[b].Status
	})
	return res, nil
}

// quote is the first 200 bytes of an answer, the token removed.
func (j *EdgeWorkerMetricsJob) quote(raw []byte) string {
	s := string(raw)
	if len(s) > edgeWorkerMetricsErrorBody {
		s = s[:edgeWorkerMetricsErrorBody]
	}
	if j.token != "" {
		s = strings.ReplaceAll(s, j.token, "[token]")
	}
	return s
}

// scrub removes the token from a transport error's text.
func (j *EdgeWorkerMetricsJob) scrub(err error) error {
	if j.token == "" || !strings.Contains(err.Error(), j.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), j.token, "[token]"))
}

var (
	edgeWorkerRequestsDesc = prometheus.NewDesc("vaultaire_edge_worker_requests",
		"Worker invocations of the last 15 minutes by script and status (Cloudflare's workersInvocationsAdaptive: success, clientDisconnected, scriptThrewException, exceededCpu, …) — the last edge_worker_metrics run, read from job_runs.result on scrape.",
		[]string{"script", "status"}, nil)
	edgeWorkerCPUDesc = prometheus.NewDesc("vaultaire_edge_worker_cpu_p99_seconds",
		"The p99 CPU time of a Worker invocation of the last 15 minutes by script and status, in seconds — the last edge_worker_metrics run.",
		[]string{"script", "status"}, nil)
)

// edgeWorkerCollector reads the last run's result on scrape (cached 15 s; a
// failed read keeps the last values; no series before the first run).
type edgeWorkerCollector struct {
	db      *sql.DB
	jobName string
	logger  *zap.Logger

	mu   sync.Mutex
	at   time.Time
	rows []EdgeWorkerScriptStatus
}

func newEdgeWorkerCollector(db *sql.DB, jobName string, logger *zap.Logger) *edgeWorkerCollector {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &edgeWorkerCollector{db: db, jobName: jobName, logger: logger}
}

func (c *edgeWorkerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- edgeWorkerRequestsDesc
	ch <- edgeWorkerCPUDesc
}

func (c *edgeWorkerCollector) Collect(ch chan<- prometheus.Metric) {
	for _, r := range c.read() {
		ch <- prometheus.MustNewConstMetric(edgeWorkerRequestsDesc, prometheus.GaugeValue, float64(r.Requests), r.Script, r.Status)
		ch <- prometheus.MustNewConstMetric(edgeWorkerCPUDesc, prometheus.GaugeValue, r.CPUTimeP99Micros/1e6, r.Script, r.Status)
	}
}

func (c *edgeWorkerCollector) read() []EdgeWorkerScriptStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (!c.at.IsZero() && time.Since(c.at) < 15*time.Second) {
		return c.rows
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var raw []byte
	err := c.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, c.jobName).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.rows = nil
	case err != nil:
		c.logger.Warn("edge worker metrics: could not read job_runs — keeping the last values", zap.Error(err))
	default:
		var res EdgeWorkerMetricsResult
		if len(raw) > 0 && json.Unmarshal(raw, &res) == nil {
			c.rows = res.Rows
		}
	}
	return c.rows
}
