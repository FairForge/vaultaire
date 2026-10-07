package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// config is every knob of a run (flags in main.go).
type config struct {
	URL, User, Password, Root string
	// URLs / Passwords (-urls, WEBDAV_PASSWORDS): several bridges of one
	// folder, driven through the multi-bridge driver; URL/Password are the
	// first (the raw client's).
	URLs, Passwords []string
	Tenant          string
	Suites          []string
	Out             string
	Cleanup         bool
	Seed            int64

	SmallSizes []int64
	SmallConc  []int
	SmallN     int

	LargeSizes []int64
	LargeConc  []int

	RangeObject int64
	RangeSizes  []int64
	RangeCount  int

	ConsistencyN int

	ListCounts []int
	ListMax    int
	ListConc   int

	LimitPaths      []int
	LimitsFolder    bool
	LimitsFolderMax int
	LimitsConc      int

	ParitySizes      []int64
	ParityK, ParityM int
	ParitySmallN     int
	ParitySmallSize  int64
	ParitySmallConc  int

	CrossN       int
	CrossSize    int64
	CrossPoll    time.Duration
	CrossTimeout time.Duration
	CrossConc    int

	Proc        string
	SpillDir    string
	SampleEvery time.Duration

	// The driver's limits (drivers.WebDAVOption) and the bench's own bound
	// on one operation: a stalled op fails, it never hangs a suite.
	IdleTimeout    time.Duration
	PutTimeout     time.Duration
	MaxConcurrency int
	// LargeConcurrency (multi-bridge): large transfers per bridge and
	// direction (0 = the driver's default, 3).
	LargeConcurrency int
	// StripeMin / StripePiece (multi-bridge): stripe a known-length object
	// this large into pieces (-1 = never, the bench default, so throughput
	// stays comparable with older runs; 0 = the driver's default).
	StripeMin   int64
	StripePiece int64
	Attempts    int
	OpTimeout   time.Duration
}

// defaultSuites run without -run; limits is opt-in (it probes failure modes
// and its folder probe writes 50,001 files), and so is crossbridge (it needs
// -urls with two bridges or more, and waits out Sync's propagation).
var defaultSuites = []string{"small", "large", "range", "consistency", "listing", "parity"}

var allSuites = append(append([]string(nil), defaultSuites...), "limits", "crossbridge")

// benchDriver is what the suites use of a driver: the single-server
// WebDAVDriver (-url) or the multi-bridge one (-urls).
type benchDriver interface {
	engine.Driver
	GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error)
	Stats() drivers.WebDAVStats
}

// Row is one measured operation of one case.
type Row struct {
	Suite   string  `json:"suite"`
	Case    string  `json:"case"`
	Op      string  `json:"op"`
	N       int     `json:"n"`
	Errors  int     `json:"errors"`
	Bytes   int64   `json:"bytes"`
	WallSec float64 `json:"wall_sec"`
	OpsSec  float64 `json:"ops_per_sec"`
	MBps    float64 `json:"mb_per_sec"`
	P50ms   float64 `json:"p50_ms"`
	P95ms   float64 `json:"p95_ms"`
	P99ms   float64 `json:"p99_ms"`
	MaxMs   float64 `json:"max_ms"`
	Note    string  `json:"note,omitempty"`
}

// Report is the JSON written to -out.
type Report struct {
	Started     time.Time          `json:"started"`
	Finished    time.Time          `json:"finished"`
	URL         string             `json:"url"`
	Root        string             `json:"root"`
	Tenant      string             `json:"tenant"`
	RunID       string             `json:"run_id"`
	Host        string             `json:"host"`
	Suites      []string           `json:"suites"`
	Seed        int64              `json:"seed"`
	Rows        []Row              `json:"rows"`
	Mismatches  []string           `json:"mismatches"`
	Errors      []string           `json:"errors"`
	Consistency *ConsistencyResult `json:"consistency,omitempty"`
	Limits      *LimitsResult      `json:"limits,omitempty"`
	Resources   *ResourceResult    `json:"resources,omitempty"`
	Stalls      StallResult        `json:"stalls"`
	Cleanup     string             `json:"cleanup"`
}

// StallResult is what bounded the run: the driver's stalls (no progress for
// -idle-timeout) and retries, and ops cut off by -op-timeout.
type StallResult struct {
	UploadStalls   int64 `json:"upload_stalls"`
	DownloadStalls int64 `json:"download_stalls"`
	Retries        int64 `json:"retries"`
	OpTimeouts     int64 `json:"op_timeouts"`
}

