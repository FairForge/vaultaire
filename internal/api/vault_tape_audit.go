package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// The tape audit (WP-VAULT-2 A4, docs/reviews/WP-VAULT-2.md § 7): the parity
// scrub covers the second copy, never the first. Once every AuditEvery the
// vault_tape_audit job reads ONE random vault object back from its own
// backend and hashes the stored bytes against what protect read
// (vault_parity.object_sha256, bound to the head row's etag by digest_etag).
// An object on tape is restored first: the job asks, records the object as
// pending in job_runs.result, and reads it on the first hourly run after the
// restore landed — a restore that has not landed in RestoreTimeout is an
// error. The restored copy is read once and never served or billed.
//
// Outcomes: ok | mismatch (the bytes are not what was stored — page-worthy,
// it warns) | error (no answer, a refusal, a restore that never landed) |
// nothing (no auditable object: counted as a clean audit, so an empty attic
// never reads as a stale one) | waiting | not_due | changed (the object was
// overwritten or deleted while its restore ran: dropped, the next run picks
// another). A finding is never a failed run; only an unreadable table is.

const (
	defaultTapeAuditEvery   = 7 * 24 * time.Hour
	defaultTapeAuditTimeout = 48 * time.Hour
	// defaultTapeAuditMaxBytes: the largest object an audit reads (through
	// SLC, 20–40 MB/s from Geyser: under a minute).
	defaultTapeAuditMaxBytes int64 = 1 << 30
	tapeAuditRestoreDays           = 1
)

var vaultTapeAudits = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_vault_tape_audit_total",
	Help: "Audits of the vault objects' own copy by the vault_tape_audit job (one object a week, restored from tape first when it is there): ok, mismatch (the stored bytes do not hash to what was written), error (no answer, a refusal, a restore that did not land), nothing (no auditable object).",
}, []string{"outcome"})

func init() {
	for _, o := range []string{"ok", "mismatch", "error", "nothing"} {
		vaultTapeAudits.WithLabelValues(o)
	}
}

