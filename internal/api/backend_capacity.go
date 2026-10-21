package api

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/idrive"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// The backend_capacity job (WP-CAP-1 B, docs/IMPLEMENTATION_PLAN.md
// § WP-CAP): daily at 08:30 UTC, per backend and kind, the bytes OUR tables
// say the backend holds — whole head rows, retained versions, chunk blobs
// (compressed: what the vendor bills), active multipart holdings, parity
// shards, pack files — summed in SQL, never walked; plus, for `idrive`, the
// VENDOR's own numbers through the Reseller API (storage used, egress this
// month). The result lands in job_runs.result and the gauges are read from
// it on scrape (the R13 lesson, metrics_capacity.go); the capacity /
// renewal / egress alerts (deploy/monitoring/vaultaire-capacity.yml) fire
// from them against the backend_plans row. The drift `vendor_used − our
// stored` is the orphan signal the erasure sweep and chunk_orphans cannot
// see from inside.

const backendCapacityJobName = "backend_capacity"

// BackendCapacityRunner is the job.
type BackendCapacityRunner struct {
	db     *sql.DB
	engine *engine.CoreEngine
	logger *zap.Logger
	// JobName: tests use their own (job_runs rows are global).
	JobName    string
	MaxRunTime time.Duration
	// Reseller is the vendor view of `idrive` (nil = the key is not set —
	// a note on an ok run, never a failed one); ResellerUser is the
	// sub-account the prod driver runs as (IDRIVE_RESELLER_USER).
	Reseller     *idrive.ResellerClient
	ResellerUser string

	now func() time.Time
}

// BackendBytes is what one backend holds, by kind, in bytes.
type BackendBytes struct {
	// Objects: whole head rows (object_head_cache by backend_name).
	Objects int64 `json:"objects,omitempty"`
	// Versions: retained non-current versions (the usage.RetainedVersionsFrom
	// rule, so a version sharing the head's blob is not counted twice).
	Versions int64 `json:"versions,omitempty"`
	// Chunks: ready chunk blobs (global_content_index by backend_id,
	// COALESCE(compressed_size, size_bytes) — what the vendor bills).
	Chunks int64 `json:"chunks,omitempty"`
	// Multipart: active uploads' reserved_bytes — parts stage locally but
	// land on the upload's placement; attributed to the engine PRIMARY
	// (the placement of everything the class/region does not move).
	Multipart int64 `json:"multipart,omitempty"`
	// Parity: vault parity shards on this leg (shards recorded × shard_bytes).
	Parity int64 `json:"parity,omitempty"`
	// Packs: live pack files (the pack store).
	Packs int64 `json:"packs,omitempty"`
	Total int64 `json:"total"`
}

// VendorView is what the vendor's own API says: idrive through the Reseller
// API, and every driver that reports its own usage
// (engine.VendorUsageReporter — blomp's container HEAD).
type VendorView struct {
	// UsedBytes = storage_used (decimal GB) × 1e9 on idrive (the raw value
	// rides too); the driver's own byte count elsewhere.
	UsedBytes     int64   `json:"used_bytes"`
	StorageUsedGB float64 `json:"storage_used_gb"`
	// LimitBytes: the most the vendor itself holds before it refuses a
	// write (Blomp's container quota → 507); 0 = it reports none. A hard
	// wall: the BackendVendorQuota rules read it, no plan row needed.
	LimitBytes int64 `json:"limit_bytes,omitempty"`
	// EgressMonthBytes / UploadsMonthBytes: this UTC month to date.
	EgressMonthBytes  int64 `json:"egress_month_bytes"`
	UploadsMonthBytes int64 `json:"uploads_month_bytes"`
	// EgressUnmetered: a driver's own report carries no egress meter — no
	// egress series is exported for it (a 0 would read as "none spent").
	EgressUnmetered bool `json:"egress_unmetered,omitempty"`
	// UnitChecked: the GB interpretation was sane against our stored total
	// (both over 100 MB, within 4× of each other) — a note until then.
	UnitChecked bool   `json:"unit_checked"`
	Note        string `json:"note,omitempty"`
}

// BackendCapacityResult is one run's outcome (job_runs.result).
type BackendCapacityResult struct {
	Backends map[string]*BackendBytes `json:"backends"`
	// RetiredBlobRows: retired_blobs has no size column — the row count per
	// backend says what the collector still owes, not bytes.
	RetiredBlobRows map[string]int64      `json:"retired_blobs_rows,omitempty"`
	Vendor          map[string]VendorView `json:"vendor,omitempty"`
	Notes           []string              `json:"notes,omitempty"`
}

