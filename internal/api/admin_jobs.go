package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Admin view and triggers of the background jobs (WP-R13-3).
//
//	GET  /api/v1/admin/jobs            job_runs joined with the schedule
//	POST /api/v1/admin/jobs/{job}/run  start one run now
//
// and the four older triggers (dedup-gc, smart-demotion, retention,
// account-deletion), which are the same call. A trigger answers
// 202 {"job":…,"status":"started"} as soon as the run holds the job's lock
// and returns: the run continues on the server's context — a closed admin
// tab or HAProxy's 50 s timeout cannot cancel it (Review R13-08), a
// shutdown can. 409 already_running while another run holds the lock. The
// result is in job_runs (GET /api/v1/admin/jobs) and in the log. The one
// synchronous call is smart demotion's ?dry_run=true: it moves nothing and
// its answer is the report.

// adminJobView is one job as the admin API shows it.
type adminJobView struct {
	Job      string `json:"job"`
	Schedule string `json:"schedule"`
	// Registered is false for a job_runs row that no job of this process
	// claims (a job that was removed, or whose backends are not configured).
	Registered     bool       `json:"registered"`
	Running        bool       `json:"running"`
	LastStartedAt  *time.Time `json:"last_started_at"`
	LastFinishedAt *time.Time `json:"last_finished_at"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastOutcome    string     `json:"last_outcome"`
	// LastError is the failure text of an "error" run; on "ok" it is the
	// run's note (items that failed without failing the run).
	LastError    string     `json:"last_error"`
	RowsAffected int64      `json:"rows_affected"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	// Result is the structured result of the last run that wrote one
	// (job_runs.result, migration 074 — routing_truth's counts).
	Result json.RawMessage `json:"result,omitempty"`
}

func timePtr(t time.Time, valid bool) *time.Time {
	if !valid || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// jobViews joins the registered jobs with job_runs.
func (s *jobScheduler) jobViews(ctx context.Context) ([]adminJobView, error) {
	rows, err := jobRunRows(ctx, s.db)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []adminJobView
	seen := map[string]bool{}
	for _, name := range s.names() {
		j := s.job(name)
		r := rows[name]
		seen[name] = true
		out = append(out, adminJobView{
			Job: name, Schedule: j.spec.schedule(), Registered: true,
			Running:        r.Outcome == jobOutcomeRunning,
			LastStartedAt:  timePtr(r.LastStarted.Time, r.LastStarted.Valid),
			LastFinishedAt: timePtr(r.LastFinished.Time, r.LastFinished.Valid),
			LastSuccessAt:  timePtr(r.LastSuccess.Time, r.LastSuccess.Valid),
			LastOutcome:    r.Outcome, LastError: r.Error, RowsAffected: r.Rows, Result: r.Result,
			NextRunAt: timePtr(j.nextRun(now, r.LastStarted.Time), true),
		})
	}
	var rest []string
	for name := range rows {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		r := rows[name]
		out = append(out, adminJobView{
			Job: name, Running: r.Outcome == jobOutcomeRunning,
			LastStartedAt:  timePtr(r.LastStarted.Time, r.LastStarted.Valid),
			LastFinishedAt: timePtr(r.LastFinished.Time, r.LastFinished.Valid),
			LastSuccessAt:  timePtr(r.LastSuccess.Time, r.LastSuccess.Valid),
			LastOutcome:    r.Outcome, LastError: r.Error, RowsAffected: r.Rows,
		})
	}
	return out, nil
}

// handleAdminJobsList serves GET /api/v1/admin/jobs.
func (s *Server) handleAdminJobsList(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		http.Error(w, "jobs not available", http.StatusServiceUnavailable)
		return
	}
	views, err := s.jobs.jobViews(r.Context())
	if err != nil {
		s.logger.Error("admin jobs: read job_runs", zap.Error(err))
		http.Error(w, "could not read job_runs", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "jobs": views})
}

// handleAdminJobRun serves POST /api/v1/admin/jobs/{job}/run.
func (s *Server) handleAdminJobRun(w http.ResponseWriter, r *http.Request) {
	s.triggerJob(w, r, chi.URLParam(r, "job"), http.StatusNotFound)
}

// adminJobTrigger is one of the named triggers that predate the generic one.
func (s *Server) adminJobTrigger(job string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.triggerJob(w, r, job, http.StatusServiceUnavailable)
	}
}

// triggerJob starts one run of a job. missing is the status for a job this
// process does not run (404 on the generic route, 503 on a named one).
//
// requested may come from the URL. It is only a lookup key: nothing below
// writes it back — the name in every answer, audit row and log line is the
// registered job's own (the unknown-job answer used to quote the path
// parameter; CodeQL go/reflected-xss).
func (s *Server) triggerJob(w http.ResponseWriter, r *http.Request, requested string, missing int) {
	j := s.jobs.job(requested)
	if j == nil {
		http.Error(w, "no such job on this server (GET /api/v1/admin/jobs lists them)", missing)
		return
	}
	name := j.spec.Name
	actor, _ := r.Context().Value(userIDKey).(string)

	// The dry run: synchronous, under the job's lock, never recorded as a run.
	if dry, _ := strconv.ParseBool(r.URL.Query().Get("dry_run")); dry {
		if s.smartDemotion == nil || name != s.smartDemotion.JobName {
			http.Error(w, "dry_run is only supported by smart_demotion", http.StatusBadRequest)
			return
		}
		var res SmartDemotionResult
		err := j.WithLock(r.Context(), func(ctx context.Context) error {
			var runErr error
			res, runErr = s.smartDemotion.RunOnce(ctx, true)
			return runErr
		})
		if errors.Is(err, errJobAlreadyRunning) {
			writeJobAlreadyRunning(w, name)
			return
		}
		audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin." + name, Error: err,
			Metadata: map[string]any{"dry_run": true}})
		if err != nil {
			s.logger.Error("smart demotion dry run failed", zap.Error(err))
			http.Error(w, "smart demotion failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
		return
	}

	err := j.StartDetached(r.Context(), nil)
	if errors.Is(err, errJobAlreadyRunning) {
		writeJobAlreadyRunning(w, name)
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin." + name, Error: err,
		Metadata: map[string]any{"started": err == nil}})
	if err != nil {
		s.logger.Error("admin job trigger failed", zap.String("job", name), zap.Error(err))
		http.Error(w, "could not start "+name+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"job": name, "status": "started", "status_url": "/api/v1/admin/jobs",
	})
}
