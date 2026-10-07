package api

import (
	"context"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// The job table (WP-R13-3). Every background job of the server is registered
// here with the one scheduler (jobs.go); account_deletion joins in Start(),
// once Stripe, auth and the session store exist.
//
// Daily jobs — due once per day at the UTC time below; a catch-up check
// BootDelay after start, then one check per hour. The times are an hour
// apart so the short ones never start together; a long run (account
// deletion and smart demotion have 6 h ceilings) may still be in progress
// when the next job starts, which is safe: they hold different locks and
// touch different rows (R13 invariant 6; the deletion walk and a demotion of
// the same object are both etag/row-guarded).
//
//	job               UTC    boot   ceiling  a run FAILS (and is retried hourly) when
//	inventory         00:30  +4 m   6 h      the configuration list cannot be read
//	dedup_gc          02:30  +3 m   2 h      the reconcile or the candidate scan fails
//	retention         03:30  +1 m   10 m     any table could not be pruned
//	account_deletion  04:30  +2 m   6 h      a tenant was deferred (Stripe, a backend)
//	routing_truth     05:30  +6 m   1 h      the head table cannot be read (a backend that cannot be asked is a note)
//	smart_demotion    06:30  +5 m   6 h      the tenant list cannot be read
//
// A single report, chunk, object or tenant that fails inside inventory,
// dedup_gc or smart_demotion is a note on an "ok" run, not a failure: it is
// the next day's work, and a daily job must not repeat every hour for it.
//
// Interval jobs — one run BootDelay after start, then one per interval:
//
//	multipart_reaper     1 h    +10 s
//	cdn_rollup           1 h    +20 s
//	idempotency_cleanup  1 h    +30 s
//	sts_cleanup          1 h    +30 s
//	session_cleanup      1 h    +30 s
//	bandwidth_alerts     1 h    +40 s
//	access_log_delivery  5 m    +1 m
//	account_export       1 m    +15 s   (renders pending GDPR exports, WP-R10-3b)
//	vault_parity         2 m    +45 s   (parity shards of vault-floor objects, WP-VAULT-1; only with a leg)
//	pack_gc              1 h    +7 m    (the pack store, internal/packstore; only with the sync backend)
//	stripe_gc            1 h    +9 m    (orphan pieces of striped objects, drivers/webdav_stripe.go; only with the sync backend)
//
// The three cleanups used to wait a full hour after every start before
// their first pass.
func (s *Server) registerJobs() {
	if s.jobs == nil {
		return
	}
	logger := s.logger

	// --- daily ---
	if s.inventoryRunner != nil {
		s.jobs.Register(s.inventoryRunner.spec(s.jobs))
	}
	if s.dedupGCRunner != nil {
		s.jobs.Register(s.dedupGCRunner.spec())
	}
	if s.retention != nil {
		s.jobs.Register(s.retention.spec())
	}
	if s.routingTruth != nil {
		s.jobs.Register(s.routingTruth.spec())
	}
	if s.smartDemotion != nil {
		// The job needs both backends. Without them (a local or CI build, a
		// deployment with no archive) it is not a job of this process: it
		// would fail every hour and read as stale forever.
		_, hot := s.engine.GetDriver(s.smartDemotion.HotBackend)
		_, cold := s.engine.GetDriver(s.smartDemotion.ColdBackend)
		if hot && cold {
			s.jobs.Register(s.smartDemotion.spec())
		} else {
			logger.Info("smart demotion not scheduled: the hot or the cold backend is not registered",
				zap.String("hot", s.smartDemotion.HotBackend), zap.Bool("hot_registered", hot),
				zap.String("cold", s.smartDemotion.ColdBackend), zap.Bool("cold_registered", cold))
		}
	}

	// --- interval ---
	if s.multipartReaper != nil {
		s.jobs.Register(s.multipartReaper.spec())
	}
	if s.cdnAnalytics != nil && s.cdnAnalytics.db != nil {
		s.jobs.Register(jobSpec{Name: "cdn_rollup", Every: time.Hour, BootDelay: 20 * time.Second, MaxRunTime: 5 * time.Minute,
			Run: func(ctx context.Context) (jobReport, error) {
				n, err := s.cdnAnalytics.rollup(ctx)
				return jobReport{Rows: n}, err
			}})
	}
	if s.db != nil {
		_, im := s.jsonAPIMiddleware()
		s.jobs.Register(jobSpec{Name: "idempotency_cleanup", Every: time.Hour, BootDelay: 30 * time.Second, MaxRunTime: 5 * time.Minute,
			Run: func(ctx context.Context) (jobReport, error) {
				n, err := im.cleanupExpired(ctx)
				return jobReport{Rows: n}, err
			}})
		s.jobs.Register(jobSpec{Name: "sts_cleanup", Every: time.Hour, BootDelay: 30 * time.Second, MaxRunTime: 5 * time.Minute,
			Run: func(ctx context.Context) (jobReport, error) {
				n, err := auth.CleanupExpiredSTSTokens(ctx, s.db)
				return jobReport{Rows: n}, err
			}})
	}
	if ds, ok := s.sessionStore.(*dashauth.DBStore); ok {
		s.jobs.Register(jobSpec{Name: "session_cleanup", Every: time.Hour, BootDelay: 30 * time.Second, MaxRunTime: 5 * time.Minute,
			Run: func(ctx context.Context) (jobReport, error) {
				n, err := ds.CleanupExpired(ctx)
				return jobReport{Rows: n}, err
			}})
	}
	if s.bandwidthAlerter != nil && s.bandwidthAlerter.db != nil {
		s.jobs.Register(s.bandwidthAlerter.spec())
	}
	if s.accountExports != nil {
		s.jobs.Register(s.accountExports.spec())
	}
	if s.vaultParity != nil {
		// Needs a free leg to write to; without one (a local build, a
		// deployment with neither permafrost nor lyve) it is not a job of
		// this process — it would fail every pass and read as stale.
		if leg, _, ok := s.vaultParity.Leg(); ok {
			s.jobs.Register(s.vaultParity.spec())
			logger.Info("vault parity job registered", zap.String("leg", leg))
		} else {
			logger.Info("vault parity not scheduled: no parity leg (permafrost or lyve) is registered")
		}
	}
	if s.packGC != nil {
		s.jobs.Register(s.packGC.spec())
	}
	if g := newStripeGC(s.engine, logger); g != nil {
		s.jobs.Register(g.spec())
	}
	// The logging_enabled gate is loaded here, before the first request can
	// be recorded against it; every delivery pass refreshes it.
	if s.accessLogTracker != nil && s.accessLogTracker.PrepareLogDelivery(context.Background(), s.engine) {
		s.jobs.Register(jobSpec{Name: "access_log_delivery", Every: 5 * time.Minute, BootDelay: time.Minute, MaxRunTime: 2 * time.Minute,
			Run: func(ctx context.Context) (jobReport, error) {
				n, err := s.accessLogTracker.deliverLogs(ctx)
				return jobReport{Rows: int64(n)}, err
			}})
	}
}
