// internal/devops/pipeline.go
package devops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Triggers
const (
	TriggerPush        = "push"
	TriggerPullRequest = "pull_request"
	TriggerTag         = "tag"
	TriggerSchedule    = "schedule"
	TriggerManual      = "manual"
)

// Run statuses
const (
	RunStatusPending  = "pending"
	RunStatusRunning  = "running"
	RunStatusSuccess  = "success"
	RunStatusFailed   = "failed"
	RunStatusCanceled = "canceled"
)

// JobConfig configures a job
type JobConfig struct {
	Name     string        `json:"name"`
	Script   string        `json:"script"`
	Executor string        `json:"executor"`
	Retries  int           `json:"retries"`
	Timeout  time.Duration `json:"timeout"`
}

// StageConfig configures a stage
type StageConfig struct {
	Name     string      `json:"name"`
	Jobs     []JobConfig `json:"jobs"`
	Parallel bool        `json:"parallel"`
}

// PipelineConfig configures a pipeline
type PipelineConfig struct {
	Name     string        `json:"name"`
	Trigger  string        `json:"trigger"`
	Branches []string      `json:"branches"`
	Stages   []StageConfig `json:"stages"`
}

// Validate checks configuration
func (c *PipelineConfig) Validate() error {
	if c.Name == "" {
		return errors.New("pipeline: name is required")
	}
	if len(c.Stages) == 0 {
		return errors.New("pipeline: at least one stage is required")
	}
	return nil
}

// Artifact represents a build artifact
type Artifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Job represents a running job
type Job struct {
	id        string
	name      string
	status    string
	variables map[string]string
	artifacts []Artifact
	mu        sync.Mutex
}

// Name returns the job name
func (j *Job) Name() string {
	return j.name
}

// Status returns the job status
func (j *Job) Status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// GetVariable returns a variable value
func (j *Job) GetVariable(name string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.variables[name]
}

// AddArtifact adds an artifact
func (j *Job) AddArtifact(name, path string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.artifacts = append(j.artifacts, Artifact{Name: name, Path: path})
}

// Artifacts returns all artifacts
func (j *Job) Artifacts() []Artifact {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.artifacts
}

// Stage represents a pipeline stage
type Stage struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Jobs   []*Job `json:"jobs"`
}

// RunOptions configures a pipeline run
type RunOptions struct {
	Variables map[string]string
}

// TriggerContext contains trigger information
type TriggerContext struct {
	Branch string
	Commit string
	Tag    string
}

// JobExecutor executes a job
type JobExecutor func(ctx context.Context, job *Job) error

// PipelineRun represents a pipeline execution
type PipelineRun struct {
	id        string
	pipeline  *Pipeline
	status    string
	stages    []*Stage
	artifacts []Artifact
	variables map[string]string
	startedAt time.Time
	endedAt   time.Time
	cancelFn  context.CancelFunc
	mu        sync.Mutex
}

// ID returns the run ID
func (r *PipelineRun) ID() string {
	return r.id
}

// Status returns the run status
func (r *PipelineRun) Status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Stages returns all stages
func (r *PipelineRun) Stages() []*Stage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stages
}

// Artifacts returns all artifacts
func (r *PipelineRun) Artifacts() []Artifact {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.artifacts
}

// Cancel cancels the run
func (r *PipelineRun) Cancel() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cancelFn != nil {
		r.cancelFn()
	}
	r.status = RunStatusCanceled
	r.endedAt = time.Now()
	return nil
}

// Wait waits for the run to complete
func (r *PipelineRun) Wait(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.mu.Lock()
			status := r.status
			r.mu.Unlock()

			if status == RunStatusSuccess || status == RunStatusFailed || status == RunStatusCanceled {
				return nil
			}
		}
	}
}

func (r *PipelineRun) execute(ctx context.Context) {
	r.mu.Lock()
	r.status = RunStatusRunning
	r.startedAt = time.Now()
	r.mu.Unlock()

	success := true

	for _, stage := range r.stages {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.status = RunStatusCanceled
			r.mu.Unlock()
			return
		default:
		}

		if !r.executeStage(ctx, stage) {
			success = false
			break
		}
	}

	r.mu.Lock()
	if success {
		r.status = RunStatusSuccess
	} else {
		r.status = RunStatusFailed
	}
	r.endedAt = time.Now()
	r.mu.Unlock()
}

func (r *PipelineRun) executeStage(ctx context.Context, stage *Stage) bool {
	stageConfig := r.getStageConfig(stage.Name)
	if stageConfig == nil {
		return false
	}

	if stageConfig.Parallel {
		return r.executeJobsParallel(ctx, stage.Jobs)
	}
	return r.executeJobsSequential(ctx, stage.Jobs)
}

func (r *PipelineRun) getStageConfig(name string) *StageConfig {
	for _, s := range r.pipeline.config.Stages {
		if s.Name == name {
			return &s
		}
	}
	return nil
}

func (r *PipelineRun) getJobConfig(name string) *JobConfig {
	for _, s := range r.pipeline.config.Stages {
		for _, j := range s.Jobs {
			if j.Name == name {
				return &j
			}
		}
	}
	return nil
}

func (r *PipelineRun) executeJobsSequential(ctx context.Context, jobs []*Job) bool {
	for _, job := range jobs {
		if !r.executeJob(ctx, job) {
			return false
		}
	}
	return true
}

func (r *PipelineRun) executeJobsParallel(ctx context.Context, jobs []*Job) bool {
	var wg sync.WaitGroup
	results := make(chan bool, len(jobs))

	for _, job := range jobs {
		wg.Add(1)
		go func(j *Job) {
			defer wg.Done()
			results <- r.executeJob(ctx, j)
		}(job)
	}

	wg.Wait()
	close(results)

	for result := range results {
		if !result {
			return false
		}
	}
	return true
}