// NewBackendCapacityRunner builds the job; nil without a database.
func NewBackendCapacityRunner(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *BackendCapacityRunner {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BackendCapacityRunner{db: db, engine: eng, logger: logger,
		JobName: backendCapacityJobName, MaxRunTime: 10 * time.Minute, now: time.Now}
}

// spec: daily 08:30 UTC (after entitlement_lapse 07:30), a catch-up seven
// minutes after boot, ten minutes at most. What fails a run: a sum query
// failed. A vendor call that fails, or a key not set, is a note on an ok
// run (the account-admin API has answered 5xx on dead regions).
func (r *BackendCapacityRunner) spec() jobSpec {
	return jobSpec{
		Name: r.JobName, Hour: 8, Minute: 30,
		BootDelay: 7 * time.Minute, MaxRunTime: r.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := r.RunOnce(ctx)
			if err != nil {
				return jobReport{}, err
			}
			rep := jobReport{Rows: int64(len(res.Backends)), Result: res, Note: res.summary()}
			return rep, nil
		},
	}
}

func (res *BackendCapacityResult) summary() string {
	names := make([]string, 0, len(res.Backends))
	for n := range res.Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	s := ""
	for _, n := range names {
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("%s %s", n, formatByteSize(res.Backends[n].Total))
	}
	for _, note := range res.Notes {
		s += "; " + note
	}
	return s
}

// formatByteSize is a short human size for the job note.
func formatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// kindSum is one GROUP BY backend query.
type kindSum struct {
	sql  string
	add  func(b *BackendBytes, v int64)
	name string
}

// backendCapacitySums: every holder of bytes, grouped by backend, summed in
// SQL. The column facts are the prompt's (Prompt 21): object_head_cache
// whole rows by backend_name; retained versions by the one billing rule;
// GCI ready rows by backend_id at the compressed size; vault_parity legs
// unnested (legs[j] is shard j's recorded leg); live packs.
var backendCapacitySums = []kindSum{
	{name: "objects", add: func(b *BackendBytes, v int64) { b.Objects = v },
		sql: `SELECT backend_name, COALESCE(SUM(size_bytes), 0) FROM object_head_cache
		       WHERE NOT is_chunked AND backend_name IS NOT NULL AND backend_name <> '' AND backend_name <> 'chunked'
		       GROUP BY backend_name`},
	{name: "versions", add: func(b *BackendBytes, v int64) { b.Versions = v },
		sql: `SELECT v.backend_name, COALESCE(SUM(v.size_bytes), 0) FROM ` + usage.RetainedVersionsFrom + `
		         AND v.backend_name IS NOT NULL AND v.backend_name <> '' AND v.backend_name <> 'chunked'
		       GROUP BY v.backend_name`},
	{name: "chunks", add: func(b *BackendBytes, v int64) { b.Chunks = v },
		sql: `SELECT backend_id, COALESCE(SUM(COALESCE(compressed_size, size_bytes)), 0) FROM global_content_index
		       WHERE state = 'ready' AND backend_id IS NOT NULL AND backend_id <> ''
		       GROUP BY backend_id`},
	{name: "parity", add: func(b *BackendBytes, v int64) { b.Parity = v },
		sql: `SELECT leg, COALESCE(SUM(shard_bytes), 0) FROM vault_parity CROSS JOIN LATERAL unnest(legs) AS leg
		       GROUP BY leg`},
	{name: "packs", add: func(b *BackendBytes, v int64) { b.Packs = v },
		sql: `SELECT backend, COALESCE(SUM(size), 0) FROM packs WHERE retired_at IS NULL GROUP BY backend`},
}

// multipartHoldingsSQL: active uploads' reserved bytes (091) — attributed
// to the engine primary (see BackendBytes.Multipart).
const multipartHoldingsSQL = `SELECT COALESCE(SUM(reserved_bytes), 0) FROM multipart_uploads WHERE status = 'active'`

// retiredBlobRowsSQL: rows, not bytes (the table records no size).
const retiredBlobRowsSQL = `SELECT backend_id, COUNT(*) FROM retired_blobs GROUP BY backend_id`

// RunOnce is the work (no lock, no job_runs: the scheduler's).
func (r *BackendCapacityRunner) RunOnce(ctx context.Context) (BackendCapacityResult, error) {
	res := BackendCapacityResult{Backends: map[string]*BackendBytes{}}
	if r == nil || r.db == nil {
		return res, fmt.Errorf("backend capacity: not configured")
	}
	get := func(name string) *BackendBytes {
		b := res.Backends[name]
		if b == nil {
			b = &BackendBytes{}
			res.Backends[name] = b
		}
		return b
	}
	for _, k := range backendCapacitySums {
		rows, err := r.db.QueryContext(ctx, k.sql)
		if err != nil {
			return res, fmt.Errorf("backend capacity: sum %s: %w", k.name, err)
		}
		for rows.Next() {
			var backend string
			var v int64
			if err := rows.Scan(&backend, &v); err != nil {
				_ = rows.Close()
				return res, fmt.Errorf("backend capacity: sum %s: scan: %w", k.name, err)
			}
			k.add(get(backend), v)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("backend capacity: sum %s: %w", k.name, err)
		}
		_ = rows.Close()
	}
	var multipart int64
	if err := r.db.QueryRowContext(ctx, multipartHoldingsSQL).Scan(&multipart); err != nil {
		return res, fmt.Errorf("backend capacity: multipart holdings: %w", err)
	}
	if multipart > 0 && r.engine != nil {
		if primary := r.engine.GetPrimary(); primary != "" {
			get(primary).Multipart = multipart
		}
	}
	for _, b := range res.Backends {
		b.Total = b.Objects + b.Versions + b.Chunks + b.Multipart + b.Parity + b.Packs
	}
	rows, err := r.db.QueryContext(ctx, retiredBlobRowsSQL)
	if err != nil {
		return res, fmt.Errorf("backend capacity: retired blobs: %w", err)
	}
	for rows.Next() {
		var backend string
		var n int64
		if err := rows.Scan(&backend, &n); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("backend capacity: retired blobs: scan: %w", err)
		}
		if res.RetiredBlobRows == nil {
			res.RetiredBlobRows = map[string]int64{}
		}
		res.RetiredBlobRows[backend] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, fmt.Errorf("backend capacity: retired blobs: %w", err)
	}
	_ = rows.Close()

	r.vendorView(ctx, &res)
	r.driverVendorViews(ctx, &res)
	return res, nil
}