// bench is one run: the real driver, a raw client, and the report.
type bench struct {
	cfg       config
	drv       benchDriver
	bridges   []*drivers.WebDAVDriver // one single-server driver per -urls bridge (crossbridge)
	raw       *rawClient
	ctx       context.Context // carries the tenant
	container string          // the run folder (= the driver's container)
	out       io.Writer

	mu  sync.Mutex
	rep *Report

	opTimeouts atomic.Int64
}

// opCtx is the context of one operation: the run's, bounded by -op-timeout.
func (b *bench) opCtx() (context.Context, context.CancelFunc) {
	if b.cfg.OpTimeout <= 0 {
		return context.WithCancel(b.ctx)
	}
	return context.WithTimeout(b.ctx, b.cfg.OpTimeout)
}

// op runs fn under opCtx.
func (b *bench) op(fn func(ctx context.Context) error) error {
	ctx, cancel := b.opCtx()
	defer cancel()
	return b.noteTimeout(fn(ctx))
}

// noteTimeout counts an op that -op-timeout cut off (not the run ending).
func (b *bench) noteTimeout(err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && b.ctx.Err() == nil {
		b.opTimeouts.Add(1)
	}
	return err
}

// run executes the configured suites and returns the report; it never
// returns early on a suite failure (that is recorded), only on setup errors.
func run(ctx context.Context, cfg config, out io.Writer) (*Report, error) {
	if cfg.Password == "" {
		return nil, errors.New("WEBDAV_PASSWORD is not set")
	}
	for _, s := range cfg.Suites {
		if !contains(allSuites, s) {
			return nil, fmt.Errorf("unknown suite %q (have %s)", s, strings.Join(allSuites, ","))
		}
	}
	limits := []drivers.WebDAVOption{
		drivers.WithWebDAVIdleTimeout(cfg.IdleTimeout),
		drivers.WithWebDAVPutTimeout(cfg.PutTimeout),
		drivers.WithWebDAVMaxConcurrency(cfg.MaxConcurrency),
		drivers.WithWebDAVRetries(cfg.Attempts, -1),
	}
	var drv benchDriver
	var bridges []*drivers.WebDAVDriver
	if len(cfg.URLs) > 0 {
		mc := drivers.WebDAVConfig{User: cfg.User, Root: cfg.Root, LargeConcurrency: cfg.LargeConcurrency,
			StripeMin: cfg.StripeMin, StripePiece: cfg.StripePiece, StagingDir: filepath.Join(os.TempDir(), "webdav-bench-stripes")}
		for i, u := range cfg.URLs {
			mc.Bridges = append(mc.Bridges, drivers.WebDAVBridge{URL: u, Password: cfg.Passwords[i]})
			one, err := drivers.NewWebDAVDriver("webdav-bench", u, cfg.User, cfg.Passwords[i], cfg.Root, zap.NewNop(), limits...)
			if err != nil {
				return nil, fmt.Errorf("bridge %d: %w", i, err)
			}
			bridges = append(bridges, one)
		}
		m, err := drivers.NewMultiWebDAVDriver("webdav-bench", mc, zap.NewNop(), limits...)
		if err != nil {
			return nil, fmt.Errorf("driver: %w", err)
		}
		drv = m
	} else {
		one, err := drivers.NewWebDAVDriver("webdav-bench", cfg.URL, cfg.User, cfg.Password, cfg.Root, zap.NewNop(), limits...)
		if err != nil {
			return nil, fmt.Errorf("driver: %w", err)
		}
		drv = one
	}
	raw, err := newRawClient(cfg.URL, cfg.User, cfg.Password, cfg.OpTimeout)
	if err != nil {
		return nil, err
	}
	out = &lockedWriter{w: out} // workers print concurrently
	host, _ := os.Hostname()
	runID := "run-" + time.Now().UTC().Format("20060102-150405") + "-" + strconv.FormatInt(time.Now().UnixNano()%10000, 10)
	b := &bench{
		cfg:       cfg,
		drv:       drv,
		bridges:   bridges,
		raw:       raw,
		ctx:       common.WithTenantID(ctx, cfg.Tenant),
		container: runID,
		out:       out,
		rep: &Report{
			Started: time.Now().UTC(), URL: reportURL(cfg), Root: cfg.Root, Tenant: cfg.Tenant, RunID: runID,
			Host: host, Suites: cfg.Suites, Seed: cfg.Seed, Mismatches: []string{}, Errors: []string{},
		},
	}
	if err := drv.HealthCheck(b.ctx); err != nil {
		return nil, fmt.Errorf("health check (URL, user, password?): %w", err)
	}
	fprintf(out, "webdav-bench %s → %s root=%s run=%s suites=%s\n", host, reportURL(cfg), cfg.Root, runID, strings.Join(cfg.Suites, ","))

	smp := startSampler(ctx, cfg.Proc, cfg.SpillDir, cfg.SampleEvery)
	suites := map[string]func(){
		"small": b.suiteSmall, "large": b.suiteLarge, "range": b.suiteRange,
		"consistency": b.suiteConsistency, "listing": b.suiteListing,
		"parity": b.suiteParity, "limits": b.suiteLimits, "crossbridge": b.suiteCrossbridge,
	}
	for _, s := range cfg.Suites {
		if ctx.Err() != nil {
			b.fail("interrupted before suite %s", s)
			break
		}
		fprintf(out, "== %s\n", s)
		start := time.Now()
		suites[s]()
		fprintf(out, "== %s done in %s\n", s, time.Since(start).Round(time.Millisecond))
	}
	b.rep.Resources = smp.stop()
	st := drv.Stats()
	b.rep.Stalls = StallResult{UploadStalls: st.UploadStalls, DownloadStalls: st.DownloadStalls,
		Retries: st.Retries, OpTimeouts: b.opTimeouts.Load()}

	if cfg.Cleanup {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		b.rep.Cleanup = b.cleanup(cctx)
		cancel()
	} else {
		b.rep.Cleanup = "kept: " + raw.pathOf(b.runSegs(), true)
	}
	b.rep.Finished = time.Now().UTC()
	return b.rep, nil
}

