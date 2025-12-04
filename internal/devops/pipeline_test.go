// internal/devops/pipeline_test.go
package devops

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPipelineConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &PipelineConfig{
			Name:    "build-deploy",
			Trigger: TriggerPush,
			Stages: []StageConfig{
				{Name: "build", Jobs: []JobConfig{{Name: "compile"}}},
			},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &PipelineConfig{Trigger: TriggerPush}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects empty stages", func(t *testing.T) {
		config := &PipelineConfig{Name: "test", Trigger: TriggerPush}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "stage")
	})
}

func TestNewPipelineEngine(t *testing.T) {
	t.Run("creates engine", func(t *testing.T) {
		engine := NewPipelineEngine(nil)
		assert.NotNil(t, engine)
	})
}

func TestPipelineEngine_Register(t *testing.T) {
	engine := NewPipelineEngine(nil)

	t.Run("registers pipeline", func(t *testing.T) {
		config := &PipelineConfig{
			Name:    "test-pipeline",
			Trigger: TriggerPush,
			Stages: []StageConfig{
				{Name: "test", Jobs: []JobConfig{{Name: "unit-tests"}}},
			},
		}

		pipeline, err := engine.Register(config)
		require.NoError(t, err)
		assert.Equal(t, "test-pipeline", pipeline.Name())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		config := &PipelineConfig{
			Name:    "duplicate",
			Trigger: TriggerPush,
			Stages:  []StageConfig{{Name: "s1", Jobs: []JobConfig{{Name: "j1"}}}},
		}
		_, _ = engine.Register(config)
		_, err := engine.Register(config)
		assert.Error(t, err)
	})
}

func TestPipeline_Run(t *testing.T) {
	engine := NewPipelineEngine(nil)

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "simple-pipeline",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "build", Jobs: []JobConfig{{Name: "compile", Script: "echo build"}}},
		},
	})

	t.Run("executes pipeline", func(t *testing.T) {
		ctx := context.Background()
		run, err := pipeline.Run(ctx, &RunOptions{})

		require.NoError(t, err)
		assert.NotEmpty(t, run.ID())
		assert.Equal(t, RunStatusPending, run.Status())
	})
}

func TestPipelineRun_Stages(t *testing.T) {
	engine := NewPipelineEngine(nil)

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "multi-stage",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "build", Jobs: []JobConfig{{Name: "compile"}}},
			{Name: "test", Jobs: []JobConfig{{Name: "unit"}, {Name: "integration"}}},
			{Name: "deploy", Jobs: []JobConfig{{Name: "production"}}},
		},
	})

	t.Run("tracks stage progress", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{})

		stages := run.Stages()
		assert.Len(t, stages, 3)
		assert.Equal(t, "build", stages[0].Name)
	})
}

func TestPipelineRun_Jobs(t *testing.T) {
	engine := NewPipelineEngine(nil)

	jobExecuted := false
	engine.RegisterExecutor("test", func(ctx context.Context, job *Job) error {
		jobExecuted = true
		return nil
	})

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "job-test",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "test", Jobs: []JobConfig{{Name: "run", Executor: "test"}}},
		},
	})

	t.Run("executes jobs", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{})

		// Wait for execution
		_ = run.Wait(ctx)

		assert.True(t, jobExecuted)
	})
}

func TestPipelineRun_ParallelJobs(t *testing.T) {
	engine := NewPipelineEngine(nil)

	var executed sync.Map
	engine.RegisterExecutor("track", func(ctx context.Context, job *Job) error {
		executed.Store(job.Name(), true)
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "parallel-jobs",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{
				Name: "parallel",
				Jobs: []JobConfig{
					{Name: "job-1", Executor: "track"},
					{Name: "job-2", Executor: "track"},
					{Name: "job-3", Executor: "track"},
				},
				Parallel: true,
			},
		},
	})

	t.Run("runs jobs in parallel", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{})
		_ = run.Wait(ctx)

		_, ok := executed.Load("job-1")
		assert.True(t, ok)
		_, ok = executed.Load("job-2")
		assert.True(t, ok)
		_, ok = executed.Load("job-3")
		assert.True(t, ok)
	})
}

func TestPipelineRun_Artifacts(t *testing.T) {
	engine := NewPipelineEngine(nil)

	engine.RegisterExecutor("artifact", func(ctx context.Context, job *Job) error {
		job.AddArtifact("binary", "/tmp/app")
		return nil
	})

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "artifact-test",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "build", Jobs: []JobConfig{{Name: "compile", Executor: "artifact"}}},
		},
	})

	t.Run("collects artifacts", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{})
		_ = run.Wait(ctx)

		artifacts := run.Artifacts()
		assert.NotEmpty(t, artifacts)
	})
}

func TestPipelineRun_Variables(t *testing.T) {
	engine := NewPipelineEngine(nil)

	var capturedEnv string
	engine.RegisterExecutor("env-check", func(ctx context.Context, job *Job) error {
		capturedEnv = job.GetVariable("DEPLOY_ENV")
		return nil
	})

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "var-test",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "deploy", Jobs: []JobConfig{{Name: "run", Executor: "env-check"}}},
		},
	})

	t.Run("passes variables to jobs", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{
			Variables: map[string]string{"DEPLOY_ENV": "production"},
		})
		_ = run.Wait(ctx)

		assert.Equal(t, "production", capturedEnv)
	})
}

