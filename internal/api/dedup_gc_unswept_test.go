package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/engine"
)

// RP8-1: dedup GC logged one Error line per chunk it could not ask on a
// backend that is known dead — 306 since 2026-10-14 on prod (~100 a night,
// the dead iDrive key), each a real "not swept" the job's note already
// counted. Now: one Warn per backend per run (the error class, the first
// error, the count), the count per backend in job_runs.result
// (`not_swept`) and on vaultaire_dedup_gc_unswept_chunks{backend}; a
// backend whose breaker is open is skipped before its first call (one
// Warn, the count). The run stays ok — an item failure is a note (R13).

// deadChunkDriver answers every Exists with a backend failure.
type deadChunkDriver struct {
	name   string
	exists atomic.Int32
}

func (d *deadChunkDriver) Name() string { return d.name }
func (d *deadChunkDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("503 SlowDown")
}
func (d *deadChunkDriver) Put(context.Context, string, string, io.Reader, ...engine.PutOption) error {
	return errors.New("503 SlowDown")
}
func (d *deadChunkDriver) Delete(context.Context, string, string) error { return errors.New("503") }
func (d *deadChunkDriver) List(context.Context, string, string) ([]string, error) {
	return nil, errors.New("503")
}
func (d *deadChunkDriver) Exists(context.Context, string, string) (bool, error) {
	d.exists.Add(1)
	return false, errors.New("operation error S3: HeadObject, https response error StatusCode: 403, AccessDenied")
}
func (d *deadChunkDriver) HealthCheck(context.Context) error { return errors.New("403") }

// seedUnsweepable writes n GCI rows of its own (a unique scope) on backend,
// marked past the grace; the cleanup removes exactly them.
func seedUnsweepable(t *testing.T, f *adapterTestFixture, backend string, n int) string {
	t.Helper()
	scope := "rp81-" + uuid.NewString()[:8]
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("%064x", i+1)
		_, err := f.db.Exec(`INSERT INTO global_content_index
			(dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count,
			 marked_for_deletion, marked_at, last_accessed_at)
			VALUES ($1, $2, $3, $4, 10, 0, TRUE, NOW() - INTERVAL '8 days', NOW() - INTERVAL '8 days')`,
			scope, hash, backend, "_chunks/"+hash)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1`, scope)
	})
	return scope
}

func gcLogs(logs *observer.ObservedLogs, level zapcore.Level, backend string) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.All() {
		if e.Level != level {
			continue
		}
		if backend == "" || e.ContextMap()["backend"] == backend || strings.Contains(fmt.Sprint(e.ContextMap()), backend) {
			out = append(out, e)
		}
	}
	return out
}

func TestDedupGC_AnUnreachableBackendIsOneWarnPerRunNotOneErrorPerChunk(t *testing.T) {
	_, f := setupGCFixture(t)
	dead := &deadChunkDriver{name: "rp81dead-" + uuid.NewString()[:6]}
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: dead.name})
	eng.AddDriver(dead.name, dead)
	core, logs := observer.New(zap.InfoLevel)
	runner := NewDedupGCRunner(f.db, eng, nil, zap.New(core))
	runner.GracePeriod = 7 * 24 * time.Hour
	seedUnsweepable(t, f, dead.name, 3) // under the breaker's 5 failures

	res, err := runner.RunOnce(context.Background())

	require.NoError(t, err, "an item failure is a note, never the run's error")
	assert.Equal(t, 3, res.NotSwept[dead.name])
	assert.GreaterOrEqual(t, res.Failed, 3)
	assert.Empty(t, gcLogs(logs, zapcore.ErrorLevel, ""), "no Error line for a chunk the backend could not be asked about")
	warns := gcLogs(logs, zapcore.WarnLevel, dead.name)
	require.Len(t, warns, 1, "ONE Warn for the backend this run")
	assert.Equal(t, int64(3), warns[0].ContextMap()["not_swept"])
	assert.Contains(t, warns[0].ContextMap(), "error", "the first error, for the class")
}

func TestDedupGC_ABackendWithAnOpenBreakerIsSkippedBeforeItsFirstCall(t *testing.T) {
	_, f := setupGCFixture(t)
	dead := &deadChunkDriver{name: "rp81open-" + uuid.NewString()[:6]}
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: dead.name})
	eng.AddDriver(dead.name, dead)
	for i := 0; i < 5; i++ { // five failures open the breaker
		_, _ = eng.ExistsOn(engine.ChunkContext(context.Background()), dead.name, chunkContainer, "x")
	}
	require.Error(t, eng.CanAsk(dead.name), "the breaker is open")
	asked := dead.exists.Load()
	core, logs := observer.New(zap.InfoLevel)
	runner := NewDedupGCRunner(f.db, eng, nil, zap.New(core))
	runner.GracePeriod = 7 * 24 * time.Hour
	seedUnsweepable(t, f, dead.name, 7)

	res, err := runner.RunOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, asked, dead.exists.Load(), "not one call to a backend whose breaker is open")
	assert.Equal(t, 7, res.NotSwept[dead.name])
	assert.Empty(t, gcLogs(logs, zapcore.ErrorLevel, ""))
	warns := gcLogs(logs, zapcore.WarnLevel, dead.name)
	require.Len(t, warns, 1)
	assert.Equal(t, int64(7), warns[0].ContextMap()["not_swept"])
}

// The job's result carries the count per backend, and the note says so.
func TestDedupGC_TheRunsResultCarriesNotSweptPerBackend(t *testing.T) {
	_, f := setupGCFixture(t)
	dead := &deadChunkDriver{name: "rp81res-" + uuid.NewString()[:6]}
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: dead.name})
	eng.AddDriver(dead.name, dead)
	runner := NewDedupGCRunner(f.db, eng, nil, zap.NewNop())
	runner.GracePeriod = 7 * 24 * time.Hour
	seedUnsweepable(t, f, dead.name, 2)

	rep, err := runner.spec().Run(context.Background())

	require.NoError(t, err)
	res, ok := rep.Result.(DedupGCResult)
	require.True(t, ok, "the run's result is recorded in job_runs.result")
	assert.Equal(t, 2, res.NotSwept[dead.name])
	assert.Contains(t, rep.Note, dead.name)
}

// vaultaire_dedup_gc_unswept_chunks{backend} is read from job_runs.result —
// a process that has just started reports the run before it (the R13
// lesson: never an in-process value).
func TestDedupGC_TheUnsweptGaugeIsReadFromTheTable(t *testing.T) {
	_, f := setupGCFixture(t)
	runner := NewDedupGCRunner(f.db, f.eng, nil, zap.NewNop())
	runner.JobName = "dedup_gc_test_" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, runner.JobName) })
	_, err := f.db.Exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)`,
		runner.JobName, `{"deleted":0,"failed":793,"not_swept":{"idrive":793}}`)
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	reg.MustRegister(newDedupGCCollector(runner))

	body := scrape(t, reg)

	assert.Contains(t, body, `vaultaire_dedup_gc_unswept_chunks{backend="idrive"} 793`)
}