// runSegs is the run folder: <root>/t-<tenant>/<run>.
func (b *bench) runSegs(more ...string) []string {
	segs := splitPath(b.cfg.Root)
	segs = append(segs, "t-"+b.cfg.Tenant, b.container)
	return append(segs, more...)
}

func (b *bench) cleanup(ctx context.Context) string {
	segs := b.runSegs()
	if err := b.raw.remove(ctx, segs, true); err != nil {
		b.fail("cleanup: %v", err)
		return "failed: " + err.Error()
	}
	there, err := b.raw.exists(ctx, segs, true)
	if err != nil {
		return "deleted (not verified: " + err.Error() + ")"
	}
	if there {
		b.fail("cleanup: %s still there after DELETE", b.raw.pathOf(segs, true))
		return "failed: still there"
	}
	return "deleted " + b.raw.pathOf(segs, true)
}

// lockedWriter serialises the progress lines of concurrent workers.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// mismatch records a byte-verification failure and says so loudly.
func (b *bench) mismatch(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.mu.Lock()
	b.rep.Mismatches = append(b.rep.Mismatches, msg)
	b.mu.Unlock()
	fprintf(b.out, "!!! MISMATCH: %s\n", msg)
}

func (b *bench) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	b.mu.Lock()
	if len(b.rep.Errors) < 200 {
		b.rep.Errors = append(b.rep.Errors, msg)
	}
	b.mu.Unlock()
	fprintf(b.out, "  error: %s\n", msg)
}

func (b *bench) addRow(r Row) {
	b.mu.Lock()
	b.rep.Rows = append(b.rep.Rows, r)
	b.mu.Unlock()
	fprintf(b.out, "  %-12s %-22s %-10s n=%-5d err=%-3d %8.2f ops/s %9.2f MB/s p50=%.1fms p95=%.1fms p99=%.1fms %s\n",
		r.Suite, r.Case, r.Op, r.N, r.Errors, r.OpsSec, r.MBps, r.P50ms, r.P95ms, r.P99ms, r.Note)
}

// pool runs fn(i) for i in [0,n) on conc workers and measures each call.
// The first few errors are recorded through fail.
func (b *bench) pool(n, conc int, label string, fn func(i int) error) ([]time.Duration, int, time.Duration) {
	if conc < 1 {
		conc = 1
	}
	lat := make([]time.Duration, n)
	var next, errs atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1))
				if i >= n || b.ctx.Err() != nil {
					return
				}
				t := time.Now()
				err := b.noteTimeout(fn(i))
				lat[i] = time.Since(t)
				if err != nil {
					if errs.Add(1) <= 3 {
						b.fail("%s #%d: %v", label, i, err)
					}
				}
			}
		}()
	}
	wg.Wait()
	return lat, int(errs.Load()), time.Since(start)
}

// row builds a Row from latencies (zero entries = not run).
func row(suite, cs, op string, lat []time.Duration, errs int, wall time.Duration, bytes int64) Row {
	var done []time.Duration
	for _, l := range lat {
		if l > 0 {
			done = append(done, l)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i] < done[j] })
	r := Row{Suite: suite, Case: cs, Op: op, N: len(done), Errors: errs, Bytes: bytes, WallSec: wall.Seconds()}
	if wall > 0 {
		r.OpsSec = float64(len(done)) / wall.Seconds()
		r.MBps = float64(bytes) / 1e6 / wall.Seconds()
	}
	if len(done) > 0 {
		r.P50ms = ms(pct(done, 0.50))
		r.P95ms = ms(pct(done, 0.95))
		r.P99ms = ms(pct(done, 0.99))
		r.MaxMs = ms(done[len(done)-1])
	}
	return r
}