// vendorUsageTimeout bounds one driver's report (one metadata call).
const vendorUsageTimeout = 30 * time.Second

// driverVendorViews asks every registered driver that reports its own
// usage (engine.VendorUsageReporter) what it holds and its limit. A driver
// that cannot be asked (open breaker) is skipped, and a failed call is a
// NOTE on an ok run, like the Reseller API's.
func (r *BackendCapacityRunner) driverVendorViews(ctx context.Context, res *BackendCapacityResult) {
	if r.engine == nil {
		return
	}
	names := r.engine.GetDriverNames()
	sort.Strings(names)
	for _, name := range names {
		drv, ok := r.engine.GetDriver(name)
		if !ok {
			continue
		}
		rep, ok := drv.(engine.VendorUsageReporter)
		if !ok {
			continue
		}
		if err := r.engine.CanAsk(name); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("%s vendor view skipped: %v", name, err))
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, vendorUsageTimeout)
		u, err := rep.VendorUsage(cctx)
		cancel()
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("%s vendor view failed: %v", name, err))
			continue
		}
		if res.Vendor == nil {
			res.Vendor = map[string]VendorView{}
		}
		// Bytes, not a converted unit: nothing to check.
		res.Vendor[name] = VendorView{UsedBytes: u.UsedBytes, LimitBytes: u.LimitBytes,
			EgressUnmetered: true, UnitChecked: true}
	}
}

// vendorView asks the Reseller API what iDrive itself holds and served this
// month. Any failure here is a NOTE on an ok run.
func (r *BackendCapacityRunner) vendorView(ctx context.Context, res *BackendCapacityResult) {
	if r.Reseller == nil || r.ResellerUser == "" {
		res.Notes = append(res.Notes, "vendor view off: IDRIVE_RESELLER_API_KEY or IDRIVE_RESELLER_USER not set")
		return
	}
	users, err := r.Reseller.Users(ctx, r.ResellerUser)
	if err != nil {
		res.Notes = append(res.Notes, "vendor users call failed: "+err.Error())
		return
	}
	var found *idrive.User
	for i := range users {
		if strings.EqualFold(users[i].Email, r.ResellerUser) {
			found = &users[i]
			break
		}
	}
	if found == nil {
		res.Notes = append(res.Notes, fmt.Sprintf("vendor view: reseller user %q not in /users", r.ResellerUser))
		return
	}
	v := VendorView{StorageUsedGB: found.StorageUsedGB,
		UsedBytes: int64(math.Round(found.StorageUsedGB * 1e9))}
	// The unit is asserted against what our rows say the backend holds: a
	// note until both are big enough to compare (the plan's "assert against
	// a known object on the first run").
	var stored int64
	if b := res.Backends["idrive"]; b != nil {
		stored = b.Total
	}
	const comparable = 100 << 20 // 100 MiB
	if v.UsedBytes > comparable && stored > comparable &&
		v.UsedBytes < 4*stored && stored < 4*v.UsedBytes {
		v.UnitChecked = true
	} else {
		v.Note = "storage_used read as decimal GB; unit unverified against our stored bytes"
	}
	now := r.now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	days, err := r.Reseller.UsageStats(ctx, r.ResellerUser, from, now)
	if err != nil {
		if v.Note != "" {
			v.Note += "; "
		}
		v.Note += "usage_stats failed: " + err.Error()
	} else {
		for _, d := range days {
			v.EgressMonthBytes += d.Downloads
			v.UploadsMonthBytes += d.Uploads
		}
	}
	if res.Vendor == nil {
		res.Vendor = map[string]VendorView{}
	}
	res.Vendor["idrive"] = v
	if v.Note != "" {
		res.Notes = append(res.Notes, "idrive vendor: "+v.Note)
	}
}