// tapeAuditPick is the object an audit reads (or waits for).
type tapeAuditPick struct {
	Tenant      string    `json:"tenant"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	Etag        string    `json:"etag"`
	StorageKey  string    `json:"storage_key,omitempty"`
	Backend     string    `json:"backend"`
	Size        int64     `json:"size"` // the STORED length the hash covers
	SHA256      string    `json:"sha256"`
	RequestedAt time.Time `json:"requested_at"`
}

// TapeAuditResult is a run's outcome and the state the next run reads.
type TapeAuditResult struct {
	Outcome     string         `json:"outcome"`
	LastOKAt    time.Time      `json:"last_ok_at,omitempty"`
	LastAuditAt time.Time      `json:"last_audit_at,omitempty"`
	Pending     *tapeAuditPick `json:"pending,omitempty"`
	Object      string         `json:"object,omitempty"`
	FromTape    bool           `json:"from_tape,omitempty"`
	Bytes       int64          `json:"bytes,omitempty"`
	Detail      string         `json:"detail,omitempty"`
}

// TapeAudit is the vault_tape_audit job.
type TapeAudit struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	logger *zap.Logger

	JobName        string
	Every          time.Duration // how often the job looks (1 h): a pending restore is read within the hour it lands
	AuditEvery     time.Duration // how often an object is audited (7 d)
	RestoreTimeout time.Duration // a pending restore older than this is an error (48 h)
	MaxBytes       int64

	mem         TapeAuditResult
	scopeTenant string // tests: the shared database
	now         func() time.Time
}

// NewTapeAudit returns nil without a database or a driver-exposing engine.
func NewTapeAudit(db *sql.DB, eng engine.Engine, logger *zap.Logger) *TapeAudit {
	ce, ok := eng.(*engine.CoreEngine)
	if db == nil || !ok {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &TapeAudit{db: db, eng: ce, logger: logger, JobName: "vault_tape_audit",
		Every: time.Hour, AuditEvery: defaultTapeAuditEvery, RestoreTimeout: defaultTapeAuditTimeout,
		MaxBytes: defaultTapeAuditMaxBytes, now: time.Now}
}

func (a *TapeAudit) spec() jobSpec {
	return jobSpec{Name: a.JobName, Every: a.Every, BootDelay: 5 * time.Minute, MaxRunTime: 30 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := a.RunOnce(ctx)
			rep := jobReport{Result: res}
			switch res.Outcome {
			case "ok":
				rep.Rows = 1
			case "mismatch", "error":
				rep.Note = fmt.Sprintf("tape audit %s: %s — %s", res.Outcome, res.Object, res.Detail)
			}
			return rep, err
		}}
}

// load is the last run's state: job_runs.result, or the in-memory copy when
// the table has none or an older one (tests calling RunOnce directly).
func (a *TapeAudit) load(ctx context.Context) TapeAuditResult {
	st := a.mem
	var raw []byte
	if err := a.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, a.JobName).Scan(&raw); err != nil || len(raw) == 0 {
		return st
	}
	var last TapeAuditResult
	if json.Unmarshal(raw, &last) != nil || last.LastAuditAt.Before(st.LastAuditAt) {
		return st
	}
	return last
}

// RunOnce is one look: a pending restore first, else a new audit when one
// is due.
func (a *TapeAudit) RunOnce(ctx context.Context) (TapeAuditResult, error) {
	last := a.load(ctx)
	res := TapeAuditResult{LastOKAt: last.LastOKAt, LastAuditAt: last.LastAuditAt, Pending: last.Pending}
	defer func() { a.mem = res }()
	now := a.now()
	if p := res.Pending; p != nil {
		res.Object = p.Bucket + "/" + p.Key
		if now.Sub(p.RequestedAt) > a.RestoreTimeout {
			res.Pending = nil
			a.finish(&res, "error", fmt.Sprintf("its restore, asked %s, has not landed in %s", p.RequestedAt.UTC().Format(time.RFC3339), a.RestoreTimeout))
			return res, nil
		}
		still, err := a.unchanged(ctx, *p)
		if err != nil {
			return res, err
		}
		if !still {
			res.Pending, res.Outcome = nil, "changed"
			return res, nil
		}
		a.audit(ctx, &res, *p, true)
		return res, nil
	}
	if !res.LastAuditAt.IsZero() && now.Sub(res.LastAuditAt) < a.AuditEvery {
		res.Outcome = "not_due"
		return res, nil
	}
	pick, err := a.pick(ctx)
	if err != nil {
		return res, err
	}
	if pick == nil {
		res.LastOKAt = now
		a.finish(&res, "nothing", "no vault object with a recorded stored-bytes hash")
		return res, nil
	}
	res.Object = pick.Bucket + "/" + pick.Key
	a.audit(ctx, &res, *pick, false)
	return res, nil
}

// finish records a verdict: the audit is done (the next one is AuditEvery
// later) and counted.
func (a *TapeAudit) finish(res *TapeAuditResult, outcome, detail string) {
	res.Outcome, res.Detail, res.LastAuditAt = outcome, detail, a.now()
	if outcome == "ok" {
		res.LastOKAt = res.LastAuditAt
	}
	vaultTapeAudits.WithLabelValues(outcome).Inc()
	if outcome == "mismatch" || outcome == "error" {
		a.logger.Warn("vault tape audit: "+outcome, zap.String("object", res.Object), zap.String("detail", detail))
	}
}

// pick is one random auditable vault object: its parity row complete for
// the head row's etag with the stored bytes' hash, no larger than MaxBytes.
func (a *TapeAudit) pick(ctx context.Context) (*tapeAuditPick, error) {
	var p tapeAuditPick
	err := a.db.QueryRowContext(ctx, `
		SELECT o.tenant_id, o.bucket, o.object_key, o.etag, COALESCE(o.storage_key, ''), COALESCE(o.backend_name, ''),
		       v.size_bytes, v.object_sha256
		FROM object_head_cache o
		JOIN vault_parity v ON v.tenant_id = o.tenant_id AND v.bucket = o.bucket AND v.object_key = o.object_key
		WHERE o.floor = 'vault' AND NOT o.is_chunked AND v.state = 'complete'
		  AND v.etag = o.etag AND v.digest_etag = o.etag AND v.object_sha256 IS NOT NULL
		  AND v.size_bytes <= $2 AND ($1 = '' OR o.tenant_id = $1)
		ORDER BY random()
		LIMIT 1`, a.scopeTenant, a.MaxBytes).
		Scan(&p.Tenant, &p.Bucket, &p.Key, &p.Etag, &p.StorageKey, &p.Backend, &p.Size, &p.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vault tape audit: pick: %w", err)
	}
	return &p, nil
}

// unchanged: the head row is still the object the pending restore is for.
func (a *TapeAudit) unchanged(ctx context.Context, p tapeAuditPick) (bool, error) {
	var etag, storageKey string
	err := a.db.QueryRowContext(ctx, `
		SELECT etag, COALESCE(storage_key, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, p.Tenant, p.Bucket, p.Key).Scan(&etag, &storageKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vault tape audit: head row: %w", err)
	}
	return etag == p.Etag && storageKey == p.StorageKey, nil
}