func TestPipelineRun_Cancel(t *testing.T) {
	engine := NewPipelineEngine(nil)

	engine.RegisterExecutor("slow", func(ctx context.Context, job *Job) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
			return nil
		}
	})

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "cancel-test",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "slow", Jobs: []JobConfig{{Name: "wait", Executor: "slow"}}},
		},
	})

	t.Run("cancels running pipeline", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{})

		time.Sleep(50 * time.Millisecond)
		err := run.Cancel()

		assert.NoError(t, err)
		assert.Equal(t, RunStatusCanceled, run.Status())
	})
}

func TestPipelineRun_Retry(t *testing.T) {
	engine := NewPipelineEngine(nil)

	attempts := 0
	engine.RegisterExecutor("flaky", func(ctx context.Context, job *Job) error {
		attempts++
		if attempts < 3 {
			return assert.AnError
		}
		return nil
	})

	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "retry-test",
		Trigger: TriggerManual,
		Stages: []StageConfig{
			{Name: "flaky", Jobs: []JobConfig{{Name: "run", Executor: "flaky", Retries: 3}}},
		},
	})

	t.Run("retries failed jobs", func(t *testing.T) {
		ctx := context.Background()
		run, _ := pipeline.Run(ctx, &RunOptions{})
		_ = run.Wait(ctx)

		assert.Equal(t, 3, attempts)
		assert.Equal(t, RunStatusSuccess, run.Status())
	})
}

func TestPipelineEngine_Triggers(t *testing.T) {
	engine := NewPipelineEngine(nil)

	_, _ = engine.Register(&PipelineConfig{
		Name:     "push-trigger",
		Trigger:  TriggerPush,
		Branches: []string{"main", "develop"},
		Stages:   []StageConfig{{Name: "build", Jobs: []JobConfig{{Name: "compile"}}}},
	})

	t.Run("triggers on push", func(t *testing.T) {
		runs := engine.TriggerByEvent(TriggerPush, &TriggerContext{
			Branch: "main",
			Commit: "abc123",
		})
		assert.Len(t, runs, 1)
	})

	t.Run("skips non-matching branch", func(t *testing.T) {
		runs := engine.TriggerByEvent(TriggerPush, &TriggerContext{
			Branch: "feature/test",
		})
		assert.Empty(t, runs)
	})
}

func TestTriggers(t *testing.T) {
	t.Run("defines triggers", func(t *testing.T) {
		assert.Equal(t, "push", TriggerPush)
		assert.Equal(t, "pull_request", TriggerPullRequest)
		assert.Equal(t, "tag", TriggerTag)
		assert.Equal(t, "schedule", TriggerSchedule)
		assert.Equal(t, "manual", TriggerManual)
	})
}

func TestRunStatuses(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "pending", RunStatusPending)
		assert.Equal(t, "running", RunStatusRunning)
		assert.Equal(t, "success", RunStatusSuccess)
		assert.Equal(t, "failed", RunStatusFailed)
		assert.Equal(t, "canceled", RunStatusCanceled)
	})
}

func TestPipelineEngine_GetPipeline(t *testing.T) {
	engine := NewPipelineEngine(nil)
	_, _ = engine.Register(&PipelineConfig{
		Name:    "get-test",
		Trigger: TriggerManual,
		Stages:  []StageConfig{{Name: "s1", Jobs: []JobConfig{{Name: "j1"}}}},
	})

	t.Run("gets pipeline", func(t *testing.T) {
		p := engine.GetPipeline("get-test")
		assert.NotNil(t, p)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		p := engine.GetPipeline("unknown")
		assert.Nil(t, p)
	})
}

func TestPipelineEngine_ListPipelines(t *testing.T) {
	engine := NewPipelineEngine(nil)
	_, _ = engine.Register(&PipelineConfig{Name: "p1", Trigger: TriggerManual, Stages: []StageConfig{{Name: "s1", Jobs: []JobConfig{{Name: "j1"}}}}})
	_, _ = engine.Register(&PipelineConfig{Name: "p2", Trigger: TriggerManual, Stages: []StageConfig{{Name: "s1", Jobs: []JobConfig{{Name: "j1"}}}}})

	t.Run("lists pipelines", func(t *testing.T) {
		pipelines := engine.ListPipelines()
		assert.Len(t, pipelines, 2)
	})
}

func TestPipelineEngine_GetRuns(t *testing.T) {
	engine := NewPipelineEngine(nil)
	pipeline, _ := engine.Register(&PipelineConfig{
		Name:    "runs-test",
		Trigger: TriggerManual,
		Stages:  []StageConfig{{Name: "s1", Jobs: []JobConfig{{Name: "j1"}}}},
	})

	ctx := context.Background()
	_, _ = pipeline.Run(ctx, &RunOptions{})
	_, _ = pipeline.Run(ctx, &RunOptions{})

	t.Run("gets all runs", func(t *testing.T) {
		runs := engine.GetRuns("runs-test")
		assert.Len(t, runs, 2)
	})
}

func TestStageConfig(t *testing.T) {
	t.Run("creates stage config", func(t *testing.T) {
		stage := StageConfig{
			Name:     "build",
			Jobs:     []JobConfig{{Name: "compile"}},
			Parallel: true,
		}
		assert.Equal(t, "build", stage.Name)
		assert.True(t, stage.Parallel)
	})
}

func TestJobConfig(t *testing.T) {
	t.Run("creates job config", func(t *testing.T) {
		job := JobConfig{
			Name:     "unit-tests",
			Script:   "go test ./...",
			Executor: "shell",
			Retries:  2,
			Timeout:  10 * time.Minute,
		}
		assert.Equal(t, "unit-tests", job.Name)
		assert.Equal(t, 2, job.Retries)
	})
}