func (r *PipelineRun) executeJob(ctx context.Context, job *Job) bool {
	jobConfig := r.getJobConfig(job.name)
	retries := 1
	if jobConfig != nil && jobConfig.Retries > 0 {
		retries = jobConfig.Retries
	}

	executor := r.pipeline.engine.getExecutor(jobConfig.Executor)
	if executor == nil {
		// Default no-op executor
		executor = func(ctx context.Context, job *Job) error { return nil }
	}

	for attempt := 0; attempt < retries; attempt++ {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		err := executor(ctx, job)
		if err == nil {
			job.mu.Lock()
			job.status = RunStatusSuccess
			job.mu.Unlock()

			// Collect artifacts
			r.mu.Lock()
			r.artifacts = append(r.artifacts, job.artifacts...)
			r.mu.Unlock()

			return true
		}
	}

	job.mu.Lock()
	job.status = RunStatusFailed
	job.mu.Unlock()
	return false
}

// Pipeline represents a CI/CD pipeline
type Pipeline struct {
	config *PipelineConfig
	runs   []*PipelineRun
	engine *PipelineEngine
	mu     sync.Mutex
}

// Name returns the pipeline name
func (p *Pipeline) Name() string {
	return p.config.Name
}

// Run executes the pipeline
func (p *Pipeline) Run(ctx context.Context, opts *RunOptions) (*PipelineRun, error) {
	if opts == nil {
		opts = &RunOptions{}
	}

	runCtx, cancelFn := context.WithCancel(ctx)

	// Build stages and jobs
	stages := make([]*Stage, len(p.config.Stages))
	for i, sc := range p.config.Stages {
		jobs := make([]*Job, len(sc.Jobs))
		for j, jc := range sc.Jobs {
			jobs[j] = &Job{
				id:        uuid.New().String(),
				name:      jc.Name,
				status:    RunStatusPending,
				variables: opts.Variables,
				artifacts: make([]Artifact, 0),
			}
		}
		stages[i] = &Stage{
			Name:   sc.Name,
			Status: RunStatusPending,
			Jobs:   jobs,
		}
	}

	run := &PipelineRun{
		id:        uuid.New().String(),
		pipeline:  p,
		status:    RunStatusPending,
		stages:    stages,
		artifacts: make([]Artifact, 0),
		variables: opts.Variables,
		cancelFn:  cancelFn,
	}

	p.mu.Lock()
	p.runs = append(p.runs, run)
	p.mu.Unlock()

	go run.execute(runCtx)

	return run, nil
}

// Runs returns all runs
func (p *Pipeline) Runs() []*PipelineRun {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

// EngineConfig configures the pipeline engine
type EngineConfig struct {
	MaxConcurrentRuns int
}

// PipelineEngine manages pipelines
type PipelineEngine struct {
	config    *EngineConfig
	pipelines map[string]*Pipeline
	executors map[string]JobExecutor
	mu        sync.RWMutex
}

// NewPipelineEngine creates a pipeline engine
func NewPipelineEngine(config *EngineConfig) *PipelineEngine {
	if config == nil {
		config = &EngineConfig{MaxConcurrentRuns: 10}
	}

	return &PipelineEngine{
		config:    config,
		pipelines: make(map[string]*Pipeline),
		executors: make(map[string]JobExecutor),
	}
}

// Register registers a pipeline
func (e *PipelineEngine) Register(config *PipelineConfig) (*Pipeline, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.pipelines[config.Name]; exists {
		return nil, fmt.Errorf("pipeline: %s already exists", config.Name)
	}

	pipeline := &Pipeline{
		config: config,
		runs:   make([]*PipelineRun, 0),
		engine: e,
	}

	e.pipelines[config.Name] = pipeline
	return pipeline, nil
}

// RegisterExecutor registers a job executor
func (e *PipelineEngine) RegisterExecutor(name string, executor JobExecutor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executors[name] = executor
}

func (e *PipelineEngine) getExecutor(name string) JobExecutor {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.executors[name]
}

// GetPipeline returns a pipeline by name
func (e *PipelineEngine) GetPipeline(name string) *Pipeline {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.pipelines[name]
}

// ListPipelines returns all pipelines
func (e *PipelineEngine) ListPipelines() []*Pipeline {
	e.mu.RLock()
	defer e.mu.RUnlock()

	pipelines := make([]*Pipeline, 0, len(e.pipelines))
	for _, p := range e.pipelines {
		pipelines = append(pipelines, p)
	}
	return pipelines
}

// GetRuns returns runs for a pipeline
func (e *PipelineEngine) GetRuns(name string) []*PipelineRun {
	e.mu.RLock()
	pipeline, exists := e.pipelines[name]
	e.mu.RUnlock()

	if !exists {
		return nil
	}

	return pipeline.Runs()
}

// TriggerByEvent triggers pipelines by event
func (e *PipelineEngine) TriggerByEvent(trigger string, ctx *TriggerContext) []*PipelineRun {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var runs []*PipelineRun

	for _, pipeline := range e.pipelines {
		if pipeline.config.Trigger != trigger {
			continue
		}

		// Check branch filter
		if len(pipeline.config.Branches) > 0 {
			matched := false
			for _, b := range pipeline.config.Branches {
				if b == ctx.Branch {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		run, err := pipeline.Run(context.Background(), &RunOptions{})
		if err == nil {
			runs = append(runs, run)
		}
	}

	return runs
}