// audit reads the object from its recorded backend and hashes it; on tape it
// asks for the restore (once) and leaves the object pending.
func (a *TapeAudit) audit(ctx context.Context, res *TapeAuditResult, p tapeAuditPick, pending bool) {
	tctx := common.WithTenantID(ctx, p.Tenant)
	container, blob := objectBlobAddress(p.Tenant, p.Bucket, p.Key, p.StorageKey)
	rc, err := a.eng.Get(engine.WithExpectedSize(engine.WithRecordedBackend(tctx, p.Backend), p.Size), container, blob)
	if errors.Is(err, engine.ErrArchived) {
		if !pending {
			drv, _ := a.eng.GetDriver(p.Backend)
			restorer, ok := drv.(engine.Restorer)
			if !ok {
				res.Pending = nil
				a.finish(res, "error", fmt.Sprintf("on tape and backend %q cannot restore it", p.Backend))
				return
			}
			if rerr := restorer.RestoreObject(tctx, container, blob, tapeAuditRestoreDays); rerr != nil && !errors.Is(rerr, engine.ErrRestoreAlreadyInProgress) {
				res.Pending = nil
				a.finish(res, "error", "its restore was refused: "+rerr.Error())
				return
			}
			p.RequestedAt = a.now()
		}
		res.Pending, res.Outcome, res.Detail = &p, "waiting", "on tape: restore asked "+p.RequestedAt.UTC().Format(time.RFC3339)
		return
	}
	res.Pending = nil
	res.FromTape = pending
	if err != nil {
		a.finish(res, "error", "read: "+err.Error())
		return
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, p.Size+1))
	res.Bytes = n
	if err != nil {
		a.finish(res, "error", fmt.Sprintf("read at %d of %d bytes: %v", n, p.Size, err))
		return
	}
	if sum := hex.EncodeToString(h.Sum(nil)); n != p.Size || sum != p.SHA256 {
		a.finish(res, "mismatch", fmt.Sprintf("%d bytes with SHA-256 %s; protect read %d with %s", n, sum, p.Size, p.SHA256))
		return
	}
	a.finish(res, "ok", "")
}

// tapeAuditCollector exports the last clean audit from job_runs.result on
// scrape (cached a minute): only the active slot runs the job.
type tapeAuditCollector struct {
	a      *TapeAudit
	desc   *prometheus.Desc
	mu     sync.Mutex
	readAt time.Time
	val    float64
}

func newTapeAuditCollector(a *TapeAudit) *tapeAuditCollector {
	return &tapeAuditCollector{a: a, desc: prometheus.NewDesc("vaultaire_vault_tape_audit_last_ok_timestamp_seconds",
		"When the vault_tape_audit job last read a vault object back clean from its own backend (or found none to audit); 0 = never; read from job_runs.", nil, nil)}
}

func (c *tapeAuditCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *tapeAuditCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if c.readAt.IsZero() || time.Since(c.readAt) >= time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var raw []byte
		err := c.a.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, c.a.JobName).Scan(&raw)
		cancel()
		c.readAt = time.Now()
		var last TapeAuditResult
		switch {
		case errors.Is(err, sql.ErrNoRows):
			c.val = 0
		case err == nil && json.Unmarshal(raw, &last) == nil && !last.LastOKAt.IsZero():
			c.val = float64(last.LastOKAt.Unix())
		}
	}
	v := c.val
	c.mu.Unlock()
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v)
}
