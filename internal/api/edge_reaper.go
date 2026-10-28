package api

// The admit reaper (WP-EDGE-1 Part A): the `edge_admit_reaper` job, every
// 5 min through the one scheduler. A pending admit past its expiry is a
// request the Worker never reported. It BRANCHES ON op (expireEdgeAdmit):
// a write is the part that landed but was never committed (40.10) — its
// reservation goes back, a PUT's address is queued for the collector on
// its backend (the body may have landed; the collector's blobNamedBy
// re-check keeps an address a late commit named — and a commit never lands
// after the expiry: 410); a GET (Part B) is a read whose commit was lost —
// its admitted size is metered, nothing released, NO orphan_write queued
// (its storage_key is the live object's blob). The row is expired.
// Terminal rows are deleted past their retention: writes after 7 days, GETs
// after 24 h (RP66-6 — nothing later reads a GET row and aws-cli writes one
// per 8 MiB range; the access log keeps the bytes).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	edgeAdmitReaperJobName = "edge_admit_reaper"
	edgeAdmitRetention     = 7 * 24 * time.Hour
	// edgeGetRetention: terminal GET rows go after a day (RP66-6) — nothing
	// later reads them (the access log has the bytes), and aws-cli downloads
	// in 8 MiB ranges, one admit row each. Writes keep the 7-day retention.
	edgeGetRetention   = 24 * time.Hour
	edgeAdmitReapBatch = 500
)

var edgeAdmitReaperTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_edge_admit_reaper_total",
	Help: "Edge admits the reaper handled: expired (a pending admit past its expiry, reservation released), retired (a whole PUT's address queued for the collector), claims_dropped (an expired edge-codec admit: its chunk claims dropped, nothing queued), metered (an expired GET admit, its admitted size metered as egress), deleted (terminal rows past retention), failed.",
}, []string{"outcome"})

func init() {
	for _, o := range []string{"expired", "retired", "claims_dropped", "metered", "deleted", "failed"} {
		edgeAdmitReaperTotal.WithLabelValues(o)
	}
}

// EdgeAdmitReapResult is one run's counts (job_runs.result).
type EdgeAdmitReapResult struct {
	Expired       int   `json:"expired"`
	ReleasedBytes int64 `json:"released_bytes"`
	Retired       int   `json:"retired"`
	ClaimsDropped int   `json:"claims_dropped"` // expired edge-codec admits (WP-EDGE-2): claims dropped, nothing queued
	GetsMetered   int   `json:"gets_metered"`   // expired GET admits (Part B)
	MeteredBytes  int64 `json:"metered_bytes"`  // their admitted sizes, metered as egress
	Deleted       int64 `json:"deleted"`
	Failed        int   `json:"failed"`
	// HostsMarkedDown: data nodes up but unseen for 90 s, marked down
	// (WP-EDGE-3; never a draining one, never the Worker or the origin).
	HostsMarkedDown int `json:"hosts_marked_down,omitempty"`
}

// EdgeAdmitReaper is the job.
type EdgeAdmitReaper struct {
	s       *Server
	JobName string
}

func newEdgeAdmitReaper(s *Server) *EdgeAdmitReaper {
	if s == nil || s.db == nil {
		return nil
	}
	return &EdgeAdmitReaper{s: s, JobName: edgeAdmitReaperJobName}
}

func (r *EdgeAdmitReaper) spec() jobSpec {
	return jobSpec{Name: r.JobName, Every: 5 * time.Minute, BootDelay: 90 * time.Second, MaxRunTime: 4 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := r.RunOnce(ctx)
			rep := jobReport{Rows: int64(res.Expired) + res.Deleted, Result: res}
			if res.Failed > 0 {
				rep.Note = fmt.Sprintf("%d expired admit(s) could not be settled; they stay for the next run", res.Failed)
			}
			return rep, err
		}}
}

// RunOnce handles the admits due now.
func (r *EdgeAdmitReaper) RunOnce(ctx context.Context) (EdgeAdmitReapResult, error) {
	var res EdgeAdmitReapResult
	rows, err := r.s.db.QueryContext(ctx, `SELECT id FROM edge_admits WHERE state = 'pending' AND expires_at < NOW() ORDER BY expires_at LIMIT $1`, edgeAdmitReapBatch)
	if err != nil {
		return res, fmt.Errorf("list expired admits: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("scan expired admit: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("list expired admits: %w", err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		row, expired, err := r.expireOne(ctx, id)
		if err != nil {
			res.Failed++
			edgeAdmitReaperTotal.WithLabelValues("failed").Inc()
			r.s.log().Warn("edge admit reaper: admit not settled", zap.String("admit_id", id), zap.Error(err))
			continue
		}
		if !expired {
			continue // settled meanwhile
		}
		res.Expired++
		edgeAdmitReaperTotal.WithLabelValues("expired").Inc()
		switch {
		case row.Codec:
			// An edge-codec admit holds no whole blob: its expiry dropped
			// its chunk claims and released its reservation (RP68-4).
			res.ReleasedBytes += row.ReservedBytes
			res.ClaimsDropped++
			edgeAdmitReaperTotal.WithLabelValues("claims_dropped").Inc()
		case row.Op == edgeOpGet:
			res.GetsMetered++
			res.MeteredBytes += row.ContentLength
			edgeAdmitReaperTotal.WithLabelValues("metered").Inc()
		case row.Op == edgeOpPut:
			res.ReleasedBytes += row.ReservedBytes
			res.Retired++
			edgeAdmitReaperTotal.WithLabelValues("retired").Inc()
		default:
			res.ReleasedBytes += row.ReservedBytes
		}
	}
	del, err := r.s.db.ExecContext(ctx, `DELETE FROM edge_admits WHERE state IN ('committed', 'failed', 'expired')
		AND finished_at < NOW() - make_interval(secs => CASE WHEN op = 'get' THEN $2::int ELSE $1::int END)`,
		int(edgeAdmitRetention.Seconds()), int(edgeGetRetention.Seconds()))
	if err != nil {
		return res, fmt.Errorf("delete terminal admits: %w", err)
	}
	res.Deleted, _ = del.RowsAffected()
	edgeAdmitReaperTotal.WithLabelValues("deleted").Add(float64(res.Deleted))
	if res.HostsMarkedDown, err = sweepUnseenDataPlaneHosts(ctx, r.s.db); err != nil {
		return res, fmt.Errorf("sweep the data-plane hosts: %w", err)
	}
	if res.HostsMarkedDown > 0 {
		r.s.dataPlaneHosts().invalidate()
		r.s.log().Warn("edge admit reaper: data nodes unseen for 90 s marked down", zap.Int("hosts", res.HostsMarkedDown))
	}
	return res, nil
}

// expireOne expires one pending admit under its lock; expired = false when
// the row was settled meanwhile.
func (r *EdgeAdmitReaper) expireOne(ctx context.Context, id string) (row edgeAdmitRow, expired bool, err error) {
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return row, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err = lockEdgeAdmit(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	if row.State != "pending" || time.Now().Before(row.ExpiresAt) {
		return row, false, nil
	}
	r.s.expireEdgeAdmit(ctx, tx, row)
	if err := tx.Commit(); err != nil {
		return row, false, err
	}
	return row, true, nil
}