func pct(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

// printTable is the human summary at the end of a run.
func printTable(w io.Writer, rep *Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fprintln(tw, "SUITE\tCASE\tOP\tN\tERR\tWALL s\tOPS/s\tMB/s\tp50 ms\tp95 ms\tp99 ms\tNOTE\t")
	for _, r := range rep.Rows {
		fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%.2f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%s\t\n",
			r.Suite, r.Case, r.Op, r.N, r.Errors, r.WallSec, r.OpsSec, r.MBps, r.P50ms, r.P95ms, r.P99ms, r.Note)
	}
	_ = tw.Flush()
	if c := rep.Consistency; c != nil {
		fprintf(w, "\nconsistency: %d rounds, stale read-after-write %d, stale overwrite %d, visible after delete %d\n",
			c.Rounds, c.StaleRAW, c.StaleOverwrite, c.StaleDelete)
		for _, e := range c.Events {
			fprintf(w, "  stale %s %s: resolved=%v after %.0f ms\n", e.Kind, e.Key, e.Resolved, e.ResolvedAfterMs)
		}
	}
	if l := rep.Limits; l != nil {
		fprintln(w, "\nlimits:")
		for _, p := range l.Paths {
			fprintf(w, "  path %4d chars: put=%d get=%v %s\n", p.Length, p.PutStatus, p.ReadBack, p.Note)
		}
		for _, n := range l.Names {
			fprintf(w, "  name %-14q put=%d get=%v listed=%v %s\n", n.Name, n.PutStatus, n.ReadBack, n.Listed, n.Note)
		}
		if f := l.Folder; f != nil {
			if f.FirstFailure < 0 {
				fprintf(w, "  folder: created %d of %d, no failure (%.0f s)\n", f.Created, f.Target, f.WallSec)
			} else {
				fprintf(w, "  folder: created %d of %d; FIRST FAILURE at file #%d: status %d %s\n", f.Created, f.Target, f.FirstFailure, f.Status, f.Body)
			}
		}
	}
	if r := rep.Resources; r != nil {
		fprintf(w, "\nresources: %s pid=%d samples=%d peak RSS %.1f MiB, CPU mean %.1f%% peak %.1f%%",
			r.Process, r.PID, r.Samples, float64(r.PeakRSSBytes)/(1<<20), r.MeanCPUPct, r.PeakCPUPct)
		if r.SpillDir != "" {
			fprintf(w, "; spill %s peak %.1f MiB end %.1f MiB", r.SpillDir, float64(r.PeakSpillByte)/(1<<20), float64(r.EndSpillBytes)/(1<<20))
		}
		if r.Note != "" {
			fprintf(w, " (%s)", r.Note)
		}
		fprintln(w)
	}
	s := rep.Stalls
	fprintf(w, "stalls: upload %d, download %d (no progress for the idle timeout); retries %d; ops cut off by -op-timeout %d\n",
		s.UploadStalls, s.DownloadStalls, s.Retries, s.OpTimeouts)
	fprintf(w, "cleanup: %s\n", rep.Cleanup)
	if len(rep.Errors) > 0 {
		fprintf(w, "errors: %d (first: %s)\n", len(rep.Errors), rep.Errors[0])
	}
	if len(rep.Mismatches) > 0 {
		fprintf(w, "\n!!! %d BYTE MISMATCHES — the server returned bytes that were not written:\n", len(rep.Mismatches))
		for _, m := range rep.Mismatches {
			fprintf(w, "!!!   %s\n", m)
		}
	} else {
		fprintln(w, "every read byte-verified (sha256): no mismatch")
	}
}

func writeJSON(path string, rep *Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sizeName is 4KiB / 16MiB / 1GiB.
func sizeName(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}

// parseSize reads 4KiB, 16MiB, 1GiB, 1G, 512K, or plain bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(strings.ToUpper(s), strings.ToUpper(u.suf)) {
			mult = u.m
			s = s[:len(s)-len(u.suf)]
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

func parseSizes(s string) ([]int64, error) {
	var out []int64
	for _, f := range strings.Split(s, ",") {
		if strings.TrimSpace(f) == "" {
			continue
		}
		n, err := parseSize(f)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if strings.TrimSpace(f) == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("bad number %q", f)
		}
		out = append(out, n)
	}
	return out, nil
}

// fprintf / fprintln write progress and the table; a failed write to the
// terminal is not worth stopping a benchmark for.
func fprintf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func fprintln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }
