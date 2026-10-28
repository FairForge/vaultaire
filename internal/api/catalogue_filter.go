package api

// The catalogue_filter job (WP-EDGE-4 PR D, Stage 1a Prompt 48; plan 10.3;
// daily 09:30 UTC): the Bloom filter of every public chunk the operator
// seeded (public_chunks, migration 114; internal/catalogue's versioned
// format), published on the `r2` backend — the public bucket, under the
// reserved tenant `_catalogue` (no S3 tenant id can take it, like `_probe`)
// — as catalogue/<version>.bloom, then catalogue/latest naming the version
// (written last, so a reader of latest always finds its filter). A data-
// plane host fetches latest, then the filter, once per isolate; a chunk whose
// plain hash the filter holds is sent `public: true` to the index. Without
// the `r2` backend the run is a note (nothing to publish to).

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/catalogue"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

const (
	catalogueFilterJobName = "catalogue_filter"
	// catalogueTenant and catalogueContainer address the published files on
	// the r2 backend: t-_catalogue/catalogue/catalogue/<…>.
	catalogueTenant    = "_catalogue"
	catalogueContainer = "catalogue"
	catalogueLatest    = "catalogue/latest"
	// catalogueFP is the filter's false-positive rate (a false positive
	// costs one index lookup that misses).
	catalogueFP = 0.01
	// catalogueMinN sizes an empty or tiny catalogue's filter.
	catalogueMinN = 1000
)

var (
	catalogueFilterVersion = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_catalogue_filter_version",
		Help: "The version of the public-catalogue filter the catalogue_filter job last published (unix seconds of the build).",
	})
	catalogueFilterChunks = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_catalogue_filter_chunks",
		Help: "Distinct public chunk hashes in the last published public-catalogue filter.",
	})
)

// CatalogueFilterJob is the job.
type CatalogueFilterJob struct {
	s *Server
	// JobName is the job_runs name (tests use their own).
	JobName string
	now     func() time.Time
}

// CatalogueFilterResult is one run, recorded as job_runs.result.
type CatalogueFilterResult struct {
	Version uint64 `json:"version"`
	Chunks  int64  `json:"chunks"`
	Bytes   int    `json:"bytes"`
	Note    string `json:"note,omitempty"`
}

func newCatalogueFilterJob(s *Server) *CatalogueFilterJob {
	if s.db == nil || s.engine == nil {
		return nil
	}
	return &CatalogueFilterJob{s: s, JobName: catalogueFilterJobName, now: time.Now}
}

func (j *CatalogueFilterJob) spec() jobSpec {
	return jobSpec{Name: j.JobName, Hour: 9, Minute: 30, BootDelay: 8 * time.Minute, MaxRunTime: 30 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := j.RunOnce(ctx)
			j.s.log().Info("catalogue filter run", zap.Uint64("version", res.Version), zap.Int64("chunks", res.Chunks),
				zap.Int("bytes", res.Bytes), zap.String("note", res.Note), zap.Error(err))
			return jobReport{Rows: res.Chunks, Result: res, Note: res.Note}, err
		}}
}

// RunOnce builds and publishes the filter.
func (j *CatalogueFilterJob) RunOnce(ctx context.Context) (CatalogueFilterResult, error) {
	var res CatalogueFilterResult
	drv, ok := j.s.engine.GetDriver("r2")
	if !ok {
		res.Note = "no r2 backend registered: nothing to publish the catalogue to"
		return res, nil
	}
	if err := j.s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT plain_hash) FROM public_chunks`).Scan(&res.Chunks); err != nil {
		return res, fmt.Errorf("catalogue: count public chunks: %w", err)
	}
	res.Version = uint64(j.now().Unix()) // #nosec G115 -- a time after 1970
	f, err := catalogue.New(int(max(res.Chunks, catalogueMinN)), catalogueFP, res.Version)
	if err != nil {
		return res, err
	}
	rows, err := j.s.db.QueryContext(ctx, `SELECT DISTINCT plain_hash FROM public_chunks`)
	if err != nil {
		return res, fmt.Errorf("catalogue: read public chunks: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			_ = rows.Close()
			return res, err
		}
		if err := f.Add(h); err != nil {
			j.s.log().Warn("catalogue: a public chunk hash that is no sha256 — skipped", zap.String("hash", h))
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	raw := f.Marshal()
	res.Bytes = len(raw)
	cctx := common.WithTenantID(ctx, catalogueTenant)
	name := fmt.Sprintf("catalogue/%d.bloom", res.Version)
	if err := drv.Put(cctx, catalogueContainer, name, bytes.NewReader(raw), engine.WithContentLength(int64(len(raw)))); err != nil {
		return res, fmt.Errorf("catalogue: publish %s: %w", name, err)
	}
	latest := []byte(strconv.FormatUint(res.Version, 10))
	if err := drv.Put(cctx, catalogueContainer, catalogueLatest, bytes.NewReader(latest), engine.WithContentLength(int64(len(latest)))); err != nil {
		return res, fmt.Errorf("catalogue: publish latest: %w", err)
	}
	catalogueFilterVersion.Set(float64(res.Version))
	catalogueFilterChunks.Set(float64(res.Chunks))
	return res, nil
}
